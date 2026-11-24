package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	clusterctl "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
	clusterctlconfig "sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// capiGroup is Cluster API's core API group.
const (
	capiGroup   = "cluster.x-k8s.io"
	capiVersion = "v1beta2"
)

// certManagerNamespace is where clusterctl installs cert-manager.
const certManagerNamespace = CertManagerName

// overridesFolderKey is clusterctl's config key for its local overrides directory.
const overridesFolderKey = "overridesFolder"

var (
	providerListGVK = schema.GroupVersionKind{Group: "clusterctl.cluster.x-k8s.io", Version: "v1alpha3", Kind: "ProviderList"}
)

// KubeCluster is the real management-cluster client: controller-runtime for
// reads and writes, the clusterctl library for installing providers.
type KubeCluster struct {
	kubeconfig string
	c          client.Client
}

// ConnectKube is a ClusterFactory for real clusters.
func ConnectKube(kubeconfig string) (Cluster, error) {
	rc, err := clientcmd.RESTConfigFromKubeConfig([]byte(kubeconfig))
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	c, err := client.New(rc, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}
	return &KubeCluster{kubeconfig: kubeconfig, c: c}, nil
}

// newKubeCluster wraps an existing client; tests use it with a stub client.
func newKubeCluster(c client.Client) *KubeCluster {
	return &KubeCluster{c: c}
}

// InstalledProviders reads clusterctl's provider inventory. Before clusterctl
// has ever run, its CRD does not exist, which means nothing is installed.
func (k *KubeCluster) InstalledProviders(ctx context.Context) (map[string]string, error) {
	// cert-manager first: it can exist before clusterctl ever ran (installed
	// some other way), and its version must be checked either way.
	out := map[string]string{}
	if v, err := k.certManagerVersion(ctx); err != nil {
		return nil, err
	} else if v != "" {
		out[CertManagerName] = v
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(providerListGVK)
	if err := k.c.List(ctx, list); err != nil {
		if isNoKind(err) {
			return out, nil
		}
		return nil, err
	}
	for _, p := range list.Items {
		name, _, _ := unstructured.NestedString(p.Object, "providerName")
		typ, _, _ := unstructured.NestedString(p.Object, "type")
		version, _, _ := unstructured.NestedString(p.Object, "version")
		out[inventoryName(typ, name)] = version
	}
	// CAAPH installed some other way (its Helm chart, kubectl apply) has its
	// CRDs but no inventory entry. clusterctl init would install over it, so
	// report it as present at an unknown version and let the caller refuse.
	if _, ok := out[caaphInventoryName]; !ok {
		var crd apiextensionsv1.CustomResourceDefinition
		switch err := k.c.Get(ctx, client.ObjectKey{Name: caaphCRD}, &crd); {
		case err == nil:
			out[caaphInventoryName] = notByClusterctl
		case apierrors.IsNotFound(err), isNoKind(err):
		default:
			return nil, fmt.Errorf("checking for an existing Cluster API Add-on Provider Helm: %w", err)
		}
	}
	return out, nil
}

// notByClusterctl is the version reported for a component present without
// clusterctl's record of it: never equal to a pin, so it is refused.
const notByClusterctl = "unknown (not installed by clusterctl)"

// caaphInventoryName and caaphCRD identify Cluster API Add-on Provider Helm.
const (
	caaphInventoryName = "addon-helm"
	caaphCRD           = "helmchartproxies.addons.cluster.x-k8s.io"
)

// certManagerVersion returns the version clusterctl recorded on the
// cert-manager it installed, "" when there is none, or "unknown" when a
// cert-manager is present without clusterctl's version annotation.
func (k *KubeCluster) certManagerVersion(ctx context.Context) (string, error) {
	var deps appsv1.DeploymentList
	if err := k.c.List(ctx, &deps, client.InNamespace(certManagerNamespace)); err != nil {
		return "", err
	}
	for _, d := range deps.Items {
		if v, ok := d.Annotations[clusterctlv1.CertManagerVersionAnnotation]; ok {
			return v, nil
		}
	}
	if len(deps.Items) > 0 {
		return notByClusterctl, nil
	}
	return "", nil
}

// inventoryName turns a Provider object's type and providerName into the
// name clusterctl prints, e.g. InfrastructureProvider/hydra -> infrastructure-hydra.
func inventoryName(typ, name string) string {
	switch clusterctlv1.ProviderType(typ) {
	case clusterctlv1.CoreProviderType:
		return name
	case clusterctlv1.BootstrapProviderType:
		return "bootstrap-" + name
	case clusterctlv1.ControlPlaneProviderType:
		return "control-plane-" + name
	case clusterctlv1.InfrastructureProviderType:
		return "infrastructure-" + name
	case clusterctlv1.AddonProviderType:
		return "addon-" + name
	}
	return typ + "/" + name
}

// Init installs exactly the given providers with clusterctl, from an
// in-memory configuration: the user's own clusterctl.yaml is never read, so
// the result depends only on this CLI's pins.
func (k *KubeCluster) Init(ctx context.Context, providers []Provider) error {
	reader, cleanup, err := clusterctlReader()
	if err != nil {
		return err
	}
	defer cleanup()
	cfg, err := clusterctlconfig.New(ctx, "", clusterctlconfig.InjectReader(reader))
	if err != nil {
		return err
	}
	cc, err := clusterctl.New(ctx, "", clusterctl.InjectConfig(cfg))
	if err != nil {
		return err
	}

	kubeconfigPath, cleanup, err := scratchFile(k.kubeconfig)
	if err != nil {
		return err
	}
	defer cleanup()

	opts, err := initOptions(kubeconfigPath, providers)
	if err != nil {
		return err
	}
	_, err = cc.Init(ctx, opts)
	return err
}

// WaitProvidersAvailable polls the Deployments in cert-manager's namespace
// and in every namespace clusterctl installed a provider into, until all are
// Available or ctx ends (5 minutes at most).
func (k *KubeCluster) WaitProvidersAvailable(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for {
		pending, err := k.unavailableDeployments(ctx)
		if err == nil && len(pending) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return err
			}
			return fmt.Errorf("still not Available: %s", strings.Join(pending, ", "))
		case <-time.After(3 * time.Second):
		}
	}
}

