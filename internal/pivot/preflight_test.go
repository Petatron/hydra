package pivot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Petatron/hydra/internal/render"
)

func TestPollDefaults(t *testing.T) {
	if got := (&Pivot{}).poll(); got != 10*time.Second {
		t.Errorf("pivot poll %v", got)
	}
	if got := (&Teardown{}).poll(); got != 5*time.Second {
		t.Errorf("teardown poll %v", got)
	}
	if got := (&Pivot{Poll: time.Millisecond}).poll(); got != time.Millisecond {
		t.Errorf("an explicit poll must win, got %v", got)
	}
}

// Locate is three-valued at heart: a read failure on either side is an
// error, never "not there".
func TestLocate(t *testing.T) {
	ctx := context.Background()
	with := func() client.Client { return newClient(t, clusterObjects("x", true)...) }
	without := func() client.Client { return newClient(t) }
	broken := func() client.Client {
		return faulty(t, newClient(t), &fault{verb: "get", kind: lcCluster, err: errBoom})
	}
	uninstalled := func() client.Client {
		return faulty(t, newClient(t), &fault{verb: "get", kind: lcCluster, err: noMatch(lcCluster)})
	}
	for _, tc := range []struct {
		name     string
		src, tgt func() client.Client
		want     Where
		wantErr  string
	}{
		{lcNowhere, without, without, Nowhere, ""},
		{"no Cluster CRD counts as absent", uninstalled, with, OnTarget, ""},
		{"source", with, without, OnSource, ""},
		{"split", with, with, Split, ""},
		{"source unreadable", broken, with, Nowhere, "reading the bootstrap cluster"},
		{"target unreadable", with, broken, Nowhere, "reading cluster c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pivotFor(t, tc.src(), tc.tgt(), &recorder{}).Locate(ctx)
			if tc.wantErr != "" {
				if !errors.Is(err, errBoom) || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	if Split.String() != "on both (split)" || Nowhere.String() != lcNowhere {
		t.Error("Where strings")
	}
}

func TestPivotRunFindsNothingOrCannotRead(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	err := pivotFor(t, newClient(t), newClient(t), rec).Run(ctx, time.Second)
	if err == nil || !strings.Contains(err.Error(), "exists on neither") {
		t.Fatalf("got %v", err)
	}
	src := faulty(t, newClient(t), &fault{verb: "get", kind: lcCluster, err: errBoom})
	if err := pivotFor(t, src, newClient(t), rec).Run(ctx, time.Second); !errors.Is(err, errBoom) {
		t.Fatalf("got %v", err)
	}
	if len(rec.moves) != 0 {
		t.Errorf("moves %v", rec.moves)
	}
	// On the target, but its Cluster cannot be read back.
	tgt := faulty(t, newClient(t, clusterObjects("tgt", true)...), &fault{verb: "get", kind: lcCluster, skip: 1, err: errBoom})
	if err := pivotFor(t, newClient(t), tgt, rec).Run(ctx, time.Second); !errors.Is(err, errBoom) {
		t.Fatalf("got %v", err)
	}
}

// A split finished with --finish-split whose move fails reports the move's
// failure, and verifies nothing.
func TestPivotFinishSplitMoveFailure(t *testing.T) {
	src, tgt := splitByInterruptedPivot(t)
	p := pivotFor(t, src, tgt, &recorder{failFwd: true})
	p.FinishSplit = true
	err := p.Run(context.Background(), time.Second)
	if err == nil || !strings.Contains(err.Error(), "clusterctl move: move failed") {
		t.Fatalf("got %v", err)
	}
}

// Every preflight failure stops before the real move.
func TestPivotPreflightFailures(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		mutate func([]client.Object) []client.Object
		srcF   []*fault
		tgtF   []*fault
		hooks  func(*Pivot)
		want   string
	}{
		{name: "status unreadable", srcF: []*fault{{verb: "get", kind: lcCluster, skip: 1, err: errBoom}}, want: "boom"},
		{name: "Cluster re-read fails", srcF: []*fault{{verb: "get", kind: lcCluster, skip: 2, err: errBoom}}, want: "boom"},
		{name: "being deleted", mutate: func(o []client.Object) []client.Object {
			now := metav1.Now()
			o[0].SetDeletionTimestamp(&now)
			o[0].SetFinalizers([]string{"cluster.cluster.x-k8s.io"})
			return o
		}, want: "is being deleted; not moving it"},
		{name: "no machines", mutate: func(o []client.Object) []client.Object {
			var out []client.Object
			for _, x := range o {
				if x.GetObjectKind().GroupVersionKind() != hydraMachineGVK {
					out = append(out, x)
				}
			}
			return out
		}, want: "no HydraMachines found"},
		{name: "Clusters unlistable", srcF: []*fault{{verb: "list", kind: lcCluster, err: errBoom}}, want: "listing Clusters: boom"},
		{name: "machines unlistable", srcF: []*fault{{verb: "list", kind: lcHydraMachine, err: errBoom}}, want: "listing HydraMachines: boom"},
		{name: "HydraCluster unreadable", srcF: []*fault{{verb: "get", kind: "HydraCluster", err: errBoom}}, want: "reading HydraCluster ns/c: boom"},
		{name: "credentials re-read fails", srcF: []*fault{{verb: "get", kind: lcSecret, name: "c-pki", skip: 1, err: errBoom}}, want: "reading credentials Secret c-pki: boom"},
		{name: "a movable kind unlistable", srcF: []*fault{{verb: "list", kind: "KubeadmConfig", err: errBoom}}, want: "listing KubeadmConfig in ns: boom"},
		{name: "Secrets unlistable", srcF: []*fault{{verb: "list", kind: lcSecret, err: errBoom}}, want: "listing Secrets in ns: boom"},
		{name: "a CRD unreadable", srcF: []*fault{{verb: "get", kind: lcCRD, err: errBoom}}, want: "source: reading CRD"},
		{name: "providers fail to install", hooks: func(p *Pivot) {
			p.Hooks.EnsureProviders = func(context.Context, []byte) error { return errBoom }
		}, want: "installing providers on cluster c: boom"},
		{name: "target CRDs unlabelled", tgtF: []*fault{{verb: "get", kind: lcCRD, err: errBoom}}, want: "cluster c: reading CRD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := append(clusterObjects("src", true), labelledCRDs()...)
			if tc.mutate != nil {
				objs = tc.mutate(objs)
			}
			src, tgt := newClient(t, objs...), newClient(t, labelledCRDs()...)
			if tc.srcF != nil {
				src = faulty(t, src, tc.srcF...)
			}
			if tc.tgtF != nil {
				tgt = faulty(t, tgt, tc.tgtF...)
			}
			rec := &recorder{takeOver: true}
			p := pivotFor(t, src, tgt, rec)
			if tc.hooks != nil {
				tc.hooks(p)
			}
			err := p.Run(ctx, time.Second)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			for _, m := range rec.moves {
				if m != "dry" {
					t.Errorf("nothing may move after a failed preflight, got %v", rec.moves)
				}
			}
		})
	}
}

func TestPivotDryRunFailureStopsTheMove(t *testing.T) {
	src := newClient(t, append(clusterObjects("src", true), labelledCRDs()...)...)
	tgt := newClient(t, labelledCRDs()...)
	p := pivotFor(t, src, tgt, &recorder{})
	forward := false
	p.Hooks.Move = func(_ context.Context, _, _ []byte, _ string, dry bool) error {
		if dry {
			return errBoom
		}
		forward = true
		return nil
	}
	err := p.Run(context.Background(), time.Second)
	if err == nil || !strings.Contains(err.Error(), "clusterctl move dry run: boom") || forward {
		t.Fatalf("got %v, forward %v", err, forward)
	}
}

// A kind whose CRD is not installed holds nothing a move could carry.
func TestPivotStrangersSkipsUninstalledKinds(t *testing.T) {
	src := faulty(t, newClient(t, append(clusterObjects("src", true), labelledCRDs()...)...),
		&fault{verb: "list", kind: lcClusterClass, err: noMatch(lcClusterClass)})
	tgt := newClient(t, labelledCRDs()...)
	if err := pivotFor(t, src, tgt, &recorder{takeOver: true}).Run(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
}

// A reconciled addon is this cluster's, not a stranger: CAAPH labels its
// HelmReleaseProxy only with Cluster API's cluster-name label and owns it by
// the HelmChartProxy, never by the Cluster and never with hydra's label.
func TestPivotAcceptsReconciledAddon(t *testing.T) {
	hcp := addonProxy("True")
	hrp := u(schema.GroupVersionKind{Group: crsGVK.Group, Version: caaphAPIVersion, Kind: "HelmReleaseProxy"}, ns, "metrics-server-c-abcde", nil)
	hrp.SetLabels(map[string]string{clusterNameLabel: "c", "helmreleaseproxy.addons.cluster.x-k8s.io/helmchartproxy-name": "c-metrics-server"})
	hrp.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: crsGVK.Group + "/" + caaphAPIVersion, Kind: "HelmChartProxy", Name: "c-metrics-server", UID: "hcp-uid"}})
	src := newClient(t, append(append(clusterObjects("src", true), hcp, hrp), labelledCRDs()...)...)
	tgt := newClient(t, append(labelledCRDs(), addonProxy("True"))...)
	if err := pivotFor(t, src, tgt, &recorder{takeOver: true}).Run(context.Background(), time.Second); err != nil {
		t.Fatalf("a reconciled addon must not block the pivot: %v", err)
	}
}

