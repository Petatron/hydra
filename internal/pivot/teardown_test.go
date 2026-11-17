package pivot

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// selfObjects is a self-hosted cluster as it holds itself: the cluster's
// objects, labelled as hydra renders them, plus its own kubeconfig Secret
// trusting the same CA as tgtKubeconfig (the cluster's kubeconfig).
func selfObjects(t *testing.T) []client.Object {
	t.Helper()
	objs := clusterObjects("self", true)
	objs[0].SetLabels(map[string]string{"hydra.io/cluster": name})
	kcs := u(secGVK, ns, name+"-kubeconfig", map[string]any{"data": map[string]any{
		"value": base64.StdEncoding.EncodeToString(tgtKubeconfig),
	}})
	kcs.SetLabels(map[string]string{clusterNameLabel: name})
	return append(append(objs, kcs), labelledCRDs()...)
}

// tdRecorder records what a teardown did.
type tdRecorder struct {
	moves                       []string
	deletedCluster, deletedKind bool
	removedFile                 bool
	takeOver                    bool
	failAfterCreate             bool
	failAfterDeleting           int
	kindExisted                 bool // the bootstrap cluster was there before
}

// teardownFor wires a Teardown from self (the cluster, reached with
// tgtKubeconfig) into kind (srcKubeconfig), moving with the clusterctl-like
// fakeMove and deleting by emptying the namespace in kind.
func teardownFor(t *testing.T, self, kind client.Client, rec *tdRecorder) *Teardown {
	t.Helper()
	kindSide := Side{Name: "bootstrap cluster", C: kind, Kubeconfig: srcKubeconfig, File: "kind.kubeconfig"}
	return &Teardown{
		Self:            Side{Name: "cluster c", C: self, Kubeconfig: tgtKubeconfig, File: "c.kubeconfig"},
		Kind:            kindSide,
		Namespace:       ns,
		Name:            name,
		TakeoverTimeout: 40 * time.Millisecond,
		Poll:            time.Millisecond,
		Out:             &bytes.Buffer{},
		Hooks: TeardownHooks{
			EnsureKind:      func(context.Context) (Side, bool, error) { return kindSide, !rec.kindExisted, nil },
			EnsureProviders: func(context.Context, []byte) error { return nil },
			Move: func(ctx context.Context, from, to []byte, _ string, dryRun bool) error {
				if dryRun {
					rec.moves = append(rec.moves, "dry")
					return nil
				}
				if !bytes.Equal(from, tgtKubeconfig) {
					t.Fatal("teardown must only ever move from the cluster itself")
				}
				rec.moves = append(rec.moves, "move")
				return fakeMove(ctx, self, kind, &recorder{takeOver: rec.takeOver, failAfterCreate: rec.failAfterCreate, failAfterDeleting: rec.failAfterDeleting})
			},
			DeleteCluster: func(ctx context.Context, k Side) error {
				rec.deletedCluster = true
				for _, gvk := range append(movedKinds, clusterGVK) {
					list := &unstructured.UnstructuredList{}
					list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
					_ = k.C.List(ctx, list, client.InNamespace(ns))
					for i := range list.Items {
						_ = k.C.Delete(ctx, &list.Items[i])
					}
				}
				return nil
			},
			DeleteKind:       func(context.Context) error { rec.deletedKind = true; return nil },
			RemoveKubeconfig: func() error { rec.removedFile = true; return nil },
		},
	}
}

func tombstoneIn(t *testing.T, kind client.Client) *corev1.ConfigMap {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := kind.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: TombstoneName}, cm); err != nil {
		return nil
	}
	return cm
}

func TestTeardownEndToEnd(t *testing.T) {
	self := newClient(t, selfObjects(t)...)
	kind := newClient(t, labelledCRDs()...)
	rec := &tdRecorder{takeOver: true}
	if err := teardownFor(t, self, kind, rec).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(rec.moves, ",") != "dry,move" || !rec.deletedCluster || !rec.removedFile || !rec.deletedKind {
		t.Fatalf("moves %v deletedCluster %v removedFile %v deletedKind %v", rec.moves, rec.deletedCluster, rec.removedFile, rec.deletedKind)
	}
	if tb := tombstoneIn(t, kind); tb == nil || tb.Data["phase"] != phaseMoved {
		t.Errorf("the teardown record must say the move finished, got %+v", tb)
	}
}

