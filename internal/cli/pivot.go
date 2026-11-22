package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Petatron/hydra/internal/bootstrap"
	"github.com/Petatron/hydra/internal/cluster"
	"github.com/Petatron/hydra/internal/config"
	"github.com/Petatron/hydra/internal/pivot"
)

// pivotHooks are the clusterctl operations a pivot runs; tests replace them.
var pivotHooks = func(out io.Writer, what string) pivot.Hooks {
	return pivot.Hooks{
		EnsureProviders: func(ctx context.Context, kubeconfig []byte) error {
			cl, err := bootstrap.ConnectKube(string(kubeconfig))
			if err != nil {
				return err
			}
			return (&bootstrap.Bootstrapper{Out: out}).EnsureProviders(ctx, cl, what)
		},
		Move: bootstrap.MoveNamespace,
	}
}

// targetKubeconfig finds the cluster's own kubeconfig. The Secret Cluster API
// keeps on the source is authoritative while the cluster is still there; an
// explicit --cluster-kubeconfig wins but must match it. After a move the
// Secret lives on the target, so the file (explicit, or the one create wrote)
// is used and proven by provePivotTarget once connected.
func targetKubeconfig(ctx context.Context, src *cluster.Manager, ns, name, explicit, defaultPath string) (kc []byte, from string, proven bool, err error) {
	secret, serr := src.Kubeconfig(ctx, ns, name)
	if serr != nil && !apierrors.IsNotFound(serr) {
		return nil, "", false, fmt.Errorf("reading the cluster's kubeconfig Secret on the source: %w", serr)
	}
	if explicit == "" && serr == nil {
		return secret, "Secret " + ns + "/" + name + "-kubeconfig", true, nil
	}
	path := explicit
	if path == "" {
		path = defaultPath
	}
	b, err := os.ReadFile(path)
	switch {
	case err != nil && serr == nil:
		return nil, "", false, fmt.Errorf("--cluster-kubeconfig %s is unreadable (%v); the source's Secret %s/%s-kubeconfig was left unused because an explicit path wins. Fix the path, or drop the flag to use the Secret", path, err, ns, name)
	case err != nil:
		return nil, "", false, fmt.Errorf("no kubeconfig for cluster %s: the source has no %s-kubeconfig Secret and %s is unreadable (%v); pass --cluster-kubeconfig", name, name, path, err)
	}
	if serr == nil {
		same, err := cluster.SameCA(b, secret)
		if err != nil {
			return nil, "", false, fmt.Errorf("%s: %w", path, err)
		}
		if !same {
			return nil, "", false, fmt.Errorf("%s does not trust cluster %s's certificate authority (from Secret %s/%s-kubeconfig on the source); it is the kubeconfig of a different cluster", path, name, ns, name)
		}
		return b, path, true, nil
	}
	return b, path, false, nil
}

// provePivotTarget checks that a kubeconfig file the source could not vouch
// for reaches cluster name itself: the cluster it reaches must hold that
// Cluster, and its own kubeconfig Secret must name the same certificate
// authority. Anything else is refused rather than moved onto.
func provePivotTarget(ctx context.Context, tc *cluster.Manager, ns, name, from string, kc []byte) error {
	st, err := tc.State(ctx, ns, name)
	if err != nil {
		return fmt.Errorf("reading Cluster %s/%s through %s: %w", ns, name, from, err)
	}
	if !st.Exists {
		return fmt.Errorf("neither the source nor the cluster %s reaches holds Cluster %s/%s, so nothing proves %s is that cluster's kubeconfig", from, ns, name, from)
	}
	self, _, err := tc.SelfHosted(ctx, ns, name, kc, st)
	if err != nil {
		return err
	}
	if !self {
		return fmt.Errorf("%s reaches a management cluster that holds Cluster %s/%s but is not that cluster (the certificate authorities differ); pivot only moves a cluster onto itself", from, ns, name)
	}
	return nil
}

type pivotOptions struct {
	clusterKubeconfig string
	keepBootstrap     bool
	timeout           time.Duration
	// fresh confirms, for create --self-hosted, that no copy of the cluster
	// exists yet when nothing local can show it.
	fresh bool
	// configFile is the hydra.yaml path, for commands printed in errors.
	configFile string
	// finishSplit completes a move interrupted part way, after proving which
	// copy is the source.
	finishSplit bool
	// rerun names the command to run again in messages; empty means pivot.
	rerun string
}

