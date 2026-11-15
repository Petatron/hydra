package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/Petatron/hydra/internal/bootstrap"
	"github.com/Petatron/hydra/internal/config"
)

// newBootstrapper builds the real Bootstrapper; tests replace it.
var newBootstrapper = func(out io.Writer) (*bootstrap.Bootstrapper, error) {
	kind, err := bootstrap.NewKindProvider()
	if err != nil {
		return nil, fmt.Errorf("no container runtime for kind (docker, podman or nerdctl): %w", err)
	}
	return &bootstrap.Bootstrapper{Kind: kind, Connect: bootstrap.ConnectKube, Out: out}, nil
}

func newBootstrapCommand() *cobra.Command {
	var file, credentials string
	c := &cobra.Command{
		Use:   "bootstrap",
		Short: "Create the temporary management cluster (kind + Cluster API + Hydra)",
		Long: `bootstrap creates a kind cluster named hydra-bootstrap-<cluster> on this machine
and installs cert-manager, Cluster API and the Hydra provider into it at the
versions this CLI pins. It then writes the cluster's hypervisor credentials
Secret, labelled so that clusterctl move takes it along at the pivot.

Nothing is recorded locally: the kind cluster's name is the only handle. If a
run is interrupted, run the same command again and it continues from what
exists. A provider already installed at another version is reported and left
alone. Your kubeconfig and current context are not changed; use
'hydra bootstrap kubeconfig' to reach the cluster.

The controllers run in the kind node container on this machine, so the
hypervisor (libvirt TLS, port 16514) and the new cluster's API endpoint must be
reachable from inside that container, not only from this host: an address on
127.0.0.1, or one only this host can route, will not work.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(file)
			if err != nil {
				return err
			}
			var opts bootstrap.Options
			if credentials != "" {
				if opts.Credentials, err = bootstrap.LoadCredentials(credentials); err != nil {
					return err
				}
			}
			b, err := newBootstrapper(cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			// The first Ctrl-C cancels cleanly (temporary files are removed);
			// a second one, while kind is still pulling or starting a node,
			// gets the default behaviour and ends the process.
			ctx, stop := signal.NotifyContext(contextOf(cmd), os.Interrupt)
			defer stop()
			go func() { <-ctx.Done(); stop() }()
			return b.Create(ctx, cfg, opts)
		},
	}
	c.Flags().StringVarP(&file, "file", "f", "hydra.yaml", "path to hydra.yaml")
	c.Flags().StringVar(&credentials, "hypervisor-credentials", "", "directory holding cacert.pem, clientcert.pem and clientkey.pem for spec.hypervisor (needed the first time)")
	c.AddCommand(newBootstrapDeleteCommand(), newBootstrapKubeconfigCommand())
	return c
}

func newBootstrapDeleteCommand() *cobra.Command {
	var file string
	var force bool
	c := &cobra.Command{
		Use:   "delete",
		Short: "Delete the temporary management cluster",
		Long: `delete removes the hydra-bootstrap-<cluster> kind cluster. It refuses while
that cluster still manages Cluster API Clusters, because their machines would
keep running with nothing managing them; pivot or delete them first, or pass
--force.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Only the name: an invalid spec must not block cleanup.
			name, err := config.LoadName(file)
			if err != nil {
				return err
			}
			b, err := newBootstrapper(cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			return b.Delete(contextOf(cmd), name, force)
		},
	}
	c.Flags().StringVarP(&file, "file", "f", "hydra.yaml", "path to hydra.yaml")
	c.Flags().BoolVar(&force, "force", false, "delete even if the cluster still manages Cluster API Clusters")
	return c
}

func newBootstrapKubeconfigCommand() *cobra.Command {
	var file string
	c := &cobra.Command{
		Use:   "kubeconfig",
		Short: "Print the temporary management cluster's kubeconfig",
		Long: `kubeconfig prints the hydra-bootstrap-<cluster> kubeconfig to stdout, e.g.

  hydra bootstrap kubeconfig -f hydra.yaml > bootstrap.kubeconfig
  kubectl --kubeconfig bootstrap.kubeconfig get providers -A`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			name, err := config.LoadName(file)
			if err != nil {
				return err
			}
			b, err := newBootstrapper(cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			kc, err := b.KubeConfig(name)
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), kc)
			return err
		},
	}
	c.Flags().StringVarP(&file, "file", "f", "hydra.yaml", "path to hydra.yaml")
	return c
}

func contextOf(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}
