// Package bootstrap creates, inspects and deletes the temporary management
// cluster: a kind cluster with Cluster API and the Hydra provider installed
// (ADR-001 decision 2).
//
// Nothing is recorded locally. The kind cluster's name is the only handle, and
// every run rediscovers what exists -- the cluster, the installed providers,
// the credentials Secret -- and does only what is missing. An interrupted run
// is resumed by running the same command again.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/Petatron/hydra/internal/config"
)

// Versions pinned in this CLI. clusterctl installs exactly these; a provider
// already installed at another version is reported, never changed.
const (
	// NodeImage is the kind node: a Kubernetes version Cluster API v1.14
	// supports for management clusters (kind's own default is newer).
	NodeImage          = "kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0"
	CertManagerVersion = "v1.21.1"
	CAPIVersion        = "v1.14.0"
	HydraVersion       = "v0.1.0"
	// MinHydraVersion is the oldest Hydra provider whose schema accepts what
	// render produces (preDrainHook, PET-88).
	MinHydraVersion = "v0.2.0"

	// HydraComponentsURL is where clusterctl finds the Hydra provider: the
	// pinned release itself, so no "latest" lookup (and no GitHub API call to
	// list releases) is needed.
	//
	// This is clusterctl's repository URL, not a download link: clusterctl
	// requires the form github.com/{owner}/{repo}/releases/{latest|tag}/{file},
	// reads the version from the segment after "releases", and itself fetches
	// .../releases/download/{tag}/{file}. A ".../releases/download/..." URL
	// here would make clusterctl take "download" as the version.
	HydraComponentsURL = "https://github.com/Petatron/cluster-api-provider-hydra/releases/" + HydraVersion + "/infrastructure-components.yaml"

	// CertManagerName keys cert-manager's version among installed providers.
	CertManagerName = "cert-manager"

	// MoveLabel makes clusterctl move carry a Secret no CAPI object owns.
	MoveLabel = "clusterctl.cluster.x-k8s.io/move"

	// FieldManager owns everything this CLI writes.
	FieldManager = "hydra"
)

// Provider is one clusterctl provider, as clusterctl names it.
type Provider struct {
	// Name is the clusterctl inventory name, e.g. "infrastructure-hydra".
	Name string
	// Ref is what clusterctl init takes, e.g. "hydra:v0.1.0".
	Ref string
	// Version is the pinned version.
	Version string
	// Kind says which InitOptions field it goes in.
	Kind ProviderKind
}

// ProviderKind is the clusterctl provider type.
type ProviderKind string

// Provider kinds this CLI installs.
const (
	CoreProvider           ProviderKind = "core"
	BootstrapProvider      ProviderKind = "bootstrap"
	ControlPlaneProvider   ProviderKind = "control-plane"
	InfrastructureProvider ProviderKind = "infrastructure"
)

// Providers returns the pinned provider set, core first.
func Providers() []Provider {
	return []Provider{
		{Name: "cluster-api", Ref: "cluster-api:" + CAPIVersion, Version: CAPIVersion, Kind: CoreProvider},
		{Name: "bootstrap-kubeadm", Ref: "kubeadm:" + CAPIVersion, Version: CAPIVersion, Kind: BootstrapProvider},
		{Name: "control-plane-kubeadm", Ref: "kubeadm:" + CAPIVersion, Version: CAPIVersion, Kind: ControlPlaneProvider},
		{Name: "infrastructure-hydra", Ref: "hydra:" + HydraVersion, Version: HydraVersion, Kind: InfrastructureProvider},
	}
}

// Kind manages kind clusters without touching the user's kubeconfig.
type Kind interface {
	Exists(name string) (bool, error)
	Create(name, nodeImage string) error
	Delete(name string) error
	// KubeConfig returns the cluster's kubeconfig, reachable from this host.
	KubeConfig(name string) (string, error)
}