func TestTeardownRefusesWhatIsNotTheClusterItself(t *testing.T) {
	objs := selfObjects(t)
	for _, o := range objs {
		if o.GetName() == name+"-kubeconfig" {
			_ = unstructured.SetNestedField(o.(*unstructured.Unstructured).Object, base64.StdEncoding.EncodeToString(srcKubeconfig), "data", "value")
		}
	}
	self := newClient(t, objs...)
	kind := newClient(t, labelledCRDs()...)
	rec := &tdRecorder{takeOver: true}
	err := teardownFor(t, self, kind, rec).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not that cluster") {
		t.Fatalf("got %v", err)
	}
	if len(rec.moves) != 0 || rec.deletedCluster {
		t.Errorf("nothing may happen: %+v", rec)
	}
}

// Ownership is checked before the move: an unlabelled Cluster moved into
// kind that Delete then refuses would be stranded there.
func TestTeardownChecksOwnershipBeforeMoving(t *testing.T) {
	objs := selfObjects(t)
	objs[0].SetLabels(nil)
	self := newClient(t, objs...)
	kind := newClient(t, labelledCRDs()...)
	rec := &tdRecorder{takeOver: true}
	err := teardownFor(t, self, kind, rec).Run(context.Background())
	if err == nil || len(rec.moves) != 0 {
		t.Fatalf("an unlabelled Cluster must be refused before moving: err %v moves %v", err, rec.moves)
	}
	td := teardownFor(t, newClient(t, objs...), newClient(t, labelledCRDs()...), &tdRecorder{takeOver: true})
	td.Adopt = true
	if err := td.Run(context.Background()); err != nil {
		t.Fatalf("--adopt must let it through: %v", err)
	}
}

// Never delete before kind's provider has taken every VM over.
func TestTeardownWaitsForTakeoverBeforeDeleting(t *testing.T) {
	self := newClient(t, selfObjects(t)...)
	kind := newClient(t, labelledCRDs()...)
	rec := &tdRecorder{takeOver: false}
	err := teardownFor(t, self, kind, rec).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "would wedge") {
		t.Fatalf("got %v", err)
	}
	if rec.deletedCluster || rec.deletedKind {
		t.Errorf("nothing may be deleted before the takeover: %+v", rec)
	}
}

// After the move, a rerun only finishes: it neither moves again nor needs
// the cluster to hold anything.
func TestTeardownResumesAfterTheMove(t *testing.T) {
	self := newClient(t, selfObjects(t)...)
	kind := newClient(t, labelledCRDs()...)
	first := &tdRecorder{takeOver: true}
	td := teardownFor(t, self, kind, first)
	td.Hooks.DeleteCluster = func(context.Context, Side) error { return errors.New("interrupted") }
	if err := td.Run(context.Background()); err == nil {
		t.Fatal("the first run must stop at the delete")
	}
	rec := &tdRecorder{takeOver: true}
	if err := teardownFor(t, self, kind, rec).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rec.moves) != 0 || !rec.deletedCluster || !rec.deletedKind {
		t.Errorf("the rerun must only finish: %+v", rec)
	}
}

// Unreachable is never "does not hold": without a recorded finished move,
// a kind copy plus an unreachable cluster is refused.
func TestTeardownUnreachableClusterNeedsARecord(t *testing.T) {
	kind := newClient(t, append(clusterObjects("kind", true), labelledCRDs()...)...)
	td := teardownFor(t, nil, kind, &tdRecorder{takeOver: true})
	td.Self.C, td.SelfErr = nil, errors.New("connection refused")
	err := td.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot be reached") {
		t.Fatalf("got %v", err)
	}
	// With the move recorded as finished, it proceeds.
	td = teardownFor(t, nil, kind, &tdRecorder{takeOver: true})
	td.Self.C, td.SelfErr = nil, errors.New("connection refused")
	if err := td.writeTombstone(context.Background(), phaseMoved); err != nil {
		t.Fatal(err)
	}
	if err := td.Run(context.Background()); err != nil {
		t.Fatalf("a recorded move must let it finish: %v", err)
	}
}

