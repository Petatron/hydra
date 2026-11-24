package cluster

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Petatron/hydra/internal/addons"
	"github.com/Petatron/hydra/internal/render"
)

var (
	hmtGVK = schema.GroupVersionKind{Group: infraGroup, Version: infraVersion, Kind: kindHydraMachineTemplate}
	kctGVK = schema.GroupVersionKind{Group: bootstrapGroup, Version: capiVersion, Kind: kindKubeadmConfigTemplate}
	mhcGVK = schema.GroupVersionKind{Group: capiGroup, Version: capiVersion, Kind: "MachineHealthCheck"}
	crsGVK = schema.GroupVersionKind{Group: addonsGroup, Version: capiVersion, Kind: "ClusterResourceSet"}
)

// withCatalogue makes "guarded" a protected addon and "plain" an
// unprotected one for the test.
func withCatalogue(t *testing.T) {
	t.Helper()
	orig := lookupAddon
	t.Cleanup(func() { lookupAddon = orig })
	lookupAddon = func(name string) (addons.Addon, bool) {
		switch name {
		case "guarded":
			return addons.Addon{Name: name, Protected: true}, true
		case "plain":
			return addons.Addon{Name: name}, true
		}
		return addons.Addon{}, false
	}
}

func proxy(name, addon string) *unstructured.Unstructured {
	o := labelled(hcpGVK, name, "c")
	if addon != "" {
		l := o.GetLabels()
		l[render.AddonLabelKey] = addon
		o.SetLabels(l)
	}
	return o
}

// The decision table, one row per case (ADR-002 decisions 3 and 4).
func TestClassify(t *testing.T) {
	withCatalogue(t)
	inUse := map[string]bool{kindHydraMachineTemplate + "/c-cp-used": true, kindKubeadmConfigTemplate + "/c-w-used": true}
	for _, tc := range []struct {
		name string
		obj  *unstructured.Unstructured
		opts PruneOptions
		want PruneAction
		why  string
	}{
		{"unprotected addon", proxy("c-plain", "plain"), PruneOptions{}, PruneDelete, "uninstalls"},
		{"protected addon", proxy("c-guarded", "guarded"), PruneOptions{}, PruneRefused, "--allow-remove=guarded"},
		{"protected addon allowed", proxy("c-guarded", "guarded"), PruneOptions{AllowRemove: []string{"guarded"}}, PruneDelete, "--allow-remove"},
		{"another addon allowed is not this one", proxy("c-guarded", "guarded"), PruneOptions{AllowRemove: []string{"plain"}}, PruneRefused, ""},
		{"uncatalogued addon", proxy("c-gone", "gone"), PruneOptions{}, PruneRefused, "--allow-remove=gone"},
		{"uncatalogued addon allowed", proxy("c-gone", "gone"), PruneOptions{AllowRemove: []string{"gone"}}, PruneDelete, ""},
		{"proxy without addon label", proxy("c-hand", ""), PruneOptions{}, PruneRefused, "--allow-remove=c-hand"},
		{"proxy without addon label allowed by name", proxy("c-hand", ""), PruneOptions{AllowRemove: []string{"c-hand"}}, PruneDelete, ""},
		{"unused machine template", labelled(hmtGVK, "c-cp-old", "c"), PruneOptions{}, PruneDelete, "unused"},
		{"used machine template", labelled(hmtGVK, "c-cp-used", "c"), PruneOptions{Pools: true}, PruneInUse, "later apply"},
		{"unused bootstrap template", labelled(kctGVK, "c-w-old", "c"), PruneOptions{}, PruneDelete, ""},
		{"used bootstrap template", labelled(kctGVK, "c-w-used", "c"), PruneOptions{}, PruneInUse, ""},
		{"removed pool", labelled(mdGVK, "c-old", "c"), PruneOptions{}, PruneNeedsFlag, "--prune-pools"},
		{"removed pool with --prune-pools", labelled(mdGVK, "c-old", "c"), PruneOptions{Pools: true}, PruneDelete, "VMs"},
		{kindHydraCluster, labelled(hydraClusterGVK, "c-x", "c"), PruneOptions{Pools: true, AllowRemove: []string{"c-x"}}, PruneNever, ""},
		{"control plane", labelled(kcpGVK, "c-x", "c"), PruneOptions{Pools: true}, PruneNever, ""},
		{"health check", labelled(mhcGVK, "c-x", "c"), PruneOptions{Pools: true}, PruneNever, ""},
		{"CNI set", labelled(crsGVK, "c-cilium", "c"), PruneOptions{Pools: true}, PruneNever, ""},
		{"CNI ConfigMap", labelled(schema.GroupVersionKind{Version: "v1", Kind: kindConfigMap}, "c-cilium", "c"), PruneOptions{}, PruneNever, ""},
	} {
		got := classify(tc.obj, tc.opts, inUse)
		if got.Action != tc.want || !strings.Contains(got.Why, tc.why) {
			t.Errorf("%s: got %s (%s), want %s containing %q", tc.name, got.Action, got.Why, tc.want, tc.why)
		}
	}
}

