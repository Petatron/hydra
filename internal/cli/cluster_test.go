package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Petatron/hydra/internal/cluster"
	"github.com/Petatron/hydra/internal/config"
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
	// As hydra renders it, with the default cni: hydra.
	u.SetLabels(map[string]string{"hydra.io/cluster": name, "cni.hydra.io/cilium": name})
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
		{"cluster", lcStatus, "-f", "../../examples/hydra-mgmt.yaml", "--management-kubeconfig", "/x"},
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

func TestDeleteConfirmsWhenOnlyLeftoversExist(t *testing.T) {
	// An interrupted create: a labelled HydraCluster and no Cluster.
	hc := &unstructured.Unstructured{Object: map[string]any{}}
	hc.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1alpha1")
	hc.SetKind("HydraCluster")
	hc.SetNamespace("default")
	hc.SetName("hydra-mgmt")
	hc.SetLabels(map[string]string{"hydra.io/cluster": "hydra-mgmt"})
	withFakeManagement(t, hc)
	origIn := confirmIn
	t.Cleanup(func() { confirmIn = origIn })
	confirmIn = strings.NewReader("nope\n")
	_, errOut, err := run(t, "cluster", "delete", "-f", "../../examples/hydra-mgmt.yaml")
	if err == nil || !strings.Contains(err.Error(), "did not match") {
		t.Fatalf("leftovers must still need confirmation: %v", err)
	}
	if !strings.Contains(errOut, "HydraCluster default/hydra-mgmt") {
		t.Errorf("the leftovers must be listed before asking:\n%s", errOut)
	}
}

func TestDeleteNothingToDo(t *testing.T) {
	withFakeManagement(t)
	_, errOut, err := run(t, "cluster", "delete", "-f", "../../examples/hydra-mgmt.yaml")
	if err != nil || !strings.Contains(errOut, "nothing to delete") {
		t.Errorf("err=%v out=%s", err, errOut)
	}
}

// orphanOf is a classified orphan of the given kind, for plan output tests.
func orphanOf(kind, name string, action cluster.PruneAction, why string) cluster.Orphan {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(schema.GroupVersionKind{Kind: kind})
	o.SetNamespace("default")
	o.SetName(name)
	return cluster.Orphan{Object: o, Action: action, Why: why}
}

func TestPlanWordingWithOrphans(t *testing.T) {
	var b bytes.Buffer
	if err := printPlan(&b, nil, []cluster.Orphan{orphanOf("MachineDeployment", "c-old", cluster.PruneNeedsFlag, "needs --prune-pools")}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "matches hydra.yaml") || !strings.Contains(b.String(), "1 object(s) no longer rendered, 0 of which apply would prune") {
		t.Errorf("orphans must not read as a clean plan:\n%s", b.String())
	}
	if strings.Contains(b.String(), "refused") {
		t.Errorf("no refusal, so the plan must not say apply will not run:\n%s", b.String())
	}
	// Only a refusal, and nothing else changed: the last line must say apply
	// will not run.
	b.Reset()
	if err := printPlan(&b, nil, []cluster.Orphan{orphanOf("HelmChartProxy", "c-guarded", cluster.PruneRefused, "addon guarded is protected")}); err != nil {
		t.Fatal(err)
	}
	if want := "No changes to apply; 1 object(s) no longer rendered, 0 of which apply would prune; 1 refused, so apply will not run"; !strings.Contains(b.String(), want) {
		t.Errorf("lacks %q:\n%s", want, b.String())
	}
}

func TestDeleteRefusesAnotherClustersCluster(t *testing.T) {
	cl := capiCluster("default", "hydra-mgmt", "", 0)
	cl.SetLabels(map[string]string{"hydra.io/cluster": "someone-else"})
	withFakeManagement(t, cl)
	_, _, err := run(t, "cluster", "delete", "-f", "../../examples/hydra-mgmt.yaml", "--yes", "--adopt")
	if err == nil || !strings.Contains(err.Error(), `belongs to cluster "someone-else"`) {
		t.Fatalf("got %v", err)
	}
}

func TestDeleteHandBuiltClusterNeedsAdopt(t *testing.T) {
	cl := capiCluster("default", "hydra-mgmt", "", 0)
	cl.SetLabels(nil)
	withFakeManagement(t, cl)
	_, _, err := run(t, "cluster", "delete", "-f", "../../examples/hydra-mgmt.yaml", "--yes")
	if err == nil || !strings.Contains(err.Error(), "--adopt") {
		t.Fatalf("got %v", err)
	}
}

// create never changes the Kubernetes version of a running cluster: that
// would roll the control plane and every pool at once.
func TestCreateRefusesAVersionChange(t *testing.T) {
	kcp := &unstructured.Unstructured{Object: map[string]any{}}
	kcp.SetAPIVersion("controlplane.cluster.x-k8s.io/v1beta2")
	kcp.SetKind("KubeadmControlPlane")
	kcp.SetNamespace("default")
	kcp.SetName("hydra-mgmt-cp")
	kcp.SetLabels(map[string]string{"hydra.io/cluster": "hydra-mgmt"})
	_ = unstructured.SetNestedField(kcp.Object, "v1.34.2", "spec", "version")
	c := withFakeManagement(t, capiCluster("default", "hydra-mgmt", "192.168.15.11", 6443), kcp)
	// A management cluster: the Cluster API kinds resolve.
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Cluster"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "addons.cluster.x-k8s.io", Version: "v1alpha1", Kind: "HelmChartProxy"}, meta.RESTScopeNamespace)
	withCAPI := fake.NewClientBuilder().WithScheme(c.Scheme()).WithRESTMapper(mapper).
		WithObjects(capiCluster("default", "hydra-mgmt", "192.168.15.11", 6443), kcp).Build()
	connectManagement = func([]byte) (client.Client, error) { return withCAPI, nil }
	_, _, err := run(t, "cluster", lcCreate, "-f", "../../examples/hydra-mgmt.yaml", "--management-kubeconfig", "/x", "--wait=false")
	if err == nil || !strings.Contains(err.Error(), "hydra cluster upgrade --to v1.35.3") || !strings.Contains(err.Error(), "control plane v1.34.2") {
		t.Fatalf("want a refusal pointing at upgrade, got %v", err)
	}
	out, _, err := run(t, "cluster", "plan", "-f", "../../examples/hydra-mgmt.yaml", "--management-kubeconfig", "/x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "! version: hydra.yaml says v1.35.3, but control plane v1.34.2") {
		t.Errorf("plan must flag the version change:\n%s", out)
	}
}

