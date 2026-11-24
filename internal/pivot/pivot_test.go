package pivot

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Petatron/hydra/internal/render"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const ns, name = "ns", "c"

func u(gvk schema.GroupVersionKind, namespace, n string, fields map[string]any) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{}}
	maps.Copy(o.Object, fields)
	o.SetGroupVersionKind(gvk)
	o.SetNamespace(namespace)
	o.SetName(n)
	return o
}

var (
	kcpGVK = schema.GroupVersionKind{Group: "controlplane.cluster.x-k8s.io", Version: "v1beta2", Kind: "KubeadmControlPlane"}
	crdGVK = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}
	secGVK = schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
	hcpGVK = schema.GroupVersionKind{Group: "addons.cluster.x-k8s.io", Version: caaphAPIVersion, Kind: "HelmChartProxy"}
	hrpGVK = schema.GroupVersionKind{Group: "addons.cluster.x-k8s.io", Version: caaphAPIVersion, Kind: "HelmReleaseProxy"}
)

// cniObjects are cluster name's CNI HelmChartProxy and the HelmReleaseProxy
// CAAPH made from it, Ready.
func cniObjects() (*unstructured.Unstructured, *unstructured.Unstructured) {
	hcp := u(hcpGVK, ns, name+"-cilium", map[string]any{"spec": map[string]any{"version": "1.19.1"}})
	hcp.SetLabels(map[string]string{"hydra.io/cluster": name, "hydra.io/addon": "cilium"})
	hrp := u(hrpGVK, ns, name+"-cilium-abc", map[string]any{
		"spec":   map[string]any{"version": "1.19.1"},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
	})
	hrp.SetLabels(map[string]string{clusterNameLabel: name, "helmreleaseproxy.addons.cluster.x-k8s.io/helmchartproxy-name": name + "-cilium"})
	return hcp, hrp
}

func readyStatus() map[string]any {
	return map[string]any{"conditions": []any{map[string]any{"type": "Available", "status": "True"}}}
}

// clusterObjects is a settled cluster as its management cluster holds it.
// uidSuffix makes UIDs differ per side, as a real move does.
func clusterObjects(uidSuffix string, identityRecorded bool) []client.Object {
	cl := u(clusterGVK, ns, name, map[string]any{"status": readyStatus()})
	cl.SetLabels(map[string]string{render.CNILabelKey: name})
	kcp := u(kcpGVK, ns, name+"-cp", map[string]any{
		"spec":   map[string]any{"replicas": int64(1)},
		"status": map[string]any{"readyReplicas": int64(1), "upToDateReplicas": int64(1), "replicas": int64(1)},
	})
	hm := u(hydraMachineGVK, ns, name+"-cp-1", map[string]any{"status": map[string]any{
		"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
	}})
	hm.SetLabels(map[string]string{clusterNameLabel: name})
	uid := types.UID("hm-" + uidSuffix)
	hm.SetUID(uid)
	if identityRecorded {
		hm.SetAnnotations(map[string]string{IdentityUIDAnnotation: "hm-original", OwnerUIDAnnotation: string(uid)})
	}
	sec := u(secGVK, ns, "c-pki", map[string]any{"data": map[string]any{
		"cacert.pem": "Y2E=", "clientcert.pem": "Y2VydA==", "clientkey.pem": "a2V5",
	}})
	sec.SetLabels(map[string]string{moveLabel: ""})
	hcp, hrp := cniObjects()
	hc := u(hydraClusterGVK, ns, name, map[string]any{"spec": map[string]any{"hypervisor": map[string]any{
		"credentialsSecretRef": map[string]any{"name": "c-pki"},
	}}})
	for _, o := range []*unstructured.Unstructured{kcp, hc} {
		o.SetLabels(map[string]string{"hydra.io/cluster": name}) // as rendered
	}
	return []client.Object{cl, kcp, hm, sec, hcp, hrp, hc}
}

func labelledCRDs() []client.Object {
	out := make([]client.Object, 0, len(hydraCRDs))
	for _, n := range hydraCRDs {
		c := u(crdGVK, "", n, nil)
		c.SetLabels(map[string]string{clusterctlCRD: ""})
		out = append(out, c)
	}
	return out
}

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

