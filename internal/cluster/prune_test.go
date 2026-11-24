package cluster

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
	pc := func(opts PruneOptions) pruneContext {
		return pruneContext{
			cluster: "c", opts: opts,
			inUse:          map[string]bool{kindHydraMachineTemplate + "/c-cp-used": true, kindKubeadmConfigTemplate + "/c-w-used": true},
			renderedAddons: map[string]bool{"kept": true},
		}
	}
	for _, tc := range []struct {
		name string
		obj  *unstructured.Unstructured
		opts PruneOptions
		want PruneAction
		why  string
	}{
		{"hydra's proxy, unprotected addon removed", proxy("c-plain", "plain"), PruneOptions{}, PruneDelete, "uninstalls"},
		{"hydra's proxy, protected addon", proxy("c-guarded", "guarded"), PruneOptions{}, PruneRefused, "--allow-remove=guarded"},
		{"hydra's proxy, protected addon allowed", proxy("c-guarded", "guarded"), PruneOptions{AllowRemove: []string{"guarded"}}, PruneDelete, "--allow-remove"},
		{"another addon allowed is not this one", proxy("c-guarded", "guarded"), PruneOptions{AllowRemove: []string{"plain"}}, PruneRefused, ""},
		{"hydra's proxy, uncatalogued addon", proxy("c-gone", "gone"), PruneOptions{}, PruneRefused, "--allow-remove=gone"},
		{"hydra's proxy, uncatalogued addon allowed", proxy("c-gone", "gone"), PruneOptions{AllowRemove: []string{"gone"}}, PruneDelete, ""},
		// Deleting a duplicate would uninstall the release the listed addon uses.
		{"duplicate proxy for an addon still listed", proxy("c-kept-copy", "kept"), PruneOptions{AllowRemove: []string{"kept"}}, PruneRefused, "still lists"},
		{"duplicate proxy allowed by proxy name", proxy("c-kept-copy", "kept"), PruneOptions{AllowRemove: []string{"proxy/c-kept-copy"}}, PruneDelete, ""},
		{"not named like hydra's, unprotected addon", proxy("handmade", "plain"), PruneOptions{}, PruneRefused, "--allow-remove=proxy/handmade"},
		{"proxy without addon label", proxy("c-hand", ""), PruneOptions{}, PruneRefused, "--allow-remove=proxy/c-hand"},
		{"proxy without addon label, bare name is not enough", proxy("c-hand", ""), PruneOptions{AllowRemove: []string{"c-hand"}}, PruneRefused, ""},
		{"proxy without addon label allowed as proxy/", proxy("c-hand", ""), PruneOptions{AllowRemove: []string{"proxy/c-hand"}}, PruneDelete, ""},
		{"unused machine template", labelled(hmtGVK, "c-cp-old", "c"), PruneOptions{}, PruneDelete, "unused"},
		{"used machine template", labelled(hmtGVK, "c-cp-used", "c"), PruneOptions{Pools: []string{"c-cp-used"}}, PruneInUse, "later apply"},
		{"unused bootstrap template", labelled(kctGVK, "c-w-old", "c"), PruneOptions{}, PruneDelete, ""},
		{"used bootstrap template", labelled(kctGVK, "c-w-used", "c"), PruneOptions{}, PruneInUse, ""},
		{"removed pool", labelled(mdGVK, "c-old", "c"), PruneOptions{}, PruneNeedsFlag, "--prune-pools=old"},
		{"removed pool named", labelled(mdGVK, "c-old", "c"), PruneOptions{Pools: []string{"old"}}, PruneDelete, "VMs"},
		{"removed pool named by MachineDeployment", labelled(mdGVK, "c-old", "c"), PruneOptions{Pools: []string{"c-old"}}, PruneDelete, ""},
		{"another pool named is not this one", labelled(mdGVK, "c-old", "c"), PruneOptions{Pools: []string{"other"}}, PruneNeedsFlag, ""},
		{kindHydraCluster, labelled(hydraClusterGVK, "c-x", "c"), PruneOptions{Pools: []string{"x"}, AllowRemove: []string{"c-x"}}, PruneNever, ""},
		{"control plane", labelled(kcpGVK, "c-x", "c"), PruneOptions{Pools: []string{"x"}}, PruneNever, ""},
		{"health check", labelled(mhcGVK, "c-x", "c"), PruneOptions{}, PruneNever, ""},
		{"CNI set", labelled(crsGVK, "c-cilium", "c"), PruneOptions{}, PruneNever, ""},
		{"CNI ConfigMap", labelled(schema.GroupVersionKind{Version: "v1", Kind: kindConfigMap}, "c-cilium", "c"), PruneOptions{}, PruneNever, ""},
	} {
		got := classify(tc.obj, pc(tc.opts))
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
	md := labelled(mdGVK, "c-w", "c")
	_ = unstructured.SetNestedField(md.Object, "c-w-md", "spec", "template", "spec", "infrastructureRef", "name")
	kc := obj(kubeadmConfigGVK, "ns", "c-w-kc1", nil)
	kc.SetLabels(map[string]string{render.ClusterLabelKey: "c"}) // only hydra's label
	kc.SetAnnotations(map[string]string{clonedFromName: "c-w-kct-machine"})
	deleting := labelled(hmtGVK, "c-cp-going", "c")
	deleting.SetFinalizers([]string{"x"})
	objs := []client.Object{
		kcp, ms, hm, md, kc,
		labelled(hmtGVK, "c-w-md", "c"), labelled(kctGVK, "c-w-kct-machine", "c"),
		labelled(hmtGVK, "c-cp-current", "c"), labelled(hmtGVK, "c-w-ms", "c"), labelled(hmtGVK, "c-cp-machine", "c"),
		labelled(hmtGVK, "c-cp-stale", "c"), labelled(kctGVK, "c-w-kct-ms", "c"), deleting,
		proxy("c-plain", "plain"), labelled(hmtGVK, "d-cp-other", "d"),
	}
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
	if err := c.Delete(ctx, deleting); err != nil {
		t.Fatal(err)
	}
	m := &Manager{C: c, Out: &bytes.Buffer{}}
	rendered := []*unstructured.Unstructured{kcp, md, labelled(hmtGVK, "c-cp-current", "c")}
	orphans, err := m.PlanPrune(ctx, rendered, "ns", "c", PruneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]PruneAction{}
	for _, o := range orphans {
		got[o.Object.GetKind()+"/"+o.Object.GetName()] = o.Action
	}
	want := map[string]PruneAction{
		kindHydraMachineTemplate + "/c-w-ms":           PruneInUse,
		kindHydraMachineTemplate + "/c-cp-machine":     PruneInUse,
		kindHydraMachineTemplate + "/c-cp-stale":       PruneDelete,
		kindKubeadmConfigTemplate + "/c-w-kct-ms":      PruneInUse,
		kindHydraMachineTemplate + "/c-w-md":           PruneInUse,
		kindKubeadmConfigTemplate + "/c-w-kct-machine": PruneInUse,
		render.KindHelmChartProxy + "/c-plain":         PruneDelete,
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
	n, skipped, err := (&Manager{C: c, Out: &out}).Prune(ctx, orphans)
	if err != nil || n != 1 || skipped != 1 {
		t.Fatalf("n=%d skipped=%d err=%v\n%s", n, skipped, err, out.String())
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

// A reference kind that is not served means no references only when the
// templates it protects are not served either; a kind that cannot be listed
// for another reason (Forbidden) stops the prune.
func TestTemplatesInUseFailsClosed(t *testing.T) {
	ctx := context.Background()
	failing := func(kind string, err error) interceptor.Funcs {
		return interceptor.Funcs{List: func(ctx context.Context, w client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if u, ok := list.(*unstructured.UnstructuredList); ok && u.GroupVersionKind().Kind == kind+"List" {
				return err
			}
			return w.List(ctx, list, opts...)
		}}
	}
	served := meta.NewDefaultRESTMapper(nil)
	served.Add(hmtGVK, meta.RESTScopeNamespace)
	served.Add(kctGVK, meta.RESTScopeNamespace)
	notServed := meta.NewDefaultRESTMapper(nil)
	noKind := &meta.NoKindMatchError{GroupKind: hydraMachineGVK.GroupKind()}
	for _, tc := range []struct {
		name    string
		mapper  meta.RESTMapper
		funcs   interceptor.Funcs
		wantErr string
	}{
		{"HydraMachine not served, its template served", served, failing("HydraMachine", noKind), "listing HydraMachine"},
		{"HydraMachine and its template not served", notServed, failing("HydraMachine", noKind), ""},
		{"MachineSets forbidden", served, failing("MachineSet", apierrors.NewForbidden(schema.GroupResource{Resource: "machinesets"}, "", errors.New("denied"))), "listing MachineSet"},
	} {
		c := fake.NewClientBuilder().WithScheme(scheme(t)).WithRESTMapper(tc.mapper).WithInterceptorFuncs(tc.funcs).Build()
		_, err := (&Manager{C: c, Out: &bytes.Buffer{}}).templatesInUse(ctx, "ns", "c")
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: want %q, got %v", tc.name, tc.wantErr, err)
		}
	}
}
