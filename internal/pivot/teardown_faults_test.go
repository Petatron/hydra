package pivot

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// recordIn writes a teardown record with data straight into kind.
func recordIn(t *testing.T, kind client.Client, data map[string]string) {
	t.Helper()
	cm := &corev1.ConfigMap{}
	cm.Namespace, cm.Name, cm.Data = ns, TombstoneName, data
	if err := kind.Create(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
}

func selfCA() string {
	return base64.StdEncoding.EncodeToString([]byte("ca-c"))
}

// A Run that keeps reporting success without changing anything stops.
func TestTeardownStepGuard(t *testing.T) {
	rec := &tdRecorder{}
	td := teardownFor(t, newClient(t, selfObjects(t)...), newClient(t, labelledCRDs()...), rec)
	td.steps = 3
	err := td.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "made no progress") || len(rec.moves) != 0 {
		t.Fatalf("got %v, moves %v", err, rec.moves)
	}
}

func TestTeardownReadFailures(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		self  client.Client
		kindF []*fault
		want  string
	}{
		{"kind unreadable", newClient(t, selfObjects(t)...), []*fault{{verb: "get", kind: lcCluster, err: errBoom}}, "reading the bootstrap cluster: boom"},
		{"record unreadable", newClient(t, selfObjects(t)...), []*fault{{verb: "get", kind: lcConfigMap, name: TombstoneName, err: errBoom}}, "reading the teardown record in the bootstrap cluster"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &tdRecorder{takeOver: true}
			kind := faulty(t, newClient(t, labelledCRDs()...), tc.kindF...)
			err := teardownFor(t, tc.self, kind, rec).Run(ctx)
			if err == nil || !strings.Contains(err.Error(), tc.want) || len(rec.moves) != 0 {
				t.Fatalf("want %q, got %v, moves %v", tc.want, err, rec.moves)
			}
		})
	}
}

// A record naming another cluster is never evidence for this one.
func TestTeardownRefusesARecordForAnotherCluster(t *testing.T) {
	kind := newClient(t, labelledCRDs()...)
	recordIn(t, kind, map[string]string{"cluster": "other", "phase": phaseMoved, "ca": selfCA()})
	rec := &tdRecorder{}
	td := teardownFor(t, nil, kind, rec)
	td.Self.C, td.SelfErr = nil, errors.New("gone")
	err := td.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), `names cluster "other", not c`) || rec.removedFile || rec.deletedKind {
		t.Fatalf("got %v, rec %+v", err, rec)
	}
}

// An unreadable cluster is "unknown", never "does not hold": with kind
// holding a copy and no finished move recorded, nothing happens.
func TestTeardownUnreadableSelfIsUnknown(t *testing.T) {
	self := faulty(t, newClient(t, selfObjects(t)...), &fault{verb: "get", kind: lcCluster, err: errBoom})
	kind := newClient(t, append(clusterObjects("kind", true), labelledCRDs()...)...)
	rec := &tdRecorder{takeOver: true}
	err := teardownFor(t, self, kind, rec).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot be reached (boom)") || rec.deletedCluster {
		t.Fatalf("got %v, rec %+v", err, rec)
	}
}

// The cluster answers but holds nothing, and there is no kind: wrong
// kubeconfig or namespace, not a finished teardown.
func TestTeardownSelfHoldsNothingWithoutKind(t *testing.T) {
	td := teardownFor(t, newClient(t), nil, &tdRecorder{})
	td.Kind = Side{}
	err := td.Run(context.Background())
	if err == nil || errors.Is(err, ErrNothingToTearDown) || !strings.Contains(err.Error(), "answers but holds no Cluster ns/c, and the bootstrap cluster holds none") {
		t.Fatalf("got %v", err)
	}
}