// Cluster is what bootstrap reads and writes in the management cluster.
type Cluster interface {
	// InstalledProviders maps clusterctl inventory names to installed versions.
	InstalledProviders(ctx context.Context) (map[string]string, error)
	// Init runs clusterctl init for exactly these providers and waits for them.
	Init(ctx context.Context, providers []Provider) error
	// WaitProvidersAvailable waits until every Deployment of every installed
	// provider, and cert-manager's, is Available. Being in clusterctl's
	// inventory only means a provider was applied, not that it runs.
	WaitProvidersAvailable(ctx context.Context) error
	// GetSecret returns what bootstrap needs to judge an existing Secret, or
	// nil when there is none. Values are never returned.
	GetSecret(ctx context.Context, namespace, name string) (*SecretInfo, error)
	// LabelSecret adds one label to an existing Secret without touching data.
	LabelSecret(ctx context.Context, namespace, name, key, value string) error
	// ApplySecret creates or updates an Opaque Secret, and its namespace.
	ApplySecret(ctx context.Context, namespace, name string, data map[string][]byte, labels map[string]string) error
	// Clusters lists every object showing machines are still managed here:
	// Clusters, Machines, HydraClusters and HydraMachines, as "Kind ns/name".
	// It errors rather than answering "none" when it cannot tell.
	Clusters(ctx context.Context) ([]string, error)
}

// SecretInfo describes an existing Secret without its values.
type SecretInfo struct {
	Keys        []string
	Labels      map[string]string
	Terminating bool
}

// ClusterFactory connects to a management cluster from its kubeconfig.
type ClusterFactory func(kubeconfig string) (Cluster, error)

// Bootstrapper runs the bootstrap commands.
type Bootstrapper struct {
	Kind       Kind
	Connect    ClusterFactory
	Out        io.Writer
	NodeImage  string
	ProviderFn func() []Provider
}

// KindName is the bootstrap kind cluster's name for a Hydra cluster.
func KindName(cluster string) string {
	return "hydra-bootstrap-" + cluster
}

func (b *Bootstrapper) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(b.Out, format+"\n", args...)
}

func (b *Bootstrapper) providers() []Provider {
	if b.ProviderFn != nil {
		return b.ProviderFn()
	}
	return Providers()
}

// Options are the inputs to Create beyond hydra.yaml.
type Options struct {
	// Credentials, when set, are written to the cluster's hypervisor
	// credentials Secret (created or replaced). When nil the Secret must
	// already exist in the management cluster.
	Credentials *Credentials
	// NoCredentials brings the cluster up without the credentials Secret:
	// neither required nor written. Only for a cluster about to receive the
	// Secret through clusterctl move (tearing down a self-hosted cluster).
	// It cannot be combined with Credentials.
	NoCredentials bool
}

// ErrNoHypervisor is returned for a config that names no hypervisor.
var ErrNoHypervisor = errors.New("spec.hypervisor is required for bootstrap: the temporary management cluster has no manager-wide hypervisor configuration, so the cluster must name its own (with credentials) and they then move with it at the pivot")

// Create brings the temporary management cluster to the pinned state.
func (b *Bootstrapper) Create(ctx context.Context, c *config.ClusterConfig, opts Options) error {
	h := c.Spec.Hypervisor
	if h == nil {
		return ErrNoHypervisor
	}
	name := KindName(c.Metadata.Name)
	if opts.NoCredentials && opts.Credentials != nil {
		return errors.New("bootstrap: credentials were given for a bootstrap told to take none")
	}

	exists, err := b.Kind.Exists(name)
	if err != nil {
		return fmt.Errorf("looking for kind cluster %s: %w", name, err)
	}
	if exists {
		b.logf("kind cluster %s exists; reusing it", name)
	} else {
		// A new cluster cannot already hold the Secret: fail before minutes of
		// installing, not after.
		if opts.Credentials == nil && !opts.NoCredentials {
			return fmt.Errorf("the first bootstrap needs --hypervisor-credentials <dir> holding %s, %s and %s for Secret %s/%s",
				CACertFile, ClientCertFile, ClientKeyFile, c.Metadata.Namespace, h.CredentialsSecretName)
		}
		b.logf("creating kind cluster %s (%s)", name, b.nodeImage())
		if err := b.Kind.Create(name, b.nodeImage()); err != nil {
			return fmt.Errorf("creating kind cluster %s: %w (kind removes a cluster whose creation failed; run the same command again)", name, err)
		}
	}

	cl, err := b.connect(name)
	if err != nil {
		return fmt.Errorf("%w (if the kind cluster is stopped or broken, remove it with `hydra bootstrap delete --force` and run bootstrap again)", err)
	}

	if err := b.ensureProviders(ctx, cl, "bootstrap cluster "+KindName(c.Metadata.Name), "delete the bootstrap cluster with hydra bootstrap delete, or use a hydra release that pins them"); err != nil {
		return err
	}
	if opts.NoCredentials {
		b.logf("credentials Secret %s/%s not written: it arrives with the move", c.Metadata.Namespace, h.CredentialsSecretName)
	} else if err := b.ensureCredentials(ctx, cl, c, opts); err != nil {
		return err
	}
	b.logf("bootstrap cluster %s is ready", name)
	b.logf("  kubeconfig: hydra bootstrap kubeconfig -f <hydra.yaml>")
	return nil
}

