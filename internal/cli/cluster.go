package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Petatron/hydra/internal/bastion"
	"github.com/Petatron/hydra/internal/bootstrap"
	"github.com/Petatron/hydra/internal/cluster"
	"github.com/Petatron/hydra/internal/config"
	"github.com/Petatron/hydra/internal/render"
	"github.com/Petatron/hydra/internal/upgrade"
)

// managementTarget says which management cluster the cluster commands act on.
// It is never the user's current kubectl context: either the kubeconfig named
// with --management-kubeconfig, or the bootstrap kind cluster for this
// cluster, found by name. Tests replace it.
var managementTarget = func(kubeconfigPath, clusterName string) (kubeconfig []byte, source string, err error) {
	if kubeconfigPath != "" {
		b, err := os.ReadFile(kubeconfigPath)
		if err != nil {
			return nil, "", err
		}
		return b, kubeconfigPath, nil
	}
	kind, err := bootstrap.NewKindProvider()
	if err != nil {
		return nil, "", fmt.Errorf("no --management-kubeconfig, and no container runtime to look for a bootstrap cluster: %w", err)
	}
	name := bootstrap.KindName(clusterName)
	ok, err := kind.Exists(name)
	if err != nil {
		return nil, "", fmt.Errorf("looking for bootstrap cluster %s: %w", name, err)
	}
	if !ok {
		return nil, "", fmt.Errorf("no management cluster: pass --management-kubeconfig, or run `hydra bootstrap` first (kind cluster %s does not exist)", name)
	}
	kc, err := kind.KubeConfig(name)
	if err != nil {
		return nil, "", err
	}
	return []byte(kc), "kind cluster " + name, nil
}

// target is the management cluster a command acts on.
type target struct {
	m *cluster.Manager
	// source says where it came from (a kubeconfig path or the kind cluster).
	source string
	// server is its API server URL.
	server string
	// bootstrapKind is true when it is the bootstrap kind cluster.
	bootstrapKind bool
	// path is the --management-kubeconfig path, "" for the kind cluster.
	path string
	// kubeconfig is the management kubeconfig as given, for identity checks
	// (which cluster it is). Never the --bastion copy pointed at 127.0.0.1:
	// the self-hosted check compares its server with the cluster's endpoint.
	kubeconfig []byte
	// tunnel is the --bastion tunnel, closed by Close.
	tunnel tunnel
	// out is where the tunnel's own messages go.
	out io.Writer
	// stopSignals ends the tunnel's signal watch.
	stopSignals func()
	// cancelOpen cancels the context the tunnel's ssh was started with; Close
	// calls it after stopping ssh (cancelling first would kill ssh, and the
	// kill would read as a drop).
	cancelOpen context.CancelFunc
	// reported is set once ssh's reason for a dropped tunnel was shown, so it
	// is shown once.
	reported atomic.Bool
}

// tunnel is what the CLI needs of a bastion.Tunnel; tests fake it.
type tunnel interface {
	Close() error
	Done() <-chan struct{}
	// Diagnose is ssh's reason for exiting, or its recent messages.
	Diagnose() string
}

// Close stops the --bastion tunnel, if there is one, first showing anything
// ssh said (such as the bastion failing to reach the API server). Safe on
// any target.
func (t *target) Close() {
	if t == nil || t.tunnel == nil {
		return
	}
	if t.stopSignals != nil {
		t.stopSignals()
	}
	if d := t.tunnel.Diagnose(); d != "" && t.out != nil && t.reported.CompareAndSwap(false, true) {
		_, _ = fmt.Fprintf(t.out, "bastion: %s\n", d)
	}
	_ = t.tunnel.Close()
	if t.cancelOpen != nil {
		t.cancelOpen()
	}
}

// watchTunnel reports, as it happens, a tunnel that ends while the command
// still needs it, and stops ssh if hydra is terminated, loses its terminal or
// is interrupted (SIGTERM, SIGHUP, SIGINT), which would otherwise skip the
// deferred Close and leave ssh forwarding: a signal sent to hydra alone, by a
// supervisor rather than a terminal, never reaches ssh. On SIGTERM or SIGHUP
// hydra exits at once. On SIGINT it stops ssh and lets a command that handles
// the interrupt finish (its Close ends the watch); one that does not fails as
// the API goes, and hydra exits after interruptGrace regardless. Once the
// tunnel is gone the signals are released at once, so they act the usual way.
// reported is set when the drop was shown. out must be safe for concurrent
// writes. It returns the function that ends the watch. sig is
// notifyTermination's channel, set up before the tunnel was opened.
func watchTunnel(tun tunnel, out io.Writer, reported *atomic.Bool, sig chan os.Signal) func() {
	stop := make(chan struct{})
	go func() {
		defer signal.Stop(sig)
		select {
		case s := <-sig:
			_ = tun.Close()
			if s == os.Interrupt {
				select {
				case <-stop:
					return
				case <-time.After(interruptGrace):
				}
			}
			exitProcess(signalExitCode(s))
		case <-tun.Done():
			select {
			case <-stop: // Close ended the watch, then stopped ssh: not a drop
				return
			default:
			}
			if d := tun.Diagnose(); d != "" && reported.CompareAndSwap(false, true) {
				_, _ = fmt.Fprintf(out, "bastion: %s; the management cluster is unreachable until hydra is run again\n", d)
			}
		case <-stop:
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { signal.Stop(sig); close(stop) }) }
}

// lockedWriter serializes writes to w.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// notifyTermination starts catching SIGTERM, SIGHUP and SIGINT, before the
// tunnel is opened, so that no moment exists where they would end hydra and
// leave ssh running (outside Linux, nothing else stops it).
func notifyTermination() chan os.Signal {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP, os.Interrupt)
	return sig
}