// cluMapper is a REST mapper that knows the Cluster API Cluster kind, as on a
// management cluster.
func cluMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Cluster"}, meta.RESTScopeNamespace)
	// hydra bootstrap installs CAAPH, so a management cluster serves it.
	m.Add(schema.GroupVersionKind{Group: "addons.cluster.x-k8s.io", Version: "v1alpha1", Kind: "HelmChartProxy"}, meta.RESTScopeNamespace)
	return m
}

// A hydra.yaml with addons is refused, before anything is written, on a
// management cluster without CAAPH.
func TestCluCreateRefusesAddonsWithoutCAAPH(t *testing.T) {
	cfg, objs := cluLoad(t, "../../examples/workload-managed-network.yaml")
	ap := &cluApplies{}
	tg, _ := cluTarget(t, true, ap, interceptor.Funcs{})
	noCAAPH := meta.NewDefaultRESTMapper(nil)
	noCAAPH.Add(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Cluster"}, meta.RESTScopeNamespace)
	tg.m.C = fake.NewClientBuilder().WithScheme(tg.m.C.Scheme()).WithRESTMapper(noCAAPH).Build()
	cmd, _ := cluCmd()
	if _, err := runCreate(t.Context(), cmd, tg, cfg, objs, createOptions{wait: false}); err == nil || !strings.Contains(err.Error(), "no Cluster API Add-on Provider Helm") {
		t.Fatalf("got %v", err)
	}
	if ap.n != 0 {
		t.Errorf("%d applies before the refusal", ap.n)
	}
}

// cluApplies counts server-side applies and can fail them.
type cluApplies struct {
	n   int
	err error
}

// cluTarget is a management-cluster target over a fake client holding objs:
// applies are intercepted and counted (never written), and mgmt says whether
// Cluster API is installed.
func cluTarget(t *testing.T, mgmt bool, ap *cluApplies, funcs interceptor.Funcs, objs ...client.Object) (*target, *bytes.Buffer) {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if funcs.Apply == nil {
		funcs.Apply = func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
			ap.n++
			return ap.err
		}
	}
	b := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithInterceptorFuncs(funcs)
	if mgmt {
		b = b.WithRESTMapper(cluMapper())
	}
	var log bytes.Buffer
	return &target{m: &cluster.Manager{C: b.Build(), Out: &log, Poll: time.Millisecond}, source: "test-mgmt", bootstrapKind: true}, &log
}

func cluLoad(t *testing.T, path string) (*config.ClusterConfig, []*unstructured.Unstructured) {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	objs, err := renderObjects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, objs
}

func cluCmd() (*cobra.Command, *bytes.Buffer) {
	var errOut bytes.Buffer
	c := &cobra.Command{}
	c.SetErr(&errOut)
	c.SetOut(&errOut)
	return c, &errOut
}

// cluAvailable is a cluster ns/name that its controllers report Available,
// rolled out, with the CNI applied and a kubeconfig Secret holding kc.
func cluAvailable(version string, kc []byte) []client.Object {
	const name = "hydra-mgmt"
	const ns = "default"
	cl := capiCluster(ns, name, "", 0)
	_ = unstructured.SetNestedSlice(cl.Object, []any{map[string]any{"type": "Available", lcStatus: "True"}}, lcStatus, "conditions")
	kcp := &unstructured.Unstructured{Object: map[string]any{}}
	kcp.SetAPIVersion("controlplane.cluster.x-k8s.io/v1beta2")
	kcp.SetKind("KubeadmControlPlane")
	kcp.SetNamespace(ns)
	kcp.SetName(name + "-cp")
	kcp.SetLabels(map[string]string{"hydra.io/cluster": name})
	_ = unstructured.SetNestedField(kcp.Object, version, "spec", "version")
	_ = unstructured.SetNestedField(kcp.Object, int64(0), "spec", "replicas")
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name + "-kubeconfig"}, Data: map[string][]byte{"value": kc}}
	return append([]client.Object{cl, kcp, sec}, cniInstalled(ns, name)...)
}

// cniInstalled is cluster ns/name's CNI HelmChartProxy at the default Cilium
// version, and the HelmReleaseProxy CAAPH made from it, Ready.
func cniInstalled(ns, name string) []client.Object {
	hcp := &unstructured.Unstructured{Object: map[string]any{}}
	hcp.SetAPIVersion("addons.cluster.x-k8s.io/v1alpha1")
	hcp.SetKind("HelmChartProxy")
	hcp.SetNamespace(ns)
	hcp.SetName(name + "-cilium")
	hcp.SetLabels(map[string]string{"hydra.io/cluster": name, "hydra.io/addon": "cilium"})
	_ = unstructured.SetNestedField(hcp.Object, "1.19.1", "spec", "version")
	hrp := &unstructured.Unstructured{Object: map[string]any{}}
	hrp.SetAPIVersion("addons.cluster.x-k8s.io/v1alpha1")
	hrp.SetKind("HelmReleaseProxy")
	hrp.SetNamespace(ns)
	hrp.SetName(name + "-cilium-abc")
	hrp.SetLabels(map[string]string{"cluster.x-k8s.io/cluster-name": name, "helmreleaseproxy.addons.cluster.x-k8s.io/helmchartproxy-name": name + "-cilium"})
	_ = unstructured.SetNestedField(hrp.Object, "1.19.1", "spec", "version")
	_ = unstructured.SetNestedSlice(hrp.Object, []any{map[string]any{"type": "Ready", lcStatus: "True", "observedGeneration": int64(1)}}, lcStatus, "conditions")
	return []client.Object{hcp, hrp}
}

