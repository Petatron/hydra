package pivot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A read failure on either side is an error naming that side, never a guess.
func TestClassifySplitReadErrorsNameTheSide(t *testing.T) {
	ctx := context.Background()
	good := newClient(t, clusterObjects("g", true)...)
	bad := faulty(t, newClient(t, clusterObjects("b", false)...), &fault{verb: "list", kind: lcHydraMachine, err: errBoom})
	for _, tc := range []struct {
		name string
		a, b Side
		want string
	}{
		{"first side", Side{Name: "kind", C: bad}, Side{Name: "self", C: good}, "on the kind"},
		{"second side", Side{Name: "kind", C: good}, Side{Name: "self", C: bad}, "on the self"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ClassifySplit(ctx, tc.a, tc.b, ns)
			if !errors.Is(err, errBoom) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q wrapping the read error, got %v", tc.want, err)
			}
		})
	}
}

// Interrupted while deleting, with the second side the source: the first
// side is complete and holds none, the second has fewer, all holding theirs.
func TestClassifySplitDeletingFromTheSecondSide(t *testing.T) {
	ctx := context.Background()
	complete := newClient(t, append(clusterObjects("a", false), u(hydraMachineGVK, ns, name+"-w-2", nil))...)
	depleted := newClient(t, clusterObjects("b", true)...)
	a, b := Side{Name: "a", C: complete}, Side{Name: "b", C: depleted}
	plan, err := ClassifySplit(ctx, a, b, ns)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Source.Name != "b" || plan.Target.Name != "a" || !plan.Deleting {
		t.Fatalf("want source b, target a, deleting; got %+v", plan)
	}
	// SplitSource refuses to name a source that must never be resumed.
	if _, _, err := SplitSource(ctx, a, b, ns); err == nil || !strings.Contains(err.Error(), "the move from the b was interrupted while deleting") {
		t.Fatalf("got %v", err)
	}
}

// A split whose evidence is unclear is reported with the evidence, and
// --finish-split refuses it without touching either side.
func TestPivotRunRefusesAnUnclearSplit(t *testing.T) {
	ctx := context.Background()
	for _, finish := range []bool{false, true} {
		src := newClient(t, append(clusterObjects("src", false), labelledCRDs()...)...)
		tgt := newClient(t, append(clusterObjects("tgt", false), labelledCRDs()...)...)
		rec := &recorder{}
		p := pivotFor(t, src, tgt, rec)
		p.FinishSplit = finish
		err := p.Run(ctx, time.Second)
		if err == nil || !strings.Contains(err.Error(), "cannot tell which copy") {
			t.Fatalf("finish=%v: got %v", finish, err)
		}
		if errors.Is(err, ErrSplit) == finish {
			t.Errorf("finish=%v: ErrSplit only when reporting, got %v", finish, err)
		}
		if len(rec.moves) != 0 {
			t.Errorf("finish=%v: nothing may move, got %v", finish, rec.moves)
		}
		for _, c := range []client.Client{src, tgt} {
			if ok, _ := hasCluster(ctx, c, ns, name); !ok {
				t.Errorf("finish=%v: neither copy may be removed", finish)
			}
		}
	}
}

// deletingSplit is a split interrupted while deleting, as a plan, with the
// source holding the Cluster and its control plane and the target complete.
func deletingSplit(t *testing.T, src, tgt client.Client) (*Pivot, SplitPlan) {
	t.Helper()
	p := pivotFor(t, src, tgt, &recorder{})
	return p, SplitPlan{Source: p.Source, Target: p.Target, Deleting: true}
}

func pausedClusterObjects(suffix string, mutate func(*unstructured.Unstructured)) []client.Object {
	objs := clusterObjects(suffix, false)
	mutate(objs[0].(*unstructured.Unstructured))
	return objs
}

func TestFinishDeletionFailures(t *testing.T) {
	ctx := context.Background()
	specPaused := func(o *unstructured.Unstructured) { _ = unstructured.SetNestedField(o.Object, true, "spec", "paused") }
	for _, tc := range []struct {
		name     string
		tgtObjs  []client.Object
		srcFault *fault
		tgtFault *fault
		want     string
		// clusterKept: the source's Cluster survives the failure (it goes
		// last, so any earlier failure keeps it).
		clusterKept bool
	}{
		{"target unreadable", clusterObjects("tgt", false), nil,
			&fault{verb: "get", kind: lcCluster, err: errBoom}, "reading the Cluster on cluster c", true},
		{"target paused by annotation", pausedClusterObjects("tgt", func(o *unstructured.Unstructured) {
			o.SetAnnotations(map[string]string{pausedAnnotation: ""})
		}), nil, nil, "someone paused it on purpose", true},
		{"listing a movable kind fails", clusterObjects("tgt", false),
			&fault{verb: "list", kind: lcKubeadmControlPlane, err: errBoom}, nil, "listing KubeadmControlPlane on the bootstrap cluster", true},
		{"removing an object fails", clusterObjects("tgt", false),
			&fault{verb: "delete", kind: lcKubeadmControlPlane, err: errBoom}, nil, "removing KubeadmControlPlane c-cp from the bootstrap cluster", true},
		{"listing the extras fails", clusterObjects("tgt", false),
			&fault{verb: "list", kind: lcSecret, err: errBoom}, nil, "listing Secret on the bootstrap cluster", false},
		{"resuming the target fails", pausedClusterObjects("tgt", specPaused), nil,
			&fault{verb: "patch", kind: lcCluster, err: errBoom}, "resuming the Cluster on cluster c", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, tgt := newClient(t, clusterObjects("src", true)...), newClient(t, tc.tgtObjs...)
			if tc.srcFault != nil {
				src = faulty(t, src, tc.srcFault)
			}
			if tc.tgtFault != nil {
				tgt = faulty(t, tgt, tc.tgtFault)
			}
			p, plan := deletingSplit(t, src, tgt)
			err := p.finishDeletion(ctx, plan)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if ok, _ := hasCluster(ctx, src, ns, name); ok != tc.clusterKept {
				t.Errorf("source still holds the Cluster: %v, want %v", ok, tc.clusterKept)
			}
		})
	}
}