// The addons the source held before the move must all be on the target:
// a target with none (where AddonsReady alone would pass) does not verify.
func TestPivotRequiresTheSourceAddonsOnTheTarget(t *testing.T) {
	src := newClient(t, append(append(clusterObjects("src", true), addonProxy("True")), labelledCRDs()...)...)
	tgt := newClient(t, labelledCRDs()...)
	err := pivotFor(t, src, tgt, &recorder{takeOver: true, lose: []string{"c-metrics-server"}}).Run(context.Background(), 200*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "addon HelmChartProxy c-metrics-server did not arrive") {
		t.Fatalf("got %v", err)
	}
}

// verifyOnce passes only for a live, resumed, taken-over target whose
// source no longer holds the Cluster.
func TestVerifyOnce(t *testing.T) {
	ctx := context.Background()
	machineNotReady := func(o []client.Object) []client.Object {
		for _, x := range o {
			if x.GetObjectKind().GroupVersionKind() == hydraMachineGVK {
				unstructured.RemoveNestedField(x.(*unstructured.Unstructured).Object, "status")
			}
		}
		return o
	}
	drop := func(gvk schema.GroupVersionKind) func([]client.Object) []client.Object {
		return func(o []client.Object) []client.Object {
			var out []client.Object
			for _, x := range o {
				if x.GetObjectKind().GroupVersionKind() != gvk {
					out = append(out, x)
				}
			}
			return out
		}
	}
	for _, tc := range []struct {
		name    string
		srcObjs []client.Object
		srcF    []*fault
		mutate  func([]client.Object) []client.Object
		tgtF    []*fault
		// expect is the addons the source held before the move.
		expect []string
		want   string
	}{
		{name: "verified", want: ""},
		{name: "source unreadable", srcF: []*fault{{verb: "get", kind: lcCluster, err: errBoom}}, want: "reading the bootstrap cluster"},
		{name: "source still holds", srcObjs: clusterObjects("src", true), want: "still holds the Cluster"},
		{name: "target status unreadable", tgtF: []*fault{{verb: "get", kind: lcCluster, err: errBoom}}, want: "boom"},
		{name: "target Cluster re-read fails", tgtF: []*fault{{verb: "get", kind: lcCluster, skip: 1, err: errBoom}}, want: "boom"},
		{name: "target still paused", mutate: func(o []client.Object) []client.Object {
			_ = unstructured.SetNestedField(o[0].(*unstructured.Unstructured).Object, true, "spec", "paused")
			return o
		}, want: "is still paused"},
		{name: "machines unlistable", tgtF: []*fault{{verb: "list", kind: lcHydraMachine, err: errBoom}}, want: "listing HydraMachines on cluster c"},
		{name: "no machines", mutate: drop(hydraMachineGVK), want: "no HydraMachines on cluster c yet"},
		{name: "machine not Ready", mutate: machineNotReady, want: "HydraMachine c-cp-1 is not Ready"},
		{name: "addon not Ready on the target", mutate: func(o []client.Object) []client.Object {
			return append(o, addonProxy("False"))
		}, want: "addons on cluster c are not all Ready"},
		{name: "addon Ready on the target", mutate: func(o []client.Object) []client.Object {
			return append(o, addonProxy("True"))
		}, want: ""},
		{name: "expected addon arrived and Ready", mutate: func(o []client.Object) []client.Object {
			return append(o, addonProxy("True"))
		}, expect: []string{"c-metrics-server"}, want: ""},
		// No proxies on the target at all: AddonsReady alone would pass.
		{name: "expected addon missing", expect: []string{"c-metrics-server"}, want: "addon HelmChartProxy c-metrics-server did not arrive"},
		{name: "only another addon on the target", mutate: func(o []client.Object) []client.Object {
			return append(o, addonProxy("True"))
		}, expect: []string{"c-cert-manager"}, want: "addon HelmChartProxy c-cert-manager did not arrive"},
		{name: "CNI proxy unreadable", tgtF: []*fault{{verb: "get", kind: "HelmChartProxy", err: errBoom}}, want: "reading the CNI HelmChartProxy"},
		// cni: external: no CNI label, and no CNI proxy to read.
		{name: "external CNI", mutate: func(o []client.Object) []client.Object {
			o[0].SetLabels(nil)
			return drop(hcpGVK)(drop(hrpGVK)(o))
		}, tgtF: []*fault{{verb: "get", kind: hcpGVK.Kind, err: errBoom}}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := newClient(t, tc.srcObjs...)
			if tc.srcF != nil {
				src = faulty(t, src, tc.srcF...)
			}
			objs := clusterObjects("tgt", true)
			if tc.mutate != nil {
				objs = tc.mutate(objs)
			}
			tgt := newClient(t, objs...)
			if tc.tgtF != nil {
				tgt = faulty(t, tgt, tc.tgtF...)
			}
			pv := pivotFor(t, src, tgt, &recorder{})
			pv.expectedAddons = tc.expect
			err := pv.verifyOnce(ctx)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

// addonProxy is cluster c's metrics-server HelmChartProxy with the given
// Ready status.
func addonProxy(ready string) client.Object {
	h := u(schema.GroupVersionKind{Group: crsGVK.Group, Version: "v1alpha1", Kind: "HelmChartProxy"}, ns, "c-metrics-server", map[string]any{
		"spec":   map[string]any{"version": "3.14.0"},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": ready}}},
	})
	h.SetLabels(map[string]string{render.ClusterLabelKey: "c", render.AddonLabelKey: "metrics-server"})
	return h
}

func TestConditionTrue(t *testing.T) {
	o := u(hydraMachineGVK, ns, "m", map[string]any{"status": map[string]any{"conditions": []any{
		"not a map",
		map[string]any{"type": "Ready", "status": "False"},
	}}})
	if conditionTrue(o, "Ready") {
		t.Error("Ready is False")
	}
	if conditionTrue(o, "Absent") {
		t.Error("an absent condition is not true")
	}
}

func TestOtherClustersListError(t *testing.T) {
	c := faulty(t, newClient(t), &fault{verb: "list", kind: lcCluster, err: errBoom})
	if _, err := otherClusters(context.Background(), c, ns, name); !errors.Is(err, errBoom) {
		t.Fatalf("got %v", err)
	}
}