func TestCluRunCreateRefusesBeforeApplying(t *testing.T) {
	cfg, objs := cluLoad(t, "../../examples/hydra-mgmt.yaml")
	terminating := capiCluster("default", "hydra-mgmt", "", 0)
	terminating.SetFinalizers([]string{"cluster.x-k8s.io"})
	now := metav1.Now()
	terminating.SetDeletionTimestamp(&now)
	theirs := capiCluster("default", "hydra-mgmt", "", 0)
	theirs.SetLabels(map[string]string{"hydra.io/cluster": "other"})
	for _, tc := range []struct {
		name    string
		mgmt    bool
		objs    []client.Object
		funcs   interceptor.Funcs
		wantErr string
	}{
		{"not a management cluster", false, nil, interceptor.Funcs{}, "not a Cluster API management cluster"},
		{"cluster being deleted", true, []client.Object{terminating}, interceptor.Funcs{}, "is being deleted"},
		{"another cluster's objects", true, []client.Object{theirs}, interceptor.Funcs{}, `belongs to cluster "other"`},
		{"state unreadable", true, nil, interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("connection refused")
		}}, "connection refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ap := &cluApplies{}
			tg, _ := cluTarget(t, tc.mgmt, ap, tc.funcs, tc.objs...)
			cmd, _ := cluCmd()
			_, err := runCreate(t.Context(), cmd, tg, cfg, objs, createOptions{wait: false})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
			if ap.n != 0 {
				t.Errorf("%d applies made before a refusal", ap.n)
			}
		})
	}
}

func TestCluRunCreateChecksHypervisorCredentials(t *testing.T) {
	cfg, objs := cluLoad(t, "../../examples/workload-managed-network.yaml")
	secretName := cfg.Spec.Hypervisor.CredentialsSecretName
	for _, tc := range []struct {
		name    string
		secret  *corev1.Secret
		wantErr string
	}{
		{"missing", nil, "does not exist in the management cluster"},
		{"incomplete", &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "demo", Name: secretName}, Data: map[string][]byte{"cacert.pem": []byte("x")}}, "lacks clientcert.pem, clientkey.pem"},
		{"complete", &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "demo", Name: secretName}, Data: map[string][]byte{
			"cacert.pem": []byte("x"), "clientcert.pem": []byte("x"), "clientkey.pem": []byte("x"),
		}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var objsIn []client.Object
			if tc.secret != nil {
				objsIn = append(objsIn, tc.secret)
			}
			ap := &cluApplies{}
			tg, _ := cluTarget(t, true, ap, interceptor.Funcs{}, objsIn...)
			cmd, _ := cluCmd()
			_, err := runCreate(t.Context(), cmd, tg, cfg, objs, createOptions{wait: false})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if ap.n == 0 {
					t.Error("valid credentials must let the apply go ahead")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
			if ap.n != 0 {
				t.Error("nothing may be applied without the credentials")
			}
		})
	}
}

func TestCluRunCreateNoWait(t *testing.T) {
	cfg, objs := cluLoad(t, "../../examples/hydra-mgmt.yaml")
	// A pool hydra.yaml no longer renders, labelled for this cluster.
	old := &unstructured.Unstructured{Object: map[string]any{}}
	old.SetAPIVersion("v1")
	old.SetKind("ConfigMap")
	old.SetNamespace("default")
	old.SetName("hydra-mgmt-old")
	old.SetLabels(map[string]string{"hydra.io/cluster": "hydra-mgmt"})
	ap := &cluApplies{}
	tg, _ := cluTarget(t, true, ap, interceptor.Funcs{}, old)
	cmd, errOut := cluCmd()
	path, err := runCreate(t.Context(), cmd, tg, cfg, objs, createOptions{wait: false})
	if err != nil || path != "" {
		t.Fatalf("path %q err %v", path, err)
	}
	// One apply for the namespace, then one per object.
	if ap.n != len(objs)+1 {
		t.Errorf("%d applies, want %d", ap.n, len(objs)+1)
	}
	for _, want := range []string{
		"creating a NEW cluster default/hydra-mgmt from the bootstrap cluster",
		"not in hydra.yaml any more",
		"ConfigMap default/hydra-mgmt-old",
		"not waiting (--wait=false)",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, errOut.String())
		}
	}
}

func TestCluRunCreateApplyAndOrphanErrors(t *testing.T) {
	cfg, objs := cluLoad(t, "../../examples/hydra-mgmt.yaml")
	t.Run("apply fails", func(t *testing.T) {
		ap := &cluApplies{err: errors.New("admission webhook denied")}
		tg, _ := cluTarget(t, true, ap, interceptor.Funcs{})
		cmd, _ := cluCmd()
		if _, err := runCreate(t.Context(), cmd, tg, cfg, objs, createOptions{}); err == nil || !strings.Contains(err.Error(), "admission webhook denied") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("orphan check fails after the apply", func(t *testing.T) {
		ap := &cluApplies{}
		tg, _ := cluTarget(t, true, ap, interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("list refused")
		}})
		cmd, errOut := cluCmd()
		if _, err := runCreate(t.Context(), cmd, tg, cfg, objs, createOptions{wait: false}); err != nil {
			t.Fatalf("a failed orphan check must only warn: %v", err)
		}
		if !strings.Contains(errOut.String(), "warning: could not check for objects hydra.yaml no longer renders: listing HydraCluster: list refused") {
			t.Errorf("no warning:\n%s", errOut.String())
		}
	})
}