// interruptGrace is how long hydra waits, after SIGINT has stopped the
// tunnel, for the command to finish before exiting.
var interruptGrace = 10 * time.Second

// exitProcess is os.Exit; tests replace it.
var exitProcess = os.Exit

// openGuarded runs open, cancelling its context if hydra gets SIGTERM, SIGHUP
// or SIGINT meanwhile; ssh, started with that context, is then stopped. It
// returns the signal caught, if any, once open has returned.
func openGuarded(sig chan os.Signal, cancel context.CancelFunc, open func()) os.Signal {
	opened, guarded := make(chan struct{}), make(chan struct{})
	var caught os.Signal
	go func() {
		defer close(guarded)
		select {
		case caught = <-sig:
			cancel()
		case <-opened:
		}
	}()
	open()
	close(opened)
	<-guarded
	return caught
}

func signalExitCode(s os.Signal) int {
	switch s {
	case syscall.SIGHUP:
		return 128 + 1
	case os.Interrupt:
		return 128 + 2
	}
	return 128 + 15
}

// bastionFlag is --bastion: the management API through an SSH jump host.
const bastionFlag = "bastion"

// bastionTimeout is how long --bastion waits for its tunnel to forward.
const bastionTimeout = 30 * time.Second

// addBastionFlag adds --bastion to a command that takes
// --management-kubeconfig; manager reads it from the command.
func addBastionFlag(c *cobra.Command) {
	c.Flags().String(bastionFlag, "", "reach the management cluster's API through this SSH jump host (user@host[:port]), "+
		"with the system ssh and ~/.ssh/config; TLS is still verified against the API server's own name")
}

// defaultConfigFile is --file's default, for commands printed in messages.
const defaultConfigFile = "hydra.yaml"

// kubectl is the kubectl invocation that reaches this management cluster, for
// commands printed in messages: hydra never uses the current context, so the
// commands it prints must not either. The bootstrap kind cluster's kubeconfig
// exists only in memory, so it is written first.
func (t *target) kubectl(configFile string) string {
	if t.path != "" {
		return "kubectl --kubeconfig " + t.path
	}
	if configFile == "" {
		configFile = defaultConfigFile
	}
	return fmt.Sprintf("hydra bootstrap kubeconfig -f %s > bootstrap.kubeconfig && kubectl --kubeconfig bootstrap.kubeconfig", configFile)
}

// connectManagement builds the management-cluster client; tests replace it.
var connectManagement = cluster.Connect

func manager(cmd *cobra.Command, kubeconfigPath, clusterName string) (*target, error) {
	kc, source, err := managementTarget(kubeconfigPath, clusterName)
	if err != nil {
		return nil, err
	}
	server := ""
	if rc, err := clientcmd.RESTConfigFromKubeConfig(kc); err == nil {
		server = rc.Host
	}
	var tun tunnel
	var sig chan os.Signal
	cancelOpen := context.CancelFunc(func() {})
	via, connectWith := "", kc
	if bastionSet(cmd) {
		dest := cmd.Flags().Lookup(bastionFlag).Value.String()
		if strings.TrimSpace(dest) == "" {
			return nil, fmt.Errorf("--bastion is empty: want user@host or user@host:port")
		}
		if kubeconfigPath == "" {
			return nil, fmt.Errorf("--bastion needs --management-kubeconfig: the bootstrap kind cluster is local")
		}
		var local string
		sig = notifyTermination()
		var openCtx context.Context
		openCtx, cancelOpen = context.WithCancel(contextOf(cmd))
		caught := openGuarded(sig, cancelOpen, func() {
			tun, local, connectWith, err = openBastion(openCtx, dest, kc, bastionTimeout)
		})
		if caught != nil {
			if tun != nil {
				_ = tun.Close()
			}
			exitProcess(signalExitCode(caught))
		}
		if err != nil {
			signal.Stop(sig)
			cancelOpen()
			return nil, err
		}
		via = fmt.Sprintf(" via bastion %s, tunnel %s", dest, local)
	}
	c, err := connectManagement(connectWith)
	if err != nil {
		if tun != nil {
			signal.Stop(sig)
			d := tun.Diagnose()
			_ = tun.Close()
			cancelOpen()
			if d != "" {
				return nil, fmt.Errorf("connecting to %s: %w (bastion: %s)", source, err, d)
			}
		}
		return nil, fmt.Errorf("connecting to %s: %w", source, err)
	}
	if tun != nil {
		// The tunnel's watch writes from its own goroutine: serialize every
		// write to the command's stderr.
		cmd.SetErr(&lockedWriter{w: cmd.ErrOrStderr()})
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "management cluster: %s (%s%s)\n", source, server, via)
	// The workload cluster's own API is read for its CNI's rollout; a
	// self-hosted cluster is read through this connection (and its tunnel).
	t := &target{m: &cluster.Manager{C: c, Out: cmd.ErrOrStderr(), Workload: cluster.ReachVia(kc, c)}, source: source, server: server,
		bootstrapKind: kubeconfigPath == "", kubeconfig: kc, path: kubeconfigPath, tunnel: tun, out: cmd.ErrOrStderr()}
	if tun != nil {
		t.stopSignals = watchTunnel(tun, cmd.ErrOrStderr(), &t.reported, sig)
		t.cancelOpen = cancelOpen
	}
	return t, nil
}

// bastionSet reports whether the command was given --bastion. An explicit
// empty value counts: it is a mistake, not "no bastion".
func bastionSet(cmd *cobra.Command) bool {
	f := cmd.Flags().Lookup(bastionFlag)
	return f != nil && f.Changed
}

