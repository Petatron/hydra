package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := NewRootCommand(&out, &errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestRenderToStdout(t *testing.T) {
	out, _, err := run(t, "cluster", "render", "-f", "../../examples/hydra-mgmt.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "kind: KubeadmControlPlane") {
		t.Error("no KubeadmControlPlane in output")
	}
}

func TestRenderToDir(t *testing.T) {
	dir := t.TempDir()
	out, errOut, err := run(t, "cluster", "render", "-f", "../../examples/hydra-mgmt.yaml", "-o", dir)
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Error("with -o nothing should go to stdout")
	}
	if !strings.Contains(errOut, "hydra-mgmt.yaml") {
		t.Errorf("expected the written path on stderr, got %q", errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "hydra-mgmt.yaml")); err != nil {
		t.Error(err)
	}
}

func TestRenderInvalid(t *testing.T) {
	f := filepath.Join(t.TempDir(), "hydra.yaml")
	if err := os.WriteFile(f, []byte("apiVersion: hydra.io/v1alpha1\nkind: ClusterConfig\nmetadata: {name: x}\nspec: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := run(t, "cluster", "render", "-f", f)
	if err == nil || !strings.Contains(err.Error(), "spec.storagePool") {
		t.Errorf("want a field-level error, got %v", err)
	}
	if out != "" {
		t.Error("an invalid config must render nothing")
	}
}

func TestVersion(t *testing.T) {
	out, _, err := run(t, "version")
	if err != nil || !strings.Contains(out, "cilium 1.19.1") {
		t.Errorf("version: %q %v", out, err)
	}
}
