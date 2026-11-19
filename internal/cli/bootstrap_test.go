package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

const pivExample = "../../examples/workload-managed-network.yaml"

// pivBadCredentials is a credentials directory missing two of its files.
func pivBadCredentials(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cacert.pem"), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestBootstrapCommand(t *testing.T) {
	for _, tc := range []struct {
		name        string
		args        []string
		kind        *pivFakeKind
		bootErr     error
		wantErr     string
		wantCreated bool
	}{
		{name: "an unreadable hydra.yaml", args: []string{"-f", "/nonexistent/hydra.yaml"}, kind: &pivFakeKind{}, wantErr: "/nonexistent/hydra.yaml"},
		{name: "invalid credentials are refused before kind", args: []string{"--hypervisor-credentials", "CREDS"}, kind: &pivFakeKind{}, wantErr: "hypervisor credentials"},
		{name: "no container runtime", kind: &pivFakeKind{}, bootErr: errors.New("no docker"), wantErr: "no docker"},
		{name: "a first bootstrap needs credentials", kind: &pivFakeKind{}, wantErr: "--hypervisor-credentials"},
		{name: "an existing kind is brought to the pinned state", kind: &pivFakeKind{exists: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pivWithBootstrapper(t, tc.kind, &pivFakeCluster{}, tc.bootErr)
			args := make([]string, 0, 3+len(tc.args))
			args = append(args, lcBootstrap, "-f", pivExample)
			for _, a := range tc.args {
				if a == "CREDS" {
					a = pivBadCredentials(t)
				}
				args = append(args, a)
			}
			_, errOut, err := run(t, args...)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatal(err)
			case tc.wantErr == "" && !strings.Contains(errOut, "bootstrap cluster hydra-bootstrap-demo is ready"):
				t.Errorf("stderr %q", errOut)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
			if tc.kind.created != tc.wantCreated {
				t.Errorf("kind created=%v, want %v", tc.kind.created, tc.wantCreated)
			}
		})
	}
}

func TestBootstrapDeleteCommand(t *testing.T) {
	for _, tc := range []struct {
		name        string
		args        []string
		kind        *pivFakeKind
		managed     []string
		bootErr     error
		wantErr     string
		wantDeleted bool
	}{
		{name: "an unreadable hydra.yaml", args: []string{"-f", "/nonexistent/hydra.yaml"}, kind: &pivFakeKind{exists: true}, wantErr: "/nonexistent/hydra.yaml"},
		{name: "no container runtime", kind: &pivFakeKind{exists: true}, bootErr: errors.New("no docker"), wantErr: "no docker"},
		{name: "refuses while it manages a cluster", kind: &pivFakeKind{exists: true}, managed: []string{"Cluster demo/demo"}, wantErr: "still manages Cluster demo/demo"},
		{name: "--force deletes anyway", args: []string{"--force"}, kind: &pivFakeKind{exists: true}, managed: []string{"Cluster demo/demo"}, wantDeleted: true},
		{name: "deletes an idle kind", kind: &pivFakeKind{exists: true}, wantDeleted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pivWithBootstrapper(t, tc.kind, &pivFakeCluster{managed: tc.managed}, tc.bootErr)
			_, _, err := run(t, append([]string{lcBootstrap, "delete", "-f", pivExample}, tc.args...)...)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
			if tc.kind.deleted != tc.wantDeleted {
				t.Errorf("kind deleted=%v, want %v", tc.kind.deleted, tc.wantDeleted)
			}
		})
	}
}

func TestBootstrapKubeconfigCommand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		file    string
		kind    *pivFakeKind
		bootErr error
		wantErr string
	}{
		{name: "an unreadable hydra.yaml", file: "/nonexistent/hydra.yaml", kind: &pivFakeKind{exists: true}, wantErr: "/nonexistent/hydra.yaml"},
		{name: "no container runtime", kind: &pivFakeKind{exists: true}, bootErr: errors.New("no docker"), wantErr: "no docker"},
		{name: "no kind cluster", kind: &pivFakeKind{}, wantErr: "hydra-bootstrap-demo does not exist"},
		{name: "prints the kubeconfig", kind: &pivFakeKind{exists: true, kubeconfig: testKubeconfig}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pivWithBootstrapper(t, tc.kind, &pivFakeCluster{}, tc.bootErr)
			file := tc.file
			if file == "" {
				file = pivExample
			}
			out, _, err := run(t, lcBootstrap, "kubeconfig", "-f", file)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want %q, got %v", tc.wantErr, err)
				}
				if out != "" {
					t.Errorf("nothing may be printed on failure: %q", out)
				}
				return
			}
			if err != nil || out != testKubeconfig {
				t.Fatalf("got %q %v", out, err)
			}
		})
	}
}

func TestContextOf(t *testing.T) {
	if contextOf(&cobra.Command{}) == nil {
		t.Fatal("a command without a context must get a background one")
	}
	type key struct{}
	cmd := &cobra.Command{}
	cmd.SetContext(context.WithValue(context.Background(), key{}, "v"))
	if contextOf(cmd).Value(key{}) != "v" {
		t.Error("a command's own context must be used")
	}
}
