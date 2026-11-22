package cluster

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Petatron/hydra/internal/config"
	"github.com/Petatron/hydra/internal/render"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// obj builds a test object in namespace "ns".
func obj(gvk schema.GroupVersionKind, _ string, name string, fields map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	maps.Copy(u.Object, fields)
	u.SetGroupVersionKind(gvk)
	u.SetNamespace("ns")
	u.SetName(name)
	return u
}

func TestObjectsFromRender(t *testing.T) {
	c, err := config.Load("../../examples/workload-managed-network.yaml")
	if err != nil {
		t.Fatal(err)
	}
	out, err := render.Render(c, render.Options{})
	if err != nil {
		t.Fatal(err)
	}
	objs, err := Objects(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 13 {
		t.Fatalf("want 13 objects (header comment skipped), got %d", len(objs))
	}
	if objs[0].GetKind() != "HydraCluster" || objs[len(objs)-1].GetKind() != "ClusterResourceSet" {
		t.Errorf("order not kept: first %s, last %s", objs[0].GetKind(), objs[len(objs)-1].GetKind())
	}
}

func TestSummaryLine(t *testing.T) {
	if got := (Summary{}).Line(); got != "Cluster does not exist" {
		t.Errorf("got %q", got)
	}
	s := Summary{Exists: true, Available: "False", AvailableReason: "NotAvailable", ControlPlane: "1/3 ready",
		Machines: map[string]int{"Running": 1, "Provisioning": 2}, CNIBindingPresent: true}
	want := "Available=False (NotAvailable) · control plane 1/3 ready · machines: 2 Provisioning, 1 Running · CNI pending"
	if got := s.Line(); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestBindingApplied(t *testing.T) {
	b := obj(bindingGVK, "ns", "c", map[string]any{"spec": map[string]any{"bindings": []any{
		map[string]any{"clusterResourceSetName": "c-cilium", "resources": []any{map[string]any{"applied": true}}},
	}}})
	if !bindingApplied(b, "c-cilium") {
		t.Error("applied binding not recognised")
	}
	if bindingApplied(b, "other") {
		t.Error("another set's binding counted")
	}
	b2 := obj(bindingGVK, "ns", "c", map[string]any{"spec": map[string]any{"bindings": []any{
		map[string]any{"clusterResourceSetName": "c-cilium", "resources": []any{map[string]any{"applied": false}}},
	}}})
	if bindingApplied(b2, "c-cilium") {
		t.Error("unapplied resource counted as applied")
	}
}

func TestStatusReadsControllers(t *testing.T) {
	ctx := context.Background()
	cl := obj(clusterGVK, "ns", "c", map[string]any{"status": map[string]any{"conditions": []any{
		map[string]any{"type": "Available", "status": "True", "reason": "Available"},
	}}})
	cl.SetLabels(map[string]string{render.CNILabelKey: "c"})
	kcp := obj(kcpGVK, "ns", "c-cp", map[string]any{"spec": map[string]any{"replicas": int64(3)}, "status": map[string]any{"replicas": int64(3), "readyReplicas": int64(3), "upToDateReplicas": int64(3)}})
	m1 := obj(machineGVK, "ns", "c-cp-1", map[string]any{"status": map[string]any{"phase": "Running"}})
	m1.SetLabels(map[string]string{"cluster.x-k8s.io/cluster-name": "c"})
	other := obj(machineGVK, "ns", "other-1", map[string]any{"status": map[string]any{"phase": "Running"}})
	other.SetLabels(map[string]string{"cluster.x-k8s.io/cluster-name": "other"})
	binding := obj(bindingGVK, "ns", "c", map[string]any{"spec": map[string]any{"bindings": []any{
		map[string]any{"clusterResourceSetName": "c-cilium", "resources": []any{map[string]any{"applied": true}}},
	}}})
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(cl, kcp, m1, other, binding).Build()
	s, err := (&Manager{C: c}).Status(ctx, "ns", "c")
	if err != nil {
		t.Fatal(err)
	}
	want := "Available=True (Available) · control plane 3/3 ready · machines: 1 Running · CNI applied"
	if got := s.Line(); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestWaitTimesOutWithoutFailingTheCluster(t *testing.T) {
	cl := obj(clusterGVK, "ns", "c", nil)
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(cl).Build()
	var out bytes.Buffer
	m := &Manager{C: c, Out: &out, Poll: 10 * time.Millisecond}
	err := m.WaitAvailable(context.Background(), "ns", "c", 50*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	if strings.Count(out.String(), "Available=") != 1 {
		t.Errorf("the summary must print once per change, not per poll:\n%s", out.String())
	}
}

func TestCheckCredentials(t *testing.T) {
	ctx := context.Background()
	full := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pki"},
		Data: map[string][]byte{"cacert.pem": {1}, "clientcert.pem": {1}, "clientkey.pem": {1}}}
	partial := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "partial"},
		Data: map[string][]byte{"cacert.pem": {1}}}
	m := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(full, partial).Build()}
	if err := m.CheckCredentials(ctx, "ns", "pki"); err != nil {
		t.Error(err)
	}
	if err := m.CheckCredentials(ctx, "ns", "absent"); err == nil || !strings.Contains(err.Error(), "hydra bootstrap --hypervisor-credentials") {
		t.Errorf("absent: %v", err)
	}
	if err := m.CheckCredentials(ctx, "ns", "partial"); err == nil || !strings.Contains(err.Error(), "clientcert.pem, clientkey.pem") {
		t.Errorf("partial: %v", err)
	}
}

