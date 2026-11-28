package cluster

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Petatron/hydra/internal/render"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var cmGVK = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}

func conflictErr() error {
	return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "x", errors.New("field owned by hydra-d"))
}

func noMatch(kind string) error {
	return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Kind: kind}}
}

func TestConnect(t *testing.T) {
	// Building the client must not dial the server.
	kc := []byte(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:1", insecure-skip-tls-verify: true}}]
contexts: [{name: c, context: {cluster: c, user: u}}]
current-context: c
users: [{name: u, user: {token: x}}]
`)
	c, err := Connect(kc)
	if err != nil || c == nil {
		t.Fatalf("got %v %v", c, err)
	}
	if !c.Scheme().Recognizes(corev1.SchemeGroupVersion.WithKind("Secret")) {
		t.Error("core types must be registered")
	}
	if _, err := Connect([]byte("not: [a kubeconfig")); err == nil {
		t.Error("an unparseable kubeconfig must be an error")
	}
}

func TestCheckManagementCluster(t *testing.T) {
	plain := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).Build()}
	if err := plain.CheckManagementCluster(); err == nil || !strings.Contains(err.Error(), "not a Cluster API management cluster") {
		t.Errorf("a cluster without CAPI must be refused: %v", err)
	}
	rm := meta.NewDefaultRESTMapper(nil)
	rm.Add(clusterGVK, meta.RESTScopeNamespace)
	capi := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).WithRESTMapper(rm).Build()}
	if err := capi.CheckManagementCluster(); err != nil {
		t.Errorf("a cluster serving %s must pass: %v", clusterGVK.GroupVersion(), err)
	}
}

func TestClusterStateCheckOwner(t *testing.T) {
	ctx := context.Background()
	if err := (ClusterState{}).CheckOwner("c", false); err != nil {
		t.Errorf("an absent Cluster has no owner to check: %v", err)
	}
	m := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(labelled(clusterGVK, "c", "d")).Build()}
	st, err := m.State(ctx, "ns", "c")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CheckOwner("c", true); err == nil || !strings.Contains(err.Error(), `belongs to cluster "d"`) {
		t.Errorf("another cluster's Cluster must be refused even with adopt: %v", err)
	}
	if err := st.CheckOwner("d", false); err != nil {
		t.Errorf("its own cluster: %v", err)
	}
}

func TestStateAbsentAndErrors(t *testing.T) {
	ctx := context.Background()
	m := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).Build()}
	if st, err := m.State(ctx, "ns", "c"); err != nil || st.Exists {
		t.Errorf("absent: %+v %v", st, err)
	}
	broken := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("forbidden")
		},
	}).Build()}
	if _, err := broken.State(ctx, "ns", "c"); err == nil {
		t.Error("an unreadable Cluster must be an error, never absent")
	}
}

func TestKubeconfigCAAndSameCA(t *testing.T) {
	a := kubeconfigFor("https://10.0.0.1:6443", "ca-a")
	a2 := kubeconfigFor("https://127.0.0.1:16443", "ca-a")
	b := kubeconfigFor("https://10.0.0.2:6443", "ca-b")
	if got := string(KubeconfigCA(a)); got != "ca-a" {
		t.Errorf("KubeconfigCA = %q", got)
	}
	if KubeconfigCA([]byte("garbage")) != nil {
		t.Error("an unparseable kubeconfig has no CA")
	}
	if same, err := SameCA(a, a2); err != nil || !same {
		t.Errorf("same CA by another address: %v %v", same, err)
	}
	if same, err := SameCA(a, b); err != nil || same {
		t.Errorf("different CAs: %v %v", same, err)
	}
	if _, err := SameCA(a, []byte("garbage")); err == nil {
		t.Error("a kubeconfig without a CA must be an error, never \"different\"")
	}
}

func TestKubeconfigIdentityMissingEntries(t *testing.T) {
	noContext := []byte("apiVersion: v1\nkind: Config\ncurrent-context: gone\n")
	if s, ca := kubeconfigIdentity(noContext); s != "" || ca != nil {
		t.Errorf("missing context: %q %v", s, ca)
	}
	noCluster := []byte(`apiVersion: v1
kind: Config
contexts: [{name: c, context: {cluster: gone, user: u}}]
current-context: c
`)
	if s, ca := kubeconfigIdentity(noCluster); s != "" || ca != nil {
		t.Errorf("missing cluster: %q %v", s, ca)
	}
}

func TestSameHostPort(t *testing.T) {
	for _, tc := range []struct {
		server, endpoint string
		want             bool
	}{
		{"https://10.0.0.5:6443", "10.0.0.5:6443", true},
		{"https://api.example", "api.example:443", true},
		{"https://api.example", "api.example:6443", false},
		{"https://[fd00::1]:6443", "[fd00::1]:6443", true},
		{"://bad", "bad:443", false},
	} {
		if got := sameHostPort(tc.server, tc.endpoint); got != tc.want {
			t.Errorf("sameHostPort(%q, %q) = %v", tc.server, tc.endpoint, got)
		}
	}
}

// applyRecorder records each server-side apply: kind/name, field owner, force.
type applyRecorder struct {
	calls []string
	err   func(kind string) error
}

func (r *applyRecorder) apply(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
	o := &client.ApplyOptions{}
	o.ApplyOptions(opts)
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return err
	}
	kind, _ := u["kind"].(string)
	name, _, _ := unstructured.NestedString(u, "metadata", "name")
	force := o.Force != nil && *o.Force
	r.calls = append(r.calls, kind+"/"+name+" "+o.FieldManager+map[bool]string{true: " force", false: ""}[force])
	if r.err != nil {
		return r.err(kind)
	}
	return nil
}

func TestApply(t *testing.T) {
	ctx := context.Background()
	objs := []*unstructured.Unstructured{
		labelled(hydraClusterGVK, "c", "c"),
		labelled(cmGVK, "c-cilium", "c"),
		{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "other"}}},
	}
	for _, force := range []bool{false, true} {
		r := &applyRecorder{}
		var out bytes.Buffer
		m := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{Apply: r.apply}).Build(), Out: &out}
		if err := m.Apply(ctx, objs, "c", force); err != nil {
			t.Fatal(err)
		}
		f := map[bool]string{true: " force", false: ""}[force]
		want := "Namespace/ns hydra-c," + // once, for both namespaced objects
			"HydraCluster/c hydra-c" + f + ",ConfigMap/c-cilium hydra-c" + f + ",Namespace/other hydra-c" + f
		if got := strings.Join(r.calls, ","); got != want {
			t.Errorf("force=%v:\n got  %s\n want %s", force, got, want)
		}
		if !strings.Contains(out.String(), "applied 3 objects") {
			t.Errorf("output: %q", out.String())
		}
	}
}

func TestApplyErrors(t *testing.T) {
	ctx := context.Background()
	objs := []*unstructured.Unstructured{labelled(cmGVK, "c-cilium", "c"), labelled(cmGVK, "c-two", "c")}
	for _, tc := range []struct {
		name    string
		err     func(kind string) error
		wantErr string
		applies int
	}{
		{"namespace refused", func(string) error { return errors.New("forbidden") }, "namespace ns", 1},
		{"conflict names --force-conflicts", func(k string) error {
			if k == "ConfigMap" {
				return conflictErr()
			}
			return nil
		}, "--force-conflicts", 2},
		{"other error", func(k string) error {
			if k == "ConfigMap" {
				return errors.New("timeout")
			}
			return nil
		}, "applying ConfigMap ns/c-cilium: timeout", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &applyRecorder{err: tc.err}
			m := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{Apply: r.apply}).Build(), Out: &bytes.Buffer{}}
			err := m.Apply(ctx, objs, "c", false)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
			if len(r.calls) != tc.applies {
				t.Errorf("must stop at the first failure: %v", r.calls)
			}
		})
	}
}

func TestKubeconfig(t *testing.T) {
	ctx := context.Background()
	good := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "c-kubeconfig"}, Data: map[string][]byte{"value": []byte("kc")}}
	empty := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "e-kubeconfig"}}
	m := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(good, empty).Build()}
	if kc, err := m.Kubeconfig(ctx, "ns", "c"); err != nil || string(kc) != "kc" {
		t.Errorf("got %q %v", kc, err)
	}
	if _, err := m.Kubeconfig(ctx, "ns", "e"); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Errorf("empty: %v", err)
	}
	if _, err := m.Kubeconfig(ctx, "ns", "absent"); err == nil || !strings.Contains(err.Error(), "ns/absent-kubeconfig") {
		t.Errorf("absent: %v", err)
	}
}

func TestLabelled(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		labelled(hydraClusterGVK, "c", "c"), labelled(cmGVK, "c-cilium", "c"),
		labelled(cmGVK, "d-cilium", "d"), labelled(cmGVK, "handmade", ""),
	).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if u, ok := list.(*unstructured.UnstructuredList); ok && u.GroupVersionKind().Group == lcAddonsGroup {
				return noMatch("ClusterResourceSet") // addons not installed: none
			}
			return cl.List(ctx, list, opts...)
		},
	}).Build()
	got, err := (&Manager{C: c}).Labelled(ctx, "ns", "c")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "HydraCluster ns/c,ConfigMap ns/c-cilium" {
		t.Errorf("got %v", got)
	}

	broken := fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("forbidden")
		},
	}).Build()
	if _, err := (&Manager{C: broken}).Labelled(ctx, "ns", "c"); err == nil || !strings.Contains(err.Error(), "listing HydraCluster") {
		t.Errorf("a refused list must be an error, never \"nothing\": %v", err)
	}
}

func TestPlanConflictsAndErrors(t *testing.T) {
	ctx := context.Background()
	live := labelled(cmGVK, "c-cilium", "c")
	var forced []bool
	patchErr := conflictErr()
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(live).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, opts ...client.PatchOption) error {
			po := &client.PatchOptions{}
			po.ApplyOptions(opts)
			forced = append(forced, po.Force != nil && *po.Force)
			if len(po.DryRun) != 1 || po.DryRun[0] != metav1.DryRunAll {
				t.Error("plan must only dry-run")
			}
			return patchErr
		},
	}).Build()
	m := &Manager{C: c}

	changes, err := m.Plan(ctx, []*unstructured.Unstructured{live.DeepCopy()}, "c", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Action != "conflict" || !strings.Contains(changes[0].Diff, "hydra-d") {
		t.Errorf("a conflict must be reported as a change: %+v", changes)
	}

	patchErr = errors.New("admission webhook denied")
	if _, err := m.Plan(ctx, []*unstructured.Unstructured{live.DeepCopy()}, "c", true); err == nil || !strings.Contains(err.Error(), "dry-run applying") {
		t.Errorf("a failed dry-run must be an error: %v", err)
	}
	if len(forced) != 2 || forced[0] || !forced[1] {
		t.Errorf("force must follow --force-conflicts: %v", forced)
	}

	unreadable := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("forbidden")
		},
	}).Build()}
	if _, err := unreadable.Plan(ctx, []*unstructured.Unstructured{live.DeepCopy()}, "c", false); err == nil || !strings.Contains(err.Error(), "reading ConfigMap") {
		t.Errorf("an unreadable live object must be an error, never \"create\": %v", err)
	}
}

func TestSummaryLineStates(t *testing.T) {
	for _, tc := range []struct {
		s    Summary
		want string
	}{
		{Summary{Exists: true, Terminating: true}, "Cluster is being deleted"},
		{Summary{Exists: true, Terminating: true, DeletingMessage: "waiting for\n  2 machines"}, "Cluster is being deleted: waiting for 2 machines"},
		{Summary{Exists: true, Available: "True", CNIApplied: true, CNIRolledOut: true}, "Available=True · rolling out · control plane - · machines: none · CNI ready"},
		{Summary{Exists: true, CNIApplied: true, CNIWhy: "1/3"}, "Available=- · control plane - · machines: none · CNI rolling out (1/3)"},
		{Summary{Exists: true, CNIApplied: true, CNIRolloutUnreadable: true, CNIWhy: "connecting to the workload cluster: refused"}, "Available=- · control plane - · machines: none · CNI ready (rollout unverified: connecting to the workload cluster: refused)"},
		{Summary{Exists: true, CNIApplied: true, CNIRolloutUnreadable: true, CNIRolloutRequired: true, CNIWhy: "connecting to the workload cluster: refused"}, "Available=- · control plane - · machines: none · CNI rollout unknown: connecting to the workload cluster: refused"},
		{Summary{Exists: true, CNIProxyPresent: true, CNIWhy: "moving to 1.19.11"}, "Available=- · control plane - · machines: none · CNI installing (moving to 1.19.11)"},
		{Summary{Exists: true, RolledOut: true}, "Available=- · control plane - · machines: none · CNI waiting for the API"},
	} {
		if got := tc.s.Line(); got != tc.want {
			t.Errorf("got  %q\nwant %q", got, tc.want)
		}
	}
}

func TestStatusTerminatingAndMissingControlPlane(t *testing.T) {
	cl := obj(clusterGVK, "ns", "c", map[string]any{"status": map[string]any{"conditions": []any{
		map[string]any{"type": "Deleting", "status": "True", "message": "waiting for machines"},
	}}})
	now := metav1.Now()
	cl.SetDeletionTimestamp(&now)
	cl.SetFinalizers([]string{"cluster.cluster.x-k8s.io"})
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(cl).Build()
	s, err := (&Manager{C: c}).Status(context.Background(), "ns", "c")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Terminating || s.DeletingMessage != "waiting for machines" {
		t.Errorf("terminating not reported: %+v", s)
	}
	if s.RolledOut {
		t.Error("no control plane yet is not rolled out")
	}
	if s.CNIProxyPresent {
		t.Error("no CNI release exists")
	}
}

func TestStatusReadErrors(t *testing.T) {
	cl := obj(clusterGVK, "ns", "c", nil)
	cl.SetLabels(map[string]string{render.CNILabelKey: "c"})
	kcp := obj(kcpGVK, "ns", "c-cp", nil)
	failOn := func(getKind, listKind string) client.Client {
		return fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(cl, kcp).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
				if o.GetObjectKind().GroupVersionKind().Kind == getKind {
					return errors.New("forbidden")
				}
				return c.Get(ctx, key, o, opts...)
			},
			List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
				if u, ok := l.(*unstructured.UnstructuredList); ok && u.GroupVersionKind().Kind == listKind {
					return errors.New("forbidden")
				}
				return c.List(ctx, l, opts...)
			},
		}).Build()
	}
	for _, tc := range []struct{ getKind, listKind, want string }{
		{"Cluster", "", "forbidden"},
		{"KubeadmControlPlane", "", "reading KubeadmControlPlane"},
		{"", "MachineList", "listing Machines"},
		{"HelmChartProxy", "", "reading the CNI HelmChartProxy"},
	} {
		_, err := (&Manager{C: failOn(tc.getKind, tc.listKind)}).Status(context.Background(), "ns", "c")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s%s: want %q, got %v", tc.getKind, tc.listKind, tc.want, err)
		}
	}
}

func TestWaitShowsReadErrorsAndStopsOnInterrupt(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("token expired")
		},
	}).Build()
	var out bytes.Buffer
	m := &Manager{C: c, Out: &out, Poll: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.WaitAvailable(ctx, "ns", "c", time.Hour); !errors.Is(err, ErrInterrupted) {
		t.Errorf("want ErrInterrupted, got %v", err)
	}
	if !strings.Contains(out.String(), "cannot read status: token expired") {
		t.Errorf("a read error must be shown, not look like slow progress: %q", out.String())
	}
}

func TestPollDefault(t *testing.T) {
	if got := (&Manager{}).poll(); got != 10*time.Second {
		t.Errorf("default poll %v", got)
	}
	if got := (&Manager{Poll: time.Second}).poll(); got != time.Second {
		t.Errorf("poll override %v", got)
	}
}

func TestDeleteClusterReadError(t *testing.T) {
	var deletes int
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("forbidden")
		},
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			deletes++
			return nil
		},
	}).Build()
	err := (&Manager{C: c, Out: &bytes.Buffer{}}).Delete(context.Background(), "ns", "c", time.Second, false)
	if err == nil || !strings.Contains(err.Error(), "reading Cluster ns/c") {
		t.Errorf("got %v", err)
	}
	if deletes != 0 {
		t.Error("nothing may be deleted when the Cluster cannot be read")
	}
}

func TestDeleteLabelledSkipsAndFails(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()
	going := labelled(cmGVK, "c-going", "c")
	going.SetDeletionTimestamp(&now)
	going.SetFinalizers([]string{"x"})
	changed := labelled(cmGVK, "c-changed", "c")
	plain := labelled(cmGVK, "c-plain", "c")
	var deleted []string
	deleteErr := map[string]error{"c-changed": conflictErr()}
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(going, changed, plain).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(_ context.Context, _ client.WithWatch, o client.Object, _ ...client.DeleteOption) error {
			deleted = append(deleted, o.GetName())
			return deleteErr[o.GetName()]
		},
	}).Build()
	var out bytes.Buffer
	m := &Manager{C: c, Out: &out}
	if err := m.deleteLabelled(ctx, "ns", "c", []schema.GroupVersionKind{cmGVK}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(deleted, ",") != "c-changed,c-plain" {
		t.Errorf("an object already being deleted must not be deleted again: %v", deleted)
	}
	if !strings.Contains(out.String(), "skipped ConfigMap ns/c-changed: it changed") || !strings.Contains(out.String(), "deleted ConfigMap ns/c-plain") {
		t.Errorf("output:\n%s", out.String())
	}

	deleted = nil
	deleteErr = map[string]error{"c-changed": errors.New("forbidden")}
	err := m.deleteLabelled(ctx, "ns", "c", []schema.GroupVersionKind{cmGVK})
	if err == nil || !strings.Contains(err.Error(), "deleting ConfigMap ns/c-changed: forbidden") {
		t.Errorf("want the delete error, got %v", err)
	}
	if len(deleted) != 1 {
		t.Errorf("must stop at the first failure: %v", deleted)
	}

	broken := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("forbidden")
		},
	}).Build()}
	if err := broken.deleteLabelled(ctx, "ns", "c", []schema.GroupVersionKind{cmGVK}); err == nil {
		t.Error("a refused list must be an error")
	}
}

func TestDeleteExactlyAlreadyGone(t *testing.T) {
	m := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).Build()}
	if err := m.deleteExactly(context.Background(), labelled(cmGVK, "gone", "c")); err != nil {
		t.Errorf("an object already gone is not an error: %v", err)
	}
}

func TestWaitLabelledGone(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()
	stuck := labelled(hydraClusterGVK, "c", "c")
	stuck.SetDeletionTimestamp(&now)
	stuck.SetFinalizers([]string{"hydracluster.infrastructure.cluster.x-k8s.io"})
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(stuck).Build()
	var out bytes.Buffer
	m := &Manager{C: c, Out: &out, Poll: 5 * time.Millisecond}
	kinds := []schema.GroupVersionKind{hydraClusterGVK}

	if err := m.waitLabelledGone(ctx, "ns", "c", kinds, 30*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Errorf("want ErrTimeout, got %v", err)
	}
	if strings.Count(out.String(), "waiting for HydraCluster ns/c to be removed") != 1 {
		t.Errorf("the waiting line must print once per change:\n%s", out.String())
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := m.waitLabelledGone(cctx, "ns", "c", kinds, time.Hour); !errors.Is(err, ErrInterrupted) {
		t.Errorf("want ErrInterrupted, got %v", err)
	}

	if err := m.waitLabelledGone(ctx, "ns", "other", kinds, time.Hour); err != nil {
		t.Errorf("nothing labelled for the cluster: %v", err)
	}

	broken := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("forbidden")
		},
	}).Build()}
	if err := broken.waitLabelledGone(ctx, "ns", "c", kinds, time.Hour); err == nil || !strings.Contains(err.Error(), "listing HydraCluster") {
		t.Errorf("a refused list must end the wait with an error, never success: %v", err)
	}
}

func TestOrphansListError(t *testing.T) {
	broken := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("forbidden")
		},
	}).Build()}
	if _, err := broken.Orphans(context.Background(), nil, "ns", "c"); err == nil {
		t.Error("a refused list must be an error")
	}
}

func TestCheckOwnershipReadError(t *testing.T) {
	broken := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("forbidden")
		},
	}).Build()}
	err := broken.CheckOwnership(context.Background(), []*unstructured.Unstructured{labelled(cmGVK, "x", "c")}, "c", false)
	if err == nil {
		t.Error("an unreadable object must be an error, never \"ours\"")
	}
}

// Addons need CAAPH on the management cluster; without addons nothing is
// asked of it.
func TestCheckAddonProvider(t *testing.T) {
	hcp := obj(hcpGVK, "ns", "c-metrics-server", nil)
	other := obj(clusterGVK, "ns", "c", nil)
	plain := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).Build()}
	if err := plain.CheckAddonProvider([]*unstructured.Unstructured{other}); err != nil {
		t.Errorf("no addons, no requirement: %v", err)
	}
	if err := plain.CheckAddonProvider([]*unstructured.Unstructured{other, hcp}); err == nil || !strings.Contains(err.Error(), "clusterctl init --addon helm:"+AddonProviderVersion) {
		t.Errorf("addons without CAAPH must be refused with the fix: %v", err)
	}
	rm := meta.NewDefaultRESTMapper(nil)
	rm.Add(hcpGVK, meta.RESTScopeNamespace)
	caaph := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).WithRESTMapper(rm).Build()}
	if err := caaph.CheckAddonProvider([]*unstructured.Unstructured{hcp}); err != nil {
		t.Errorf("CAAPH installed: %v", err)
	}
}

// Only a missing kind means CAAPH is absent; any other discovery error is
// reported as itself.
func TestCheckAddonProviderPassesOtherErrorsThrough(t *testing.T) {
	hcp := obj(hcpGVK, "ns", "c-metrics-server", nil)
	m := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).WithRESTMapper(brokenMapper{}).Build()}
	err := m.CheckAddonProvider([]*unstructured.Unstructured{hcp})
	if err == nil || strings.Contains(err.Error(), "clusterctl init") || !strings.Contains(err.Error(), "discovery down") {
		t.Errorf("got %v", err)
	}
}

type brokenMapper struct{ meta.RESTMapper }

func (brokenMapper) RESTMapping(schema.GroupKind, ...string) (*meta.RESTMapping, error) {
	return nil, errors.New("discovery down")
}