// Interrupted after the delete, before the file and kind are removed: the
// tombstone lets a rerun finish instead of wedging.
func TestTeardownFinishesCleanupFromTheRecord(t *testing.T) {
	kind := newClient(t, labelledCRDs()...)
	rec := &tdRecorder{}
	td := teardownFor(t, nil, kind, rec)
	td.Self.C, td.SelfErr = nil, errors.New("no such host") // the cluster is gone
	td.createdKind = true                                   // the interrupted run had created kind
	if err := td.writeTombstone(context.Background(), phaseMoved); err != nil {
		t.Fatal(err)
	}
	if err := td.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !rec.removedFile || !rec.deletedKind || rec.deletedCluster {
		t.Errorf("only the file and kind remain to remove: %+v", rec)
	}
}

func TestTeardownAmbiguousWithoutRecordRefuses(t *testing.T) {
	td := teardownFor(t, nil, nil, &tdRecorder{})
	td.Kind.C = nil
	td.Self.C, td.SelfErr = nil, errors.New("connection refused")
	err := td.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot tell which") {
		t.Fatalf("got %v", err)
	}
}

func TestTeardownNothingFound(t *testing.T) {
	td := teardownFor(t, nil, nil, &tdRecorder{})
	td.Self, td.Kind.C = Side{Name: "cluster c"}, nil
	if err := td.Run(context.Background()); !errors.Is(err, ErrNothingToTearDown) {
		t.Fatalf("got %v", err)
	}
}

// The kubeconfig file is removed only when it trusts the recorded CA.
func TestTeardownKeepsAForeignKubeconfigFile(t *testing.T) {
	kind := newClient(t, labelledCRDs()...)
	rec := &tdRecorder{}
	td := teardownFor(t, nil, kind, rec)
	td.Self.C, td.SelfErr = nil, errors.New("gone")
	if err := td.writeTombstone(context.Background(), phaseMoved); err != nil {
		t.Fatal(err)
	}
	td.Self.Kubeconfig = srcKubeconfig // another cluster's file under this name
	err := td.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "left by another cluster") {
		t.Fatalf("a record from another cluster of this name is no evidence: %v", err)
	}
	if rec.removedFile || rec.deletedKind {
		t.Errorf("nothing may be removed: %+v", rec)
	}
}

// A split left by an interrupted pivot (source: kind) must never be
// finished by teardown.
func TestTeardownRefusesAPivotSplit(t *testing.T) {
	kind, self := splitByInterruptedPivot(t) // pivot's source is "src" = kind here
	rec := &tdRecorder{}
	td := teardownFor(t, self, kind, rec)
	td.FinishSplit = true
	err := td.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "interrupted pivot") {
		t.Fatalf("got %v", err)
	}
	if len(rec.moves) != 0 {
		t.Errorf("nothing may move: %v", rec.moves)
	}
}

// A split left by an interrupted teardown is finished with --finish-split.
func TestTeardownFinishesItsOwnSplit(t *testing.T) {
	self := newClient(t, selfObjects(t)...)
	kind := newClient(t, labelledCRDs()...)
	if err := teardownFor(t, self, kind, &tdRecorder{failAfterCreate: true}).Run(context.Background()); err == nil {
		t.Fatal("the interrupted move must fail")
	}
	err := teardownFor(t, self, kind, &tdRecorder{}).Run(context.Background())
	if !errors.Is(err, ErrSplit) {
		t.Fatalf("without --finish-split a split is reported: %v", err)
	}
	rec := &tdRecorder{takeOver: true}
	td := teardownFor(t, self, kind, rec)
	td.FinishSplit = true
	if err := td.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !rec.deletedCluster || !rec.deletedKind {
		t.Errorf("the finished split must be torn down: %+v", rec)
	}
}

// A kind left over with objects in the namespace is refused before moving.
func TestTeardownRefusesAStaleKind(t *testing.T) {
	self := newClient(t, selfObjects(t)...)
	stale := u(hydraMachineGVK, ns, "old-1", nil)
	kind := newClient(t, append(labelledCRDs(), stale)...)
	rec := &tdRecorder{takeOver: true}
	err := teardownFor(t, self, kind, rec).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "left from an earlier run") || len(rec.moves) != 0 {
		t.Fatalf("got %v, moves %v", err, rec.moves)
	}
}

// A degraded cluster can still be torn down: teardown does not require it
// Available and rolled out.
func TestTeardownDoesNotRequireASettledCluster(t *testing.T) {
	objs := selfObjects(t)
	for _, o := range objs {
		if o.GetName() == name+"-cp" {
			_ = unstructured.SetNestedField(o.(*unstructured.Unstructured).Object, int64(0), "status", "readyReplicas")
		}
	}
	rec := &tdRecorder{takeOver: true}
	if err := teardownFor(t, newClient(t, objs...), newClient(t, labelledCRDs()...), rec).Run(context.Background()); err != nil {
		t.Fatalf("a degraded cluster must still be removable: %v", err)
	}
}

