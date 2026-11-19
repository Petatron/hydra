package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Petatron/hydra/internal/bootstrap"
	"github.com/Petatron/hydra/internal/cluster"
	"github.com/Petatron/hydra/internal/config"
	"github.com/Petatron/hydra/internal/pivot"
)

func TestDeleteSelfHostedRefusesAManagementKubeconfig(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewRootCommand(&out, &errOut)
	root.SetArgs([]string{"cluster", "delete", "--self-hosted", "--management-kubeconfig", "x", "-f", "../../examples/workload-managed-network.yaml"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("got %v", err)
	}
}

func TestTeardownNothingLocal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		answers  bool
		probeErr error
		wantErr  string
	}{
		{"endpoint refuses: nothing to do", false, nil, ""},
		{"endpoint answers: refuse", true, nil, "may still run"},
		{"endpoint unreachable: cannot tell, refuse", false, errors.New("i/o timeout"), "cannot reach"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFakeKind(t, &fakeKind{})
			orig := endpointAnswers
			endpointAnswers = func(string) (bool, error) { return tc.answers, tc.probeErr }
			t.Cleanup(func() { endpointAnswers = orig })
			var errOut bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetErr(&errOut)
			o := teardownOptions{clusterKubeconfig: filepath.Join(t.TempDir(), "missing.kubeconfig")}
			err := runTeardown(context.Background(), cmd, selfHostedConfig("10.0.0.12"), o)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatal(err)
			case tc.wantErr == "" && !strings.Contains(errOut.String(), "found nothing to tear down"):
				t.Errorf("output %q", errOut.String())
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// Hydra chose the endpoint (a managed network): with nothing local there is
// nothing to probe, so nothing proves the cluster is gone.
func TestTeardownNothingLocalWithoutAnEndpointRefuses(t *testing.T) {
	withFakeKind(t, &fakeKind{})
	cmd := &cobra.Command{}
	cmd.SetErr(&bytes.Buffer{})
	o := teardownOptions{clusterKubeconfig: filepath.Join(t.TempDir(), "missing.kubeconfig")}
	err := runTeardown(context.Background(), cmd, selfHostedConfig(""), o)
	if err == nil || !strings.Contains(err.Error(), "no endpoint to check") {
		t.Fatalf("got %v", err)
	}
}

// pivTombstone is a teardown record in kind for cluster ns/c, written for the
// cluster whose kubeconfig is pivOwnKC.
func pivTombstone(phase, createdKind string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: pivot.TombstoneName},
		Data: map[string]string{"cluster": "c", "phase": phase, "createdKind": createdKind,
			"ca": base64.StdEncoding.EncodeToString(cluster.KubeconfigCA(pivOwnKC))},
	}
}