// targetKubeconfigFile returns a file holding the target kubeconfig, for
// commands an operator may need to run. A kubeconfig read from a file is that
// file. One read from the source's Secret is written to ./<name>.kubeconfig
// (the file create writes), unless a different cluster's kubeconfig is
// already there.
func targetKubeconfigFile(explicit, name, from string, kc []byte) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	path := name + ".kubeconfig"
	if from == path {
		return path, nil
	}
	if old, err := os.ReadFile(path); err == nil {
		same, err := cluster.SameCA(old, kc)
		if err != nil || !same {
			return "", fmt.Errorf("%s exists but is not cluster %s's kubeconfig (its certificate authority differs or is unreadable); move it aside or pass --cluster-kubeconfig", path, name)
		}
		return path, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	if err := writeKubeconfig(path, kc, false); err != nil {
		return "", err
	}
	return path, nil
}

// runPivot moves the cluster from the source onto itself, verifies, and then
// removes the bootstrap kind cluster when that was the source.
func runPivot(ctx context.Context, cmd *cobra.Command, src *target, cfg config.Metadata, o pivotOptions) error {
	out := cmd.ErrOrStderr()
	kc, from, proven, err := targetKubeconfig(ctx, src.m, cfg.Namespace, cfg.Name, o.clusterKubeconfig, cfg.Name+".kubeconfig")
	if err != nil {
		return err
	}
	tc, err := connectManagement(kc)
	if err != nil {
		return fmt.Errorf("connecting to cluster %s (kubeconfig from %s): %w", cfg.Name, from, err)
	}
	if !proven {
		if err := provePivotTarget(ctx, &cluster.Manager{C: tc, Out: io.Discard}, cfg.Namespace, cfg.Name, from, kc); err != nil {
			return err
		}
	}
	targetFile, err := targetKubeconfigFile(o.clusterKubeconfig, cfg.Name, from, kc)
	if err != nil {
		return err
	}
	srcSide := pivot.Side{Name: src.source, C: src.m.C, Kubeconfig: src.kubeconfig, File: src.path}
	if src.bootstrapKind {
		srcSide.File = bootstrap.KindName(cfg.Name) + ".kubeconfig"
		cf := o.configFile
		if cf == "" {
			cf = defaultConfigFile
		}
		srcSide.Prep = fmt.Sprintf("hydra bootstrap kubeconfig -f %s > %s", cf, srcSide.File)
	}
	p := &pivot.Pivot{
		Source:      srcSide,
		Target:      pivot.Side{Name: "cluster " + cfg.Name, C: tc, Kubeconfig: kc, File: targetFile},
		Namespace:   cfg.Namespace,
		Name:        cfg.Name,
		Hooks:       pivotHooks(out, "cluster "+cfg.Name),
		Out:         out,
		FinishSplit: o.finishSplit,
		Rerun:       o.rerun,
	}
	if err := p.Run(ctx, o.timeout); errors.Is(err, pivot.ErrAlreadySelfHosted) {
		_, _ = fmt.Fprintf(out, "cluster %s/%s already manages itself (%s is the cluster itself); nothing to move\n", cfg.Namespace, cfg.Name, src.source)
		return nil
	} else if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "cluster %s/%s now manages itself; pass %s as --management-kubeconfig to hydra from here on\n", cfg.Namespace, cfg.Name, targetFile)
	if !src.bootstrapKind || o.keepBootstrap {
		return nil
	}
	b, err := newBootstrapper(out)
	if err != nil {
		return err
	}
	// Delete refuses if the kind cluster still manages anything.
	return b.Delete(ctx, cfg.Name, false)
}