// MachineHealthChecks are paused before the move.
func TestTeardownPausesHealthChecks(t *testing.T) {
	mhc := u(mhcGVK, ns, name+"-cp-mhc", nil)
	mhc.SetLabels(map[string]string{clusterNameLabel: name})
	self := newClient(t, append(selfObjects(t), mhc)...)
	rec := &tdRecorder{takeOver: true}
	if err := teardownFor(t, self, newClient(t, labelledCRDs()...), rec).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(mhcGVK)
	if err := self.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name + "-cp-mhc"}, got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.GetAnnotations()[pausedAnnotation]; !ok {
		t.Error("the MachineHealthCheck must be paused before the move")
	}
}

// Review defect 1: no kubeconfig file is not "the cluster holds nothing".
// With the file missing and a split from this teardown, the partial kind
// copy must never be resumed.
func TestTeardownMissingFileNeverResumesAPartialCopy(t *testing.T) {
	self := newClient(t, selfObjects(t)...)
	kind := newClient(t, labelledCRDs()...)
	if err := teardownFor(t, self, kind, &tdRecorder{failAfterCreate: true}).Run(context.Background()); err == nil {
		t.Fatal("the interrupted move must fail")
	}
	rec := &tdRecorder{}
	td := teardownFor(t, nil, kind, rec)
	td.Self = Side{Name: "cluster c", File: "elsewhere/c.kubeconfig"} // no file here
	err := td.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no kubeconfig") {
		t.Fatalf("got %v", err)
	}
	if rec.deletedCluster || rec.deletedKind {
		t.Errorf("nothing may be deleted: %+v", rec)
	}
	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(clusterGVK)
	if err := kind.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, cl); err != nil {
		t.Fatal(err)
	}
	if !paused(cl) {
		t.Error("the partial kind copy must stay paused")
	}
}

// Review defect 2: a move that paused the cluster and failed before creating
// anything in kind, then the cluster unreachable: never clean up.
func TestTeardownUnreachableAfterAFailedMoveRefuses(t *testing.T) {
	kind := newClient(t, labelledCRDs()...)
	td := teardownFor(t, nil, kind, &tdRecorder{})
	if err := td.writeTombstone(context.Background(), phaseMoving); err != nil {
		t.Fatal(err)
	}
	rec := &tdRecorder{}
	td = teardownFor(t, nil, kind, rec)
	td.Self.C, td.SelfErr = nil, errors.New("connection refused")
	err := td.Run(context.Background())
	if err == nil || rec.removedFile || rec.deletedKind {
		t.Fatalf("an unfinished move with the cluster unseen must not be cleaned up: err %v rec %+v", err, rec)
	}
}

// Review defect 3: a split interrupted while clusterctl was deleting the
// source (target complete, source with fewer machines) must not be resumed
// from the source.
func TestSplitSourceRefusesASourceMidDeletion(t *testing.T) {
	a := newClient(t, clusterObjects("a", true)[:1]...) // Cluster only: its machines already deleted
	b := newClient(t, clusterObjects("b", false)...)    // complete copy, nothing taken over
	if _, _, err := SplitSource(context.Background(), Side{Name: "a", C: a}, Side{Name: "b", C: b}, ns); err == nil {
		t.Fatal("a source with fewer machines than the target must not be resumed")
	}
	// Source with some machines left, but fewer than the complete target.
	two := append(clusterObjects("t", false), u(hydraMachineGVK, ns, name+"-w-2", nil))
	b = newClient(t, two...)
	a = newClient(t, clusterObjects("a", true)...)
	if _, _, err := SplitSource(context.Background(), Side{Name: "a", C: a}, Side{Name: "b", C: b}, ns); err == nil {
		t.Fatal("fewer source machines than target machines means a deletion was under way")
	}
}

// Review defect 4: this teardown's own failed move left the cluster paused
// with nothing in kind; a rerun lifts that pause and finishes.
func TestTeardownResumesAfterItsOwnFailedMove(t *testing.T) {
	self := newClient(t, selfObjects(t)...)
	kind := newClient(t, labelledCRDs()...)
	td := teardownFor(t, self, kind, &tdRecorder{})
	if err := td.writeTombstone(context.Background(), phaseMoving); err != nil {
		t.Fatal(err)
	}
	if err := setPaused(context.Background(), self, true); err != nil {
		t.Fatal(err)
	}
	rec := &tdRecorder{takeOver: true}
	if err := teardownFor(t, self, kind, rec).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !rec.deletedCluster || !rec.deletedKind {
		t.Errorf("the rerun must finish: %+v", rec)
	}
}