// PlanPrune skips what is rendered or already being deleted, and sees which
// templates live objects still use: the control plane, a pool, a MachineSet,
// and machines cloned from one.
func TestPlanPrune(t *testing.T) {
	withCatalogue(t)
	ctx := context.Background()
	kcp := labelled(kcpGVK, "c-cp", "c")
	_ = unstructured.SetNestedField(kcp.Object, "c-cp-current", "spec", "machineTemplate", "spec", "infrastructureRef", "name")
	ms := obj(msGVK, "ns", "c-w-abc", nil)
	ms.SetLabels(map[string]string{clusterNameLabel: "c"})
	_ = unstructured.SetNestedField(ms.Object, "c-w-ms", "spec", "template", "spec", "infrastructureRef", "name")
	_ = unstructured.SetNestedField(ms.Object, "c-w-kct-ms", "spec", "template", "spec", "bootstrap", "configRef", "name")
	hm := obj(hydraMachineGVK, "ns", "c-cp-x1", nil)
	hm.SetLabels(map[string]string{clusterNameLabel: "c"})
	hm.SetAnnotations(map[string]string{clonedFromName: "c-cp-machine"})
	deleting := labelled(hmtGVK, "c-cp-going", "c")
	deleting.SetFinalizers([]string{"x"})
	objs := []client.Object{
		kcp, ms, hm,
		labelled(hmtGVK, "c-cp-current", "c"), labelled(hmtGVK, "c-w-ms", "c"), labelled(hmtGVK, "c-cp-machine", "c"),
		labelled(hmtGVK, "c-cp-stale", "c"), labelled(kctGVK, "c-w-kct-ms", "c"), deleting,
		proxy("c-plain", "plain"), labelled(hmtGVK, "d-cp-other", "d"),
	}
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
	if err := c.Delete(ctx, deleting); err != nil {
		t.Fatal(err)
	}
	m := &Manager{C: c, Out: &bytes.Buffer{}}
	rendered := []*unstructured.Unstructured{kcp, labelled(hmtGVK, "c-cp-current", "c")}
	orphans, err := m.PlanPrune(ctx, rendered, "ns", "c", PruneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]PruneAction{}
	for _, o := range orphans {
		got[o.Object.GetKind()+"/"+o.Object.GetName()] = o.Action
	}
	want := map[string]PruneAction{
		kindHydraMachineTemplate + "/c-w-ms":       PruneInUse,
		kindHydraMachineTemplate + "/c-cp-machine": PruneInUse,
		kindHydraMachineTemplate + "/c-cp-stale":   PruneDelete,
		kindKubeadmConfigTemplate + "/c-w-kct-ms":  PruneInUse,
		render.KindHelmChartProxy + "/c-plain":     PruneDelete,
	}
	if len(got) != len(want) {
		t.Fatalf("orphans: %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q, want %q", k, got[k], v)
		}
	}
}

// Prune deletes only what is marked for deletion, and skips an object that
// changed after it was classified.
func TestPrune(t *testing.T) {
	ctx := context.Background()
	stale := labelled(hmtGVK, "c-cp-stale", "c")
	kept := labelled(mdGVK, "c-old", "c")
	changed := labelled(hmtGVK, "c-cp-changed", "c")
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(stale, kept, changed).Build()
	read := func(o *unstructured.Unstructured) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(o.GroupVersionKind())
		if err := c.Get(ctx, client.ObjectKeyFromObject(o), u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	orphans := []Orphan{
		{Object: read(stale), Action: PruneDelete},
		{Object: read(kept), Action: PruneNeedsFlag},
		{Object: read(changed), Action: PruneDelete},
	}
	// Someone edits it between classification and deletion.
	live := read(changed)
	live.SetAnnotations(map[string]string{"edited": time.Now().String()})
	if err := c.Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	n, err := (&Manager{C: c, Out: &out}).Prune(ctx, orphans)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v\n%s", n, err, out.String())
	}
	for _, o := range []*unstructured.Unstructured{kept, changed} {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(o.GroupVersionKind())
		if err := c.Get(ctx, client.ObjectKeyFromObject(o), u); err != nil {
			t.Errorf("%s must survive: %v", o.GetName(), err)
		}
	}
	if !strings.Contains(out.String(), "skipped") {
		t.Errorf("the changed object must be reported as skipped:\n%s", out.String())
	}
}

func TestRefusals(t *testing.T) {
	if Refusals([]Orphan{{Object: proxy("c-a", "a"), Action: PruneDelete}}) != nil {
		t.Error("nothing refused")
	}
	err := Refusals([]Orphan{{Object: proxy("c-g", "guarded"), Action: PruneRefused, Why: "pass --allow-remove=guarded"}})
	if err == nil || !strings.Contains(err.Error(), "not applying") || !strings.Contains(err.Error(), "--allow-remove=guarded") {
		t.Errorf("got %v", err)
	}
}