type recorder struct {
	moves []string // "dry" / "forward"
	// failFwd fails the move before it touches anything; failAfterCreate
	// after creating everything on the target (a split); failAfterDelete after
	// deleting the source but before unpausing the target.
	failFwd, failAfterCreate, failAfterDelete bool
	// failAfterDeleting stops the move after deleting that many source
	// objects (0: no such failure): a split interrupted while deleting.
	failAfterDeleting int
	// takeOver controls whether moved HydraMachines claim their VM on arrival.
	takeOver bool
	// lose names objects the move never creates on the target.
	lose []string
}

var movedKinds = []schema.GroupVersionKind{clusterGVK, kcpGVK, hydraMachineGVK, secGVK, hcpGVK, hrpGVK, hydraClusterGVK}

func setPaused(ctx context.Context, c client.Client, v bool) error {
	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(clusterGVK)
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, cl); err != nil {
		return err
	}
	_ = unstructured.SetNestedField(cl.Object, v, "spec", "paused")
	return c.Update(ctx, cl)
}

// fakeMove moves the namespace the way clusterctl move does: it refuses a
// paused cluster, pauses the source, creates every object on the target
// (updating any already there) with new UIDs, deletes the source objects with
// the Cluster last, and unpauses the target last of all.
func fakeMove(ctx context.Context, from, to client.Client, rec *recorder) error {
	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(clusterGVK)
	if err := from.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, cl); err != nil {
		return err
	}
	if paused(cl) {
		return errors.New("cannot start the move operation while cluster is paused")
	}
	if err := setPaused(ctx, from, true); err != nil {
		return err
	}
	var all []unstructured.Unstructured
	for _, gvk := range movedKinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := from.List(ctx, list, client.InNamespace(ns)); err != nil {
			return err
		}
		all = append(all, list.Items...)
	}
	for i := range all {
		if slices.Contains(rec.lose, all[i].GetName()) {
			continue
		}
		o := all[i].DeepCopy()
		o.SetResourceVersion("")
		newUID := types.UID(string(o.GetUID()) + "-moved")
		o.SetUID(newUID)
		if err := to.Create(ctx, o); apierrors.IsAlreadyExists(err) {
			existing := &unstructured.Unstructured{}
			existing.SetGroupVersionKind(o.GroupVersionKind())
			if err := to.Get(ctx, client.ObjectKeyFromObject(o), existing); err != nil {
				return err
			}
			o.SetUID(existing.GetUID())
			o.SetResourceVersion(existing.GetResourceVersion())
			if err := to.Update(ctx, o); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	if rec.failAfterCreate {
		return errors.New("move failed after creating the target objects")
	}
	deleted := 0
	for _, o := range slices.Backward(all) { // the Cluster, listed first, goes last
		if rec.failAfterDeleting > 0 && deleted == rec.failAfterDeleting {
			return errors.New("move failed while deleting the source")
		}
		if err := from.Delete(ctx, &o); err != nil {
			return err
		}
		deleted++
	}
	if rec.failAfterDelete {
		return errors.New("move failed before unpausing the target")
	}
	if err := setPaused(ctx, to, false); err != nil {
		return err
	}
	if rec.takeOver {
		takeOverAll(ctx, to)
	}
	return nil
}

// takeOverAll does what the resumed target's provider does: each moved
// HydraMachine claims its VM, recording its own UID as owner. A paused
// provider takes nothing over, so this only follows a resume.
func takeOverAll(ctx context.Context, c client.Client) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(hydraMachineGVK.GroupVersion().WithKind("HydraMachineList"))
	_ = c.List(ctx, list, client.InNamespace(ns))
	for i := range list.Items {
		m := &list.Items[i]
		a := m.GetAnnotations()
		if a == nil {
			a = map[string]string{}
		}
		a[OwnerUIDAnnotation] = string(m.GetUID())
		m.SetAnnotations(a)
		_ = c.Update(ctx, m)
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

var (
	srcKubeconfig = kubeconfigFor("https://127.0.0.1:40000", "ca-kind")
	tgtKubeconfig = kubeconfigFor("https://10.0.0.12:6443", "ca-c")
)

// pivotFor wires a Pivot whose Move hook runs fakeMove between the two fake
// clients.
func pivotFor(t *testing.T, src, tgt client.Client, rec *recorder) *Pivot {
	t.Helper()
	return &Pivot{
		Source:    Side{Name: "bootstrap cluster", C: src, Kubeconfig: srcKubeconfig, File: "kind.kubeconfig", Prep: "hydra bootstrap kubeconfig -f hydra.yaml > kind.kubeconfig"},
		Target:    Side{Name: "cluster c", C: tgt, Kubeconfig: tgtKubeconfig, File: "c.kubeconfig"},
		Namespace: ns,
		Name:      name,
		Out:       &bytes.Buffer{},
		Poll:      time.Millisecond,
		Hooks: Hooks{
			EnsureProviders: func(context.Context, []byte) error { return nil },
			Move: func(ctx context.Context, from, _ []byte, _ string, dryRun bool) error {
				switch {
				case dryRun:
					rec.moves = append(rec.moves, "dry")
					return nil
				case !bytes.Equal(from, srcKubeconfig):
					t.Fatal("pivot must never move from the target")
				}
				rec.moves = append(rec.moves, "forward")
				if rec.failFwd {
					return errors.New("move failed")
				}
				return fakeMove(ctx, src, tgt, rec)
			},
		},
	}
}

func TestPivotMovesAndVerifies(t *testing.T) {
	src := newClient(t, append(clusterObjects("src", true), labelledCRDs()...)...)
	tgt := newClient(t, labelledCRDs()...)
	rec := &recorder{takeOver: true}
	if err := pivotFor(t, src, tgt, rec).Run(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	if strings.Join(rec.moves, ",") != "dry,forward" {
		t.Errorf("moves %v", rec.moves)
	}
}

func TestPivotRefusesBeforeMoving(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		mutate  func(objs []client.Object) []client.Object
		crds    bool
		wantErr string
	}{
		{"machine without identity", func(o []client.Object) []client.Object {
			return append(clusterObjects("src", false), labelledCRDs()...)
		}, true, "has not recorded its identity"},
		{"another cluster in the namespace", func(o []client.Object) []client.Object {
			return append(o, u(clusterGVK, ns, "other", map[string]any{"status": readyStatus()}))
		}, true, "also holds Cluster other"},
		{"credentials Secret not labelled", func(o []client.Object) []client.Object {
			for _, x := range o {
				if x.GetName() == "c-pki" {
					x.SetLabels(nil)
				}
			}
			return o
		}, true, "lacks the clusterctl.cluster.x-k8s.io/move label"},
		{"credentials Secret incomplete", func(o []client.Object) []client.Object {
			for _, x := range o {
				if x.GetName() == "c-pki" {
					unstructured.RemoveNestedField(x.(*unstructured.Unstructured).Object, "data", "clientkey.pem")
				}
			}
			return o
		}, true, "lacks clientkey.pem"},
		{"credentials Secret being deleted", func(o []client.Object) []client.Object {
			for _, x := range o {
				if x.GetName() == "c-pki" {
					now := metav1.Now()
					x.SetDeletionTimestamp(&now)
					x.SetFinalizers([]string{"example.com/hold"})
				}
			}
			return o
		}, true, "is being deleted"},
		{"CRDs not labelled", func(o []client.Object) []client.Object {
			for _, n := range hydraCRDs {
				o = append(o, u(crdGVK, "", n, nil)) // present, no label
			}
			return o
		}, false, "lacks the clusterctl.cluster.x-k8s.io label"},
		{"cluster not settled", func(o []client.Object) []client.Object {
			for _, x := range o {
				if x.GetName() == name+"-cp" {
					_ = unstructured.SetNestedField(x.(*unstructured.Unstructured).Object, int64(0), "status", "readyReplicas")
				}
			}
			return o
		}, true, "not settled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := clusterObjects("src", true)
			if tc.crds {
				objs = append(objs, labelledCRDs()...)
			}
			objs = tc.mutate(objs)
			src := newClient(t, objs...)
			tgt := newClient(t, labelledCRDs()...)
			rec := &recorder{takeOver: true}
			err := pivotFor(t, src, tgt, rec).Run(ctx, time.Second)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
			for _, m := range rec.moves {
				if m != "dry" {
					t.Errorf("nothing may move after a refused preflight, got %v", rec.moves)
				}
			}
		})
	}
}

func TestPivotNeverMovesBack(t *testing.T) {
	src := newClient(t, append(clusterObjects("src", true), labelledCRDs()...)...)
	tgt := newClient(t, labelledCRDs()...)
	rec := &recorder{takeOver: false} // the target never takes the VMs over
	err := pivotFor(t, src, tgt, rec).Run(context.Background(), 30*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "never moves a cluster back") || !strings.Contains(err.Error(), "clusterctl move --kubeconfig") || !strings.Contains(err.Error(), "not verified within") {
		t.Fatalf("want a stays-on-target failure with the manual undo, got %v", err)
	}
	if strings.Join(rec.moves, ",") != "dry,forward" {
		t.Errorf("moves %v", rec.moves)
	}
	if ok, _ := hasCluster(context.Background(), tgt, ns, name); !ok {
		t.Error("the target must still hold the Cluster")
	}
}

func TestPivotResumesAfterTheMove(t *testing.T) {
	// The objects are already on the target (an earlier run moved them).
	tgt := newClient(t, append(clusterObjects("tgt", true), labelledCRDs()...)...)
	src := newClient(t, labelledCRDs()...)
	rec := &recorder{takeOver: true}
	if err := pivotFor(t, src, tgt, rec).Run(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	if len(rec.moves) != 0 {
		t.Errorf("an already-moved cluster must only be verified, got moves %v", rec.moves)
	}
}

func TestPivotResumeOnlyVerifies(t *testing.T) {
	tgt := newClient(t, append(clusterObjects("tgt", false), labelledCRDs()...)...) // never took over
	src := newClient(t, labelledCRDs()...)
	rec := &recorder{}
	err := pivotFor(t, src, tgt, rec).Run(context.Background(), 30*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "Nothing was moved") {
		t.Fatalf("got %v", err)
	}
	if len(rec.moves) != 0 {
		t.Errorf("a resume must never move, got %v", rec.moves)
	}
}

func TestPivotInterruptedMoveLeavesASplit(t *testing.T) {
	ctx := context.Background()
	src := newClient(t, append(clusterObjects("src", true), labelledCRDs()...)...)
	tgt := newClient(t, labelledCRDs()...)
	rec := &recorder{takeOver: true, failAfterCreate: true}
	if err := pivotFor(t, src, tgt, rec).Run(ctx, time.Second); err == nil {
		t.Fatal("the move failed; Run must fail")
	}
	rec = &recorder{}
	err := pivotFor(t, src, tgt, rec).Run(ctx, time.Second)
	if !errors.Is(err, ErrSplit) || !strings.Contains(err.Error(), "(1) hydra bootstrap kubeconfig -f hydra.yaml > kind.kubeconfig") || !strings.Contains(err.Error(), "clusterctl move --kubeconfig kind.kubeconfig --to-kubeconfig c.kubeconfig") || !strings.Contains(err.Error(), `"paused":false`) {
		t.Fatalf("want ErrSplit with the finishing commands, got %v", err)
	}
	if len(rec.moves) != 0 {
		t.Errorf("a split must not move anything, got %v", rec.moves)
	}
	// Following the advice finishes the move: unpause the source, move again.
	if err := setPaused(ctx, src, false); err != nil {
		t.Fatal(err)
	}
	if err := fakeMove(ctx, src, tgt, &recorder{takeOver: true}); err != nil {
		t.Fatalf("the advised clusterctl move must succeed over a split: %v", err)
	}
	if err := pivotFor(t, src, tgt, &recorder{}).Run(ctx, time.Second); err != nil {
		t.Fatalf("after finishing by hand, a rerun verifies: %v", err)
	}
}

func TestPivotReportsAPausedTarget(t *testing.T) {
	ctx := context.Background()
	src := newClient(t, append(clusterObjects("src", true), labelledCRDs()...)...)
	tgt := newClient(t, labelledCRDs()...)
	if err := pivotFor(t, src, tgt, &recorder{takeOver: true, failAfterDelete: true}).Run(ctx, time.Second); err == nil {
		t.Fatal("the move failed; Run must fail")
	}
	err := pivotFor(t, src, tgt, &recorder{}).Run(ctx, time.Second)
	if err == nil || !strings.Contains(err.Error(), "is paused") || !strings.Contains(err.Error(), `"paused":false`) {
		t.Fatalf("got %v", err)
	}
	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(clusterGVK)
	if err := tgt.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, cl); err != nil {
		t.Fatal(err)
	}
	if !paused(cl) {
		t.Error("pivot must never unpause on its own")
	}
	if err := setPaused(ctx, tgt, false); err != nil {
		t.Fatal(err)
	}
	takeOverAll(ctx, tgt) // the resumed provider claims the VMs
	if err := pivotFor(t, src, tgt, &recorder{}).Run(ctx, time.Second); err != nil {
		t.Fatalf("after unpausing as advised, a rerun verifies: %v", err)
	}
}

func TestPivotRefusesAPausedSource(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pause func(*unstructured.Unstructured)
		want  string
	}{
		{"spec", func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, true, "spec", "paused")
		}, `"paused":false`},
		{"annotation", func(u *unstructured.Unstructured) {
			u.SetAnnotations(map[string]string{pausedAnnotation: ""})
		}, "annotate cluster c " + pausedAnnotation + "-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := clusterObjects("src", true)
			tc.pause(objs[0].(*unstructured.Unstructured))
			src := newClient(t, append(objs, labelledCRDs()...)...)
			tgt := newClient(t, labelledCRDs()...)
			rec := &recorder{}
			err := pivotFor(t, src, tgt, rec).Run(context.Background(), time.Second)
			if err == nil || !strings.Contains(err.Error(), "is paused") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if len(rec.moves) != 0 {
				t.Errorf("moves %v", rec.moves)
			}
		})
	}
}