func TestTeardownMoveToKindFailures(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		selfF []*fault
		kindF []*fault
		hooks func(*Teardown)
		want  string
		// recorded: whether a record should be in kind afterwards.
		recorded bool
	}{
		{name: "state unreadable", selfF: []*fault{{verb: "get", kind: lcCluster, skip: 1, err: errBoom}}, want: "boom"},
		{name: "own kubeconfig Secret unreadable", selfF: []*fault{{verb: "get", kind: lcSecret, name: name + "-kubeconfig", err: errBoom}}, want: "cannot prove cluster c is cluster c managing itself"},
		{name: "kind fails to come up", hooks: func(td *Teardown) {
			td.Hooks.EnsureKind = func(context.Context) (Side, bool, error) { return Side{}, false, errBoom }
		}, want: "boom"},
		{name: "namespace cannot be created", kindF: []*fault{{verb: "create", kind: "Namespace", err: errBoom}}, want: "creating namespace ns in the bootstrap cluster"},
		{name: "record unreadable while writing", kindF: []*fault{{verb: "get", kind: lcConfigMap, name: TombstoneName, skip: 1, err: errBoom}}, want: "reading the teardown record: boom"},
		{name: "record cannot be written", kindF: []*fault{{verb: "create", kind: lcConfigMap, err: errBoom}}, want: "writing the teardown record in the bootstrap cluster"},
		{name: "kind namespace unlistable", kindF: []*fault{{verb: "list", kind: lcHydraMachine, err: errBoom}}, want: "listing HydraMachine in the bootstrap cluster", recorded: true},
		{name: "kind Secrets unlistable", kindF: []*fault{{verb: "list", kind: lcSecret, err: errBoom}}, want: "listing Secrets in the bootstrap cluster", recorded: true},
		{name: "kind ConfigMaps unlistable", kindF: []*fault{{verb: "list", kind: lcConfigMap, err: errBoom}}, want: "listing ConfigMaps in the bootstrap cluster", recorded: true},
		{name: "health checks unlistable", selfF: []*fault{{verb: "list", kind: lcMachineHealthCheck, err: errBoom}}, want: "listing MachineHealthChecks: boom", recorded: true},
		{name: "moving phase cannot be recorded", kindF: []*fault{{verb: "update", kind: lcConfigMap, err: errBoom}}, want: "writing the teardown record", recorded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			self := newClient(t, selfObjects(t)...)
			if tc.selfF != nil {
				self = faulty(t, self, tc.selfF...)
			}
			bare := newClient(t, labelledCRDs()...)
			kind := bare
			if tc.kindF != nil {
				kind = faulty(t, bare, tc.kindF...)
			}
			rec := &tdRecorder{takeOver: true}
			td := teardownFor(t, self, kind, rec)
			if tc.hooks != nil {
				tc.hooks(td)
			}
			err := td.Run(ctx)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			for _, m := range rec.moves {
				if m == "move" {
					t.Errorf("nothing may move, got %v", rec.moves)
				}
			}
			if got := tombstoneIn(t, bare) != nil; got != tc.recorded {
				t.Errorf("record present %v, want %v", got, tc.recorded)
			}
			if ok, _ := hasCluster(ctx, self, ns, name); !ok && tc.selfF == nil {
				t.Error("the cluster must still hold itself")
			}
		})
	}
}

// A ConfigMap of anyone else's in kind's namespace is stale; kube-root-ca.crt
// and this teardown's own record are not.
func TestTeardownRefusesAStaleConfigMapInKind(t *testing.T) {
	rec := &tdRecorder{takeOver: true}
	kind := newClient(t, append(labelledCRDs(), u(configMapGVK, ns, "old-cilium", nil))...)
	err := teardownFor(t, newClient(t, selfObjects(t)...), kind, rec).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ConfigMap old-cilium") || len(rec.moves) != 0 {
		t.Fatalf("got %v, moves %v", err, rec.moves)
	}
}

// Uninstalled kinds in kind, or no MachineHealthCheck CRD on the cluster,
// hold nothing: the teardown goes ahead.
func TestTeardownSkipsUninstalledKinds(t *testing.T) {
	self := faulty(t, newClient(t, selfObjects(t)...), &fault{verb: "list", kind: lcMachineHealthCheck, err: noMatch(lcMachineHealthCheck)})
	kind := faulty(t, newClient(t, labelledCRDs()...), &fault{verb: "list", kind: lcClusterClass, err: noMatch(lcClusterClass)})
	rec := &tdRecorder{takeOver: true}
	if err := teardownFor(t, self, kind, rec).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !rec.deletedCluster || !rec.deletedKind {
		t.Errorf("the teardown must finish: %+v", rec)
	}
}