func (b *Bootstrapper) nodeImage() string {
	if b.NodeImage != "" {
		return b.NodeImage
	}
	return NodeImage
}

func (b *Bootstrapper) connect(name string) (Cluster, error) {
	kc, err := b.Kind.KubeConfig(name)
	if err != nil {
		return nil, fmt.Errorf("reading kubeconfig of kind cluster %s: %w", name, err)
	}
	cl, err := b.Connect(kc)
	if err != nil {
		return nil, fmt.Errorf("connecting to kind cluster %s: %w", name, err)
	}
	return cl, nil
}

// EnsureProviders brings any management cluster (the bootstrap one, or a
// pivot target) to the pinned providers: installs what is missing, refuses
// what is at another version, and waits for all of them to be available.
//
// what names the cluster for messages. A version mismatch is refused with no
// advice to delete anything: on a pivot target the objects still live on the
// source, and deleting the source would leave the VMs unmanaged.
func (b *Bootstrapper) EnsureProviders(ctx context.Context, cl Cluster, what string) error {
	return b.ensureProviders(ctx, cl, what, "nothing has been moved, and the source still manages the cluster; upgrade or remove those providers on "+what+" yourself, or use a hydra release that pins them")
}

// ensureProviders installs whichever pinned providers are missing and refuses
// to touch one installed at another version.
func (b *Bootstrapper) ensureProviders(ctx context.Context, cl Cluster, what, remedy string) error {
	installed, err := cl.InstalledProviders(ctx)
	if err != nil {
		return fmt.Errorf("reading installed providers: %w", err)
	}
	var missing []Provider
	var wrong []string
	if v, ok := installed[CertManagerName]; ok && v != CertManagerVersion {
		wrong = append(wrong, fmt.Sprintf("%s is %s, this CLI pins %s", CertManagerName, v, CertManagerVersion))
	}
	for _, p := range b.providers() {
		v, ok := installed[p.Name]
		switch {
		case !ok:
			missing = append(missing, p)
		case v != p.Version:
			wrong = append(wrong, fmt.Sprintf("%s is %s, this CLI pins %s", p.Name, v, p.Version))
		default:
			b.logf("provider %s %s installed", p.Name, v)
		}
	}
	if len(wrong) > 0 {
		return fmt.Errorf("providers on %s at unexpected versions (not changing them; %s): %s", what, remedy, strings.Join(wrong, "; "))
	}
	// cert-manager is installed by clusterctl init, not tracked in its
	// inventory: with every provider present but cert-manager gone, run init
	// with nothing to add, which restores the pinned cert-manager.
	_, haveCertManager := installed[CertManagerName]
	if len(missing) > 0 || !haveCertManager {
		if err := b.install(ctx, cl, missing); err != nil {
			return err
		}
	}
	b.logf("waiting for the providers to be available")
	if err := cl.WaitProvidersAvailable(ctx); err != nil {
		return fmt.Errorf("providers not available: %w (run the same command again to keep waiting)", err)
	}
	return nil
}

func (b *Bootstrapper) install(ctx context.Context, cl Cluster, missing []Provider) error {
	refs := make([]string, 0, len(missing))
	for _, p := range missing {
		refs = append(refs, p.Ref)
	}
	if len(refs) == 0 {
		refs = append(refs, "no providers")
	}
	b.logf("installing %s (cert-manager %s)", strings.Join(refs, ", "), CertManagerVersion)
	if err := cl.Init(ctx, missing); err != nil {
		return fmt.Errorf("clusterctl init: %w (run the same command again to continue)", err)
	}
	return nil
}