func TestPivotRefusesAClusterWithoutHypervisor(t *testing.T) {
	objs := clusterObjects("src", true)
	for _, o := range objs {
		if o.GetObjectKind().GroupVersionKind() == hydraClusterGVK {
			unstructured.RemoveNestedField(o.(*unstructured.Unstructured).Object, "spec", "hypervisor")
		}
	}
	src := newClient(t, append(objs, labelledCRDs()...)...)
	tgt := newClient(t, labelledCRDs()...)
	err := pivotFor(t, src, tgt, &recorder{}).Run(context.Background(), time.Second)
	if err == nil || !strings.Contains(err.Error(), "names no hypervisor") {
		t.Fatalf("got %v", err)
	}
}

// clusterctl move is namespace-wide: anything there that is not this
// cluster's could be carried along (or claimed on the target), so preflight
// refuses it -- and accepts what is this cluster's by label, owner or name.
func TestPivotRefusesStrangersInTheNamespace(t *testing.T) {
	ccGVK := schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: capiVersion, Kind: "ClusterClass"}
	foreignSecret := u(secGVK, ns, "other-pki", nil)
	foreignSecret.SetLabels(map[string]string{moveLabel: ""})
	otherMachine := u(hydraMachineGVK, ns, "other-1", nil)
	otherMachine.SetLabels(map[string]string{clusterNameLabel: "other"})
	for _, tc := range []struct {
		name  string
		extra client.Object
		want  string
	}{
		{"another cluster's machine", otherMachine, "HydraMachine other-1"},
		{"an unlabelled leftover machine", u(hydraMachineGVK, ns, "leftover", nil), "HydraMachine leftover"},
		{"another ClusterResourceSet", u(crsGVK, ns, "other-cilium", nil), "ClusterResourceSet other-cilium"},
		{"a ClusterClass", u(ccGVK, ns, "cc", nil), "ClusterClass cc"},
		{"a Secret labelled for move", foreignSecret, "Secret other-pki"},
		{"another tool's HelmChartProxy", u(schema.GroupVersionKind{Group: crsGVK.Group, Version: "v1alpha1", Kind: "HelmChartProxy"}, ns, "hand-made", nil), "HelmChartProxy hand-made"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := newClient(t, append(append(clusterObjects("src", true), tc.extra), labelledCRDs()...)...)
			tgt := newClient(t, labelledCRDs()...)
			rec := &recorder{takeOver: true}
			err := pivotFor(t, src, tgt, rec).Run(context.Background(), time.Second)
			if err == nil || !strings.Contains(err.Error(), "not cluster c's") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			for _, m := range rec.moves {
				if m != "dry" {
					t.Errorf("nothing may move, got %v", rec.moves)
				}
			}
		})
	}
}