func TestTeardownPauseHealthChecks(t *testing.T) {
	ctx := context.Background()
	already := u(mhcGVK, ns, name+"-paused", nil)
	already.SetLabels(map[string]string{clusterNameLabel: name})
	already.SetAnnotations(map[string]string{pausedAnnotation: "operator"})
	// A health check already paused keeps its pause untouched: no patch.
	self := faulty(t, newClient(t, already), &fault{verb: "patch", kind: lcMachineHealthCheck, err: errBoom})
	td := teardownFor(t, self, newClient(t), &tdRecorder{})
	if err := td.pauseHealthChecks(ctx); err != nil {
		t.Fatal(err)
	}
	// A failed pause stops the teardown, naming the check.
	mine := u(mhcGVK, ns, name+"-mhc", nil)
	mine.SetLabels(map[string]string{clusterNameLabel: name})
	self = faulty(t, newClient(t, mine), &fault{verb: "patch", kind: lcMachineHealthCheck, err: errBoom})
	td = teardownFor(t, self, newClient(t), &tdRecorder{})
	if err := td.pauseHealthChecks(ctx); err == nil || !strings.Contains(err.Error(), "pausing MachineHealthCheck c-mhc: boom") {
		t.Fatalf("got %v", err)
	}
}

// A "moving" record with the cluster not spec-paused: nothing to lift; an
// unreadable or unpatchable cluster stops the rerun before kind is touched.
func TestTeardownResumeOwnPause(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		paused bool
		selfF  []*fault
		want   string
	}{
		{"not paused", false, nil, ""},
		{"unreadable", true, []*fault{{verb: "get", kind: lcCluster, err: errBoom}}, "boom"},
		{"unpatchable", true, []*fault{{verb: "patch", kind: lcCluster, err: errBoom}}, "boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bare := newClient(t, selfObjects(t)...)
			if tc.paused {
				if err := setPaused(ctx, bare, true); err != nil {
					t.Fatal(err)
				}
			}
			self := bare
			if tc.selfF != nil {
				self = faulty(t, bare, tc.selfF...)
			}
			td := teardownFor(t, self, newClient(t), &tdRecorder{})
			err := td.resumeOwnPause(ctx)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, errBoom) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// A split with unclear evidence and --finish-split: refused, nothing moves.
func TestTeardownFinishSplitRefusesUnclearEvidence(t *testing.T) {
	self := newClient(t, append(clusterObjects("self", false), labelledCRDs()...)...)
	kind := newClient(t, append(clusterObjects("kind", false), labelledCRDs()...)...)
	rec := &tdRecorder{}
	td := teardownFor(t, self, kind, rec)
	td.FinishSplit = true
	err := td.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot tell which copy") || len(rec.moves) != 0 || rec.deletedCluster {
		t.Fatalf("got %v, rec %+v", err, rec)
	}
}

// A split with the cluster as source but no teardown record was not this
// command's: finishing it is refused.
func TestTeardownFinishSplitNeedsARecord(t *testing.T) {
	self := newClient(t, append(clusterObjects("self", true), labelledCRDs()...)...)
	kind := newClient(t, append(clusterObjects("kind", false), labelledCRDs()...)...)
	rec := &tdRecorder{}
	td := teardownFor(t, self, kind, rec)
	td.FinishSplit = true
	err := td.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no teardown is recorded") || len(rec.moves) != 0 {
		t.Fatalf("got %v, rec %+v", err, rec)
	}
}

func TestTeardownFinishSplitFailures(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		kindF []*fault
		hooks func(*Teardown)
		want  string
	}{
		{"the move fails", nil, func(td *Teardown) {
			td.Hooks.Move = func(context.Context, []byte, []byte, string, bool) error { return errBoom }
		}, "clusterctl move: boom"},
		{"the record cannot be updated", []*fault{{verb: "update", kind: lcConfigMap, err: errBoom}}, nil, "writing the teardown record"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			self := newClient(t, selfObjects(t)...)
			bare := newClient(t, labelledCRDs()...)
			if err := teardownFor(t, self, bare, &tdRecorder{failAfterCreate: true}).Run(ctx); err == nil {
				t.Fatal("the interrupted move must fail")
			}
			kind := bare
			if tc.kindF != nil {
				kind = faulty(t, bare, tc.kindF...)
			}
			rec := &tdRecorder{takeOver: true}
			td := teardownFor(t, self, kind, rec)
			td.FinishSplit = true
			if tc.hooks != nil {
				tc.hooks(td)
			}
			err := td.Run(ctx)
			if err == nil || !strings.Contains(err.Error(), tc.want) || rec.deletedCluster {
				t.Fatalf("want %q, got %v, rec %+v", tc.want, err, rec)
			}
		})
	}
}