func TestPlanCreateUpdateAndNoChange(t *testing.T) {
	ctx := context.Background()
	live := obj(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, "ns", "c-cilium", map[string]any{"data": map[string]any{"cilium.yaml": "a: 1\nmtu: \"1500\"\n"}})
	live.SetLabels(map[string]string{render.CNILabelKey: "c"})
	same := obj(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, "ns", "same", map[string]any{"data": map[string]any{"k": "v"}})
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(live, same).WithInterceptorFuncs(interceptor.Funcs{
		// The server's dry-run result is the applied object itself here.
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return nil
		},
	}).Build()

	desired := live.DeepCopy()
	_ = unstructured.SetNestedField(desired.Object, "a: 1\nmtu: \"1450\"\n", "data", "cilium.yaml")
	newObj := obj(clusterGVK, "ns", "c", nil)
	changes, err := (&Manager{C: c}).Plan(ctx, []*unstructured.Unstructured{desired, same.DeepCopy(), newObj}, "c", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("want an update and a create, got %+v", changes)
	}
	up := changes[0]
	if up.Action != "update" || !strings.Contains(up.Diff, `-  mtu: "1500"`) || !strings.Contains(up.Diff, `+  mtu: "1450"`) {
		t.Errorf("update diff should show the changed line:\n%s", up.Diff)
	}
	if up.Note == "" {
		t.Error("a CNI ConfigMap change must carry the ApplyOnce note")
	}
	if changes[1].Action != "create" || changes[1].Object != "Cluster ns/c" {
		t.Errorf("create: %+v", changes[1])
	}
}

func TestDeleteRemovesLeftoversByLabel(t *testing.T) {
	ctx := context.Background()
	mine := obj(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, "ns", "c-cilium", nil)
	mine.SetLabels(map[string]string{render.ClusterLabelKey: "c"})
	theirs := obj(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, "ns", "d-cilium", nil)
	theirs.SetLabels(map[string]string{render.ClusterLabelKey: "d"})
	unlabelled := obj(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, "ns", "c-other", nil)
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(mine, theirs, unlabelled).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if u, ok := list.(*unstructured.UnstructuredList); ok && u.GroupVersionKind().Group != "" {
				return nil // CAPI kinds: none left
			}
			return cl.List(ctx, list, opts...)
		},
	}).Build()
	var out bytes.Buffer
	m := &Manager{C: c, Out: &out, Poll: time.Millisecond}
	if err := m.Delete(ctx, "ns", "c", time.Second, false); err != nil {
		t.Fatal(err)
	}
	var cms corev1.ConfigMapList
	if err := c.List(ctx, &cms); err != nil {
		t.Fatal(err)
	}
	left := make([]string, 0, len(cms.Items))
	for _, cm := range cms.Items {
		left = append(left, cm.Name)
	}
	if strings.Join(left, ",") != "c-other,d-cilium" {
		t.Errorf("only this cluster's labelled objects may be deleted; left %v", left)
	}
}