// openBastion opens the --bastion tunnel and returns it, its local address
// and the kubeconfig pointed at it; tests replace it.
var openBastion = func(ctx context.Context, dest string, kc []byte, timeout time.Duration) (tunnel, string, []byte, error) {
	t, rewritten, err := bastion.Open(ctx, dest, kc, timeout)
	if err != nil {
		return nil, "", nil, err // never a typed nil in the interface
	}
	return t, t.Local, rewritten, nil
}

func interruptible(cmd *cobra.Command) (context.Context, func()) {
	ctx, stop := signal.NotifyContext(contextOf(cmd), os.Interrupt)
	go func() { <-ctx.Done(); stop() }()
	return ctx, stop
}

func renderObjects(cfg *config.ClusterConfig) ([]*unstructured.Unstructured, error) {
	out, err := render.Render(cfg, render.Options{CLIVersion: Version})
	if err != nil {
		return nil, err
	}
	return cluster.Objects(out)
}

func newCreateCommand() *cobra.Command {
	var file, mgmt, kubeconfigOut, credentials string
	var timeout time.Duration
	var wait, adopt, adoptCNI, forceConflicts, selfHosted bool
	var po pivotOptions
	c := &cobra.Command{
		Use:   "create",
		Short: "Create the cluster hydra.yaml describes and wait for it to be Available",
		Long: `create renders hydra.yaml, server-side applies the result to the management
cluster (field manager "hydra"), waits until Cluster API reports the cluster
Available with its CNI applied, and writes the cluster's kubeconfig.

With spec.network.cni: external, hydra installs no CNI and the nodes stay
NotReady until something else does, so the wait ends only once it has. Install
it while create waits, or pass --wait=false and follow with status.

Progress is what the controllers report. --timeout ends the wait, not the
creation: the controllers keep going, and running create again picks up where
things are. Rerunning create is always safe; applying the same objects changes
nothing.

The management cluster is --management-kubeconfig, or the bootstrap kind cluster
'hydra bootstrap' made for this cluster. Your current kubectl context is never
used.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(file)
			if err != nil {
				return err
			}
			objs, err := renderObjects(cfg)
			if err != nil {
				return err
			}
			ctx, stop := interruptible(cmd)
			defer stop()
			co := createOptions{kubeconfigOut: kubeconfigOut, configFile: file, timeout: timeout, wait: wait, adopt: adopt, adoptCNI: adoptCNI, forceConflicts: forceConflicts}
			if selfHosted {
				if mgmt != "" {
					return fmt.Errorf("--self-hosted builds its own bootstrap cluster; it cannot be combined with --management-kubeconfig (use pivot for that)")
				}
				if bastionSet(cmd) {
					return fmt.Errorf("--self-hosted cannot go through --bastion: it reaches a local bootstrap cluster and the new cluster directly")
				}
				if !wait {
					return fmt.Errorf("--self-hosted needs to wait for the cluster before moving it; drop --wait=false")
				}
				if po.clusterKubeconfig == "" {
					po.clusterKubeconfig = kubeconfigOut
				}
				po.configFile = file
				po.rerun = "`hydra cluster create --self-hosted`"
				return runSelfHosted(ctx, cmd, cfg, objs, credentials, co, po)
			}
			t, err := manager(cmd, mgmt, cfg.Metadata.Name)
			if err != nil {
				return err
			}
			defer t.Close()
			_, err = runCreate(ctx, cmd, t, cfg, objs, co)
			return err
		},
	}
	c.Flags().StringVarP(&file, "file", "f", "hydra.yaml", "path to hydra.yaml")
	c.Flags().StringVar(&mgmt, "management-kubeconfig", "", "kubeconfig of the management cluster (default: the cluster's bootstrap kind cluster)")
	addBastionFlag(c)
	c.Flags().StringVar(&kubeconfigOut, "kubeconfig-out", "", "where to write the new cluster's kubeconfig (default ./<name>.kubeconfig)")
	c.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "how long to wait for the cluster to be Available")
	c.Flags().BoolVar(&wait, "wait", true, "wait for the cluster to be Available")
	c.Flags().BoolVar(&adopt, "adopt", false, "take over existing objects of the same names that hydra did not make (never ones labelled for another cluster)")
	c.Flags().BoolVar(&adoptCNI, "adopt-cni", false, adoptCNIUsage)
	c.Flags().BoolVar(&forceConflicts, "force-conflicts", false, "take over fields another field manager owns (e.g. a hand edit) instead of failing")
	c.Flags().BoolVar(&selfHosted, "self-hosted", false, "bootstrap a kind cluster, create the cluster from it, pivot the cluster onto itself and delete kind")
	c.Flags().StringVar(&credentials, "hypervisor-credentials", "", "with --self-hosted: directory holding cacert.pem, clientcert.pem and clientkey.pem (needed the first time)")
	addPivotFlags(c, &po)
	c.Flags().BoolVar(&po.fresh, "fresh", false, "with --self-hosted: confirm no copy of this cluster exists yet, when hydra cannot check (Hydra chooses the endpoint, or this machine cannot reach it)")
	return c
}

func newApplyCommand() *cobra.Command {
	var file, mgmt, kubeconfigOut string
	var timeout time.Duration
	var wait, adopt, adoptCNI, forceConflicts bool
	var prune cluster.PruneOptions
	c := &cobra.Command{
		Use:   "apply",
		Short: "Create or update the cluster to match hydra.yaml, and prune what it no longer lists",
		Long: `apply does what create does -- server-side apply, wait, write the kubeconfig
-- and then prunes: objects labelled for the cluster that hydra.yaml no longer
renders are deleted where that is safe (ADR-002).

  - An addon removed from spec.addons: hydra's own HelmChartProxy for it
    (<cluster>-<addon>) is deleted, which uninstalls the addon. A protected
    or uncatalogued addon is refused unless named with --allow-remove; any
    other proxy labelled for the cluster (a duplicate, one made by hand) is
    refused unless named as --allow-remove=proxy/<name>.
  - A superseded machine or bootstrap template: deleted once no machine,
    MachineSet or control plane uses it (a later apply removes one still in
    use).
  - A node pool removed from hydra.yaml: kept unless named with
    --prune-pools=<pool>, because deleting it deletes its machines and VMs.
    A value that names more than one removed pool refuses instead.
  - The Cluster, its control plane, health check and CNI objects: never.

Pruning is the last step: an apply that times out waiting, or cannot write the
kubeconfig, deletes nothing (with --wait=false it prunes right after the
apply). Refusals are checked before anything is written. Run plan first: it shows
each object and what apply would do with it. Like create, apply refuses a
Kubernetes version change (use upgrade) and keeps nothing locally; an
interrupted apply is finished by running it again.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(file)
			if err != nil {
				return err
			}
			objs, err := renderObjects(cfg)
			if err != nil {
				return err
			}
			ctx, stop := interruptible(cmd)
			defer stop()
			t, err := manager(cmd, mgmt, cfg.Metadata.Name)
			if err != nil {
				return err
			}
			defer t.Close()
			p := prune
			_, err = runCreate(ctx, cmd, t, cfg, objs, createOptions{prune: &p, kubeconfigOut: kubeconfigOut, configFile: file, timeout: timeout, wait: wait, adopt: adopt, adoptCNI: adoptCNI, forceConflicts: forceConflicts})
			return err
		},
	}
	c.Flags().StringVarP(&file, "file", "f", "hydra.yaml", "path to hydra.yaml")
	c.Flags().StringVar(&mgmt, "management-kubeconfig", "", "kubeconfig of the management cluster (default: the cluster's bootstrap kind cluster)")
	addBastionFlag(c)
	c.Flags().StringVar(&kubeconfigOut, "kubeconfig-out", "", "where to write the cluster's kubeconfig (default ./<name>.kubeconfig)")
	c.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "how long to wait for the cluster to be Available")
	c.Flags().BoolVar(&wait, "wait", true, "wait for the cluster to be Available")
	c.Flags().BoolVar(&adopt, "adopt", false, "take over existing objects of the same names that hydra did not make (never ones labelled for another cluster)")
	c.Flags().BoolVar(&adoptCNI, "adopt-cni", false, adoptCNIUsage)
	c.Flags().BoolVar(&forceConflicts, "force-conflicts", false, "take over fields another field manager owns (e.g. a hand edit) instead of failing")
	addPruneFlags(c, &prune)
	return c
}