// Without a teardown record, a reachable kubeconfig that holds nothing must
// be proven to be this cluster before kind's copy is resumed or deleted.
func TestTeardownWithoutRecordProvesTheFile(t *testing.T) {
	// kind holds the cluster with its own kubeconfig Secret trusting tgtKubeconfig's CA.
	kindObjs := selfObjects(t)
	kind := newClient(t, kindObjs...)
	other := newClient(t, labelledCRDs()...) // some other cluster: holds nothing
	rec := &tdRecorder{takeOver: true}
	td := teardownFor(t, other, kind, rec)
	td.Self.Kubeconfig = kubeconfigFor("https://10.9.9.9:6443", "ca-other")
	err := td.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is not cluster c's kubeconfig") || rec.deletedCluster {
		t.Fatalf("got %v, rec %+v", err, rec)
	}
}

// A machine with no owner record never counts as holding its VM, even where
// its UID reads empty too.
func TestTakenOverNeedsARecord(t *testing.T) {
	bare := u(hydraMachineGVK, ns, name+"-cp-1", nil) // no UID, no annotations
	a := newClient(t, append(clusterObjects("a", false)[:1], bare)...)
	b := newClient(t, clusterObjects("b", true)...)
	src, _, err := SplitSource(context.Background(), Side{Name: "a", C: a}, Side{Name: "b", C: b}, ns)
	if err != nil || src.Name != "b" {
		t.Fatalf("the copy with recorded owners is the source: got %q, %v", src.Name, err)
	}
}

// A teardown's own move interrupted while deleting the cluster's copy: the
// cluster itself is never resumed, kind's complete copy is.
func TestTeardownFinishesItsSplitInterruptedWhileDeleting(t *testing.T) {
	ctx := context.Background()
	self := newClient(t, selfObjects(t)...)
	kind := newClient(t, labelledCRDs()...)
	// selfObjects has one more Secret than clusterObjects (the cluster's own
	// kubeconfig), so one more deletion reaches the HydraMachines.
	if err := teardownFor(t, self, kind, &tdRecorder{failAfterDeleting: deletionsLeavingClusterAndKCP + 1}).Run(ctx); err == nil {
		t.Fatal("the interrupted move must fail")
	}
	rec := &tdRecorder{}
	td := teardownFor(t, self, kind, rec)
	td.FinishSplit = true
	if err := td.Run(ctx); err == nil || !strings.Contains(err.Error(), "would wedge") {
		t.Fatalf("the takeover is still to come: %v", err)
	}
	if len(rec.moves) != 0 || rec.deletedCluster {
		t.Errorf("no move again, nothing deleted before the takeover: %+v", rec)
	}
	if ok, _ := hasCluster(ctx, self, ns, name); ok {
		t.Error("the cluster's leftover copy must be removed")
	}
	takeOverAll(ctx, kind)
	rec = &tdRecorder{}
	if err := teardownFor(t, self, kind, rec).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !rec.deletedCluster || !rec.deletedKind {
		t.Errorf("once taken over, the teardown finishes: %+v", rec)
	}
}

// A stale MachineHealthCheck in kind's namespace is refused like any other
// leftover: not paused, it could remediate the moved machines.
func TestTeardownRefusesAStaleHealthCheckInKind(t *testing.T) {
	self := newClient(t, selfObjects(t)...)
	kind := newClient(t, append(labelledCRDs(), u(mhcGVK, ns, "old-mhc", nil))...)
	rec := &tdRecorder{takeOver: true}
	err := teardownFor(t, self, kind, rec).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "MachineHealthCheck old-mhc") || len(rec.moves) != 0 {
		t.Fatalf("got %v, moves %v", err, rec.moves)
	}
}

// A bootstrap kind cluster that existed before the teardown is not the
// teardown's to delete.
func TestTeardownLeavesAPreexistingKind(t *testing.T) {
	rec := &tdRecorder{takeOver: true, kindExisted: true}
	if err := teardownFor(t, newClient(t, selfObjects(t)...), newClient(t, labelledCRDs()...), rec).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !rec.deletedCluster || rec.deletedKind {
		t.Errorf("the cluster goes, a kind that was already there stays: %+v", rec)
	}
}