func labelled(gvk schema.GroupVersionKind, name, cluster string) *unstructured.Unstructured {
	u := obj(gvk, "ns", name, nil)
	if cluster != "" {
		u.SetLabels(map[string]string{render.ClusterLabelKey: cluster})
	}
	return u
}

func TestCheckOwnership(t *testing.T) {
	ctx := context.Background()
	cm := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		labelled(cm, "mine", "c"), labelled(cm, "theirs", "d"), labelled(cm, "handmade", ""),
	).Build()
	m := &Manager{C: c}
	if err := m.CheckOwnership(ctx, []*unstructured.Unstructured{labelled(cm, "mine", "c"), labelled(cm, "new", "c")}, "c", false); err != nil {
		t.Errorf("own and new objects must pass: %v", err)
	}
	err := m.CheckOwnership(ctx, []*unstructured.Unstructured{labelled(cm, "theirs", "c")}, "c", true)
	if err == nil || !strings.Contains(err.Error(), `belongs to cluster "d"`) {
		t.Errorf("another cluster's object must be refused even with adopt: %v", err)
	}
	err = m.CheckOwnership(ctx, []*unstructured.Unstructured{labelled(cm, "handmade", "c")}, "c", false)
	if err == nil || !strings.Contains(err.Error(), "--adopt") {
		t.Errorf("an unlabelled object must need --adopt: %v", err)
	}
	if err := m.CheckOwnership(ctx, []*unstructured.Unstructured{labelled(cm, "handmade", "c")}, "c", true); err != nil {
		t.Errorf("adopt must allow an unlabelled object: %v", err)
	}
}

func TestOrphans(t *testing.T) {
	cm := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		labelled(cm, "c-cilium", "c"), labelled(cm, "c-old", "c"), labelled(cm, "d-cilium", "d"),
	).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if u, ok := list.(*unstructured.UnstructuredList); ok && u.GroupVersionKind().Group != "" {
				return nil
			}
			return cl.List(ctx, list, opts...)
		},
	}).Build()
	got, err := (&Manager{C: c}).Orphans(context.Background(), []*unstructured.Unstructured{labelled(cm, "c-cilium", "c")}, "ns", "c")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "ConfigMap ns/c-old" {
		t.Errorf("got %v", got)
	}
}

func kubeconfigFor(server, ca string) []byte {
	return []byte(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "` + server + `", certificate-authority-data: "` + base64.StdEncoding.EncodeToString([]byte(ca)) + `"}}]
