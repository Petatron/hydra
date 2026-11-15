package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testKubeconfig = `apiVersion: v1
kind: Config
clusters: [{name: m, cluster: {server: "https://mgmt.example:6443"}}]
contexts: [{name: m, context: {cluster: m, user: u}}]
current-context: m
users: [{name: u, user: {token: x}}]
`

// withFakeManagement points the cluster commands at a fake client holding objs.
func withFakeManagement(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	origT, origC := managementTarget, connectManagement
	managementTarget = func(string, string) ([]byte, string, error) { return []byte(testKubeconfig), "test-mgmt", nil }
	connectManagement = func([]byte) (client.Client, error) { return c, nil }
	t.Cleanup(func() { managementTarget, connectManagement = origT, origC })
	return c
}

func capiCluster(ns, name, endpointHost string, port int64) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetAPIVersion("cluster.x-k8s.io/v1beta2")
	u.SetKind("Cluster")
	u.SetNamespace(ns)
	u.SetName(name)
	if endpointHost != "" {
		_ = unstructured.SetNestedField(u.Object, endpointHost, "spec", "controlPlaneEndpoint", "host")
		_ = unstructured.SetNestedField(u.Object, port, "spec", "controlPlaneEndpoint", "port")
	}
	return u
}

func TestDeleteConfirmsAfterShowingTheTarget(t *testing.T) {
	c := withFakeManagement(t, capiCluster("default", "hydra-mgmt", "10.0.0.5", 6443))
	origIn := confirmIn
	t.Cleanup(func() { confirmIn = origIn })
	confirmIn = strings.NewReader("wrong-name\n")

	_, errOut, err := run(t, "cluster", "delete", "-f", "../../examples/hydra-mgmt.yaml")
	if err == nil || !strings.Contains(err.Error(), "did not match") {
		t.Fatalf("a wrong name must refuse: %v", err)
	}
	if !strings.Contains(errOut, "management cluster: test-mgmt (https://mgmt.example:6443)") ||
		!strings.Contains(errOut, "default/hydra-mgmt (managed by test-mgmt)") {
		t.Errorf("the target must be shown before and in the prompt:\n%s", errOut)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "hydra-mgmt"}, capiCluster("", "", "", 0)); err != nil {
		t.Errorf("the Cluster must still exist: %v", err)
	}
}

func TestDeleteRefusesASelfHostedCluster(t *testing.T) {
	// The management API server is the cluster's own endpoint.
	c := withFakeManagement(t, capiCluster("default", "hydra-mgmt", "mgmt.example", 6443))
	_, _, err := run(t, "cluster", "delete", "-f", "../../examples/hydra-mgmt.yaml", "--yes")
	if err == nil || !strings.Contains(err.Error(), "on itself") {
		t.Fatalf("want a self-hosted refusal, got %v", err)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "hydra-mgmt"}, capiCluster("", "", "", 0)); err != nil {
		t.Errorf("the Cluster must still exist: %v", err)
	}
}

func TestClusterCommandsNeverUseCurrentContext(t *testing.T) {
	var got []string
	orig := managementTarget
	managementTarget = func(path, name string) ([]byte, string, error) {
		got = append(got, path+"|"+name)
		return []byte("not a kubeconfig"), "test", nil
	}
	t.Cleanup(func() { managementTarget = orig })
	for _, args := range [][]string{
		{"cluster", "plan", "-f", "../../examples/hydra-mgmt.yaml"},
		{"cluster", "status", "-f", "../../examples/hydra-mgmt.yaml", "--management-kubeconfig", "/x"},
	} {
		var out, errOut bytes.Buffer
		cmd := NewRootCommand(&out, &errOut)
		cmd.SetArgs(args)
		_ = cmd.Execute() // the fake kubeconfig cannot connect; only the target matters
	}
	if strings.Join(got, ",") != "|hydra-mgmt,/x|hydra-mgmt" {
		t.Errorf("targets: %v", got)
	}
}

func TestWriteKubeconfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.kubeconfig")
	// An existing world-readable file must not keep its mode.
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeKubeconfig(path, []byte("new"), false); err == nil {
		t.Fatal("must not overwrite a different file at the default path")
	}
	if err := writeKubeconfig(path, []byte("new"), true); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	if b, _ := os.ReadFile(path); string(b) != "new" {
		t.Errorf("content %q", b)
	}
	if err := writeKubeconfig(path, []byte("new"), false); err != nil {
		t.Errorf("rewriting the same content must succeed: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("temporary files left behind: %v", entries)
	}
}