// Without a record, a reachable kubeconfig holding nothing, proven to be the
// cluster's own: kind's copy is deleted, the file removed as the cluster's,
// and a kind nobody recorded creating stays.
func TestTeardownWithoutRecordFinishesAProvenCluster(t *testing.T) {
	kind := newClient(t, selfObjects(t)...)
	rec := &tdRecorder{}
	if err := teardownFor(t, newClient(t, labelledCRDs()...), kind, rec).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !rec.deletedCluster || !rec.removedFile || rec.deletedKind {
		t.Errorf("delete the cluster and its file, keep kind: %+v", rec)
	}
}

func TestTeardownFinishInKindFailures(t *testing.T) {
	ctx := context.Background()
	annotated := func(o []client.Object) []client.Object {
		o[0].SetAnnotations(map[string]string{pausedAnnotation: ""})
		return o
	}
	specPaused := func(o []client.Object) []client.Object {
		_ = unstructured.SetNestedField(o[0].(*unstructured.Unstructured).Object, true, "spec", "paused")
		return o
	}
	for _, tc := range []struct {
		name   string
		phase  string // "" for no record
		mutate func([]client.Object) []client.Object
		kindF  []*fault
		hooks  func(*Teardown)
		want   string
	}{
		{name: "no record, kubeconfig Secret unreadable", kindF: []*fault{{verb: "get", kind: lcSecret, name: name + "-kubeconfig", err: errBoom}}, want: "reading the cluster's kubeconfig Secret in the bootstrap cluster"},
		{name: "Cluster unreadable", phase: phaseMoved, kindF: []*fault{{verb: "get", kind: lcCluster, skip: 1, err: errBoom}}, want: "reading the Cluster in the bootstrap cluster"},
		{name: "operator's annotation pause", phase: phaseMoving, mutate: annotated, want: "someone paused it on purpose"},
		{name: "resuming the move's pause fails", phase: phaseMoving, mutate: specPaused, kindF: []*fault{{verb: "patch", kind: lcCluster, err: errBoom}}, want: "resuming the Cluster in the bootstrap cluster"},
		{name: "takeover unreadable", phase: phaseMoved, kindF: []*fault{{verb: "list", kind: lcHydraMachine, err: errBoom}}, want: "reading HydraMachines in the bootstrap cluster"},
		{name: "delete fails", phase: phaseMoved, hooks: func(td *Teardown) {
			td.Hooks.DeleteCluster = func(context.Context, Side) error { return errBoom }
		}, want: "boom"},
		{name: "deleted phase cannot be recorded", phase: phaseMoved, kindF: []*fault{{verb: "update", kind: lcConfigMap, err: errBoom}}, want: "writing the teardown record"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := selfObjects(t)
			if tc.mutate != nil {
				objs = tc.mutate(objs)
			}
			bare := newClient(t, objs...)
			if tc.phase != "" {
				recordIn(t, bare, map[string]string{"cluster": name, "phase": tc.phase, "ca": selfCA()})
			}
			kind := bare
			if tc.kindF != nil {
				kind = faulty(t, bare, tc.kindF...)
			}
			rec := &tdRecorder{}
			td := teardownFor(t, newClient(t, labelledCRDs()...), kind, rec)
			if tc.hooks != nil {
				tc.hooks(td)
			}
			err := td.Run(ctx)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if rec.removedFile || rec.deletedKind {
				t.Errorf("nothing may be cleaned up after a failure: %+v", rec)
			}
		})
	}
}

// The move's own pause (record still "moving") on kind's only copy is lifted,
// and the teardown finishes.
func TestTeardownResumesTheMovesPauseInKind(t *testing.T) {
	ctx := context.Background()
	objs := selfObjects(t)
	_ = unstructured.SetNestedField(objs[0].(*unstructured.Unstructured).Object, true, "spec", "paused")
	kind := newClient(t, objs...)
	recordIn(t, kind, map[string]string{"cluster": name, "phase": phaseMoving, "ca": selfCA(), "createdKind": recordTrue})
	rec := &tdRecorder{}
	if err := teardownFor(t, newClient(t, labelledCRDs()...), kind, rec).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !rec.deletedCluster || !rec.removedFile || !rec.deletedKind {
		t.Errorf("the teardown must finish: %+v", rec)
	}
}