// addPruneFlags adds apply's pruning flags, which plan takes too.
// adoptCNIUsage is --adopt-cni's help: create, apply and plan take it.
const adoptCNIUsage = "with cni: hydra, let hydra's Cilium take over a cluster that already runs without it (no CNI label, or the label but no hydra Cilium proxy): it is installed over whatever runs there, taking over Cilium objects without a Helm release. Check what runs first"

func addPruneFlags(c *cobra.Command, p *cluster.PruneOptions) {
	c.Flags().StringSliceVar(&p.Pools, "prune-pools", nil, "node pools removed from hydra.yaml to delete, with their machines and VMs, by name; repeat or comma-separate")
	c.Flags().StringSliceVar(&p.AllowRemove, "allow-remove", nil, "protected or uncatalogued addons to uninstall although apply would refuse, by addon name, or any other proxy as proxy/<name>; repeat or comma-separate")
}

type createOptions struct {
	// prune, when set, makes this apply: objects no longer rendered are
	// pruned per cluster.PruneOptions, after a pre-write refusal check.
	prune         *cluster.PruneOptions
	kubeconfigOut string
	// configFile is hydra.yaml's path, for commands printed in messages.
	configFile                            string
	timeout                               time.Duration
	wait, adopt, adoptCNI, forceConflicts bool
}

