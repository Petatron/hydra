package bootstrap

import (
	"os"
	"slices"
	"time"

	"sigs.k8s.io/kind/pkg/cluster"
)

// KindProvider drives kind through its Go library, so the hydra binary needs
// no kind binary of its own -- only a container runtime kind can use.
type KindProvider struct {
	p *cluster.Provider
}

// NewKindProvider returns a KindProvider using the container runtime kind
// detects (docker, podman or nerdctl).
func NewKindProvider() (*KindProvider, error) {
	opt, err := cluster.DetectNodeProvider()
	if err != nil {
		return nil, err
	}
	return &KindProvider{p: cluster.NewProvider(opt)}, nil
}

// Exists reports whether a kind cluster of that name exists.
func (k *KindProvider) Exists(name string) (bool, error) {
	names, err := k.p.List()
	if err != nil {
		return false, err
	}
	return slices.Contains(names, name), nil
}

// Create creates the cluster. kind always writes a kubeconfig; it is pointed
// at a throwaway file so the user's own kubeconfig and current context are
// never touched. The kubeconfig is read back from kind whenever it is needed.
func (k *KindProvider) Create(name, nodeImage string) error {
	return k.withScratchKubeconfig(func(path string) error {
		return k.p.Create(name,
			cluster.CreateWithNodeImage(nodeImage),
			cluster.CreateWithWaitForReady(3*time.Minute),
			cluster.CreateWithKubeconfigPath(path),
			cluster.CreateWithDisplayUsage(false),
			cluster.CreateWithDisplaySalutation(false),
		)
	})
}

// Delete deletes the cluster, again without touching the user's kubeconfig.
func (k *KindProvider) Delete(name string) error {
	return k.withScratchKubeconfig(func(path string) error {
		return k.p.Delete(name, path)
	})
}

// KubeConfig returns the cluster's kubeconfig for use from this host.
func (k *KindProvider) KubeConfig(name string) (string, error) {
	return k.p.KubeConfig(name, false)
}

func (k *KindProvider) withScratchKubeconfig(fn func(path string) error) error {
	f, err := os.CreateTemp("", "hydra-kind-kubeconfig-*")
	if err != nil {
		return err
	}
	path := f.Name()
	_ = f.Close()
	defer func() { _ = os.Remove(path) }()
	return fn(path)
}