func TestCluRunCreateWaitsAndWritesKubeconfig(t *testing.T) {
	cfg, objs := cluLoad(t, "../../examples/hydra-mgmt.yaml")
	t.Run("explicit path", func(t *testing.T) {
		ap := &cluApplies{}
		tg, log := cluTarget(t, true, ap, interceptor.Funcs{}, cluAvailable(cfg.Spec.KubernetesVersion, []byte("admin"))...)
		tg.bootstrapKind = false
		cmd, errOut := cluCmd()
		out := filepath.Join(t.TempDir(), "x.kubeconfig")
		path, err := runCreate(t.Context(), cmd, tg, cfg, objs, createOptions{kubeconfigOut: out, wait: true, timeout: 5 * time.Second})
		if err != nil {
			t.Fatalf("%v\n%s", err, log.String())
		}
		if path != out {
			t.Errorf("path %q", path)
		}
		if b, _ := os.ReadFile(out); string(b) != "admin" {
			t.Errorf("kubeconfig %q", b)
		}
		for _, want := range []string{"waiting up to 5s", "is Available after", "kubeconfig written to " + out} {
			if !strings.Contains(errOut.String(), want) {
				t.Errorf("output lacks %q:\n%s", want, errOut.String())
			}
		}
		if strings.Contains(errOut.String(), "creating a NEW cluster") {
			t.Error("an existing cluster must not be announced as new")
		}
	})
	t.Run("default path", func(t *testing.T) {
		t.Chdir(t.TempDir())
		tg, _ := cluTarget(t, true, &cluApplies{}, interceptor.Funcs{}, cluAvailable(cfg.Spec.KubernetesVersion, []byte("admin"))...)
		cmd, _ := cluCmd()
		path, err := runCreate(t.Context(), cmd, tg, cfg, objs, createOptions{wait: true, timeout: 5 * time.Second})
		if err != nil || path != "hydra-mgmt.kubeconfig" {
			t.Fatalf("path %q err %v", path, err)
		}
		// A different file already at the default path is never overwritten.
		if err := os.WriteFile(path, []byte("someone else's"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := runCreate(t.Context(), cmd, tg, cfg, objs, createOptions{wait: true, timeout: 5 * time.Second}); err == nil || !strings.Contains(err.Error(), "not overwriting") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("no kubeconfig Secret", func(t *testing.T) {
		live := cluAvailable(cfg.Spec.KubernetesVersion, nil)
		tg, _ := cluTarget(t, true, &cluApplies{}, interceptor.Funcs{}, append(live[:2:2], live[3:]...)...)
		cmd, _ := cluCmd()
		if _, err := runCreate(t.Context(), cmd, tg, cfg, objs, createOptions{wait: true, timeout: 5 * time.Second}); err == nil || !strings.Contains(err.Error(), "hydra-mgmt-kubeconfig") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		tg, _ := cluTarget(t, true, &cluApplies{}, interceptor.Funcs{})
		cmd, _ := cluCmd()
		if _, err := runCreate(t.Context(), cmd, tg, cfg, objs, createOptions{wait: true, timeout: 20 * time.Millisecond}); !errors.Is(err, cluster.ErrTimeout) {
			t.Fatalf("got %v", err)
		}
	})
	// apply prunes last: a timeout, or a kubeconfig it cannot fetch, leaves
	// what it would have pruned; a finished apply prunes it.
	for _, tc := range []struct {
		name    string
		live    func() []client.Object
		timeout time.Duration
		pruned  bool
	}{
		{"apply that times out prunes nothing", func() []client.Object { return nil }, 20 * time.Millisecond, false},
		{"apply without a kubeconfig prunes nothing", func() []client.Object {
			return cluAvailable(cfg.Spec.KubernetesVersion, nil)[:3]
		}, 5 * time.Second, false},
		{"finished apply prunes", func() []client.Object {
			return cluAvailable(cfg.Spec.KubernetesVersion, []byte("admin"))
		}, 5 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			stale := labelledFor("HydraMachineTemplate", "infrastructure.cluster.x-k8s.io/v1alpha1", "hydra-mgmt-cp-stale")
			tg, _ := cluTarget(t, true, &cluApplies{}, interceptor.Funcs{}, append(tc.live(), stale)...)
			cmd, errOut := cluCmd()
			_, err := runCreate(t.Context(), cmd, tg, cfg, objs, createOptions{prune: &cluster.PruneOptions{}, wait: true, timeout: tc.timeout})
			if (err == nil) != tc.pruned {
				t.Fatalf("err %v\n%s", err, errOut.String())
			}
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(stale.GroupVersionKind())
			gone := apierrors.IsNotFound(tg.m.C.Get(t.Context(), client.ObjectKeyFromObject(stale), u))
			if gone != tc.pruned {
				t.Errorf("pruned=%v, want %v\n%s", gone, tc.pruned, errOut.String())
			}
		})
	}
}

func TestCluCreateCommandRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"missing file", []string{"cluster", lcCreate, "-f", "/nonexistent/hydra.yaml"}, "no such file"},
		{"self-hosted with a management kubeconfig", []string{"cluster", lcCreate, "-f", "../../examples/hydra-mgmt.yaml", "--self-hosted", "--management-kubeconfig", "/x"}, "cannot be combined with --management-kubeconfig"},
		{"self-hosted without waiting", []string{"cluster", lcCreate, "-f", "../../examples/hydra-mgmt.yaml", "--self-hosted", "--wait=false"}, "needs to wait"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := run(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
		})
	}
	t.Run("management cluster unreachable", func(t *testing.T) {
		origT := managementTarget
		t.Cleanup(func() { managementTarget = origT })
		managementTarget = func(string, string) ([]byte, string, error) { return nil, "", errors.New("no bootstrap cluster") }
		if _, _, err := run(t, "cluster", lcCreate, "-f", "../../examples/hydra-mgmt.yaml"); err == nil || !strings.Contains(err.Error(), "no bootstrap cluster") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestCluManagementTargetReadsTheNamedKubeconfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "m.kubeconfig")
	if err := os.WriteFile(p, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	kc, source, err := managementTarget(p, "c")
	if err != nil || string(kc) != testKubeconfig || source != p {
		t.Fatalf("kc=%q source=%q err=%v", kc, source, err)
	}
	if _, _, err := managementTarget(filepath.Join(t.TempDir(), "missing"), "c"); err == nil {
		t.Error("a missing kubeconfig must be an error")
	}
}

func TestCluManagerConnectFailure(t *testing.T) {
	withFakeManagement(t)
	connectManagement = func([]byte) (client.Client, error) { return nil, errors.New("tls: bad certificate") }
	_, _, err := run(t, "cluster", lcStatus, "-f", "../../examples/hydra-mgmt.yaml")
	if err == nil || !strings.Contains(err.Error(), "connecting to test-mgmt: tls: bad certificate") {
		t.Fatalf("got %v", err)
	}
}

func TestCluStatus(t *testing.T) {
	t.Run("not available, with the reason", func(t *testing.T) {
		cl := capiCluster("default", "hydra-mgmt", "", 0)
		_ = unstructured.SetNestedSlice(cl.Object, []any{map[string]any{
			"type": "Available", lcStatus: "False", "reason": "Provisioning", "message": "waiting for the control plane",
		}}, lcStatus, "conditions")
		withFakeManagement(t, cl)
		out, _, err := run(t, "cluster", lcStatus, "-f", "../../examples/hydra-mgmt.yaml")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(out, "default/hydra-mgmt: Available=False (Provisioning)") || !strings.Contains(out, "\n  waiting for the control plane\n") {
			t.Errorf("status:\n%s", out)
		}
	})
	t.Run("absent", func(t *testing.T) {
		withFakeManagement(t)
		out, _, err := run(t, "cluster", lcStatus, "-f", "../../examples/hydra-mgmt.yaml")
		if err != nil || out != "default/hydra-mgmt: Cluster does not exist\n" {
			t.Errorf("out %q err %v", out, err)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		if _, _, err := run(t, "cluster", lcStatus, "-f", "/nonexistent/hydra.yaml"); err == nil {
			t.Error("want an error")
		}
	})
	t.Run("status unreadable", func(t *testing.T) {
		withFakeManagement(t)
		bad := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("forbidden")
		}}).Build()
		connectManagement = func([]byte) (client.Client, error) { return bad, nil }
		if _, _, err := run(t, "cluster", lcStatus, "-f", "../../examples/hydra-mgmt.yaml"); err == nil || !strings.Contains(err.Error(), "forbidden") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestCluPrintPlan(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changes []cluster.Change
		orphans []cluster.Orphan
		want    []string
	}{
		{"clean", nil, nil, []string{"No changes: the live cluster matches hydra.yaml.\n"}},
		{"every action", []cluster.Change{
			{Object: "Cluster default/c", Action: lcCreate},
			{Object: "ConfigMap default/c-cilium", Action: "update", Diff: "-a\n+b", Note: "applies once"},
			{Object: "HydraCluster default/c", Action: "conflict", Diff: "spec.x"},
		}, nil, []string{
			"+ create Cluster default/c\n",
			"~ update ConfigMap default/c-cilium\n  note: applies once\n-a\n+b\n",
			"! conflict HydraCluster default/c: another field manager owns fields",
			"--force-conflicts",
			"3 change(s).\n",
		}},
		{"changes and orphans", []cluster.Change{{Object: "Cluster default/c", Action: lcCreate}}, []cluster.Orphan{
			orphanOf("MachineDeployment", "c-old", cluster.PruneNeedsFlag, "needs --prune-pools"),
			orphanOf("HydraMachineTemplate", "c-cp-1", cluster.PruneDelete, "superseded and unused"),
			orphanOf("HelmChartProxy", "c-x", cluster.PruneRefused, "pass --allow-remove=x"),
		}, []string{
			"? kept MachineDeployment default/c-old (no longer rendered; needs --prune-pools)",
			"- prune HydraMachineTemplate default/c-cp-1 (no longer rendered; apply deletes it, create leaves it: superseded and unused)",
			"! refused HelmChartProxy default/c-x (no longer rendered; apply refuses to run: pass --allow-remove=x)",
			"1 change(s); 3 object(s) no longer rendered, 1 of which apply would prune; 1 refused, so apply will not run until each is resolved (see the ! refused lines).\n",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			if err := printPlan(&b, tc.changes, tc.orphans); err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(b.String(), w) {
					t.Errorf("lacks %q:\n%s", w, b.String())
				}
			}
		})
	}
}

func TestCluPlanCommand(t *testing.T) {
	t.Run("everything new", func(t *testing.T) {
		withFakeManagement(t)
		c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return apierrors.NewNotFound(schema.GroupResource{}, "x")
			},
			Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
				return nil
			},
		}).Build()
		connectManagement = func([]byte) (client.Client, error) { return c, nil }
		out, _, err := run(t, "cluster", "plan", "-f", "../../examples/hydra-mgmt.yaml")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "+ create Cluster default/hydra-mgmt") || strings.Contains(out, "! version") {
			t.Errorf("plan:\n%s", out)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		if _, _, err := run(t, "cluster", "plan", "-f", "/nonexistent/hydra.yaml"); err == nil {
			t.Error("want an error")
		}
	})
	t.Run("dry-run refused", func(t *testing.T) {
		withFakeManagement(t)
		c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errors.New("forbidden")
			},
		}).Build()
		connectManagement = func([]byte) (client.Client, error) { return c, nil }
		if _, _, err := run(t, "cluster", "plan", "-f", "../../examples/hydra-mgmt.yaml"); err == nil || !strings.Contains(err.Error(), "forbidden") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestCluDeleteConfirmedDeletes(t *testing.T) {
	c := withFakeManagement(t, capiCluster("default", "hydra-mgmt", "", 0))
	origIn := confirmIn
	t.Cleanup(func() { confirmIn = origIn })
	confirmIn = strings.NewReader("hydra-mgmt\n")
	_, errOut, err := run(t, "cluster", "delete", "-f", "../../examples/hydra-mgmt.yaml", "--timeout", "5s")
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	if !strings.Contains(errOut, "Type the cluster name to confirm") {
		t.Errorf("no prompt:\n%s", errOut)
	}
	err = c.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "hydra-mgmt"}, capiCluster("", "", "", 0))
	if !apierrors.IsNotFound(err) {
		t.Errorf("the Cluster must be gone: %v", err)
	}
}