// runCreate is create's body after the management cluster is resolved: every
// check that can refuse, the apply, the wait and the kubeconfig write. It
// returns where the kubeconfig was written ("" when not waiting).
func runCreate(ctx context.Context, cmd *cobra.Command, t *target, cfg *config.ClusterConfig, objs []*unstructured.Unstructured, o createOptions) (string, error) {
	kubeconfigOut, timeout, wait, adopt, forceConflicts := o.kubeconfigOut, o.timeout, o.wait, o.adopt, o.forceConflicts
	m := t.m
	ns, name := cfg.Metadata.Namespace, cfg.Metadata.Name
	// Everything that can refuse runs before the first write.
	if err := m.CheckManagementCluster(); err != nil {
		return "", err
	}
	if err := m.CheckAddonProvider(objs); err != nil {
		return "", err
	}
	st, err := m.State(ctx, ns, name)
	if err != nil {
		return "", err
	}
	if st.Terminating {
		return "", fmt.Errorf("cluster %s/%s is being deleted; let `hydra cluster delete` finish before creating it again", ns, name)
	}
	if !st.Exists && t.bootstrapKind {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "creating a NEW cluster %s/%s from the bootstrap cluster; if it already runs elsewhere (pivoted), stop and use --management-kubeconfig\n", ns, name)
	}
	if h := cfg.Spec.Hypervisor; h != nil {
		if err := m.CheckCredentials(ctx, ns, h.CredentialsSecretName); err != nil {
			return "", err
		}
	}
	if err := m.CheckOwnership(ctx, objs, name, adopt); err != nil {
		return "", err
	}
	if msg := st.CNIConflict(render.HydraCNI(cfg), o.adoptCNI, t.kubectl(o.configFile)); msg != "" {
		return "", errors.New(msg)
	}
	if drift, err := upgrade.Drift(ctx, m.C, cfg); err != nil {
		return "", err
	} else if len(drift) > 0 {
		return "", fmt.Errorf("hydra.yaml says kubernetesVersion %s, but %s: create would change the control plane and every pool at once, which the version-skew policy forbids. Upgrade with `hydra cluster upgrade --to %s`, or set kubernetesVersion back to what runs", cfg.Spec.KubernetesVersion, strings.Join(drift, ", "), cfg.Spec.KubernetesVersion)
	}
	if unprotected, err := m.UnprotectedRoll(ctx, objs, ns, name); err != nil {
		return "", err
	} else if unprotected {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "note: %s\n", cluster.UnprotectedRollNote)
	}
	if err := checkPruneRefusals(ctx, m, objs, ns, name, o.prune); err != nil {
		return "", err
	}
	if err := applyNotingCNIChange(ctx, cmd.ErrOrStderr(), m, cfg, objs, forceConflicts); err != nil {
		return "", err
	}
	if !render.HydraCNI(cfg) {
		// A label that survives the apply keeps hydra's ClusterResourceSet
		// selecting the cluster, and it installs hydra's Cilium if it has not
		// applied yet. The apply has landed and rerunning is safe; failing is
		// what makes the leftover selector impossible to miss.
		after, err := m.State(ctx, ns, name)
		if err != nil {
			return "", fmt.Errorf("applied, but could not re-read the Cluster to check that its CNI label is gone (rerun create): %w", err)
		}
		if msg := after.CNILabelLeft(false, t.kubectl(o.configFile)); msg != "" {
			return "", fmt.Errorf("applied, but %s", msg)
		}
	}
	if !wait {
		// Asked not to wait: prune now, as the last step.
		if err := pruneOrReport(ctx, cmd, m, objs, ns, name, o.prune); err != nil {
			return "", err
		}
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "not waiting (--wait=false); follow with `hydra cluster status`")
		return "", nil
	}
	start := time.Now()
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "waiting up to %s for cluster %s/%s to be Available\n", timeout, ns, name)
	s, err := waitAvailable(ctx, cmd, m, ns, name, timeout)
	if err != nil {
		return "", err
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "cluster %s/%s is Available after %s\n", ns, name, time.Since(start).Round(time.Second))
	if len(cfg.Spec.Addons) > 0 {
		// Addons settle on their own, after the cluster; create does not wait,
		// but says plainly which addons are not there yet and why.
		for _, line := range addonNotes(s) {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), line)
		}
	}
	kc, err := m.Kubeconfig(ctx, ns, name)
	if err != nil {
		return "", err
	}
	path := kubeconfigOut
	if path == "" {
		path = name + ".kubeconfig"
	}
	if err := writeKubeconfig(path, kc, kubeconfigOut != ""); err != nil {
		return "", err
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "kubeconfig written to %s\n", path)
	// Prune last: a cluster that never became Available, or a kubeconfig that
	// could not be written, deletes nothing.
	if err := pruneOrReport(ctx, cmd, m, objs, ns, name, o.prune); err != nil {
		return path, err
	}
	return path, nil
}

// applyNotingCNIChange applies objs and records on m whether that changed the
// CNI, which decides how its rollout is waited for (PET-103): the CNI
// proxy's generation moves when it is created or its spec changes. A
// generation that cannot be read counts as changed: the strict wait is the
// safe one.
func applyNotingCNIChange(ctx context.Context, out io.Writer, m *cluster.Manager, cfg *config.ClusterConfig, objs []*unstructured.Unstructured, forceConflicts bool) error {
	ns, name := cfg.Metadata.Namespace, cfg.Metadata.Name
	before, beforeErr := m.CNIProxyGeneration(ctx, ns, name)
	if err := m.Apply(ctx, objs, name, forceConflicts); err != nil {
		return err
	}
	if !render.HydraCNI(cfg) {
		return nil
	}
	after, err := m.CNIProxyGeneration(ctx, ns, name)
	m.RequireCNIRollout = beforeErr != nil || err != nil || before != after
	if err == nil && after != 0 && before != after {
		// On the cluster, so a rerun from anywhere requires the rollout too
		// until someone sees it complete. Failing to mark it only leaves this
		// run strict, which it is already.
		if err := m.MarkCNIRolloutPending(ctx, ns, name, after); err != nil {
			_, _ = fmt.Fprintf(out, "note: %v; only this run will require the CNI's rollout\n", err)
		}
	}
	return nil
}

// waitAvailable is m.WaitAvailable, saying first when this run's CNI change
// requires the rollout, and after when the CNI was counted ready without it.
func waitAvailable(ctx context.Context, cmd *cobra.Command, m *cluster.Manager, ns, name string, timeout time.Duration) (cluster.Summary, error) {
	if m.RequireCNIRollout {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "this run changes the CNI: waiting for Cilium's agents and operator to roll out, read through the cluster's own API")
	}
	s, err := m.WaitAvailable(ctx, ns, name, timeout)
	if err == nil && s.CNIRolloutUnverified() {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "note: the CNI is counted ready on its release alone; its rollout could not be read (%s). No unverified change to it is recorded\n", s.CNIWhy)
	}
	return s, err
}

// checkPruneRefusals refuses an apply, before anything is written, when it
// would have to remove something it may not. Refusals depend only on what is
// labelled and what is rendered, so they hold before and after the apply.
// With prune nil (create) there is nothing to check.
func checkPruneRefusals(ctx context.Context, m *cluster.Manager, objs []*unstructured.Unstructured, ns, name string, prune *cluster.PruneOptions) error {
	if prune == nil {
		return nil
	}
	orphans, err := m.PlanPrune(ctx, objs, ns, name, *prune)
	if err != nil {
		return err
	}
	return cluster.Refusals(orphans)
}