func TestPivotAcceptsWhatIsOwnedByItsCluster(t *testing.T) {
	owned := u(schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha1", Kind: "HydraMachineTemplate"}, ns, "hand-made", nil)
	owned.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: clusterGVK.GroupVersion().String(), Kind: "Cluster", Name: name, UID: "cl"}})
	src := newClient(t, append(append(clusterObjects("src", true), owned), labelledCRDs()...)...)
	tgt := newClient(t, labelledCRDs()...)
	if err := pivotFor(t, src, tgt, &recorder{takeOver: true}).Run(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestPivotInterruptedVerifyKeepsTheMove(t *testing.T) {
	src := newClient(t, append(clusterObjects("src", true), labelledCRDs()...)...)
	tgt := newClient(t, labelledCRDs()...)
	rec := &recorder{takeOver: false}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := pivotFor(t, src, tgt, rec)
	inner := p.Hooks.Move
	p.Hooks.Move = func(c context.Context, from, to []byte, n string, dry bool) error {
		err := inner(c, from, to, n, dry)
		if !dry {
			cancel()
		}
		return err
	}
	err := p.Run(ctx, time.Second)
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("got %v", err)
	}
	// A Ctrl-C is not a timeout: the message must not claim one.
	if strings.Contains(err.Error(), "not verified within") {
		t.Errorf("an interrupt reported as a timeout: %v", err)
	}
}