contexts: [{name: c, context: {cluster: c, user: u}}]
current-context: c
users: [{name: u, user: {token: x}}]
`)
}

func TestSelfHosted(t *testing.T) {
	ctx := context.Background()
	cl := obj(clusterGVK, "ns", "c", map[string]any{"spec": map[string]any{"controlPlaneEndpoint": map[string]any{"host": "10.0.0.5", "port": int64(6443)}}})
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "c-kubeconfig"},
		Data: map[string][]byte{"value": kubeconfigFor("https://10.0.0.5:6443", "workload-ca")}}
	m := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(cl, secret).Build()}
	st, err := m.State(ctx, "ns", "c")
	if err != nil || !st.Exists || st.Terminating || st.Endpoint != "10.0.0.5:6443" {
		t.Fatalf("state %+v %v", st, err)
	}
	for _, tc := range []struct {
		name string
		mgmt []byte
		want bool
	}{
		{"same endpoint", kubeconfigFor("https://10.0.0.5:6443", "other-ca"), true},
		{"same CA through a tunnel", kubeconfigFor("https://127.0.0.1:16443", "workload-ca"), true},
		{"kind management cluster", kubeconfigFor("https://127.0.0.1:41234", "kind-ca"), false},
	} {
		self, _, err := m.SelfHosted(ctx, "ns", "c", tc.mgmt, st)
		if err != nil || self != tc.want {
			t.Errorf("%s: self=%v err=%v, want %v", tc.name, self, err, tc.want)
		}
	}
	// No kubeconfig Secret: nothing to compare, not self-hosted.
	m2 := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(cl).Build()}
	if self, _, err := m2.SelfHosted(ctx, "ns", "c", kubeconfigFor("https://127.0.0.1:41234", "kind-ca"), st); err != nil || self {
		t.Errorf("no Secret: self=%v err=%v", self, err)
	}
	// A management CA given as a file is read from it.
	caFile := t.TempDir() + "/ca.crt"
	if err := os.WriteFile(caFile, []byte("workload-ca"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileKC := []byte(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:16443", certificate-authority: "` + caFile + `"}}]
contexts: [{name: c, context: {cluster: c, user: u}}]
current-context: c
users: [{name: u, user: {token: x}}]
`)
	if self, _, err := m.SelfHosted(ctx, "ns", "c", fileKC, st); err != nil || !self {
		t.Errorf("CA from a file through a tunnel: self=%v err=%v, want self-hosted", self, err)
	}
	// No CA at all: indeterminate, so an error rather than a guess.
	noCA := []byte(strings.Replace(string(fileKC), `certificate-authority: "`+caFile+`"`, `insecure-skip-tls-verify: true`, 1))
	if _, _, err := m.SelfHosted(ctx, "ns", "c", noCA, st); err == nil {
		t.Error("an undeterminable management CA must be an error")
	}
	// A workload kubeconfig Secret without a readable CA: indeterminate.
	broken := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "c-kubeconfig"}, Data: map[string][]byte{"value": []byte("not a kubeconfig")}}
	m4 := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(cl, broken).Build()}
	if _, _, err := m4.SelfHosted(ctx, "ns", "c", kubeconfigFor("https://127.0.0.1:41234", "kind-ca"), st); err == nil {
		t.Error("a workload kubeconfig without a CA must be an error")
	}
	// A Secret that cannot be read is an error, never "not self-hosted".
	m3 := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("forbidden")
		},
	}).Build()}
	if _, _, err := m3.SelfHosted(ctx, "ns", "c", kubeconfigFor("https://127.0.0.1:1", "x"), ClusterState{Exists: true}); err == nil {
		t.Error("an unreadable Secret must be an error")
	}
}

func TestUpToDate(t *testing.T) {
	mk := func(gen, observed, replicas, upto int64, withReplicas bool) *unstructured.Unstructured {
		u := obj(kcpGVK, "ns", "c-cp", map[string]any{"status": map[string]any{"observedGeneration": observed, "upToDateReplicas": upto, "replicas": replicas}})
		u.SetGeneration(gen)
		if withReplicas {
			_ = unstructured.SetNestedField(u.Object, replicas, "spec", "replicas")
		}
		return u
	}
	if !UpToDate(withReady(mk(2, 2, 3, 3, true), 3)) {
		t.Error("all up to date")
	}
	if UpToDate(mk(3, 2, 3, 3, true)) {
		t.Error("an unobserved generation is still rolling")
	}
	if UpToDate(mk(2, 2, 3, 1, true)) {
		t.Error("replicas not yet up to date")
	}
	if UpToDate(mk(2, 2, 3, 3, true)) {
		t.Error("up to date but not Ready must keep waiting")
	}
	if !UpToDate(mk(1, 1, 0, 0, false)) {
		t.Error("an autoscaled pool at zero is up to date")
	}
	// Scaled down from 2 to 1: the surplus Machine is still being deleted, so
	// status.replicas is 2 while 2 are Ready and up to date.
	down := withReady(mk(2, 2, 2, 2, false), 2)
	_ = unstructured.SetNestedField(down.Object, int64(1), "spec", "replicas")
	if UpToDate(down) {
		t.Error("a scale-down is not done while a surplus Machine remains")
	}
}