// pruneOrReport handles what hydra.yaml no longer renders, after the apply:
// apply prunes it (decided again now, as the apply may just have made a
// template unused) and says what it kept; create only lists it.
func pruneOrReport(ctx context.Context, cmd *cobra.Command, m *cluster.Manager, objs []*unstructured.Unstructured, ns, name string, prune *cluster.PruneOptions) error {
	if prune != nil {
		orphans, err := m.PlanPrune(ctx, objs, ns, name, *prune)
		if err != nil {
			return fmt.Errorf("applied, but could not work out what to prune (run apply again): %w", err)
		}
		_, skipped, err := m.Prune(ctx, orphans)
		if err != nil {
			return fmt.Errorf("applied, but %w (run apply again)", err)
		}
		for _, or := range orphans {
			if or.Action != cluster.PruneDelete {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s\n", or)
			}
		}
		if skipped > 0 {
			// Not done: a script must not read this as a finished apply.
			return fmt.Errorf("applied, but %d object(s) to prune changed while apply ran and were left; run apply again", skipped)
		}
		return nil
	}
	switch orphans, err := m.Orphans(ctx, objs, ns, name); {
	case err != nil:
		// The apply succeeded; only the check for leftovers failed.
		// Say so rather than imply there are none.
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not check for objects hydra.yaml no longer renders: %v\n", err)
	case len(orphans) > 0:
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "not in hydra.yaml any more (left in place; `hydra cluster apply` prunes what is safe):\n  %s\n", strings.Join(orphans, "\n  "))
	}
	return nil
}

// addonNotes says, for each addon that is not Ready, whether it is still
// being installed or has failed, and whether the addons could be read at all.
func addonNotes(s cluster.Summary) []string {
	if s.AddonsError != "" {
		return []string{"addons could not be read: " + s.AddonsError}
	}
	var out []string
	for _, a := range s.Addons {
		switch a.Ready {
		case cluster.ConditionTrue:
		case "", "Unknown":
			out = append(out, "addon "+a.Name+": still being installed by Cluster API Add-on Provider Helm; follow it with `hydra cluster status`")
		default:
			out = append(out, "addon "+a.Name+" is not Ready: "+a.Why())
		}
	}
	return out
}