// Interrupted while verifying a cluster already on the target: reported as
// an interrupt, not as a cluster that does not verify.
func TestPivotInterruptedResumeSaysInterrupted(t *testing.T) {
	tgt := newClient(t, append(clusterObjects("tgt", false), labelledCRDs()...)...) // never verifies
	src := newClient(t, labelledCRDs()...)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := pivotFor(t, src, tgt, &recorder{}).Run(ctx, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "interrupted while verifying") || strings.Contains(err.Error(), "not verified within") {
		t.Fatalf("got %v", err)
	}
}

func TestPivotForwardMoveFailureIsResumable(t *testing.T) {
	src := newClient(t, append(clusterObjects("src", true), labelledCRDs()...)...)
	tgt := newClient(t, labelledCRDs()...)
	rec := &recorder{failFwd: true}
	err := pivotFor(t, src, tgt, rec).Run(context.Background(), time.Second)
	if err == nil || !strings.Contains(err.Error(), "rerun pivot") {
		t.Fatalf("got %v", err)
	}
}

// A caller that runs the pivot under another command (create --self-hosted)
// names that command in every rerun hint, never pivot.
func TestPivotRerunHintNamesTheCaller(t *testing.T) {
	src := newClient(t, append(clusterObjects("src", true), labelledCRDs()...)...)
	tgt := newClient(t, labelledCRDs()...)
	p := pivotFor(t, src, tgt, &recorder{failFwd: true})
	p.Rerun = "`hydra cluster create --self-hosted`"
	err := p.Run(context.Background(), time.Second)
	if err == nil || !strings.Contains(err.Error(), "rerun `hydra cluster create --self-hosted`") || strings.Contains(err.Error(), "rerun pivot") {
		t.Fatalf("got %v", err)
	}
}

