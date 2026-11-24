package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Petatron/hydra/internal/bootstrap"
	"github.com/Petatron/hydra/internal/cluster"
	"github.com/Petatron/hydra/internal/config"
	"github.com/Petatron/hydra/internal/pivot"
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
	t.Run("an unreadable explicit file says the Secret was left unused", func(t *testing.T) {
		src := &cluster.Manager{C: fakeClient(t, kubeconfigSecret(own))}
		_, _, _, err := targetKubeconfig(ctx, src, "ns", "c", filepath.Join(t.TempDir(), "missing"), "")
		if err == nil || !strings.Contains(err.Error(), "left unused") || strings.Contains(err.Error(), "has no") {
			t.Fatalf("got %v", err)
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
	answering := func(t *testing.T, v bool, err error) {
		orig := endpointAnswers
		endpointAnswers = func(string) (bool, error) { return v, err }
		t.Cleanup(func() { endpointAnswers = orig })
	}
	unreachable := errors.New("dial tcp 10.0.0.12:6443: i/o timeout")
	for _, tc := range []struct {
		name     string
		endpoint string
		answers  bool
		probeErr error
		file     []byte // nil: no kubeconfig file
		fileDir  bool   // the path is a directory: unreadable
		wantErr  string
	}{
		{"endpoint already answers", "10.0.0.12", true, nil, nil, false, "already answers"},
		{"endpoint unreachable, no --fresh", "10.0.0.12", false, unreachable, nil, false, "cannot reach its endpoint"},
		{"Hydra chooses the endpoint, no --fresh", "", false, nil, nil, false, "--fresh"},
		{"unreadable kubeconfig path", "10.0.0.12", false, nil, nil, true, "reading"},
		{"kubeconfig file but cluster unreachable", "10.0.0.12", false, nil, []byte("not a kubeconfig"), false, "cannot show whether"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := &fakeKind{}
			withFakeKind(t, k)
			answering(t, tc.answers, tc.probeErr)
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

func TestTargetKubeconfigFile(t *testing.T) {
	own := kubeconfigWithCA("https://10.0.0.12:6443", "ca-c")
	other := kubeconfigWithCA("https://10.0.0.99:6443", "ca-other")
	t.Chdir(t.TempDir())
	got, err := targetKubeconfigFile("", "c", "Secret ns/c-kubeconfig", own)
	if err != nil || got != "c.kubeconfig" {
		t.Fatalf("got %q %v", got, err)
	}
	if b, _ := os.ReadFile("c.kubeconfig"); !bytes.Equal(b, own) {
		t.Error("the Secret's kubeconfig must be written for the printed commands")
	}
	if err := os.WriteFile("c.kubeconfig", other, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := targetKubeconfigFile("", "c", "Secret ns/c-kubeconfig", own); err == nil {
		t.Error("another cluster's kubeconfig must not be overwritten or used")
	}
}

func TestProbeTCP(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	open := l.Addr().String()
	if ok, err := probeTCP(open); !ok || err != nil {
		t.Errorf("a listener must answer: %v %v", ok, err)
	}
	_ = l.Close()
	// The port is closed now: the host refuses, which is evidence nothing
	// listens there, not an unknown.
	if ok, err := probeTCP(open); ok || err != nil {
		t.Errorf("a refused connection is a plain no: %v %v", ok, err)
	}
}

// pivFakeKind is a kind provider that records what was done to it.
type pivFakeKind struct {
	exists           bool
	existsErr        error
	kubeErr          error
	kubeconfig       string
	created, deleted bool
}

func (k *pivFakeKind) Exists(string) (bool, error) { return k.exists, k.existsErr }
func (k *pivFakeKind) Create(string, string) error {
	k.created, k.exists = true, true
	return nil
}
func (k *pivFakeKind) Delete(string) error { k.deleted, k.exists = true, false; return nil }
func (k *pivFakeKind) KubeConfig(string) (string, error) {
	return k.kubeconfig, k.kubeErr
}

// pivFakeCluster is a bootstrap cluster with every pinned provider installed
// and a usable credentials Secret; managed is what it still manages.
type pivFakeCluster struct {
	managed []string
	waited  bool
}

func (c *pivFakeCluster) InstalledProviders(context.Context) (map[string]string, error) {
	m := map[string]string{bootstrap.CertManagerName: bootstrap.CertManagerVersion}
	for _, p := range bootstrap.Providers() {
		m[p.Name] = p.Version
	}
	return m, nil
}
func (c *pivFakeCluster) Init(context.Context, []bootstrap.Provider) error { return nil }
func (c *pivFakeCluster) WaitProvidersAvailable(context.Context) error {
	c.waited = true
	return nil
}
func (c *pivFakeCluster) GetSecret(context.Context, string, string) (*bootstrap.SecretInfo, error) {
	return &bootstrap.SecretInfo{
		Keys:   []string{bootstrap.CACertFile, bootstrap.ClientCertFile, bootstrap.ClientKeyFile},
		Labels: map[string]string{bootstrap.MoveLabel: ""},
	}, nil
}
func (c *pivFakeCluster) LabelSecret(context.Context, string, string, string, string) error {
	return nil
}
func (c *pivFakeCluster) ApplySecret(context.Context, string, string, map[string][]byte, map[string]string) error {
	return nil
}
func (c *pivFakeCluster) Clusters(context.Context) ([]string, error) { return c.managed, nil }

// pivWithBootstrapper makes newBootstrapper return a Bootstrapper over k and
// cl, or err when err is set.
func pivWithBootstrapper(t *testing.T, k *pivFakeKind, cl *pivFakeCluster, err error) {
	t.Helper()
	orig := newBootstrapper
	newBootstrapper = func(out io.Writer) (*bootstrap.Bootstrapper, error) {
		if err != nil {
			return nil, err
		}
		return &bootstrap.Bootstrapper{Kind: k, Out: out,
			Connect: func(string) (bootstrap.Cluster, error) { return cl, nil }}, nil
	}
	t.Cleanup(func() { newBootstrapper = orig })
}

// pivConnectBy makes connectManagement pick the fake client by kubeconfig;
// an unknown kubeconfig fails to connect.
func pivConnectBy(t *testing.T, clients map[string]client.Client) {
	t.Helper()
	orig := connectManagement
	connectManagement = func(kc []byte) (client.Client, error) {
		if c, ok := clients[string(kc)]; ok {
			return c, nil
		}
		return nil, errors.New("connection refused")
	}
	t.Cleanup(func() { connectManagement = orig })
}

var (
	pivKindKC  = kubeconfigWithCA("https://127.0.0.1:40000", "ca-kind")
	pivOwnKC   = kubeconfigWithCA("https://10.0.0.12:6443", "ca-c")
	pivOtherKC = kubeconfigWithCA("https://10.0.0.99:6443", "ca-other")
)

func pivObj(apiVersion, kind, name string, fields map[string]any) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{}}
	maps.Copy(o.Object, fields)
	o.SetAPIVersion(apiVersion)
	o.SetKind(kind)
	o.SetNamespace("ns")
	o.SetName(name)
	return o
}

// pivSettled is cluster ns/c holding itself and verifying: Available, rolled
// out, every machine taken over and Ready, the CNI applied, and its own
// kubeconfig Secret.
func pivSettled() []client.Object {
	cl := pivObj("cluster.x-k8s.io/v1beta2", "Cluster", "c", map[string]any{lcStatus: map[string]any{
		"conditions": []any{map[string]any{"type": "Available", lcStatus: "True"}},
	}})
	cl.SetLabels(map[string]string{"hydra.io/cluster": "c"})
	kcp := pivObj("controlplane.cluster.x-k8s.io/v1beta2", "KubeadmControlPlane", "c-cp", map[string]any{
		"spec":   map[string]any{"replicas": int64(1)},
		lcStatus: map[string]any{"readyReplicas": int64(1), "upToDateReplicas": int64(1), "replicas": int64(1)},
	})
	kcp.SetLabels(map[string]string{"hydra.io/cluster": "c"})
	hm := pivObj("infrastructure.cluster.x-k8s.io/v1alpha1", "HydraMachine", "c-cp-1", map[string]any{lcStatus: map[string]any{
		"conditions": []any{map[string]any{"type": "Ready", lcStatus: "True"}},
	}})
	hm.SetLabels(map[string]string{"cluster.x-k8s.io/cluster-name": "c"})
	hm.SetUID("hm-1")
	hm.SetAnnotations(map[string]string{pivot.OwnerUIDAnnotation: "hm-1"})
	return append([]client.Object{cl, kcp, hm, kubeconfigSecret(pivOwnKC)}, cniInstalled("ns", "c")...)
}

func pivCmd() (*cobra.Command, *bytes.Buffer) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&out)
	return cmd, &out
}

func pivKindTarget(c client.Client) *target {
	return &target{m: &cluster.Manager{C: c, Out: io.Discard}, source: "kind cluster hydra-bootstrap-c",
		bootstrapKind: true, kubeconfig: pivKindKC}
}

var pivMeta = config.Metadata{Name: "c", Namespace: "ns"}

// A verified pivot from the bootstrap cluster removes it, unless kept or the
// source was not the bootstrap cluster; the Secret's kubeconfig is written
// for the operator.
func TestRunPivotVerifiesThenRemovesBootstrap(t *testing.T) {
	for _, tc := range []struct {
		name          string
		bootstrapKind bool
		keep          bool
		wantDeleted   bool
	}{
		{"bootstrap source is removed", true, false, true},
		{"--keep-bootstrap keeps it", true, true, false},
		{"another management cluster is never removed", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			src := fakeClient(t, kubeconfigSecret(pivOwnKC))
			pivConnectBy(t, map[string]client.Client{string(pivOwnKC): fakeClient(t, pivSettled()...)})
			k := &pivFakeKind{exists: true}
			pivWithBootstrapper(t, k, &pivFakeCluster{}, nil)
			tg := pivKindTarget(src)
			tg.bootstrapKind = tc.bootstrapKind
			cmd, out := pivCmd()
			if err := runPivot(context.Background(), cmd, tg, pivMeta, pivotOptions{keepBootstrap: tc.keep, timeout: time.Second}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "now manages itself; pass c.kubeconfig") {
				t.Errorf("output %q", out.String())
			}
			if b, _ := os.ReadFile("c.kubeconfig"); !bytes.Equal(b, pivOwnKC) {
				t.Error("the cluster's kubeconfig must be written to ./c.kubeconfig")
			}
			if k.deleted != tc.wantDeleted {
				t.Errorf("kind deleted=%v, want %v", k.deleted, tc.wantDeleted)
			}
		})
	}
}

// A source that is the cluster itself has nothing to move.
func TestRunPivotAlreadySelfHosted(t *testing.T) {
	t.Chdir(t.TempDir())
	self := fakeClient(t, pivSettled()...)
	pivConnectBy(t, map[string]client.Client{string(pivOwnKC): self})
	k := &pivFakeKind{exists: true}
	pivWithBootstrapper(t, k, &pivFakeCluster{}, nil)
	tg := &target{m: &cluster.Manager{C: self, Out: io.Discard}, source: "c.kubeconfig", kubeconfig: pivOwnKC, path: "c.kubeconfig"}
	cmd, out := pivCmd()
	if err := runPivot(context.Background(), cmd, tg, pivMeta, pivotOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "already manages itself") || k.deleted {
		t.Errorf("deleted=%v output %q", k.deleted, out.String())
	}
}

func TestRunPivotRefuses(t *testing.T) {
	for _, tc := range []struct {
		name      string
		srcObjs   []client.Object
		file      []byte // ./c.kubeconfig, nil for none
		clients   map[string]client.Client
		bootErr   error
		wantErr   string
		wantNoDel bool
	}{
		{name: "no kubeconfig anywhere", wantErr: "no kubeconfig for cluster c"},
		{name: "the cluster cannot be reached", srcObjs: []client.Object{kubeconfigSecret(pivOwnKC)},
			clients: map[string]client.Client{}, wantErr: "connecting to cluster c"},
		{name: "an unproven file reaching an empty cluster", file: pivOwnKC,
			clients: map[string]client.Client{string(pivOwnKC): nil}, wantErr: "nothing proves"},
		{name: "another cluster's file in the way", srcObjs: []client.Object{kubeconfigSecret(pivOwnKC)}, file: pivOtherKC,
			clients: map[string]client.Client{string(pivOwnKC): nil}, wantErr: "is not cluster c's kubeconfig"},
		{name: "the Cluster is on neither side", srcObjs: []client.Object{kubeconfigSecret(pivOwnKC)},
			clients: map[string]client.Client{string(pivOwnKC): nil}, wantErr: "exists on neither"},
		{name: "no container runtime to remove kind", srcObjs: []client.Object{kubeconfigSecret(pivOwnKC)},
			clients: map[string]client.Client{string(pivOwnKC): fakeClient(t, pivSettled()...)},
			bootErr: errors.New("no docker"), wantErr: "no docker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if tc.file != nil {
				if err := os.WriteFile("c.kubeconfig", tc.file, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			clients := map[string]client.Client{}
			for k, v := range tc.clients {
				if v == nil {
					v = fakeClient(t)
				}
				clients[k] = v
			}
			pivConnectBy(t, clients)
			k := &pivFakeKind{exists: true}
			pivWithBootstrapper(t, k, &pivFakeCluster{}, tc.bootErr)
			cmd, _ := pivCmd()
			err := runPivot(context.Background(), cmd, pivKindTarget(fakeClient(t, tc.srcObjs...)), pivMeta, pivotOptions{timeout: time.Second})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
			if k.deleted {
				t.Error("the bootstrap cluster must not be removed after a refusal")
			}
		})
	}
}

func TestPivotCommand(t *testing.T) {
	example, err := filepath.Abs("../../examples/workload-managed-network.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("an unreadable hydra.yaml", func(t *testing.T) {
		if _, _, err := run(t, "cluster", "pivot", "-f", filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("no management cluster", func(t *testing.T) {
		orig := managementTarget
		managementTarget = func(string, string) ([]byte, string, error) { return nil, "", errors.New("no management cluster here") }
		t.Cleanup(func() { managementTarget = orig })
		if _, _, err := run(t, "cluster", "pivot", "-f", example); err == nil || !strings.Contains(err.Error(), "no management cluster here") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("reaches the pivot with the config's cluster", func(t *testing.T) {
		t.Chdir(t.TempDir())
		withFakeManagement(t)
		_, errOut, err := run(t, "cluster", "pivot", "-f", example, "--pivot-timeout", "1s")
		if err == nil || !strings.Contains(err.Error(), "no kubeconfig for cluster demo") {
			t.Fatalf("got %v", err)
		}
		if !strings.Contains(errOut, "management cluster: test-mgmt") {
			t.Errorf("stderr %q", errOut)
		}
	})
}

func TestPivotHooksEnsureProvidersNeedsAKubeconfig(t *testing.T) {
	h := pivotHooks(io.Discard, "cluster c")
	if h.Move == nil {
		t.Fatal("pivot must move with clusterctl")
	}
	if err := h.EnsureProviders(context.Background(), []byte("not a kubeconfig")); err == nil {
		t.Fatal("an unparsable kubeconfig must fail before anything is installed")
	}
}

func TestEndpointAnswersRejectsAnInvalidEndpoint(t *testing.T) {
	if ok, err := endpointAnswers("10.0.0.12:99"); ok || err == nil || !strings.Contains(err.Error(), "6443") {
		t.Fatalf("got %v %v", ok, err)
	}
}

func TestTargetKubeconfigFileCases(t *testing.T) {
	t.Run("an explicit path is used as is", func(t *testing.T) {
		if got, err := targetKubeconfigFile("/x/c.kubeconfig", "c", "Secret ns/c-kubeconfig", pivOwnKC); err != nil || got != "/x/c.kubeconfig" {
			t.Fatalf("got %q %v", got, err)
		}
	})
	t.Run("the file it was read from", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if got, err := targetKubeconfigFile("", "c", "c.kubeconfig", pivOwnKC); err != nil || got != "c.kubeconfig" {
			t.Fatalf("got %q %v", got, err)
		}
		if _, err := os.Stat("c.kubeconfig"); err == nil {
			t.Error("nothing should be written for a kubeconfig read from that file")
		}
	})
	t.Run("the same cluster's file already there", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.WriteFile("c.kubeconfig", pivOwnKC, 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := targetKubeconfigFile("", "c", "Secret ns/c-kubeconfig", pivOwnKC); err != nil || got != "c.kubeconfig" {
			t.Fatalf("got %q %v", got, err)
		}
	})
	t.Run("an unreadable path", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.Mkdir("c.kubeconfig", 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := targetKubeconfigFile("", "c", "Secret ns/c-kubeconfig", pivOwnKC); err == nil || !strings.Contains(err.Error(), "reading c.kubeconfig") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestTargetKubeconfigErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("the Secret cannot be read", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errors.New("forbidden")
			},
		}).Build()
		_, _, _, err := targetKubeconfig(ctx, &cluster.Manager{C: c}, "ns", "c", "", "/nonexistent")
		if err == nil || !strings.Contains(err.Error(), "reading the cluster's kubeconfig Secret") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("an explicit file without a CA", func(t *testing.T) {
		src := &cluster.Manager{C: fakeClient(t, kubeconfigSecret(pivOwnKC))}
		p := writeFile(t, []byte(testKubeconfig))
		_, _, _, err := targetKubeconfig(ctx, src, "ns", "c", p, "")
		if err == nil || !strings.Contains(err.Error(), p) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestProvePivotTargetUnreadable(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("forbidden")
		},
	}).Build()
	err := provePivotTarget(context.Background(), &cluster.Manager{C: c, Out: io.Discard}, "ns", "c", "f", pivOwnKC)
	if err == nil || !strings.Contains(err.Error(), "reading Cluster ns/c through f") {
		t.Fatalf("got %v", err)
	}
}

// pivKindManagement makes the bootstrap kind cluster the management target.
func pivKindManagement(t *testing.T, err error) {
	t.Helper()
	orig := managementTarget
	managementTarget = func(string, string) ([]byte, string, error) {
		return pivKindKC, "kind cluster hydra-bootstrap-c", err
	}
	t.Cleanup(func() { managementTarget = orig })
}

// A rerun after the move: kind is still there and the kubeconfig file reaches
// the cluster managing itself, so the pivot is only finished (verified) and
// kind removed; nothing is built.
func TestSelfHostedFinishesAnInterruptedPivot(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("c.kubeconfig", pivOwnKC, 0o600); err != nil {
		t.Fatal(err)
	}
	pivKindManagement(t, nil)
	pivConnectBy(t, map[string]client.Client{
		string(pivKindKC): fakeClient(t),
		string(pivOwnKC):  fakeClient(t, pivSettled()...),
	})
	k := &pivFakeKind{exists: true}
	pivWithBootstrapper(t, k, &pivFakeCluster{}, nil)
	cmd, out := pivCmd()
	err := runSelfHosted(context.Background(), cmd, selfHostedConfig("10.0.0.12"), nil, "", createOptions{}, pivotOptions{timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "finishing the pivot") || !strings.Contains(out.String(), "now manages itself") {
		t.Errorf("output %q", out.String())
	}
	if k.created || !k.deleted {
		t.Errorf("created=%v deleted=%v: want only the leftover kind removed", k.created, k.deleted)
	}
}

func TestSelfHostedStopsBeforeCreating(t *testing.T) {
	unreachable := errors.New("i/o timeout")
	hv := &config.HypervisorSpec{Address: "h:16514", CredentialsSecretName: "c-pki"}
	for _, tc := range []struct {
		name      string
		kind      *pivFakeKind
		bootErr   error
		mgmtErr   error
		kindObjs  []client.Object
		endpoint  string
		probeErr  error
		fresh     bool
		hv        *config.HypervisorSpec
		creds     string
		wantErr   string
		wantReady bool // the bootstrap cluster was brought up
	}{
		{name: "no container runtime", kind: &pivFakeKind{}, bootErr: errors.New("no docker"), wantErr: "no docker"},
		{name: "kind cannot be listed", kind: &pivFakeKind{existsErr: errors.New("docker down")}, wantErr: "looking for kind cluster hydra-bootstrap-c"},
		{name: "kind cannot be reached", kind: &pivFakeKind{exists: true}, mgmtErr: errors.New("kind gone"), wantErr: "kind gone"},
		{name: "unreadable credentials on a fresh start", kind: &pivFakeKind{}, endpoint: "", fresh: true,
			creds: "/nonexistent/creds", wantErr: "/nonexistent/creds"},
		{name: "unreachable endpoint with --fresh carries on", kind: &pivFakeKind{}, endpoint: "10.0.0.12", probeErr: unreachable, fresh: true,
			creds: "/nonexistent/creds", wantErr: "/nonexistent/creds"},
		{name: "kind holding the Cluster skips the fresh-start check", kind: &pivFakeKind{exists: true},
			kindObjs: []client.Object{capiCluster("ns", "c", "10.0.0.12", 6443)}, wantErr: bootstrap.ErrNoHypervisor.Error()},
		{name: "after bootstrapping, create runs on kind", kind: &pivFakeKind{exists: true}, hv: hv,
			kindObjs: []client.Object{capiCluster("ns", "c", "10.0.0.12", 6443)}, wantErr: "not a Cluster API management cluster", wantReady: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			pivKindManagement(t, tc.mgmtErr)
			pivConnectBy(t, map[string]client.Client{string(pivKindKC): fakeClient(t, tc.kindObjs...)})
			orig := endpointAnswers
			endpointAnswers = func(string) (bool, error) { return false, tc.probeErr }
			t.Cleanup(func() { endpointAnswers = orig })
			fc := &pivFakeCluster{}
			pivWithBootstrapper(t, tc.kind, fc, tc.bootErr)
			cfg := selfHostedConfig(tc.endpoint)
			cfg.Spec.Hypervisor = tc.hv
			cmd, out := pivCmd()
			err := runSelfHosted(context.Background(), cmd, cfg, nil, tc.creds, createOptions{}, pivotOptions{fresh: tc.fresh})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
			if tc.kind.created {
				t.Error("no kind cluster may be created")
			}
			if tc.wantReady != fc.waited || tc.wantReady && !strings.Contains(out.String(), "management cluster: kind cluster hydra-bootstrap-c") {
				t.Errorf("bootstrapped=%v, want %v; output %q", fc.waited, tc.wantReady, out.String())
			}
		})
	}
}