func TestCluDeleteRefusesWithoutATerminal(t *testing.T) {
	c := withFakeManagement(t, capiCluster("default", "hydra-mgmt", "", 0))
	f, err := os.Open(filepath.Join("..", "..", "examples", "hydra-mgmt.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	origIn := confirmIn
	t.Cleanup(func() { confirmIn = origIn })
	confirmIn = f
	_, _, err = run(t, "cluster", "delete", "-f", "../../examples/hydra-mgmt.yaml")
	if err == nil || !strings.Contains(err.Error(), "no terminal") {
		t.Fatalf("got %v", err)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "hydra-mgmt"}, capiCluster("", "", "", 0)); err != nil {
		t.Errorf("the Cluster must still exist: %v", err)
	}
}

func TestCluDeleteAlreadyTerminatingAndUnknownIdentity(t *testing.T) {
	cl := capiCluster("default", "hydra-mgmt", "", 0)
	cl.SetFinalizers([]string{"cluster.x-k8s.io"})
	now := metav1.Now()
	cl.SetDeletionTimestamp(&now)
	// A kubeconfig Secret, but the management kubeconfig carries no CA to
	// compare it with: delete must refuse rather than guess.
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "hydra-mgmt-kubeconfig"}, Data: map[string][]byte{"value": []byte(testKubeconfig)}}
	withFakeManagement(t, cl, sec)
	_, errOut, err := run(t, "cluster", "delete", "-f", "../../examples/hydra-mgmt.yaml", "--yes")
	if err == nil || !strings.Contains(err.Error(), "cannot tell whether hydra-mgmt manages itself") {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(errOut, "already being deleted") {
		t.Errorf("output:\n%s", errOut)
	}
}