// Both kubeconfigs reaching one cluster (pivot run again with the cluster's
// own kubeconfig as the source) must not look like a split, and must never
// print a clusterctl move: on one cluster that deletes the only copy.
func TestPivotSameClusterIsAlreadySelfHosted(t *testing.T) {
	c := newClient(t, append(clusterObjects("tgt", true), labelledCRDs()...)...)
	rec := &recorder{}
	p := pivotFor(t, c, c, rec)
	p.Source.Kubeconfig = kubeconfigFor("https://other-name:6443", "ca-c") // same CA, other server name
	err := p.Run(context.Background(), time.Second)
	if !errors.Is(err, ErrAlreadySelfHosted) || strings.Contains(err.Error(), "clusterctl move") {
		t.Fatalf("got %v", err)
	}
	if len(rec.moves) != 0 {
		t.Errorf("moves %v", rec.moves)
	}
}

func TestPivotRefusesWithoutAReadableCA(t *testing.T) {
	src := newClient(t, append(clusterObjects("src", true), labelledCRDs()...)...)
	tgt := newClient(t, labelledCRDs()...)
	p := pivotFor(t, src, tgt, &recorder{})
	p.Source.Kubeconfig = []byte("not a kubeconfig")
	if err := p.Run(context.Background(), time.Second); err == nil || !strings.Contains(err.Error(), "certificate authority") {
		t.Fatalf("got %v", err)
	}
}

