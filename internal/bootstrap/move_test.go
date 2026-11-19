package bootstrap

import (
	"context"
	"strings"
	"testing"
)

// A move that cannot load its kubeconfigs fails without contacting any
// cluster, and the kubeconfig copies it wrote are removed: they hold
// credentials.
func TestMoveNamespaceCleansUpOnFailure(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	err := MoveNamespace(context.Background(), []byte("not a kubeconfig"), []byte("neither is this"), "clusters", true)
	if err == nil || !strings.Contains(err.Error(), "hydra-kubeconfig-") {
		t.Fatalf("want a failure loading the scratch kubeconfig, got %v", err)
	}
	assertEmptyDir(t, tmp)
}
