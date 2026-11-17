// Package cli wires the hydra command tree.
package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Petatron/hydra/internal/bootstrap"
	"github.com/Petatron/hydra/internal/config"
	"github.com/Petatron/hydra/internal/render"
)

// deleteVerb is the delete subcommand of both cluster and bootstrap.
const deleteVerb = "delete"

// Version is set at build time with -ldflags "-X github.com/Petatron/hydra/internal/cli.Version=...".
var Version = "dev"

// NewRootCommand builds the command tree. Output goes to out, diagnostics to
// errOut. Errors are returned, not printed: the caller reports them once.
func NewRootCommand(out, errOut io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:           "hydra",
		Short:         "Create and manage on-prem Kubernetes clusters with Cluster API",
		Long:          "hydra is a stateless orchestrator over Cluster API: every cluster's desired\nand actual state lives in Kubernetes, never in this CLI.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(out)
	root.SetErr(errOut)
	root.AddCommand(newVersionCommand(), newClusterCommand(), newBootstrapCommand(), newValidateCommand())
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return fmt.Errorf("%w\nRun '%s --help' for usage", err, c.CommandPath())
	})
	return root
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version and the component versions it pins",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(),
				"hydra %s\n  kubernetes (default) %s\n  cilium %s\n  kube-vip %s\n"+
					"  bootstrap: kind node %s\n  bootstrap: cert-manager %s, cluster-api %s, hydra provider %s\n",
				Version, config.DefaultKubernetesVersion, render.CiliumVersion, render.KubeVIPImage,
				bootstrap.NodeImage, bootstrap.CertManagerVersion, bootstrap.CAPIVersion, bootstrap.HydraVersion)
			return err
		},
	}
}

func newClusterCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "cluster",
		Short: "Work with a cluster described by hydra.yaml",
	}
	c.AddCommand(newRenderCommand(), newPlanCommand(), newCreateCommand(), newStatusCommand(), newDeleteCommand(), newPivotCommand())
	return c
}

func newRenderCommand() *cobra.Command {
	var file, outDir string
	c := &cobra.Command{
		Use:   "render",
		Short: "Translate hydra.yaml into Cluster API manifests",
		Long: `render prints the Cluster API manifests hydra.yaml describes. It contacts no
cluster and writes nothing unless -o is given, and the same input always gives
the same output, so the translation can be inspected and diffed.

Secrets are never rendered: when hydra.yaml names a hypervisor, the rendered
HydraCluster references its credentials Secret by name only.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(file)
			if err != nil {
				return err
			}
			manifests, err := render.Render(cfg, render.Options{CLIVersion: Version})
			if err != nil {
				return err
			}
			if outDir == "" {
				_, err = cmd.OutOrStdout().Write(manifests)
				return err
			}
			if err := os.MkdirAll(outDir, 0o755); err != nil {
				return err
			}
			path := filepath.Join(outDir, cfg.Metadata.Name+".yaml")
			if err := os.WriteFile(path, manifests, 0o644); err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.ErrOrStderr(), "wrote", path)
			return err
		},
	}
	c.Flags().StringVarP(&file, "file", "f", "hydra.yaml", "path to hydra.yaml")
	c.Flags().StringVarP(&outDir, "output-dir", "o", "", "write <name>.yaml into this directory instead of stdout")
	return c
}
