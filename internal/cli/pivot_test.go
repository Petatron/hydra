package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Petatron/hydra/internal/bootstrap"
	"github.com/Petatron/hydra/internal/cluster"
	"github.com/Petatron/hydra/internal/config"
)

func kubeconfigWithCA(server, ca string) []byte {
	return []byte(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "` + server + `", certificate-authority-data: "` + base64.StdEncoding.EncodeToString([]byte(ca)) + `"}}]
contexts: [{name: c, context: {cluster: c, user: u}}]
current-context: c
users: [{name: u, user: {token: x}}]
`)
}

func fakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// kubeconfigSecret is cluster ns/c's kubeconfig Secret.
func kubeconfigSecret(value []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "c-kubeconfig"},
		Data:       map[string][]byte{"value": value},
	}
}

func writeFile(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.kubeconfig")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTargetKubeconfig(t *testing.T) {
	ctx := context.Background()
	own := kubeconfigWithCA("https://10.0.0.12:6443", "ca-c")
	other := kubeconfigWithCA("https://10.0.0.99:6443", "ca-other")

	t.Run("the source Secret is used and proven", func(t *testing.T) {
		src := &cluster.Manager{C: fakeClient(t, kubeconfigSecret(own))}
		kc, from, proven, err := targetKubeconfig(ctx, src, "ns", "c", "", "/nonexistent")
		if err != nil || !proven || !bytes.Equal(kc, own) || !strings.Contains(from, "Secret") {
			t.Fatalf("got %q %v %v", from, proven, err)
		}
	})
	t.Run("an explicit file of another cluster is refused", func(t *testing.T) {
		src := &cluster.Manager{C: fakeClient(t, kubeconfigSecret(own))}
		_, _, _, err := targetKubeconfig(ctx, src, "ns", "c", writeFile(t, other), "")
		if err == nil || !strings.Contains(err.Error(), "different cluster") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("an explicit matching file wins", func(t *testing.T) {
		src := &cluster.Manager{C: fakeClient(t, kubeconfigSecret(own))}
		p := writeFile(t, own)
		_, from, proven, err := targetKubeconfig(ctx, src, "ns", "c", p, "")
		if err != nil || !proven || from != p {
			t.Fatalf("got %q %v %v", from, proven, err)
		}
	})
	t.Run("without the Secret the file is used unproven", func(t *testing.T) {
		src := &cluster.Manager{C: fakeClient(t)}
		_, _, proven, err := targetKubeconfig(ctx, src, "ns", "c", "", writeFile(t, own))
		if err != nil || proven {
			t.Fatalf("got %v %v", proven, err)
		}
	})
}

func TestProvePivotTarget(t *testing.T) {
	ctx := context.Background()
	own := kubeconfigWithCA("https://10.0.0.12:6443", "ca-c")
	other := kubeconfigWithCA("https://10.0.0.99:6443", "ca-other")
	for _, tc := range []struct {
		name    string
		objs    []client.Object
		kc      []byte
		wantErr string
	}{
		{"the cluster itself", []client.Object{capiCluster("ns", "c", "10.0.0.50", 6443), kubeconfigSecret(own)}, own, ""},
		{"holds nothing", nil, own, "nothing proves"},
		{"another manager holding the Cluster", []client.Object{capiCluster("ns", "c", "10.0.0.50", 6443), kubeconfigSecret(own)}, other, "is not that cluster"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &cluster.Manager{C: fakeClient(t, tc.objs...), Out: io.Discard}
			err := provePivotTarget(ctx, m, "ns", "c", "f", tc.kc)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// fakeKind records Create; it reports kind clusters from exists.
type fakeKind struct {
	exists  bool
	created bool
}

func (k *fakeKind) Exists(string) (bool, error)       { return k.exists, nil }
func (k *fakeKind) Create(string, string) error       { k.created = true; return nil }
func (k *fakeKind) Delete(string) error               { return nil }
func (k *fakeKind) KubeConfig(string) (string, error) { return testKubeconfig, nil }

func withFakeKind(t *testing.T, k *fakeKind) {
	t.Helper()
	orig := newBootstrapper
	newBootstrapper = func(out io.Writer) (*bootstrap.Bootstrapper, error) {
		return &bootstrap.Bootstrapper{Kind: k, Out: out}, nil
	}
	t.Cleanup(func() { newBootstrapper = orig })
}

func selfHostedConfig(endpoint string) *config.ClusterConfig {
	cfg := &config.ClusterConfig{}
	cfg.Metadata.Name, cfg.Metadata.Namespace = "c", "ns"
	cfg.Spec.Network.ControlPlaneEndpoint = endpoint
	return cfg
}

func TestSelfHostedRefusesBeforeBuilding(t *testing.T) {
	ctx := context.Background()
	answering := func(t *testing.T, v bool) {
		orig := endpointAnswers
		endpointAnswers = func(string) bool { return v }
		t.Cleanup(func() { endpointAnswers = orig })
	}
	for _, tc := range []struct {
		name     string
		endpoint string
		answers  bool
		file     []byte // nil: no kubeconfig file
		fileDir  bool   // the path is a directory: unreadable
		wantErr  string
	}{
		{"endpoint already answers", "10.0.0.12", true, nil, false, "already answers"},
		{"Hydra chooses the endpoint, no --fresh", "", false, nil, false, "--fresh"},
		{"unreadable kubeconfig path", "10.0.0.12", false, nil, true, "reading"},
		{"kubeconfig file but cluster unreachable", "10.0.0.12", false, []byte("not a kubeconfig"), false, "cannot show whether"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := &fakeKind{}
			withFakeKind(t, k)
			answering(t, tc.answers)
			orig := connectManagement
			connectManagement = func(b []byte) (client.Client, error) { return cluster.Connect(b) }
			t.Cleanup(func() { connectManagement = orig })
			path := filepath.Join(t.TempDir(), "c.kubeconfig")
			switch {
			case tc.fileDir:
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case tc.file != nil:
				if err := os.WriteFile(path, tc.file, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := &cobra.Command{}
			cmd.SetErr(io.Discard)
			err := runSelfHosted(ctx, cmd, selfHostedConfig(tc.endpoint), nil, "", createOptions{}, pivotOptions{clusterKubeconfig: path})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
			if k.created {
				t.Error("nothing may be built after a refusal")
			}
		})
	}
}

func TestSelfHostedAlreadyDoneDoesNothing(t *testing.T) {
	own := kubeconfigWithCA("https://10.0.0.12:6443", "ca-c")
	c := fakeClient(t, capiCluster("ns", "c", "10.0.0.50", 6443), kubeconfigSecret(own))
	orig := connectManagement
	connectManagement = func([]byte) (client.Client, error) { return c, nil }
	t.Cleanup(func() { connectManagement = orig })
	k := &fakeKind{}
	withFakeKind(t, k)
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&out)
	err := runSelfHosted(context.Background(), cmd, selfHostedConfig("10.0.0.12"), nil, "", createOptions{}, pivotOptions{clusterKubeconfig: writeFile(t, own)})
	if err != nil {
		t.Fatal(err)
	}
	if k.created || !strings.Contains(out.String(), "--management-kubeconfig") {
		t.Errorf("created=%v out=%q", k.created, out.String())
	}
}

// A kubeconfig file that reaches a running cluster which does not manage this
// one itself must stop a fresh build, even with --fresh.
func TestSelfHostedRefusesWhenTheFileReachesALiveCluster(t *testing.T) {
	own := kubeconfigWithCA("https://10.0.0.12:6443", "ca-c")
	other := kubeconfigWithCA("https://10.0.0.99:6443", "ca-other")
	for _, tc := range []struct {
		name string
		objs []client.Object
		file []byte
	}{
		{"the cluster itself, never pivoted", nil, own},
		{"another management cluster holding it", []client.Object{capiCluster("ns", "c", "10.0.0.50", 6443), kubeconfigSecret(own)}, other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fakeClient(t, tc.objs...)
			orig := connectManagement
			connectManagement = func([]byte) (client.Client, error) { return c, nil }
			t.Cleanup(func() { connectManagement = orig })
			k := &fakeKind{}
			withFakeKind(t, k)
			cmd := &cobra.Command{}
			cmd.SetErr(io.Discard)
			err := runSelfHosted(context.Background(), cmd, selfHostedConfig(""), nil, "", createOptions{}, pivotOptions{clusterKubeconfig: writeFile(t, tc.file), fresh: true})
			if err == nil || !strings.Contains(err.Error(), "may already exist") {
				t.Fatalf("got %v", err)
			}
			if k.created {
				t.Error("nothing may be built")
			}
		})
	}
}