func (b *Bootstrapper) ensureCredentials(ctx context.Context, cl Cluster, c *config.ClusterConfig, opts Options) error {
	ns, secret := c.Metadata.Namespace, c.Spec.Hypervisor.CredentialsSecretName
	if opts.Credentials != nil {
		if err := cl.ApplySecret(ctx, ns, secret, opts.Credentials.Data(), map[string]string{MoveLabel: ""}); err != nil {
			return fmt.Errorf("writing Secret %s/%s: %w", ns, secret, err)
		}
		b.logf("hypervisor credentials written to Secret %s/%s (labelled for clusterctl move)", ns, secret)
		return nil
	}
	// No files given: the Secret already there must be usable as it is.
	// Existing is not enough -- one made by hand lacks the move label, and
	// one missing a key or being deleted would fail later, after the pivot.
	again := fmt.Sprintf("pass --hypervisor-credentials <dir> holding %s, %s and %s", CACertFile, ClientCertFile, ClientKeyFile)
	info, err := cl.GetSecret(ctx, ns, secret)
	if err != nil {
		return fmt.Errorf("reading Secret %s/%s: %w", ns, secret, err)
	}
	switch {
	case info == nil:
		return fmt.Errorf("hypervisor credentials Secret %s/%s does not exist; %s", ns, secret, again)
	case info.Terminating:
		return fmt.Errorf("hypervisor credentials Secret %s/%s is being deleted; wait for it to go, then %s", ns, secret, again)
	}
	var missing []string
	for _, k := range []string{CACertFile, ClientCertFile, ClientKeyFile} {
		if !slices.Contains(info.Keys, k) {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("hypervisor credentials Secret %s/%s lacks %s; %s", ns, secret, strings.Join(missing, ", "), again)
	}
	if _, ok := info.Labels[MoveLabel]; !ok {
		if err := cl.LabelSecret(ctx, ns, secret, MoveLabel, ""); err != nil {
			return fmt.Errorf("labelling Secret %s/%s for clusterctl move: %w", ns, secret, err)
		}
		b.logf("hypervisor credentials Secret %s/%s exists; added the clusterctl move label", ns, secret)
		return nil
	}
	b.logf("hypervisor credentials Secret %s/%s exists and is labelled for clusterctl move", ns, secret)
	return nil
}

// Delete removes the temporary management cluster. It refuses while the
// cluster still holds Cluster API Clusters, because deleting it would leave
// their machines running with nothing managing them -- unless force is set.
func (b *Bootstrapper) Delete(ctx context.Context, cluster string, force bool) error {
	name := KindName(cluster)
	exists, err := b.Kind.Exists(name)
	if err != nil {
		return fmt.Errorf("looking for kind cluster %s: %w", name, err)
	}
	if !exists {
		b.logf("kind cluster %s does not exist; nothing to delete", name)
		return nil
	}
	if !force {
		cl, err := b.connect(name)
		if err != nil {
			return fmt.Errorf("%w (use --force to delete without checking for managed clusters)", err)
		}
		clusters, err := cl.Clusters(ctx)
		if err != nil {
			return fmt.Errorf("checking what %s still manages: %w (use --force to delete without checking)", name, err)
		}
		if len(clusters) > 0 {
			slices.Sort(clusters)
			return fmt.Errorf("kind cluster %s still manages %s; deleting it would leave their machines running unmanaged. Pivot them to their own cluster first, or delete them, or pass --force", name, strings.Join(clusters, ", "))
		}
	}
	b.logf("deleting kind cluster %s", name)
	if err := b.Kind.Delete(name); err != nil {
		return fmt.Errorf("deleting kind cluster %s: %w", name, err)
	}
	return nil
}

// KubeConfig returns the temporary management cluster's kubeconfig.
func (b *Bootstrapper) KubeConfig(cluster string) (string, error) {
	name := KindName(cluster)
	exists, err := b.Kind.Exists(name)
	if err != nil {
		return "", fmt.Errorf("looking for kind cluster %s: %w", name, err)
	}
	if !exists {
		return "", fmt.Errorf("kind cluster %s does not exist; run hydra bootstrap first", name)
	}
	return b.Kind.KubeConfig(name)
}