func withReady(u *unstructured.Unstructured, n int64) *unstructured.Unstructured {
	_ = unstructured.SetNestedField(u.Object, n, "status", "readyReplicas")
	return u
}

func TestStatusReadErrorsAreErrors(t *testing.T) {
	cl := obj(clusterGVK, "ns", "c", nil)
	kcp := obj(kcpGVK, "ns", "c-cp", nil)
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(cl, kcp).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("connection refused")
		},
	}).Build()
	if _, err := (&Manager{C: c}).Status(context.Background(), "ns", "c"); err == nil {
		t.Error("a failed MachineDeployment list must fail the status read, not report the workers done")
	}
}

func TestDeleteRemovesUnownedInfrastructureFirst(t *testing.T) {
	ctx := context.Background()
	// A create interrupted after the HydraCluster but before the Cluster:
	// the HydraCluster has no owner, and delete must still remove it.
	hc := labelled(hydraClusterGVK, "c", "c")
	tmpl := labelled(schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha1", Kind: kindHydraMachineTemplate}, "c-cp-1", "c")
	cm := labelled(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, "c-cilium", "c")
	var order []string
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(hc, tmpl, cm).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
			order = append(order, o.GetObjectKind().GroupVersionKind().Kind)
			return cl.Delete(ctx, o, opts...)
		},
	}).Build()
	var out bytes.Buffer
	if err := (&Manager{C: c, Out: &out, Poll: time.Millisecond}).Delete(ctx, "ns", "c", time.Second, false); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	got := strings.Join(order, ",")
	if !strings.HasPrefix(got, "HydraCluster,") || !strings.Contains(got, "HydraMachineTemplate") || !strings.Contains(got, "ConfigMap") {
		t.Errorf("delete order %s: the HydraCluster must go before the templates", got)
	}
}

func TestApplyDoesNotForceOwnershipByDefault(t *testing.T) {
	for _, o := range applyOptions("c", false) {
		if o == client.ForceOwnership {
			t.Error("ownership must not be forced unless --force-conflicts")
		}
	}
	if got := FieldManagerFor("c"); got != "hydra-c" {
		t.Errorf("field manager %q", got)
	}
	forced := false
	for _, o := range applyOptions("c", true) {
		forced = forced || o == client.ForceOwnership
	}
	if !forced {
		t.Error("--force-conflicts must force ownership")
	}
}

func TestDeleteChecksClusterOwnership(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		label   string
		adopt   bool
		wantErr string
	}{
		{"another cluster's", "d", true, `belongs to cluster "d"`},
		{"unlabelled without adopt", "", false, "--adopt"},
		{"unlabelled with adopt", "", true, ""},
		{"ours", "c", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cl := labelled(clusterGVK, "c", tc.label)
			c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(cl).Build()
			err := (&Manager{C: c, Out: &bytes.Buffer{}, Poll: time.Millisecond}).Delete(ctx, "ns", "c", time.Second, tc.adopt)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want %q, got %v", tc.wantErr, err)
				}
				if c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "c"}, labelled(clusterGVK, "", "")) != nil {
					t.Error("a refused Cluster must still exist")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDeleteExactlySkipsChangedObjects(t *testing.T) {
	ctx := context.Background()
	cm := labelled(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, "c-cilium", "c")
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(cm).Build()
	m := &Manager{C: c}
	var live unstructured.Unstructured
	live.SetGroupVersionKind(cm.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKeyFromObject(cm), &live); err != nil {
		t.Fatal(err)
	}
	stale := live.DeepCopy()
	// Someone relabels it after it was listed.
	live.SetLabels(map[string]string{render.ClusterLabelKey: "d"})
	if err := c.Update(ctx, &live); err != nil {
		t.Fatal(err)
	}
	if err := m.deleteExactly(ctx, stale); !errors.Is(err, errChanged) {
		t.Fatalf("want errChanged for a changed object, got %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cm), &live); err != nil {
		t.Error("the relabelled object must survive")
	}
}

// An apply the provider's schema refuses names the field and the provider
// version hydra needs, instead of only the API server's message.
func TestApplyNamesAnOldProvider(t *testing.T) {
	o := obj(schema.GroupVersionKind{Group: infraGroup, Version: infraVersion, Kind: kindHydraMachineTemplate}, "ns", "c-cp-1", nil)
	refused := errors.New(`failed to create typed patch object (ns/c-cp-1; infrastructure.cluster.x-k8s.io/v1alpha1, Kind=HydraMachineTemplate): .spec.template.spec.preDrainHook: field not declared in schema`)
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Apply: func(_ context.Context, _ client.WithWatch, ac runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
			if _, isNS := ac.(interface{ GetName() *string }); isNS {
				return nil
			}
			return refused
		},
	}).Build()
	err := (&Manager{C: c, Out: &bytes.Buffer{}}).Apply(context.Background(), []*unstructured.Unstructured{o}, "c", false)
	if err == nil || !strings.Contains(err.Error(), ".spec.template.spec.preDrainHook") ||
		!strings.Contains(err.Error(), "Upgrade the provider first") || !errors.Is(err, refused) {
		t.Fatalf("got %v", err)
	}
	if undeclaredField(errors.New("some other failure")) != "" {
		t.Error("only a schema refusal names a field")
	}
}