func newPlanCommand() *cobra.Command {
	var file, mgmt string
	var forceConflicts, adoptCNI bool
	var prune cluster.PruneOptions
	c := &cobra.Command{
		Use:   "plan",
		Short: "Show what create or apply would change, against the live management cluster",
		Long: `plan renders hydra.yaml and server-side dry-runs every object against the
management cluster, then prints what would be created and a diff of what would
change. It writes nothing and compares only with the live cluster, never with
anything stored locally.

Objects labelled for the cluster that hydra.yaml no longer renders are listed
with what apply would do with them: prune (-), keep (?), or refuse to run (!).
Pass apply's --prune-pools and --allow-remove to see exactly what apply with
them would do. Templates still in use are re-checked by apply after it applies.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(file)
			if err != nil {
				return err
			}
			objs, err := renderObjects(cfg)
			if err != nil {
				return err
			}
			t, err := manager(cmd, mgmt, cfg.Metadata.Name)
			if err != nil {
				return err
			}
			defer t.Close()
			changes, err := t.m.Plan(contextOf(cmd), objs, cfg.Metadata.Name, forceConflicts)
			if err != nil {
				return err
			}
			orphans, err := t.m.PlanPrune(contextOf(cmd), objs, cfg.Metadata.Namespace, cfg.Metadata.Name, prune)
			if err != nil {
				return err
			}
			if err := t.m.CheckAddonProvider(objs); err != nil {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "! addons: create refuses this: %v\n", err)
			}
			if unprotected, err := t.m.UnprotectedRoll(contextOf(cmd), objs, cfg.Metadata.Namespace, cfg.Metadata.Name); err != nil {
				return err
			} else if unprotected {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "! note: %s\n", cluster.UnprotectedRollNote)
			}
			st, err := t.m.State(contextOf(cmd), cfg.Metadata.Namespace, cfg.Metadata.Name)
			if err != nil {
				return err
			}
			if msg := st.CNIConflict(render.HydraCNI(cfg), adoptCNI, t.kubectl(file)); msg != "" {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "! cni: create refuses this: %s\n", msg)
			}
			// Judge the label by what the apply would leave: when hydra is its
			// only manager the dry-run already drops it, and the diff says so.
			if msg := st.Planned(changes).CNILabelLeft(render.HydraCNI(cfg), t.kubectl(file)); msg != "" {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "! cni: %s\n", msg)
			}
			drift, err := upgrade.Drift(contextOf(cmd), t.m.C, cfg)
			if err != nil {
				return err
			}
			if len(drift) > 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "! version: hydra.yaml says %s, but %s. create refuses this; `hydra cluster upgrade --to %s --dry-run` shows the ordered upgrade\n", cfg.Spec.KubernetesVersion, strings.Join(drift, ", "), cfg.Spec.KubernetesVersion)
			}
			return printPlan(cmd.OutOrStdout(), changes, orphans)
		},
	}
	c.Flags().StringVarP(&file, "file", "f", "hydra.yaml", "path to hydra.yaml")
	c.Flags().StringVar(&mgmt, "management-kubeconfig", "", "kubeconfig of the management cluster (default: the cluster's bootstrap kind cluster)")
	addBastionFlag(c)
	c.Flags().BoolVar(&forceConflicts, "force-conflicts", false, "plan as create --force-conflicts would apply")
	addPruneFlags(c, &prune)
	c.Flags().BoolVar(&adoptCNI, "adopt-cni", false, adoptCNIUsage)
	return c
}

// orphanSummary closes a plan that has orphans. It says when apply will not
// run at all, so a plan with refusals never ends reading as a no-op.
func orphanSummary(orphans []cluster.Orphan) string {
	prunes, refused := 0, 0
	for _, o := range orphans {
		switch o.Action {
		case cluster.PruneDelete:
			prunes++
		case cluster.PruneRefused:
			refused++
		}
	}
	s := fmt.Sprintf("%d object(s) no longer rendered, %d of which apply would prune", len(orphans), prunes)
	if refused > 0 {
		s += fmt.Sprintf("; %d refused, so apply will not run until each is resolved (see the ! refused lines)", refused)
	}
	return s + "."
}

func printPlan(w io.Writer, changes []cluster.Change, orphans []cluster.Orphan) error {
	var b strings.Builder
	for _, o := range orphans {
		switch o.Action {
		case cluster.PruneDelete:
			fmt.Fprintf(&b, "- prune %s (no longer rendered; apply deletes it, create leaves it: %s)\n", orphanName(o), o.Why)
		case cluster.PruneRefused:
			fmt.Fprintf(&b, "! refused %s (no longer rendered; apply refuses to run: %s)\n", orphanName(o), o.Why)
		default:
			fmt.Fprintf(&b, "? kept %s (no longer rendered; %s)\n", orphanName(o), o.Why)
		}
	}
	if len(changes) == 0 {
		if len(orphans) == 0 {
			b.WriteString("No changes: the live cluster matches hydra.yaml.\n")
		} else {
			fmt.Fprintf(&b, "No changes to apply; %s\n", orphanSummary(orphans))
		}
		_, err := io.WriteString(w, b.String())
		return err
	}
	for _, ch := range changes {
		switch ch.Action {
		case "create":
			fmt.Fprintf(&b, "+ create %s\n", ch.Object)
		case "conflict":
			fmt.Fprintf(&b, "! conflict %s: another field manager owns fields hydra.yaml sets differently; create would fail (use --force-conflicts to take them over)\n  %s\n", ch.Object, ch.Diff)
		default:
			fmt.Fprintf(&b, "~ update %s\n", ch.Object)
			if ch.Note != "" {
				fmt.Fprintf(&b, "  note: %s\n", ch.Note)
			}
			fmt.Fprintf(&b, "%s\n", ch.Diff)
		}
	}
	if len(orphans) > 0 {
		fmt.Fprintf(&b, "%d change(s); %s\n", len(changes), orphanSummary(orphans))
	} else {
		fmt.Fprintf(&b, "%d change(s).\n", len(changes))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// orphanName is "Kind ns/name".
func orphanName(o cluster.Orphan) string {
	return o.Object.GetKind() + " " + o.Object.GetNamespace() + "/" + o.Object.GetName()
}

func newStatusCommand() *cobra.Command {
	var file, mgmt string
	c := &cobra.Command{
		Use:   "status",
		Short: "Show the cluster's progress as its controllers report it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			md, err := config.LoadMetadata(file)
			if err != nil {
				return err
			}
			t, err := manager(cmd, mgmt, md.Name)
			if err != nil {
				return err
			}
			defer t.Close()
			s, err := t.m.Status(contextOf(cmd), md.Namespace, md.Name)
			if err != nil {
				return err
			}
			out := fmt.Sprintf("%s/%s: %s\n", md.Namespace, md.Name, s.Line())
			if s.AvailableMessage != "" && s.Available != cluster.ConditionTrue {
				out += "  " + s.AvailableMessage + "\n"
			}
			var addonLines strings.Builder
			if s.AddonsError != "" {
				addonLines.WriteString("  addons: " + s.AddonsError + "\n")
			}
			for _, a := range s.Addons {
				if why := a.Why(); why != "" {
					addonLines.WriteString("  addon " + a.Name + ": " + why + "\n")
				}
			}
			out += addonLines.String()
			_, err = io.WriteString(cmd.OutOrStdout(), out)
			return err
		},
	}
	c.Flags().StringVarP(&file, "file", "f", "hydra.yaml", "path to hydra.yaml")
	c.Flags().StringVar(&mgmt, "management-kubeconfig", "", "kubeconfig of the management cluster (default: the cluster's bootstrap kind cluster)")
	addBastionFlag(c)
	return c
}

// confirmIn is where delete reads its confirmation; tests replace it.
var confirmIn io.Reader = os.Stdin

func newDeleteCommand() *cobra.Command {
	var file, mgmt string
	var yes, adopt, selfHosted bool
	var timeout time.Duration
	var to teardownOptions
	c := &cobra.Command{
		Use:   deleteVerb,
		Short: "Delete the cluster: its machines, their VMs and disks",
		Long: `delete deletes the Cluster object and waits while Cluster API and the Hydra
provider tear down its machines, VMs and disks; a network Hydra created for it
is removed once nothing else uses it. It then removes the rendered objects Cluster API does not own (templates,
the CNI ClusterResourceSet, addon HelmChartProxies: only after the machines are gone, since deleting a proxy
uninstalls its addon). The hypervisor credentials Secret is kept.

This cannot be undone. It shows which management cluster it will act on, then
asks for the cluster's name unless --yes is given, and refuses without --yes
when there is no terminal to ask on. It refuses outright to delete a cluster
from itself (a self-hosted management cluster). Only metadata.name and
metadata.namespace are read from hydra.yaml.

With --self-hosted it tears down a cluster that manages itself instead: it
brings up the cluster's bootstrap kind cluster (no hypervisor credentials
needed: they come back with the move), moves the cluster into it, waits until
kind's provider has taken every VM over, deletes the cluster from there, then
removes the cluster's kubeconfig file and kind. Before moving it proves the
kubeconfig is the cluster itself and that the cluster is this CLI's to delete.
It leaves a record in kind, so an interrupted teardown is finished by running
the same command again; a move interrupted part way is reported, and finished
with --finish-split.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if selfHosted {
				if mgmt != "" {
					return fmt.Errorf("--self-hosted reaches the cluster with its own kubeconfig (--cluster-kubeconfig); it cannot be combined with --management-kubeconfig")
				}
				if bastionSet(cmd) {
					return fmt.Errorf("--self-hosted cannot go through --bastion: it reaches the cluster and a local bootstrap cluster directly")
				}
				cfg, err := config.Load(file)
				if err != nil {
					return err
				}
				ctx, stop := interruptible(cmd)
				defer stop()
				to.yes, to.adopt, to.timeout, to.configFile = yes, adopt, timeout, file
				return runTeardown(ctx, cmd, cfg, to)
			}
			md, err := config.LoadMetadata(file)
			if err != nil {
				return err
			}
			// Resolve and show the target before asking: the confirmation
			// must be about this cluster on that management cluster.
			t, err := manager(cmd, mgmt, md.Name)
			if err != nil {
				return err
			}
			defer t.Close()
			ctx, stop := interruptible(cmd)
			defer stop()
			st, err := t.m.State(ctx, md.Namespace, md.Name)
			if err != nil {
				return err
			}
			// Ownership first: never offer to delete another cluster's Cluster.
			if err := st.CheckOwner(md.Name, adopt); err != nil {
				return err
			}
			var leftovers []string
			if !st.Exists {
				// An interrupted create can leave a HydraCluster, control
				// plane or MachineDeployments with no Cluster: deleting those
				// removes VMs, so it needs the same confirmation.
				if leftovers, err = t.m.Labelled(ctx, md.Namespace, md.Name); err != nil {
					return err
				}
				if len(leftovers) == 0 {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Cluster %s/%s does not exist there, and nothing is labelled for it; nothing to delete\n", md.Namespace, md.Name)
					return nil
				}
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Cluster %s/%s does not exist there, but these objects labelled for it would be deleted:\n  %s\n", md.Namespace, md.Name, strings.Join(leftovers, "\n  "))
			} else if st.Terminating {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Cluster %s/%s is already being deleted\n", md.Namespace, md.Name)
			}
			self, why, err := t.m.SelfHosted(ctx, md.Namespace, md.Name, t.kubeconfig, st)
			if err != nil {
				return fmt.Errorf("cannot tell whether %s manages itself, so not deleting: %w", md.Name, err)
			}
			if self {
				return fmt.Errorf("refusing to delete cluster %s/%s on itself: %s. Its controllers run on the machines being deleted, so the teardown would stop half way. Use `hydra cluster delete --self-hosted`, which moves it into a bootstrap cluster and deletes it from there", md.Namespace, md.Name, why)
			}
			if !yes {
				if err := confirm(cmd, md, t.source); err != nil {
					return err
				}
			}
			return t.m.Delete(ctx, md.Namespace, md.Name, timeout, adopt)
		},
	}
	c.Flags().StringVarP(&file, "file", "f", "hydra.yaml", "path to hydra.yaml")
	c.Flags().StringVar(&mgmt, "management-kubeconfig", "", "kubeconfig of the management cluster (default: the cluster's bootstrap kind cluster)")
	addBastionFlag(c)
	c.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	c.Flags().BoolVar(&adopt, "adopt", false, "delete a Cluster hydra did not make (unlabelled); one labelled for another cluster is always refused")
	c.Flags().DurationVar(&timeout, "timeout", 20*time.Minute, "how long to wait for the teardown")
	c.Flags().BoolVar(&selfHosted, "self-hosted", false, "tear down a cluster that manages itself: move it into its bootstrap kind cluster, delete it there, remove kind")
	c.Flags().StringVar(&to.clusterKubeconfig, "cluster-kubeconfig", "", "with --self-hosted: the cluster's own kubeconfig (default ./<name>.kubeconfig)")
	c.Flags().BoolVar(&to.finishSplit, "finish-split", false, "with --self-hosted: finish a move interrupted part way, after proving which copy is the source")
	c.Flags().DurationVar(&to.takeoverTimeout, "takeover-timeout", 10*time.Minute, "with --self-hosted: how long to wait for the bootstrap cluster to take every VM over before deleting")
	return c
}

func confirm(cmd *cobra.Command, md config.Metadata, source string) error {
	if f, ok := confirmIn.(*os.File); ok && !term.IsTerminal(int(f.Fd())) {
		return errors.New("refusing to delete without --yes when there is no terminal to confirm on")
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "This deletes cluster %s/%s (managed by %s), its machines and their disks. Type the cluster name to confirm: ", md.Namespace, md.Name, source)
	line, err := bufio.NewReader(confirmIn).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if strings.TrimSpace(line) != md.Name {
		return errors.New("confirmation did not match; nothing deleted")
	}
	return nil
}

// writeKubeconfig writes admin credentials so that they are never readable by
// others, even when replacing an existing file: a temporary file created 0600
// in the same directory is renamed over the target (replacing, not following,
// a symlink). Without an explicit path, an existing file with different
// content is not overwritten.
func writeKubeconfig(path string, kc []byte, explicit bool) error {
	if !explicit {
		if old, err := os.ReadFile(path); err == nil && !bytes.Equal(old, kc) {
			return fmt.Errorf("%s exists with other content; not overwriting it (pass --kubeconfig-out to choose where to write)", path)
		}
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".hydra-kubeconfig-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(kc); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