// A kind whose CRD is not installed holds nothing: skipped, not an error.
// An unpaused target is left untouched (no patch at all).
func TestFinishDeletionSkipsUninstalledKinds(t *testing.T) {
	ctx := context.Background()
	src := faulty(t, newClient(t, clusterObjects("src", true)...),
		&fault{verb: "list", kind: lcHydraMachineTemplate, err: noMatch(lcHydraMachineTemplate)},
		&fault{verb: "list", kind: lcMachineHealthCheck, err: noMatch(lcMachineHealthCheck)})
	tgt := faulty(t, newClient(t, clusterObjects("tgt", false)...), &fault{verb: "patch", kind: lcCluster, err: errBoom})
	p, plan := deletingSplit(t, src, tgt)
	if err := p.finishDeletion(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if ok, _ := hasCluster(ctx, src, ns, name); ok {
		t.Error("the source's Cluster must be removed")
	}
}

// The extras removed are this cluster's own and Secrets labelled for move;
// another cluster's objects and the teardown record stay.
func TestDeleteLeftoverExtrasRemovesOnlyThisClustersObjects(t *testing.T) {
	ctx := context.Background()
	own := u(configMapGVK, ns, name+"-cilium", nil)
	own.SetLabels(map[string]string{"hydra.io/cluster": name})
	record := u(configMapGVK, ns, TombstoneName, nil)
	record.SetLabels(map[string]string{"hydra.io/cluster": name})
	foreignCM := u(configMapGVK, ns, "other-cilium", nil)
	foreignCM.SetLabels(map[string]string{"hydra.io/cluster": "other"})
	moveSecret := u(secGVK, ns, "shared-pki", nil)
	moveSecret.SetLabels(map[string]string{moveLabel: ""})
	plainSecret := u(secGVK, ns, "unrelated", nil)
	otherMHC := u(mhcGVK, ns, "other-mhc", nil)
	otherMHC.SetLabels(map[string]string{clusterNameLabel: "other"})
	src := newClient(t, own, record, foreignCM, moveSecret, plainSecret, otherMHC)
	p := pivotFor(t, src, newClient(t), &recorder{})
	if err := p.deleteLeftoverExtras(ctx, p.Source); err != nil {
		t.Fatal(err)
	}
	for _, k := range []struct {
		gvk  schema.GroupVersionKind
		n    string
		gone bool
	}{
		{configMapGVK, name + "-cilium", true},
		{secGVK, "shared-pki", true},
		{configMapGVK, TombstoneName, false},
		{configMapGVK, "other-cilium", false},
		{secGVK, "unrelated", false},
		{mhcGVK, "other-mhc", false},
	} {
		o := &unstructured.Unstructured{}
		o.SetGroupVersionKind(k.gvk)
		err := src.Get(ctx, client.ObjectKey{Namespace: ns, Name: k.n}, o)
		if gone := apierrors.IsNotFound(err); gone != k.gone {
			t.Errorf("%s %s: removed %v, want %v (err %v)", k.gvk.Kind, k.n, gone, k.gone, err)
		}
	}
}

func TestDeleteLeftoverExtrasReportsARemovalFailure(t *testing.T) {
	own := u(configMapGVK, ns, name+"-cilium", nil)
	own.SetLabels(map[string]string{"hydra.io/cluster": name})
	src := faulty(t, newClient(t, own), &fault{verb: "patch", kind: lcConfigMap, err: errBoom})
	p := pivotFor(t, src, newClient(t), &recorder{})
	err := p.deleteLeftoverExtras(context.Background(), p.Source)
	if !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "removing ConfigMap c-cilium from the bootstrap cluster") {
		t.Fatalf("got %v", err)
	}
}