func newPivotCommand() *cobra.Command {
	var file, mgmt string
	var o pivotOptions
	c := &cobra.Command{
		Use:   "pivot",
		Short: "Move the cluster's management onto the cluster itself",
		Long: `pivot moves the cluster's Cluster API objects from the management cluster that
built it (by default its bootstrap kind cluster) onto the cluster itself, with
clusterctl move, so it manages itself. The VMs are not touched; the cluster's
own Hydra provider takes them over.

Before moving it checks what docs/moving.md asks: the cluster is Available
with no rollout, its namespace holds nothing but
this cluster (clusterctl move is namespace-wide), every machine has
recorded its identity, the credentials Secret and the Hydra CRDs carry the
labels clusterctl move needs, and a dry run succeeds. It installs the pinned
providers on the target first.

After moving it verifies on the target that the cluster is Available, every
machine has taken its VM over and is Ready, and, when hydra installs the CNI
(cni: hydra), that its objects came along. If
that fails, the cluster stays on the target: pivot never moves a cluster back
on its own, because a VM may already have been taken over and moving that
machine back would strand it. The error says how to inspect the cluster and
how to undo the move by hand. Only then is the bootstrap kind cluster
deleted (unless --keep-bootstrap).

Rerunning is safe: it finds where the objects are and continues; a cluster
already on the target is only verified. It never unpauses a cluster: when one
is paused it says what to run. The target is reached with the
cluster's kubeconfig: the Secret on the source, or --cluster-kubeconfig
(default ./<name>.kubeconfig, which create writes). Either way pivot proves it
is the cluster itself by its certificate authority before moving anything.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(file)
			if err != nil {
				return err
			}
			t, err := manager(cmd, mgmt, cfg.Metadata.Name)
			if err != nil {
				return err
			}
			ctx, stop := interruptible(cmd)
			defer stop()
			o.configFile = file
			return runPivot(ctx, cmd, t, cfg.Metadata, o)
		},
	}
	c.Flags().StringVarP(&file, "file", "f", "hydra.yaml", "path to hydra.yaml")
	c.Flags().StringVar(&mgmt, "management-kubeconfig", "", "kubeconfig of the source management cluster (default: the cluster's bootstrap kind cluster)")
	addPivotFlags(c, &o)
	return c
}

func addPivotFlags(c *cobra.Command, o *pivotOptions) {
	c.Flags().StringVar(&o.clusterKubeconfig, "cluster-kubeconfig", "", "the cluster's own kubeconfig, used once the source no longer holds it (default ./<name>.kubeconfig)")
	c.Flags().BoolVar(&o.keepBootstrap, "keep-bootstrap", false, "keep the bootstrap kind cluster after a verified pivot")
	c.Flags().DurationVar(&o.timeout, "pivot-timeout", 15*time.Minute, "how long to wait for the cluster to verify on itself")
	c.Flags().BoolVar(&o.finishSplit, "finish-split", false, "finish a move interrupted part way (the Cluster on both sides): prove from the takeover evidence which copy is the source, unpause it and move again")
}

// endpointAnswers probes the cluster's stated API endpoint: true when
// something accepts connections, false with no error when the host refuses
// them (nothing listens), and an error when this machine cannot tell (a
// timeout, no route): that is not evidence the cluster is absent. Tests
// replace it.
var endpointAnswers = func(endpoint string) (bool, error) {
	host, port, err := config.SplitEndpoint(endpoint)
	if err != nil {
		return false, err
	}
	return probeTCP(net.JoinHostPort(host, strconv.Itoa(port)))
}

// probeTCP is endpointAnswers' dial: true when addr accepts, false with no
// error when refused, and the dial error when this machine cannot tell.
func probeTCP(addr string) (bool, error) {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err == nil {
		_ = conn.Close()
		return true, nil
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return false, nil
	}
	return false, err
}

// runSelfHosted is `create --self-hosted`: bootstrap, create, pivot, delete
// kind. Each rerun starts from what exists:
//
//   - the bootstrap kind cluster holds the Cluster: carry on from there;
//   - the cluster's kubeconfig file reaches the cluster managing itself:
//     nothing to create, only a leftover kind cluster to clean up;
//   - neither: a fresh start, but only after making sure no copy of the
//     cluster is already running (its endpoint is silent, or --fresh when
//     Hydra chooses the endpoint and nothing local could show it).
func runSelfHosted(ctx context.Context, cmd *cobra.Command, cfg *config.ClusterConfig, objs []*unstructured.Unstructured, credentialsDir string, co createOptions, po pivotOptions) error {
	out := cmd.ErrOrStderr()
	md := cfg.Metadata
	path := po.clusterKubeconfig
	if path == "" {
		path = md.Name + ".kubeconfig"
	}
	b, err := newBootstrapper(out)
	if err != nil {
		return err
	}
	kindName := bootstrap.KindName(md.Name)
	kindExists, err := b.Kind.Exists(kindName)
	if err != nil {
		return fmt.Errorf("looking for kind cluster %s: %w", kindName, err)
	}
	kindHolds := false
	if kindExists {
		t, err := manager(cmd, "", md.Name)
		if err != nil {
			return err
		}
		st, err := t.m.State(ctx, md.Namespace, md.Name)
		if err != nil {
			return fmt.Errorf("reading Cluster %s/%s on kind cluster %s: %w", md.Namespace, md.Name, kindName, err)
		}
		kindHolds = st.Exists
	}

	// Already moved? Then never create again.
	kc, rerr := os.ReadFile(path)
	if rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		return fmt.Errorf("reading %s: %w", path, rerr)
	}
	if rerr == nil {
		self, why, err := selfHostedThrough(ctx, kc, md.Namespace, md.Name)
		switch {
		case err != nil && !kindHolds:
			return fmt.Errorf("%s exists but cannot show whether cluster %s already runs (%v). If that cluster is gone, remove the file; otherwise fix access and rerun", path, md.Name, err)
		case err == nil && self && kindExists:
			// The move ran, at least in part: let the pivot find where the
			// objects are (it verifies, or reports a pause or a split with
			// the commands to run; it never unpauses) and then remove the
			// kind cluster once the target verifies.
			_, _ = fmt.Fprintf(out, "cluster %s/%s already holds its own Cluster object; finishing the pivot\n", md.Namespace, md.Name)
			t, err := manager(cmd, "", md.Name)
			if err != nil {
				return err
			}
			po.clusterKubeconfig = path
			return runPivot(ctx, cmd, t, md, po)
		case err == nil && self:
			_, _ = fmt.Fprintf(out, "cluster %s/%s already manages itself (%s); nothing to do. To apply changes to it, run: hydra cluster create --management-kubeconfig %s\n", md.Namespace, md.Name, why, path)
			return nil
		case err == nil && !kindHolds:
			// The file reaches a live cluster that does not manage this one
			// itself: the cluster before its pivot (its bootstrap cluster
			// gone), or another management cluster holding it. Either way it
			// may exist, so never build a second one.
			return fmt.Errorf("%s reaches a running cluster, and no bootstrap cluster holds %s/%s; it may already exist (created but never pivoted, or managed elsewhere). Check with `hydra cluster status --management-kubeconfig <its management cluster>`, and pivot it with `hydra cluster pivot --management-kubeconfig <that kubeconfig>`; if the file is stale, remove it", path, md.Namespace, md.Name)
		}
	}

	if !kindHolds {
		if err := checkFreshStart(cfg, path, po.fresh); err != nil {
			return err
		}
	}

	var opts bootstrap.Options
	if credentialsDir != "" {
		if opts.Credentials, err = bootstrap.LoadCredentials(credentialsDir); err != nil {
			return err
		}
	}
	if err := b.Create(ctx, cfg, opts); err != nil {
		return err
	}
	t, err := manager(cmd, "", md.Name)
	if err != nil {
		return err
	}
	written, err := runCreate(ctx, cmd, t, cfg, objs, co)
	if err != nil {
		return err
	}
	po.clusterKubeconfig = written
	return runPivot(ctx, cmd, t, md, po)
}

// selfHostedThrough reports whether kubeconfig reaches cluster name managing
// itself. A cluster that does not hold the Cluster object is not it.
func selfHostedThrough(ctx context.Context, kubeconfig []byte, ns, name string) (bool, string, error) {
	tc, err := connectManagement(kubeconfig)
	if err != nil {
		return false, "", err
	}
	m := &cluster.Manager{C: tc, Out: io.Discard}
	st, err := m.State(ctx, ns, name)
	if err != nil {
		return false, "", err
	}
	if !st.Exists {
		return false, "", nil
	}
	return m.SelfHosted(ctx, ns, name, kubeconfig, st)
}

// checkFreshStart refuses a fresh build beside a cluster that may already be
// running (a self-hosted cluster whose kubeconfig file is gone, say): when
// its endpoint answers, or when this machine cannot tell and --fresh was not
// given.
func checkFreshStart(cfg *config.ClusterConfig, path string, fresh bool) error {
	md := cfg.Metadata
	ep := cfg.Spec.Network.ControlPlaneEndpoint
	if ep == "" {
		if !fresh {
			return fmt.Errorf("this cluster's endpoint is left for Hydra to choose, and there is no bootstrap cluster or kubeconfig file (%s) to show whether it already exists. If you are sure it does not, rerun with --fresh", path)
		}
		return nil
	}
	answers, err := endpointAnswers(ep)
	switch {
	case answers:
		return fmt.Errorf("something already answers on %s, this cluster's API endpoint; it may already be running. Pass --cluster-kubeconfig with its kubeconfig to finish or check it, rather than building a second one", ep)
	case err != nil && !fresh:
		return fmt.Errorf("cannot tell whether cluster %s already runs: this machine cannot reach its endpoint %s (%v), and there is no bootstrap cluster or kubeconfig file (%s) to show it. If you are sure it does not exist, rerun with --fresh", md.Name, ep, err, path)
	}
	return nil
}
