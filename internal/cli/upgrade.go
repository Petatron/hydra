package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/Petatron/hydra/internal/config"
	"github.com/Petatron/hydra/internal/upgrade"
)

// newRemote is the preflight's internet access; tests replace it.
var newRemote = func() upgrade.Remote { return &upgrade.HTTPRemote{} }

func newUpgradeCommand() *cobra.Command {
	var file, mgmt, to string
	var timeout time.Duration
	var dryRun, skipRemote bool
	c := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade the cluster's Kubernetes one minor version, control plane first, then each pool",
		Long: `upgrade moves a running cluster to the Kubernetes version --to names, in the
order the version-skew policy requires: the control plane first, until every
control-plane machine runs it, then each worker pool in hydra.yaml's order.
Drains respect PodDisruptionBudgets; a blocked drain is shown and waited on.

It refuses a downgrade, a jump of more than one minor version, a cluster that
is not settled, hydra.yaml edits beyond the version (apply them with create
first), a version pkgs.k8s.io does not carry, and, when a pool is autoscaled,
a minor Cluster Autoscaler has no release for.

Nothing is kept locally: each run reads the live versions and continues from
there, so an interrupted upgrade is finished by running the same command again.
hydra.yaml may say the version that runs or the target; afterwards, set it to
the target. --dry-run prints the next stage and its changes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if to == "" {
				return fmt.Errorf("--to is required: the Kubernetes version to upgrade to, like v1.36.6")
			}
			if timeout <= 0 {
				return fmt.Errorf("--timeout must be positive")
			}
			target, err := upgrade.ParseVersion(to)
			if err != nil {
				return err
			}
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
			u := &upgrade.Upgrader{
				M: t.m, Config: cfg, Render: renderObjects, Target: target,
				Timeout: timeout, Out: cmd.ErrOrStderr(), Kubectl: t.kubectl(file),
			}
			if !skipRemote {
				u.Remote = newRemote()
			}
			if dryRun {
				return u.Plan(ctx)
			}
			return u.Run(ctx)
		},
	}
	c.Flags().StringVarP(&file, "file", "f", "hydra.yaml", "path to hydra.yaml")
	c.Flags().StringVar(&mgmt, "management-kubeconfig", "", "kubeconfig of the management cluster (default: the cluster's bootstrap kind cluster)")
	c.Flags().StringVar(&to, "to", "", "the Kubernetes version to upgrade to, e.g. v1.36.6")
	c.Flags().DurationVar(&timeout, "timeout", 45*time.Minute, "how long to wait for each stage's rollout; it ends the wait, not the rollout")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print the next stage and what it would change, and change nothing")
	c.Flags().BoolVar(&skipRemote, "skip-remote-checks", false, "skip checking pkgs.k8s.io and registry.k8s.io (when this machine cannot reach them and you have checked by hand)")
	return c
}