func TestCluDeleteCommandErrors(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		if _, _, err := run(t, "cluster", "delete", "-f", "/nonexistent/hydra.yaml"); err == nil {
			t.Error("want an error")
		}
	})
	t.Run("self-hosted missing file", func(t *testing.T) {
		if _, _, err := run(t, "cluster", "delete", "--self-hosted", "-f", "/nonexistent/hydra.yaml"); err == nil {
			t.Error("want an error")
		}
	})
	t.Run("state unreadable", func(t *testing.T) {
		withFakeManagement(t)
		c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errors.New("forbidden")
			},
		}).Build()
		connectManagement = func([]byte) (client.Client, error) { return c, nil }
		if _, _, err := run(t, "cluster", "delete", "-f", "../../examples/hydra-mgmt.yaml", "--yes"); err == nil || !strings.Contains(err.Error(), "forbidden") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("management cluster unreachable", func(t *testing.T) {
		origT := managementTarget
		t.Cleanup(func() { managementTarget = origT })
		managementTarget = func(string, string) ([]byte, string, error) { return nil, "", errors.New("no bootstrap cluster") }
		if _, _, err := run(t, "cluster", "delete", "-f", "../../examples/hydra-mgmt.yaml"); err == nil || !strings.Contains(err.Error(), "no bootstrap cluster") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestCluWriteKubeconfigIntoAMissingDirectory(t *testing.T) {
	if err := writeKubeconfig(filepath.Join(t.TempDir(), "nope", "c.kubeconfig"), []byte("x"), true); err == nil {
		t.Error("want an error for a directory that does not exist")
	}
}

// A Cluster whose CNI hydra did not install (no CNI label) must not get
// hydra's Cilium layered on top: create refuses with cni: hydra before any
// write, plan warns, and cni: external proceeds.
func TestCluCreateRefusesHydraCNIOnAForeignCNI(t *testing.T) {
	foreign := func() *unstructured.Unstructured {
		cl := capiCluster("default", "hydra-mgmt", "", 0)
		cl.SetLabels(map[string]string{"hydra.io/cluster": "hydra-mgmt"})
		// Running: its API is up, so some CNI may be there.
		_ = unstructured.SetNestedField(cl.Object, true, "status", "initialization", "controlPlaneInitialized")
		return cl
	}
	cfg, objs := cluLoad(t, "../../examples/hydra-mgmt.yaml")
	ap := &cluApplies{}
	tg, _ := cluTarget(t, true, ap, interceptor.Funcs{}, foreign())
	cmd, _ := cluCmd()
	if _, err := runCreate(t.Context(), cmd, tg, cfg, objs, createOptions{wait: false}); err == nil || !strings.Contains(err.Error(), "spec.network.cni: external") {
		t.Fatalf("want a refusal naming cni: external, got %v", err)
	}
	if ap.n != 0 {
		t.Errorf("%d applies before the refusal", ap.n)
	}
	// --adopt-cni is the operator's say-so: create applies.
	ap = &cluApplies{}
	tg, _ = cluTarget(t, true, ap, interceptor.Funcs{}, foreign())
	cmd, _ = cluCmd()
	if _, err := runCreate(t.Context(), cmd, tg, cfg, objs, createOptions{wait: false, adoptCNI: true}); err != nil {
		t.Fatalf("--adopt-cni: %v", err)
	}
	if ap.n == 0 {
		t.Error("--adopt-cni applied nothing")
	}

	cfg.Spec.Network.CNI, cfg.Spec.Network.MTU, cfg.Spec.Network.CiliumVersion = config.CNIExternal, 0, ""
	objs, err := renderObjects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ap = &cluApplies{}
	tg, _ = cluTarget(t, true, ap, interceptor.Funcs{}, foreign())
	cmd, _ = cluCmd()
	if _, err := runCreate(t.Context(), cmd, tg, cfg, objs, createOptions{wait: false}); err != nil {
		t.Fatalf("cni: external: %v", err)
	}
	if ap.n != len(objs)+1 {
		t.Errorf("%d applies, want %d", ap.n, len(objs)+1)
	}

	// external, but the label survived the apply (another field manager
	// owns it too; the fake client never prunes it): the old
	// ClusterResourceSet still selects the cluster, so create fails, and the
	// fix it prints reaches the management cluster, not the current context.
	labelled := foreign()
	labelled.SetLabels(map[string]string{"hydra.io/cluster": "hydra-mgmt", "cni.hydra.io/cilium": "hydra-mgmt"})
	tg, _ = cluTarget(t, true, &cluApplies{}, interceptor.Funcs{}, labelled)
	cmd, _ = cluCmd()
	_, err = runCreate(t.Context(), cmd, tg, cfg, objs, createOptions{wait: false, configFile: "mgmt.yaml"})
	if err == nil || !strings.Contains(err.Error(), "applied, but cluster default/hydra-mgmt still carries the cni.hydra.io/cilium label") {
		t.Fatalf("want a stale-label error, got %v", err)
	}
	if !strings.Contains(err.Error(), "hydra bootstrap kubeconfig -f mgmt.yaml > bootstrap.kubeconfig && kubectl --kubeconfig bootstrap.kubeconfig label cluster -n default hydra-mgmt cni.hydra.io/cilium-") {
		t.Errorf("the fix does not reach the bootstrap cluster: %v", err)
	}
	tg.path = "/m.yaml"
	if _, err := runCreate(t.Context(), cmd, tg, cfg, objs, createOptions{wait: false}); err == nil || !strings.Contains(err.Error(), "kubectl --kubeconfig /m.yaml label cluster") {
		t.Errorf("the fix does not pass --management-kubeconfig: %v", err)
	}

	withFakeManagement(t)
	c := fake.NewClientBuilder().WithScheme(withFakeManagement(t).Scheme()).WithRESTMapper(cluMapper()).WithObjects(foreign()).Build()
	connectManagement = func([]byte) (client.Client, error) { return c, nil }
	out, _, err := run(t, "cluster", "plan", "-f", "../../examples/hydra-mgmt.yaml", "--management-kubeconfig", "/x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "! cni: create refuses this: cluster default/hydra-mgmt exists without the cni.hydra.io/cilium label") ||
		!strings.Contains(out, "rerun with --adopt-cni") {
		t.Errorf("plan does not warn with a runnable fix:\n%s", out)
	}
}

// create's addon note tells installing, failed and unreadable apart.
func TestAddonNotes(t *testing.T) {
	s := cluster.Summary{Addons: []cluster.AddonState{
		{Name: "ok", Ready: cluster.ConditionTrue},
		{Name: "new"},
		{Name: "broken", Ready: "False", Message: "install  failed:\n chart not found"},
		{Name: "terse", Ready: "False", Reason: "HelmInstallOrUpgradeFailed"},
	}}
	got := strings.Join(addonNotes(s), "\n")
	if strings.Contains(got, "addon ok") || !strings.Contains(got, "addon new: still being installed") ||
		!strings.Contains(got, "addon broken is not Ready: install failed: chart not found") ||
		!strings.Contains(got, "addon terse is not Ready: HelmInstallOrUpgradeFailed") {
		t.Errorf("got:\n%s", got)
	}
	if got := addonNotes(cluster.Summary{AddonsError: "forbidden"}); len(got) != 1 || got[0] != "addons could not be read: forbidden" {
		t.Errorf("unreadable: %v", got)
	}
}

// labelledFor is an object labelled for cluster hydra-mgmt.
func labelledFor(kind, apiVersion, name string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetAPIVersion(apiVersion)
	o.SetKind(kind)
	o.SetNamespace("default")
	o.SetName(name)
	o.SetLabels(map[string]string{"hydra.io/cluster": "hydra-mgmt"})
	return o
}

// apply refuses, before writing anything, when hydra.yaml no longer lists
// a proxy it may not remove on its own.
func TestCluApplyRefusesBeforeWriting(t *testing.T) {
	cfg, objs := cluLoad(t, "../../examples/hydra-mgmt.yaml")
	hand := labelledFor("HelmChartProxy", "addons.cluster.x-k8s.io/v1alpha1", "hydra-mgmt-unknown")
	ap := &cluApplies{}
	tg, _ := cluTarget(t, true, ap, interceptor.Funcs{}, hand)
	cmd, _ := cluCmd()
	_, err := runCreate(t.Context(), cmd, tg, cfg, objs, createOptions{prune: &cluster.PruneOptions{}, wait: false})
	if err == nil || !strings.Contains(err.Error(), "not applying") || !strings.Contains(err.Error(), "--allow-remove=proxy/hydra-mgmt-unknown") {
		t.Fatalf("got %v", err)
	}
	if ap.n != 0 {
		t.Errorf("%d applies before the refusal", ap.n)
	}
}

// After applying, apply prunes what is safe and keeps a removed pool unless
// --prune-pools; create prunes nothing.
func TestCluApplyPrunes(t *testing.T) {
	cfg, objs := cluLoad(t, "../../examples/hydra-mgmt.yaml")
	for _, tc := range []struct {
		name     string
		opts     *cluster.PruneOptions
		wantGone []string
	}{
		{"create prunes nothing", nil, nil},
		{"apply prunes the unused template, keeps the pool", &cluster.PruneOptions{}, []string{"hydra-mgmt-cp-stale"}},
		{"apply --prune-pools=old", &cluster.PruneOptions{Pools: []string{"old"}}, []string{"hydra-mgmt-cp-stale", "hydra-mgmt-old"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stale := labelledFor("HydraMachineTemplate", "infrastructure.cluster.x-k8s.io/v1alpha1", "hydra-mgmt-cp-stale")
			pool := labelledFor("MachineDeployment", "cluster.x-k8s.io/v1beta2", "hydra-mgmt-old")
			tg, _ := cluTarget(t, true, &cluApplies{}, interceptor.Funcs{}, stale, pool)
			cmd, errOut := cluCmd()
			if _, err := runCreate(t.Context(), cmd, tg, cfg, objs, createOptions{prune: tc.opts, wait: false}); err != nil {
				t.Fatalf("%v\n%s", err, errOut.String())
			}
			exists := func(o *unstructured.Unstructured) bool {
				u := &unstructured.Unstructured{}
				u.SetGroupVersionKind(o.GroupVersionKind())
				return tg.m.C.Get(t.Context(), client.ObjectKeyFromObject(o), u) == nil
			}
			for _, o := range []*unstructured.Unstructured{stale, pool} {
				gone := !exists(o)
				if slices.Contains(tc.wantGone, o.GetName()) != gone {
					t.Errorf("%s gone=%v\n%s", o.GetName(), gone, errOut.String())
				}
			}
			if tc.opts != nil && len(tc.opts.Pools) == 0 && !strings.Contains(errOut.String(), "needs --prune-pools=old") {
				t.Errorf("the kept pool must say how to remove it:\n%s", errOut.String())
			}
		})
	}
}

// A template a live MachineSet still uses survives apply's prune.
func TestCluApplyKeepsTemplatesInUse(t *testing.T) {
	cfg, objs := cluLoad(t, "../../examples/hydra-mgmt.yaml")
	used := labelledFor("HydraMachineTemplate", "infrastructure.cluster.x-k8s.io/v1alpha1", "hydra-mgmt-w-old")
	ms := &unstructured.Unstructured{}
	ms.SetAPIVersion("cluster.x-k8s.io/v1beta2")
	ms.SetKind("MachineSet")
	ms.SetNamespace("default")
	ms.SetName("hydra-mgmt-w-abc")
	ms.SetLabels(map[string]string{"cluster.x-k8s.io/cluster-name": "hydra-mgmt"})
	_ = unstructured.SetNestedField(ms.Object, "hydra-mgmt-w-old", "spec", "template", "spec", "infrastructureRef", "name")
	tg, _ := cluTarget(t, true, &cluApplies{}, interceptor.Funcs{}, used, ms)
	cmd, errOut := cluCmd()
	if _, err := runCreate(t.Context(), cmd, tg, cfg, objs, createOptions{prune: &cluster.PruneOptions{}, wait: false}); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(used.GroupVersionKind())
	if err := tg.m.C.Get(t.Context(), client.ObjectKeyFromObject(used), u); err != nil {
		t.Errorf("a template a MachineSet uses must survive: %v\n%s", err, errOut.String())
	}
	if !strings.Contains(errOut.String(), "still referenced") {
		t.Errorf("apply must say why it kept it:\n%s", errOut.String())
	}
}

// An object that changes between classification and deletion is left, and
// apply exits non-zero so a script does not read it as finished.
func TestCluApplyFailsWhenAPruneIsSkipped(t *testing.T) {
	cfg, objs := cluLoad(t, "../../examples/hydra-mgmt.yaml")
	stale := labelledFor("HydraMachineTemplate", "infrastructure.cluster.x-k8s.io/v1alpha1", "hydra-mgmt-cp-stale")
	conflict := interceptor.Funcs{Delete: func(ctx context.Context, w client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
		return apierrors.NewConflict(schema.GroupResource{Resource: "hydramachinetemplates"}, o.GetName(), errors.New("changed"))
	}}
	tg, _ := cluTarget(t, true, &cluApplies{}, conflict, stale)
	cmd, _ := cluCmd()
	_, err := runCreate(t.Context(), cmd, tg, cfg, objs, createOptions{prune: &cluster.PruneOptions{}, wait: false})
	if err == nil || !strings.Contains(err.Error(), "changed while apply ran") {
		t.Fatalf("got %v", err)
	}
}