// The roll from a template without preDrainHook to one with it is the one
// that hands nothing over, and only it is reported.
func TestUnprotectedRoll(t *testing.T) {
	ctx := context.Background()
	hmtGVK := schema.GroupVersionKind{Group: infraGroup, Version: infraVersion, Kind: kindHydraMachineTemplate}
	kcp := func(ref string) *unstructured.Unstructured {
		return obj(kcpGVK, "ns", "c-cp", map[string]any{"spec": map[string]any{"machineTemplate": map[string]any{"spec": map[string]any{
			"infrastructureRef": map[string]any{"name": ref}}}}})
	}
	tmpl := func(name string, hook bool) *unstructured.Unstructured {
		spec := map[string]any{"vcpus": int64(2)}
		if hook {
			spec["preDrainHook"] = map[string]any{"path": "/usr/local/sbin/hydra-pre-drain"}
		}
		return obj(hmtGVK, "ns", name, map[string]any{"spec": map[string]any{"template": map[string]any{"spec": spec}}})
	}
	rendered := []*unstructured.Unstructured{kcp("c-cp-new"), tmpl("c-cp-new", true)}
	for _, tc := range []struct {
		name string
		live []client.Object
		want bool
	}{
		{"old template without the hook", []client.Object{kcp("c-cp-old"), tmpl("c-cp-old", false)}, true},
		{"old template already hooked", []client.Object{kcp("c-cp-old"), tmpl("c-cp-old", true)}, false},
		{"no change of template", []client.Object{kcp("c-cp-new"), tmpl("c-cp-new", true)}, false},
		{"a new cluster", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(tc.live...).Build()
			got, err := (&Manager{C: c}).UnprotectedRoll(ctx, rendered, "ns", "c")
			if err != nil || got != tc.want {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

// A Cluster without the CNI label has no hydra ClusterResourceSet selecting
// it (cni: external): status says so, never reads a binding, and the wait
// ends on Available and rolled out alone.
func TestStatusAndWaitWithExternalCNI(t *testing.T) {
	ctx := context.Background()
	cl := obj(clusterGVK, "ns", "c", map[string]any{"status": map[string]any{"conditions": []any{
		map[string]any{"type": "Available", "status": "True", "reason": "Available"},
	}}})
	kcp := obj(kcpGVK, "ns", "c-cp", map[string]any{"spec": map[string]any{"replicas": int64(1)}, "status": map[string]any{"replicas": int64(1), "readyReplicas": int64(1), "upToDateReplicas": int64(1)}})
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(cl, kcp).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			if o.GetObjectKind().GroupVersionKind().Kind == bindingGVK.Kind {
				return errors.New("binding read with an external CNI")
			}
			return c.Get(ctx, key, o, opts...)
		},
	}).Build()
	m := &Manager{C: c, Out: io.Discard, Poll: time.Millisecond}
	s, err := m.Status(ctx, "ns", "c")
	if err != nil {
		t.Fatal(err)
	}
	if !s.CNIExternal || !s.CNIReady() || s.CNIApplied {
		t.Errorf("summary: %+v", s)
	}
	if want := "Available=True (Available) · control plane 1/1 ready · machines: none · CNI external (not installed by hydra)"; s.Line() != want {
		t.Errorf("got  %q\nwant %q", s.Line(), want)
	}
	if err := m.WaitAvailable(ctx, "ns", "c", 5*time.Second); err != nil {
		t.Errorf("wait: %v", err)
	}
}

func TestCNIConflict(t *testing.T) {
	ctx := context.Background()
	labelled := obj(clusterGVK, "ns", "c", nil)
	labelled.SetLabels(map[string]string{render.CNILabelKey: "c"})
	handBuilt := obj(clusterGVK, "ns", "h", nil)
	m := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(labelled, handBuilt).Build()}
	for _, tc := range []struct {
		name     string
		hydraCNI bool
		conflict bool
	}{
		{"c", true, false},      // hydra installed its CNI: rerunning create is fine
		{"h", true, true},       // CNI from elsewhere: hydra's would land on top
		{"h", false, false},     // cni: external says so
		{"absent", true, false}, // a new cluster
	} {
		st, err := m.State(ctx, "ns", tc.name)
		if err != nil {
			t.Fatal(err)
		}
		msg := st.CNIConflict(tc.hydraCNI)
		if (msg != "") != tc.conflict {
			t.Errorf("%s hydraCNI=%v: got %q", tc.name, tc.hydraCNI, msg)
		}
		if tc.conflict && !strings.Contains(msg, "spec.network.cni: external") {
			t.Errorf("message does not name the fix: %q", msg)
		}
	}
}