// deleteForMove annotates, deletes, then strips finalizers so nothing is torn
// down by a provider: an object with a finalizer is gone afterwards.
func TestDeleteForMoveStripsFinalizers(t *testing.T) {
	ctx := context.Background()
	o := u(hydraMachineGVK, ns, "m", nil)
	o.SetFinalizers([]string{"hydramachine.infrastructure.cluster.x-k8s.io"})
	c := newClient(t, o)
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(hydraMachineGVK)
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "m"}, live); err != nil {
		t.Fatal(err)
	}
	var annotated bool
	spy := &patchSpy{Client: c, onPatch: func(p []byte) {
		if strings.Contains(string(p), deleteForMoveAnnotation) {
			annotated = true
		}
	}}
	if err := deleteForMove(ctx, spy, live); err != nil {
		t.Fatal(err)
	}
	if !annotated {
		t.Error("the delete-for-move annotation must be set before the delete")
	}
	after := &unstructured.Unstructured{}
	after.SetGroupVersionKind(hydraMachineGVK)
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "m"}, after); !apierrors.IsNotFound(err) {
		t.Fatalf("the object must be gone once its finalizers are stripped, got %v", err)
	}
}

type patchSpy struct {
	client.Client
	onPatch func([]byte)
}

func (s *patchSpy) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if b, err := patch.Data(obj); err == nil {
		s.onPatch(b)
	}
	return s.Client.Patch(ctx, obj, patch, opts...)
}

func TestDeleteForMoveErrors(t *testing.T) {
	ctx := context.Background()
	gone := apierrors.NewNotFound(schema.GroupResource{Resource: "hydramachines"}, "m")
	for _, tc := range []struct {
		name   string
		faults []*fault
		want   error
	}{
		{"already gone at the annotation", []*fault{{verb: "patch", kind: lcHydraMachine, err: gone}}, nil},
		{"annotation fails", []*fault{{verb: "patch", kind: lcHydraMachine, err: errBoom}}, errBoom},
		{"already gone at the delete", []*fault{{verb: "delete", kind: lcHydraMachine, err: gone}}, nil},
		{"delete fails", []*fault{{verb: "delete", kind: lcHydraMachine, err: errBoom}}, errBoom},
		{"finalizer strip fails", []*fault{{verb: "patch", kind: lcHydraMachine, skip: 1, err: errBoom}}, errBoom},
		{"gone before the strip", []*fault{{verb: "patch", kind: lcHydraMachine, skip: 1, err: gone}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := u(hydraMachineGVK, ns, "m", nil)
			o.SetFinalizers([]string{"example.com/hold"})
			c := faulty(t, newClient(t, o), tc.faults...)
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(hydraMachineGVK)
			if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "m"}, live); err != nil {
				t.Fatal(err)
			}
			if err := deleteForMove(ctx, c, live); !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestFinishSplitFailures(t *testing.T) {
	ctx := context.Background()
	specPaused := func(o *unstructured.Unstructured) { _ = unstructured.SetNestedField(o.Object, true, "spec", "paused") }
	for _, tc := range []struct {
		name     string
		srcObjs  []client.Object
		srcFault *fault
		moveErr  error
		want     string
		moved    bool
	}{
		{"source unreadable", clusterObjects("src", true), &fault{verb: "get", kind: lcCluster, err: errBoom}, nil,
			"reading the Cluster on the bootstrap cluster", false},
		{"source paused by annotation", pausedClusterObjects("src", func(o *unstructured.Unstructured) {
			o.SetAnnotations(map[string]string{pausedAnnotation: ""})
		}), nil, nil, "someone paused it on purpose", false},
		{"unpausing fails", pausedClusterObjects("src", specPaused), &fault{verb: "patch", kind: lcCluster, err: errBoom}, nil,
			"unpausing the source copy on the bootstrap cluster", false},
		{"the move fails", pausedClusterObjects("src", specPaused), nil, errBoom,
			"clusterctl move: boom (rerun", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := newClient(t, tc.srcObjs...)
			if tc.srcFault != nil {
				src = faulty(t, src, tc.srcFault)
			}
			p := pivotFor(t, src, newClient(t), &recorder{})
			moved := false
			p.Hooks.Move = func(context.Context, []byte, []byte, string, bool) error { moved = true; return tc.moveErr }
			err := p.finishSplit(ctx, p.Source, p.Target)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if moved != tc.moved {
				t.Errorf("move invoked %v, want %v", moved, tc.moved)
			}
		})
	}
}

// An unpaused source is moved without being patched.
func TestFinishSplitMovesAnUnpausedSourceAsIs(t *testing.T) {
	src := faulty(t, newClient(t, clusterObjects("src", true)...), &fault{verb: "patch", kind: lcCluster, err: errBoom})
	p := pivotFor(t, src, newClient(t), &recorder{})
	var got []string
	p.Hooks.Move = func(_ context.Context, from, to []byte, namespace string, dry bool) error {
		if dry {
			t.Error("finishing a split is a real move")
		}
		got = append(got, namespace)
		return nil
	}
	if err := p.finishSplit(context.Background(), p.Source, p.Target); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != ns {
		t.Errorf("one move of namespace %s, got %v", ns, got)
	}
}