// afterMove runs mutate on the target once the forward move has finished.
func afterMove(p *Pivot, mutate func(ctx context.Context) error) {
	inner := p.Hooks.Move
	p.Hooks.Move = func(ctx context.Context, from, to []byte, n string, dry bool) error {
		if err := inner(ctx, from, to, n, dry); err != nil || dry {
			return err
		}
		return mutate(ctx)
	}
}

// A target being deleted, or with the CNI binding not applied, is no
// management path: verify must not pass (the bootstrap cluster is deleted
// once it does).
func TestPivotVerifyNeedsALiveTargetWithCNI(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(ctx context.Context, tgt client.Client) error
		want   string
	}{
		{"CNI not installed on the target", func(ctx context.Context, tgt client.Client) error {
			// CAAPH on the target replaced the moved HelmReleaseProxy and has
			// not installed it yet.
			h := &unstructured.Unstructured{}
			h.SetGroupVersionKind(hrpGVK)
			if err := tgt.Get(ctx, client.ObjectKey{Namespace: ns, Name: name + "-cilium-abc"}, h); err != nil {
				return err
			}
			_ = unstructured.SetNestedSlice(h.Object, []any{map[string]any{"type": "Ready", "status": "False", "reason": "HelmReleasePending"}}, "status", "conditions")
			return tgt.Update(ctx, h)
		}, "CNI installing"},
		{"target Cluster being deleted", func(ctx context.Context, tgt client.Client) error {
			cl := &unstructured.Unstructured{}
			cl.SetGroupVersionKind(clusterGVK)
			if err := tgt.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, cl); err != nil {
				return err
			}
			cl.SetFinalizers([]string{"cluster.cluster.x-k8s.io"})
			if err := tgt.Update(ctx, cl); err != nil {
				return err
			}
			return tgt.Delete(ctx, cl)
		}, "did not verify"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := newClient(t, append(clusterObjects("src", true), labelledCRDs()...)...)
			tgt := newClient(t, labelledCRDs()...)
			p := pivotFor(t, src, tgt, &recorder{takeOver: true})
			afterMove(p, func(ctx context.Context) error { return tc.mutate(ctx, tgt) })
			err := p.Run(context.Background(), 30*time.Millisecond)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

// Ctrl-C landing while a verification read is in flight: that read fails
// with the context's error, and the message must still carry the last state
// actually observed, not "context canceled".
func TestPivotInterruptReportsLastObservedState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gets := 0
	inner := newClient(t, append(clusterObjects("tgt", false), labelledCRDs()...)...) // never verifies
	tgt := interceptor.NewClient(inner.(client.WithWatch), interceptor.Funcs{
		Get: func(c context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			gets++
			if gets == 12 { // a later round: interrupt with this read in flight
				cancel()
				return c.Err()
			}
			return cl.Get(c, key, obj, opts...)
		},
	})
	src := newClient(t, labelledCRDs()...)
	err := pivotFor(t, src, tgt, &recorder{}).Run(ctx, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "has not taken its VM over") || strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("want the last observed state, got %v", err)
	}
}

// splitByInterruptedPivot leaves the realistic split an interrupted pivot
// leaves: the source paused and complete, a partial paused copy on the target.
func splitByInterruptedPivot(t *testing.T) (src, tgt client.Client) {
	t.Helper()
	src = newClient(t, append(clusterObjects("src", true), labelledCRDs()...)...)
	tgt = newClient(t, labelledCRDs()...)
	if err := pivotFor(t, src, tgt, &recorder{failAfterCreate: true}).Run(context.Background(), time.Second); err == nil {
		t.Fatal("the interrupted move must fail")
	}
	return src, tgt
}

func TestFinishSplitCompletesAPivotSplit(t *testing.T) {
	src, tgt := splitByInterruptedPivot(t)
	p := pivotFor(t, src, tgt, &recorder{takeOver: true})
	p.FinishSplit = true
	if err := p.Run(context.Background(), time.Second); err != nil {
		t.Fatalf("finishing the split must converge: %v", err)
	}
	if ok, _ := hasCluster(context.Background(), src, ns, name); ok {
		t.Error("the source must no longer hold the Cluster")
	}
}

