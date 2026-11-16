package bootstrap

import (
	"context"

	clusterctl "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
	clusterctlconfig "sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
)

// MoveNamespace runs `clusterctl move` for one namespace, from one management
// cluster to another, with the same isolated configuration Init uses. With
// dryRun it only builds and checks the move graph. clusterctl moves every
// cluster in the namespace; callers check it holds only theirs.
func MoveNamespace(ctx context.Context, fromKubeconfig, toKubeconfig []byte, namespace string, dryRun bool) error {
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
	from, cleanFrom, err := scratchFile(string(fromKubeconfig))
	if err != nil {
		return err
	}
	defer cleanFrom()
	to, cleanTo, err := scratchFile(string(toKubeconfig))
	if err != nil {
		return err
	}
	defer cleanTo()
	return cc.Move(ctx, clusterctl.MoveOptions{
		FromKubeconfig: clusterctl.Kubeconfig{Path: from},
		ToKubeconfig:   clusterctl.Kubeconfig{Path: to},
		Namespace:      namespace,
		DryRun:         dryRun,
	})
}