// pivTeardownSetup writes the cluster's kubeconfig file (unless file is nil),
// wires kind and the two clients, and returns the file's path.
func pivTeardownSetup(t *testing.T, file []byte, k *pivFakeKind, clients map[string]client.Client) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.kubeconfig")
	if file != nil {
		if err := os.WriteFile(path, file, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if k.kubeconfig == "" {
		k.kubeconfig = string(pivKindKC)
	}
	pivWithBootstrapper(t, k, &pivFakeCluster{}, nil)
	pivConnectBy(t, clients)
	return path
}

// Kind holds a record of a finished move and deletion: the rerun only cleans
// up, removing the kubeconfig file and the kind cluster this teardown made.
func TestTeardownFinishesCleanupFromItsRecord(t *testing.T) {
	k := &pivFakeKind{exists: true}
	path := pivTeardownSetup(t, pivOwnKC, k, map[string]client.Client{
		string(pivOwnKC):  fakeClient(t),
		string(pivKindKC): fakeClient(t, pivTombstone("deleted", "true")),
	})
	cmd, out := pivCmd()
	err := runTeardown(context.Background(), cmd, selfHostedConfig("10.0.0.12"), teardownOptions{clusterKubeconfig: path, yes: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the cluster's kubeconfig must be removed: %v", err)
	}
	if !k.deleted || !strings.Contains(out.String(), "cluster ns/c is torn down") {
		t.Errorf("kind deleted=%v output %q", k.deleted, out.String())
	}
}

// Kind holds the moved Cluster: it is deleted there, and a kind that existed
// before the teardown is kept, with the record removed.
func TestTeardownDeletesTheClusterFromKind(t *testing.T) {
	k := &pivFakeKind{exists: true}
	kind := fakeClient(t, capiCluster("ns", "c", "10.0.0.12", 6443), pivTombstone("moved", "false"))
	path := pivTeardownSetup(t, pivOwnKC, k, map[string]client.Client{
		string(pivOwnKC):  fakeClient(t),
		string(pivKindKC): kind,
	})
	cmd, out := pivCmd()
	err := runTeardown(context.Background(), cmd, selfHostedConfig("10.0.0.12"), teardownOptions{clusterKubeconfig: path, yes: true, timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	cl := capiCluster("ns", "c", "", 0)
	if err := kind.Get(context.Background(), client.ObjectKeyFromObject(cl), cl); !apierrors.IsNotFound(err) {
		t.Errorf("the Cluster must be deleted from kind: %v", err)
	}
	if k.deleted {
		t.Error("a kind cluster that existed before the teardown must be kept")
	}
	if err := kind.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: pivot.TombstoneName}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Errorf("the record must not outlive the teardown: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the cluster's kubeconfig must be removed: %v", err)
	}
}

// The cluster holds itself and there is no kind: the teardown brings kind up
// (without credentials, which arrive with the move) and records that it did,
// before any check that can refuse.
func TestTeardownBringsUpKind(t *testing.T) {
	self := fakeClient(t, capiCluster("ns", "c", "10.0.0.12", 6443), kubeconfigSecret(pivOwnKC))
	kind := fakeClient(t)
	for _, tc := range []struct {
		name        string
		hv          *config.HypervisorSpec
		wantCreated bool
		wantErr     string
	}{
		{"no hypervisor: bootstrap refuses", nil, false, bootstrap.ErrNoHypervisor.Error()},
		{"kind created and recorded", &config.HypervisorSpec{Address: "h:16514", CredentialsSecretName: "c-pki"}, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := &pivFakeKind{}
			path := pivTeardownSetup(t, pivOwnKC, k, map[string]client.Client{string(pivOwnKC): self, string(pivKindKC): kind})
			cfg := selfHostedConfig("10.0.0.12")
			cfg.Spec.Hypervisor = tc.hv
			cmd, _ := pivCmd()
			err := runTeardown(context.Background(), cmd, cfg, teardownOptions{clusterKubeconfig: path, yes: true, configFile: "hydra.yaml"})
			// Without labelled CRDs the preflight refuses after kind is up.
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
			if k.created != tc.wantCreated {
				t.Errorf("kind created=%v, want %v", k.created, tc.wantCreated)
			}
			if !tc.wantCreated {
				return
			}
			cm := &corev1.ConfigMap{}
			if err := kind.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: pivot.TombstoneName}, cm); err != nil {
				t.Fatal(err)
			}
			if cm.Data["createdKind"] != "true" {
				t.Errorf("the record must say this teardown created kind: %v", cm.Data)
			}
			if _, err := os.Stat(path); err != nil {
				t.Errorf("the kubeconfig must stay until the cluster is gone: %v", err)
			}
		})
	}
}

func TestTeardownRefuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		file    []byte
		fileDir bool
		kind    *pivFakeKind
		bootErr error
		clients map[string]client.Client
		confirm string // with no --yes, the typed answer
		wantErr string
	}{
		{name: "unreadable kubeconfig path", fileDir: true, kind: &pivFakeKind{}, wantErr: "reading"},
		{name: "no container runtime", file: pivOwnKC, kind: &pivFakeKind{}, bootErr: errors.New("no docker"), wantErr: "no docker"},
		{name: "kind cannot be listed", file: pivOwnKC, kind: &pivFakeKind{existsErr: errors.New("docker down")}, wantErr: "looking for hydra-bootstrap-c"},
		{name: "kind's kubeconfig unreadable", file: pivOwnKC, kind: &pivFakeKind{exists: true, kubeErr: errors.New("no kubeconfig")}, wantErr: "no kubeconfig"},
		{name: "kind unreachable", file: pivOwnKC, kind: &pivFakeKind{exists: true}, clients: map[string]client.Client{}, wantErr: "connecting to hydra-bootstrap-c"},
		{name: "confirmation mismatch", file: pivOwnKC, kind: &pivFakeKind{}, confirm: "wrong\n", wantErr: "did not match"},
		{name: "the cluster unreachable and nothing recorded", file: pivOwnKC, kind: &pivFakeKind{}, clients: map[string]client.Client{},
			wantErr: "cannot be reached"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := pivTeardownSetup(t, tc.file, tc.kind, tc.clients)
			if tc.fileDir {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if tc.bootErr != nil {
				pivWithBootstrapper(t, tc.kind, &pivFakeCluster{}, tc.bootErr)
			}
			if tc.clients == nil {
				pivConnectBy(t, map[string]client.Client{string(pivOwnKC): fakeClient(t), string(pivKindKC): fakeClient(t)})
			}
			origIn := confirmIn
			confirmIn = strings.NewReader(tc.confirm)
			t.Cleanup(func() { confirmIn = origIn })
			cmd, _ := pivCmd()
			o := teardownOptions{clusterKubeconfig: path, yes: tc.confirm == ""}
			err := runTeardown(context.Background(), cmd, selfHostedConfig("10.0.0.12"), o)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
			if tc.kind.created || tc.kind.deleted {
				t.Error("a refusal must leave kind alone")
			}
			if tc.file != nil {
				if _, err := os.Stat(path); err != nil {
					t.Errorf("a refusal must keep the kubeconfig: %v", err)
				}
			}
		})
	}
}