// Waiting for the takeover: Ctrl-C ends it with a rerun hint, deleting
// nothing; the default timeout is ten minutes.
func TestTeardownWaitTakenOverInterrupted(t *testing.T) {
	kind := newClient(t, clusterObjects("kind", false)...)
	td := teardownFor(t, nil, kind, &tdRecorder{})
	td.TakeoverTimeout = 0 // the default, far longer than this test
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := td.waitTakenOver(ctx)
	if err == nil || !strings.Contains(err.Error(), "interrupted while waiting for the takeover (0 of 1 machines taken over by the bootstrap cluster)") {
		t.Fatalf("got %v", err)
	}
}

func TestTeardownCleanup(t *testing.T) {
	ctx := context.Background()
	otherCA := base64.StdEncoding.EncodeToString([]byte("ca-other"))
	for _, tc := range []struct {
		name        string
		ca          string
		removeErr   error
		kindNil     bool
		deleteFault *fault
		wantErr     string
		wantRemoved bool
		wantLog     string
	}{
		{name: "nothing recorded", ca: "", wantLog: "leaving c.kubeconfig: nothing recorded"},
		{name: "another CA", ca: otherCA, wantLog: "trusts another certificate authority"},
		{name: "this CA", ca: selfCA(), wantRemoved: true, wantLog: "removed c.kubeconfig"},
		{name: "removal fails", ca: selfCA(), removeErr: errBoom, wantErr: "removing c.kubeconfig: boom"},
		{name: "no kind", ca: selfCA(), kindNil: true, wantRemoved: true},
		{name: "record removal fails", ca: selfCA(), deleteFault: &fault{verb: "delete", kind: lcConfigMap, err: errBoom}, wantRemoved: true, wantErr: "removing the teardown record from the bootstrap cluster"},
		{name: "record already gone", ca: selfCA(), deleteFault: &fault{verb: "delete", kind: lcConfigMap, err: apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, TombstoneName)}, wantRemoved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind := newClient(t)
			if tc.deleteFault != nil {
				kind = faulty(t, kind, tc.deleteFault)
			}
			rec := &tdRecorder{}
			td := teardownFor(t, nil, kind, rec)
			if tc.kindNil {
				td.Kind.C = nil
			}
			removed := false
			td.Hooks.RemoveKubeconfig = func() error { removed = tc.removeErr == nil; return tc.removeErr }
			err := td.cleanup(ctx, tc.ca, false)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want %q, got %v", tc.wantErr, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if removed != tc.wantRemoved {
				t.Errorf("file removed %v, want %v", removed, tc.wantRemoved)
			}
			if !strings.Contains(td.Out.(*bytes.Buffer).String(), tc.wantLog) {
				t.Errorf("log %q lacks %q", td.Out.(*bytes.Buffer).String(), tc.wantLog)
			}
			if rec.deletedKind {
				t.Error("a kind not created by this teardown stays")
			}
		})
	}
}

func TestTeardownKindNameDefault(t *testing.T) {
	if got := (&Teardown{}).kindName(); got != defaultKindName {
		t.Errorf("got %q", got)
	}
	if got := (&Teardown{Kind: Side{Name: "kind-x"}}).kindName(); got != "kind-x" {
		t.Errorf("got %q", got)
	}
}

// A rerun after this teardown's own failed move that cannot lift that pause
// stops there: nothing is moved.
func TestTeardownRerunStopsWhenItsPauseCannotBeLifted(t *testing.T) {
	ctx := context.Background()
	bare := newClient(t, selfObjects(t)...)
	if err := setPaused(ctx, bare, true); err != nil {
		t.Fatal(err)
	}
	kind := newClient(t, labelledCRDs()...)
	recordIn(t, kind, map[string]string{"cluster": name, "phase": phaseMoving, "ca": selfCA()})
	rec := &tdRecorder{takeOver: true}
	self := faulty(t, bare, &fault{verb: "patch", kind: lcCluster, err: errBoom})
	if err := teardownFor(t, self, kind, rec).Run(ctx); !errors.Is(err, errBoom) || len(rec.moves) != 0 {
		t.Fatalf("got %v, moves %v", err, rec.moves)
	}
}
