package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Petatron/hydra/internal/bootstrap"
	"github.com/Petatron/hydra/internal/cluster"
	"github.com/Petatron/hydra/internal/config"
	"github.com/Petatron/hydra/internal/pivot"
)

type teardownOptions struct {
	clusterKubeconfig string
	yes, adopt        bool
	finishSplit       bool
	timeout           time.Duration
	takeoverTimeout   time.Duration
	// configFile is the hydra.yaml path, for commands printed in errors.
	configFile string
}

// runTeardown is `cluster delete --self-hosted`: move the cluster from itself
// into its bootstrap kind cluster, delete it there, then remove its
// kubeconfig file and kind. Each run does the next safe step.
func runTeardown(ctx context.Context, cmd *cobra.Command, cfg *config.ClusterConfig, o teardownOptions) error {
	out := cmd.ErrOrStderr()
	md := cfg.Metadata
	path := o.clusterKubeconfig
	if path == "" {
		path = md.Name + ".kubeconfig"
	}

	self := pivot.Side{Name: "cluster " + md.Name, File: path}
	var selfErr error
	kc, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// No file: the cluster is reached only through what kind holds.
	case err != nil:
		return fmt.Errorf("reading %s: %w", path, err)
	default:
		self.Kubeconfig = kc
		if self.C, err = connectManagement(kc); err != nil {
			self.C, selfErr = nil, err
		}
	}

	b, err := newBootstrapper(out)
	if err != nil {
		return err
	}
	kindName := bootstrap.KindName(md.Name)
	kind := pivot.Side{Name: "bootstrap cluster " + kindName, File: kindName + ".kubeconfig",
		Prep: fmt.Sprintf("hydra bootstrap kubeconfig -f %s > %s.kubeconfig", o.configFile, kindName)}
	connectKind := func() (pivot.Side, error) {
		kkc, err := b.KubeConfig(md.Name)
		if err != nil {
			return pivot.Side{}, err
		}
		k := kind
		k.Kubeconfig = []byte(kkc)
		if k.C, err = connectManagement(k.Kubeconfig); err != nil {
			return pivot.Side{}, fmt.Errorf("connecting to %s: %w", kindName, err)
		}
		return k, nil
	}
	kindExists, err := b.Kind.Exists(kindName)
	if err != nil {
		return fmt.Errorf("looking for %s: %w", kindName, err)
	}
	if kindExists {
		if kind, err = connectKind(); err != nil {
			return err
		}
	}

	// Nothing local at all: say so only if the cluster's endpoint is silent,
	// never while something answers there.
	if self.Kubeconfig == nil && !kindExists {
		if ep := cfg.Spec.Network.ControlPlaneEndpoint; ep != "" {
			answers, err := endpointAnswers(ep)
			switch {
			case answers:
				return fmt.Errorf("no kubeconfig at %s and no %s, but something answers on %s, this cluster's endpoint: it may still run. Pass --cluster-kubeconfig with its kubeconfig", path, kindName, ep)
			case err != nil:
				// A dial error is not evidence the endpoint is empty.
				return fmt.Errorf("no kubeconfig at %s and no %s, and this machine cannot reach %s (%v) to show the cluster is gone. If it still runs, pass --cluster-kubeconfig; if you know it is gone, there is nothing to tear down", path, kindName, ep, err)
			}
		}
		_, _ = fmt.Fprintf(out, "found nothing to tear down for cluster %s/%s here: no kubeconfig at %s, no %s\n", md.Namespace, md.Name, path, kindName)
		return nil
	}

	if !o.yes {
		if err := confirm(cmd, md, "itself or "+kindName+"; a self-hosted teardown deletes it from "+kindName); err != nil {
			return err
		}
	}
	td := &pivot.Teardown{
		Self: self, SelfErr: selfErr, Kind: kind,
		Namespace: md.Namespace, Name: md.Name,
		Adopt: o.adopt, FinishSplit: o.finishSplit,
		TakeoverTimeout: o.takeoverTimeout,
		Out:             out,
		Hooks: pivot.TeardownHooks{
			EnsureKind: func(ctx context.Context) (pivot.Side, error) {
				if err := b.Create(ctx, cfg, bootstrap.Options{NoCredentials: true}); err != nil {
					return pivot.Side{}, err
				}
				return connectKind()
			},
			EnsureProviders: pivotHooks(out, kindName).EnsureProviders,
			Move:            bootstrap.MoveNamespace,
			DeleteCluster: func(ctx context.Context, k pivot.Side) error {
				return (&cluster.Manager{C: k.C, Out: out}).Delete(ctx, md.Namespace, md.Name, o.timeout, o.adopt)
			},
			DeleteKind:       func(ctx context.Context) error { return b.Delete(ctx, md.Name, false) },
			RemoveKubeconfig: func() error { return os.Remove(path) },
		},
	}
	if err := td.Run(ctx); errors.Is(err, pivot.ErrNothingToTearDown) {
		_, _ = fmt.Fprintf(out, "found nothing to tear down for cluster %s/%s\n", md.Namespace, md.Name)
		return nil
	} else if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "cluster %s/%s is torn down\n", md.Namespace, md.Name)
	return nil
}