func TestCNILabelLeft(t *testing.T) {
	ctx := context.Background()
	labelled := obj(clusterGVK, "ns", "c", nil)
	labelled.SetLabels(map[string]string{render.CNILabelKey: "c"})
	bare := obj(clusterGVK, "ns", "h", nil)
	m := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(labelled, bare).Build()}
	for _, tc := range []struct {
		name     string
		hydraCNI bool
		left     bool
	}{
		{"c", false, true},  // external, label still there
		{"c", true, false},  // cni: hydra wants it
		{"h", false, false}, // external, already gone
		{"absent", false, false},
	} {
		st, err := m.State(ctx, "ns", tc.name)
		if err != nil {
			t.Fatal(err)
		}
		msg := st.CNILabelLeft(tc.hydraCNI)
		if (msg != "") != tc.left {
			t.Errorf("%s hydraCNI=%v: got %q", tc.name, tc.hydraCNI, msg)
		}
		if tc.left && !strings.Contains(msg, "kubectl label cluster -n ns c cni.hydra.io/cilium-") {
			t.Errorf("message does not give the command: %q", msg)
		}
	}
}

// With an external CNI the wait still holds while the control plane's
// Machines are not Ready (their Nodes wait for the CNI).
func TestExternalCNIWaitHoldsUntilMachinesReady(t *testing.T) {
	cl := obj(clusterGVK, "ns", "c", map[string]any{"status": map[string]any{"conditions": []any{
		map[string]any{"type": "Available", "status": "True"},
	}}})
	kcp := obj(kcpGVK, "ns", "c-cp", map[string]any{"spec": map[string]any{"replicas": int64(1)}, "status": map[string]any{"replicas": int64(1), "readyReplicas": int64(0), "upToDateReplicas": int64(1)}})
	m := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(cl, kcp).Build(), Out: io.Discard, Poll: time.Millisecond}
	if err := m.WaitAvailable(context.Background(), "ns", "c", 50*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("want a timeout while machines are not Ready, got %v", err)
	}
}