// A run that created kind and was interrupted: a later run finds kind
// already there, and still removes it, from the record.
func TestTeardownRemembersItCreatedKind(t *testing.T) {
	self := newClient(t, selfObjects(t)...)
	kind := newClient(t, labelledCRDs()...)
	first := teardownFor(t, self, kind, &tdRecorder{takeOver: true})
	first.Hooks.DeleteCluster = func(context.Context, Side) error { return errors.New("interrupted") }
	if err := first.Run(context.Background()); err == nil {
		t.Fatal("the first run must stop at the delete")
	}
	rec := &tdRecorder{takeOver: true, kindExisted: true} // kind is there now
	if err := teardownFor(t, self, kind, rec).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !rec.deletedKind {
		t.Error("kind this teardown created must be removed, whichever run finishes")
	}
}

// Without a teardown record, a paused kind copy may be an operator's pause:
// refused, never resumed.
func TestTeardownWithoutRecordLeavesAPausedCopy(t *testing.T) {
	kindObjs := selfObjects(t)
	_ = unstructured.SetNestedField(kindObjs[0].(*unstructured.Unstructured).Object, true, "spec", "paused")
	kind := newClient(t, kindObjs...)
	self := newClient(t, labelledCRDs()...) // the cluster itself, holding nothing
	rec := &tdRecorder{takeOver: true}
	err := teardownFor(t, self, kind, rec).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "operator's pause") || rec.deletedCluster {
		t.Fatalf("got %v, rec %+v", err, rec)
	}
	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(clusterGVK)
	if err := kind.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, cl); err != nil {
		t.Fatal(err)
	}
	if !paused(cl) {
		t.Error("the copy must stay paused")
	}
}

// Kind's namespace must hold no stale Secret or ConfigMap either, except
// what Kubernetes puts in every namespace.
func TestTeardownStaleSecretsAndConfigMaps(t *testing.T) {
	stale := u(secGVK, ns, "old-pki", nil)
	rec := &tdRecorder{takeOver: true}
	err := teardownFor(t, newClient(t, selfObjects(t)...), newClient(t, append(labelledCRDs(), stale)...), rec).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Secret old-pki") {
		t.Fatalf("got %v", err)
	}
	rootCA := u(configMapGVK, ns, "kube-root-ca.crt", nil)
	rec = &tdRecorder{takeOver: true}
	if err := teardownFor(t, newClient(t, selfObjects(t)...), newClient(t, append(labelledCRDs(), rootCA)...), rec).Run(context.Background()); err != nil {
		t.Fatalf("kube-root-ca.crt is in every namespace and must not count: %v", err)
	}
}

// Finishing a split interrupted while deleting removes every leftover the
// move would have carried: the cluster's Secrets (credentials included),
// ConfigMaps and MachineHealthChecks too.
func TestFinishDeletionRemovesTheWholeGraph(t *testing.T) {
	ctx := context.Background()
	mhc := u(mhcGVK, ns, name+"-mhc", nil)
	mhc.SetLabels(map[string]string{clusterNameLabel: name})
	cm := u(configMapGVK, ns, name+"-cilium", nil)
	cm.SetLabels(map[string]string{"hydra.io/cluster": name})
	src := newClient(t, append(append(clusterObjects("src", true), mhc, cm), labelledCRDs()...)...)
	tgt := newClient(t, labelledCRDs()...)
	if err := pivotFor(t, src, tgt, &recorder{failAfterDeleting: deletionsLeavingClusterAndKCP}).Run(ctx, time.Second); err == nil {
		t.Fatal("the interrupted move must fail")
	}
	p := pivotFor(t, src, tgt, &recorder{})
	p.FinishSplit = true
	_ = p.Run(ctx, 20*time.Millisecond)
	for _, k := range []struct {
		gvk schema.GroupVersionKind
		n   string
	}{{mhcGVK, name + "-mhc"}, {configMapGVK, name + "-cilium"}, {secGVK, "c-pki"}} {
		o := &unstructured.Unstructured{}
		o.SetGroupVersionKind(k.gvk)
		if err := src.Get(ctx, client.ObjectKey{Namespace: ns, Name: k.n}, o); err == nil {
			t.Errorf("%s %s must be removed from the source", k.gvk.Kind, k.n)
		}
	}
}
