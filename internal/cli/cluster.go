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
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Petatron/hydra/internal/bootstrap"
	"github.com/Petatron/hydra/internal/cluster"
	"github.com/Petatron/hydra/internal/config"
	"github.com/Petatron/hydra/internal/render"
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
	c, err := connectManagement(kc)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", source, err)
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "management cluster: %s (%s)\n", source, server)
	return &target{m: &cluster.Manager{C: c, Out: cmd.ErrOrStderr()}, source: source, server: server,
		bootstrapKind: kubeconfigPath == ""}, nil
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
	var file, mgmt, kubeconfigOut string
	var timeout time.Duration
	var wait, adopt bool
	c := &cobra.Command{
		Use:   "create",
		Short: "Create the cluster hydra.yaml describes and wait for it to be Available",
		Long: `create renders hydra.yaml, server-side applies the result to the management
cluster (field manager "hydra"), waits until Cluster API reports the cluster
Available with its CNI applied, and writes the cluster's kubeconfig.

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
			t, err := manager(cmd, mgmt, cfg.Metadata.Name)
			if err != nil {
				return err
			}
			m := t.m
			ctx, stop := interruptible(cmd)
			defer stop()
			ns, name := cfg.Metadata.Namespace, cfg.Metadata.Name
			// Everything that can refuse runs before the first write.
			if err := m.CheckManagementCluster(); err != nil {
				return err
			}
			st, err := m.State(ctx, ns, name)
			if err != nil {
				return err
			}
			if st.Terminating {
				return fmt.Errorf("cluster %s/%s is being deleted; let `hydra cluster delete` finish before creating it again", ns, name)
			}
			if !st.Exists && t.bootstrapKind {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "creating a NEW cluster %s/%s from the bootstrap cluster; if it already runs elsewhere (pivoted), stop and use --management-kubeconfig\n", ns, name)
			}
			if h := cfg.Spec.Hypervisor; h != nil {
				if err := m.CheckCredentials(ctx, ns, h.CredentialsSecretName); err != nil {
					return err
				}
			}
			if err := m.CheckOwnership(ctx, objs, name, adopt); err != nil {
				return err
			}
			if err := m.Apply(ctx, objs); err != nil {
				return err
			}
			if orphans, err := m.Orphans(ctx, objs, ns, name); err == nil && len(orphans) > 0 {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "not in hydra.yaml any more (left in place; delete them yourself if unused):\n  %s\n", strings.Join(orphans, "\n  "))
			}
			if !wait {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "not waiting (--wait=false); follow with `hydra cluster status`")
				return nil
			}
			start := time.Now()
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "waiting up to %s for cluster %s/%s to be Available\n", timeout, ns, name)
			if err := m.WaitAvailable(ctx, ns, name, timeout); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "cluster %s/%s is Available after %s\n", ns, name, time.Since(start).Round(time.Second))
			kc, err := m.Kubeconfig(ctx, ns, name)
			if err != nil {
				return err
			}
			path := kubeconfigOut
			if path == "" {
				path = name + ".kubeconfig"
			}
			if err := writeKubeconfig(path, kc, kubeconfigOut != ""); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "kubeconfig written to %s\n", path)
			return nil
		},
	}
	c.Flags().StringVarP(&file, "file", "f", "hydra.yaml", "path to hydra.yaml")
	c.Flags().StringVar(&mgmt, "management-kubeconfig", "", "kubeconfig of the management cluster (default: the cluster's bootstrap kind cluster)")
	c.Flags().StringVar(&kubeconfigOut, "kubeconfig-out", "", "where to write the new cluster's kubeconfig (default ./<name>.kubeconfig)")
	c.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "how long to wait for the cluster to be Available")
	c.Flags().BoolVar(&wait, "wait", true, "wait for the cluster to be Available")
	c.Flags().BoolVar(&adopt, "adopt", false, "take over existing objects of the same names that hydra did not make (never ones labelled for another cluster)")
	return c
}

func newPlanCommand() *cobra.Command {
	var file, mgmt string
	c := &cobra.Command{
		Use:   "plan",
		Short: "Show what create would change, against the live management cluster",
		Long: `plan renders hydra.yaml and server-side dry-runs every object against the
management cluster, then prints what would be created and a diff of what would
change. It writes nothing and compares only with the live cluster, never with
anything stored locally.`,
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
			changes, err := t.m.Plan(contextOf(cmd), objs)
			if err != nil {
				return err
			}
			orphans, err := t.m.Orphans(contextOf(cmd), objs, cfg.Metadata.Namespace, cfg.Metadata.Name)
			if err != nil {
				return err
			}
			return printPlan(cmd.OutOrStdout(), changes, orphans)
		},
	}
	c.Flags().StringVarP(&file, "file", "f", "hydra.yaml", "path to hydra.yaml")
	c.Flags().StringVar(&mgmt, "management-kubeconfig", "", "kubeconfig of the management cluster (default: the cluster's bootstrap kind cluster)")
	return c
}

func printPlan(w io.Writer, changes []cluster.Change, orphans []string) error {
	var b strings.Builder
	for _, o := range orphans {
		fmt.Fprintf(&b, "? orphaned %s (labelled for this cluster, no longer rendered; create leaves it in place)\n", o)
	}
	if len(changes) == 0 {
		b.WriteString("No changes: the live cluster matches hydra.yaml.\n")
		_, err := io.WriteString(w, b.String())
		return err
	}
	for _, ch := range changes {
		switch ch.Action {
		case "create":
			fmt.Fprintf(&b, "+ create %s\n", ch.Object)
		default:
			fmt.Fprintf(&b, "~ update %s\n", ch.Object)
			if ch.Note != "" {
				fmt.Fprintf(&b, "  note: %s\n", ch.Note)
			}
			fmt.Fprintf(&b, "%s\n", ch.Diff)
		}
	}
	fmt.Fprintf(&b, "%d change(s).\n", len(changes))
	_, err := io.WriteString(w, b.String())
	return err
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
			s, err := t.m.Status(contextOf(cmd), md.Namespace, md.Name)
			if err != nil {
				return err
			}
			out := fmt.Sprintf("%s/%s: %s\n", md.Namespace, md.Name, s.Line())
			if s.AvailableMessage != "" && s.Available != cluster.ConditionTrue {
				out += "  " + s.AvailableMessage + "\n"
			}
			_, err = io.WriteString(cmd.OutOrStdout(), out)
			return err
		},
	}
	c.Flags().StringVarP(&file, "file", "f", "hydra.yaml", "path to hydra.yaml")
	c.Flags().StringVar(&mgmt, "management-kubeconfig", "", "kubeconfig of the management cluster (default: the cluster's bootstrap kind cluster)")
	return c
}

// confirmIn is where delete reads its confirmation; tests replace it.
var confirmIn io.Reader = os.Stdin

func newDeleteCommand() *cobra.Command {
	var file, mgmt string
	var yes bool
	var timeout time.Duration
	c := &cobra.Command{
		Use:   "delete",
		Short: "Delete the cluster: its machines, their VMs and disks",
		Long: `delete deletes the Cluster object and waits while Cluster API and the Hydra
provider tear down its machines, VMs and disks; a network Hydra created for it
is removed once nothing else uses it. It then removes the rendered objects Cluster API does not own (templates,
the CNI ClusterResourceSet). The hypervisor credentials Secret is kept.

This cannot be undone. It shows which management cluster it will act on, then
asks for the cluster's name unless --yes is given, and refuses without --yes
when there is no terminal to ask on. It refuses outright to delete a cluster
from itself (a self-hosted management cluster). Only metadata.name and
metadata.namespace are read from hydra.yaml.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			ctx, stop := interruptible(cmd)
			defer stop()
			st, err := t.m.State(ctx, md.Namespace, md.Name)
			if err != nil {
				return err
			}
			switch {
			case !st.Exists:
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Cluster %s/%s does not exist there; only leftover objects labelled for it will be removed\n", md.Namespace, md.Name)
			case st.Terminating:
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Cluster %s/%s is already being deleted; waiting for it\n", md.Namespace, md.Name)
			}
			if st.Exists {
				if self, why := t.m.SelfHosted(ctx, md.Namespace, md.Name, t.server, st); self {
					return fmt.Errorf("refusing to delete cluster %s/%s on itself: %s. Its controllers run on the machines being deleted, so the teardown would stop half way. Move it to another management cluster first (clusterctl move), then delete it from there", md.Namespace, md.Name, why)
				}
			}
			if !yes && st.Exists && !st.Terminating {
				if err := confirm(cmd, md, t.source); err != nil {
					return err
				}
			}
			return t.m.Delete(ctx, md.Namespace, md.Name, timeout)
		},
	}
	c.Flags().StringVarP(&file, "file", "f", "hydra.yaml", "path to hydra.yaml")
	c.Flags().StringVar(&mgmt, "management-kubeconfig", "", "kubeconfig of the management cluster (default: the cluster's bootstrap kind cluster)")
	c.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	c.Flags().DurationVar(&timeout, "timeout", 20*time.Minute, "how long to wait for the teardown")
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