func (k *KubeCluster) unavailableDeployments(ctx context.Context) ([]string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(providerListGVK)
	if err := k.c.List(ctx, list); err != nil {
		return nil, err
	}
	namespaces := map[string]bool{certManagerNamespace: true}
	for _, p := range list.Items {
		namespaces[p.GetNamespace()] = true
	}
	var pending []string
	for _, ns := range slices.Sorted(maps.Keys(namespaces)) {
		var deps appsv1.DeploymentList
		if err := k.c.List(ctx, &deps, client.InNamespace(ns)); err != nil {
			return nil, err
		}
		if len(deps.Items) == 0 {
			pending = append(pending, ns+"/(no deployments yet)")
		}
		for _, d := range deps.Items {
			if !deploymentAvailable(&d) {
				pending = append(pending, ns+"/"+d.Name)
			}
		}
	}
	return pending, nil
}

func deploymentAvailable(d *appsv1.Deployment) bool {
	if d.Generation > d.Status.ObservedGeneration {
		return false
	}
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// clusterctlReader is clusterctl's whole configuration: the Hydra provider
// entry, the cert-manager pin, an optional GitHub token, and an empty
// overrides directory. cleanup removes that directory.
func clusterctlReader() (*clusterctlconfig.MemoryReader, func(), error) {
	reader := clusterctlconfig.NewMemoryReader()
	if _, err := reader.AddProvider("hydra", clusterctlv1.InfrastructureProviderType, HydraComponentsURL); err != nil {
		return nil, nil, err
	}
	reader.Set(clusterctlconfig.CertManagerConfigKey, "version: "+CertManagerVersion)
	// clusterctl reads GitHub release metadata; a token avoids the anonymous
	// API rate limit. The memory reader has no environment fallback, so it is
	// set under clusterctl's own key. Passed through, never stored.
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		reader.Set(clusterctlconfig.GitHubTokenVariable, tok)
	}
	// clusterctl also reads local override manifests from
	// ~/.config/cluster-api/overrides (or ~/.cluster-api/overrides), whatever
	// the injected config says. Point it at an empty directory so a provider
	// developer's local build is never installed in place of a pinned release.
	noOverrides, err := os.MkdirTemp("", "hydra-no-clusterctl-overrides-*")
	if err != nil {
		return nil, nil, err
	}
	reader.Set(overridesFolderKey, noOverrides)
	return reader, func() { _ = os.RemoveAll(noOverrides) }, nil
}

// initOptions maps the providers to install onto clusterctl's options. The
// bootstrap and control-plane lists always start with clusterctl's no-op
// entry: when its inventory has no core provider, clusterctl otherwise fills
// empty lists with the *latest* kubeadm providers, not the pinned ones. An
// empty provider list still makes Init install the pinned cert-manager.
func initOptions(kubeconfigPath string, providers []Provider) (clusterctl.InitOptions, error) {
	opts := clusterctl.InitOptions{
		Kubeconfig:            clusterctl.Kubeconfig{Path: kubeconfigPath},
		BootstrapProviders:    []string{clusterctl.NoopProvider},
		ControlPlaneProviders: []string{clusterctl.NoopProvider},
		WaitProviders:         true,
		WaitProviderTimeout:   5 * time.Minute,
	}
	for _, p := range providers {
		switch p.Kind {
		case CoreProvider:
			opts.CoreProvider = p.Ref
		case BootstrapProvider:
			opts.BootstrapProviders = append(opts.BootstrapProviders, p.Ref)
		case ControlPlaneProvider:
			opts.ControlPlaneProviders = append(opts.ControlPlaneProviders, p.Ref)
		case InfrastructureProvider:
			opts.InfrastructureProviders = append(opts.InfrastructureProviders, p.Ref)
		case AddonProvider:
			opts.AddonProviders = append(opts.AddonProviders, p.Ref)
		default:
			return opts, fmt.Errorf("unknown provider kind %q", p.Kind)
		}
	}
	return opts, nil
}