// The review's critical case: a split from a move one way, finished by the
// command for the other way, would unpause the partial copy. Refuse, and
// touch nothing.
func TestFinishSplitRefusesTheWrongDirection(t *testing.T) {
	src, tgt := splitByInterruptedPivot(t)
	rec := &recorder{}
	p := pivotFor(t, src, tgt, rec)
	p.Source, p.Target = p.Target, p.Source // the opposite command
	p.Source.Kubeconfig, p.Target.Kubeconfig = tgtKubeconfig, srcKubeconfig
	p.FinishSplit = true
	err := p.Run(context.Background(), time.Second)
	if err == nil || !strings.Contains(err.Error(), "a move the other way") {
		t.Fatalf("got %v", err)
	}
	if len(rec.moves) != 0 {
		t.Errorf("nothing may move, got %v", rec.moves)
	}
	for side, c := range map[string]client.Client{"source": src, "target": tgt} {
		cl := &unstructured.Unstructured{}
		cl.SetGroupVersionKind(clusterGVK)
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, cl); err != nil {
			t.Fatal(err)
		}
		if !paused(cl) {
			t.Errorf("the %s copy must stay paused", side)
		}
	}
}

func TestSplitSourceRefusesAmbiguousEvidence(t *testing.T) {
	// Neither side's machines hold their VM: no way to tell.
	a := newClient(t, clusterObjects("a", false)...)
	b := newClient(t, clusterObjects("b", false)...)
	_, _, err := SplitSource(context.Background(), Side{Name: "a", C: a}, Side{Name: "b", C: b}, ns)
	if err == nil || !strings.Contains(err.Error(), "cannot tell which copy") {
		t.Fatalf("got %v", err)
	}
	// Both sides owning: also refused.
	a = newClient(t, clusterObjects("a", true)...)
	b = newClient(t, clusterObjects("b", true)...)
	if _, _, err := SplitSource(context.Background(), Side{Name: "a", C: a}, Side{Name: "b", C: b}, ns); err == nil {
		t.Fatal("both sides owning must be refused")
	}
}

// Delete order in fakeMove is the reverse of movedKinds' listing: after 5
// deletions the source keeps only its Cluster and control plane, no
// HydraMachines, while the target is complete and still paused.
const deletionsLeavingClusterAndKCP = 5

func TestPivotFinishesASplitInterruptedWhileDeleting(t *testing.T) {
	ctx := context.Background()
	src := newClient(t, append(clusterObjects("src", true), labelledCRDs()...)...)
	tgt := newClient(t, labelledCRDs()...)
	if err := pivotFor(t, src, tgt, &recorder{failAfterDeleting: deletionsLeavingClusterAndKCP}).Run(ctx, time.Second); err == nil {
		t.Fatal("the interrupted move must fail")
	}
	// Without --finish-split: the advice must never be to resume the source.
	err := pivotFor(t, src, tgt, &recorder{}).Run(ctx, time.Second)
	if !errors.Is(err, ErrSplit) || !strings.Contains(err.Error(), "Never unpause the source") || strings.Contains(err.Error(), "clusterctl move --kubeconfig") {
		t.Fatalf("got %v", err)
	}
	rec := &recorder{}
	p := pivotFor(t, src, tgt, rec)
	p.FinishSplit = true
	_ = p.Run(ctx, 30*time.Millisecond) // verify waits for a takeover the fake does not do
	if len(rec.moves) != 0 {
		t.Errorf("a source interrupted while deleting must never be moved again (that resumes it): %v", rec.moves)
	}
	if ok, _ := hasCluster(ctx, src, ns, name); ok {
		t.Error("the source's leftover Cluster must be removed")
	}
	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(clusterGVK)
	if err := tgt.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, cl); err != nil {
		t.Fatal(err)
	}
	if paused(cl) {
		t.Error("the complete copy must be resumed")
	}
	takeOverAll(ctx, tgt)
	if err := pivotFor(t, src, tgt, &recorder{}).Run(ctx, time.Second); err != nil {
		t.Fatalf("once taken over, a rerun verifies: %v", err)
	}
}