// GetSecret reads the Secret's keys, labels and deletion state.
func (k *KubeCluster) GetSecret(ctx context.Context, namespace, name string) (*SecretInfo, error) {
	var sec corev1.Secret
	err := k.c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sec)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &SecretInfo{
		Keys:        slices.Sorted(maps.Keys(sec.Data)),
		Labels:      sec.Labels,
		Terminating: sec.DeletionTimestamp != nil,
	}, nil
}

// LabelSecret merge-patches one label onto the Secret; its data is untouched.
func (k *KubeCluster) LabelSecret(ctx context.Context, namespace, name, key, value string) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]string{key: value}}})
	if err != nil {
		return err
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	return k.c.Patch(ctx, sec, client.RawPatch(types.MergePatchType, patch), client.FieldOwner(FieldManager))
}

// ApplySecret server-side applies the namespace and an Opaque Secret.
func (k *KubeCluster) ApplySecret(ctx context.Context, namespace, name string, data map[string][]byte, labels map[string]string) error {
	if err := k.c.Apply(ctx, corev1ac.Namespace(namespace), client.FieldOwner(FieldManager)); err != nil {
		return fmt.Errorf("namespace %s: %w", namespace, err)
	}
	s := corev1ac.Secret(name, namespace).
		WithType(corev1.SecretTypeOpaque).
		WithLabels(labels).
		WithData(data)
	return k.c.Apply(ctx, s, client.FieldOwner(FieldManager), client.ForceOwnership)
}

// managedKinds are the objects whose presence means this management cluster
// still manages machines: a Cluster, and also the machine objects that
// outlive a Cluster while its deletion tears VMs down.
var managedKinds = []struct {
	list schema.GroupVersionKind
	crd  string
}{
	{schema.GroupVersionKind{Group: capiGroup, Version: capiVersion, Kind: "ClusterList"}, "clusters.cluster.x-k8s.io"},
	{schema.GroupVersionKind{Group: capiGroup, Version: capiVersion, Kind: "MachineList"}, "machines.cluster.x-k8s.io"},
	{schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha1", Kind: "HydraClusterList"}, "hydraclusters.infrastructure.cluster.x-k8s.io"},
	{schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha1", Kind: "HydraMachineList"}, "hydramachines.infrastructure.cluster.x-k8s.io"},
}

// Clusters lists every object that shows machines are still managed here, as
// Kind namespace/name. A kind whose CRD does not exist has none. A CRD that
// exists but cannot be listed at the version asked for is an error, never
// "none": guessing empty is how a delete orphans VMs.
func (k *KubeCluster) Clusters(ctx context.Context) ([]string, error) {
	var out []string
	for _, mk := range managedKinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(mk.list)
		err := k.c.List(ctx, list)
		if err != nil {
			if !isNoKind(err) {
				return nil, fmt.Errorf("listing %s: %w", mk.crd, err)
			}
			crdErr := k.c.Get(ctx, client.ObjectKey{Name: mk.crd}, &apiextensionsv1.CustomResourceDefinition{})
			if apierrors.IsNotFound(crdErr) {
				continue
			}
			if crdErr == nil {
				return nil, fmt.Errorf("CRD %s exists but does not serve %s, so its objects cannot be counted", mk.crd, mk.list.GroupVersion())
			}
			return nil, fmt.Errorf("checking CRD %s: %w", mk.crd, crdErr)
		}
		kind := strings.TrimSuffix(mk.list.Kind, "List")
		for _, o := range list.Items {
			out = append(out, kind+" "+o.GetNamespace()+"/"+o.GetName())
		}
	}
	return out, nil
}

func isNoKind(err error) bool {
	return apierrors.IsNotFound(err) || meta.IsNoMatchError(err)
}

// scratchFile writes content to a private temporary file for libraries that
// only take a path, and returns a function that removes it.
func scratchFile(content string) (string, func(), error) {
	f, err := os.CreateTemp("", "hydra-kubeconfig-*")
	if err != nil {
		return "", nil, err
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}
