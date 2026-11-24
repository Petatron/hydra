package cluster

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Petatron/hydra/internal/addons"
	"github.com/Petatron/hydra/internal/render"
)

// PruneAction is what `hydra cluster apply` does with an object labelled for
// the cluster that hydra.yaml no longer renders (ADR-002 decisions 3 and 4).
type PruneAction string

// Prune actions.
const (
	// PruneDelete: apply deletes it.
	PruneDelete PruneAction = "delete"
	// PruneNeedsFlag: kept; a flag would delete it (--prune-pools).
	PruneNeedsFlag PruneAction = "kept: needs a flag"
	// PruneInUse: kept while something still references it; a later apply
	// deletes it.
	PruneInUse PruneAction = "kept: still referenced"
	// PruneNever: kept; apply never deletes this kind.
	PruneNever PruneAction = "kept: never pruned"
	// PruneRefused: apply refuses to run at all until it is allowed
	// (--allow-remove) or put back in hydra.yaml.
	PruneRefused PruneAction = "refused"
)

// PruneOptions are apply's pruning flags.
type PruneOptions struct {
	// Pools names removed node pools apply may delete, with their machines
	// and VMs: by pool name (as in hydra.yaml) or MachineDeployment name.
	// Named one by one, so a wrong or stale hydra.yaml cannot take them all.
	Pools []string
	// AllowRemove names what apply may remove although it would refuse:
	// protected or uncatalogued addons by addon name, and any other proxy as
	// proxy/<name>.
	AllowRemove []string
}

// Orphan is one object hydra.yaml no longer renders, and what apply does with
// it.
type Orphan struct {
	Object *unstructured.Unstructured
	Action PruneAction
	// Why explains the action, with the flag or fix when there is one.
	Why string
}

// String is the orphan as plan and apply print it.
func (o Orphan) String() string {
	return fmt.Sprintf("%s %s (%s)", o.Action, describe(o.Object), o.Why)
}

// pruneContext is what classify needs beyond the object.
type pruneContext struct {
	cluster string
	opts    PruneOptions
	// inUse holds the template names something live still references, as
	// Kind/name.
	inUse map[string]bool
	// renderedAddons are the addons hydra.yaml still lists.
	renderedAddons map[string]bool
	// poolFlag is the --prune-pools value that names each removed
	// MachineDeployment and no other, by MachineDeployment name; "" when no
	// value does. Missing means the pool name.
	poolFlag map[string]string
}

// ProxyPrefix marks a HelmChartProxy named in --allow-remove by its object
// name rather than its addon.
const ProxyPrefix = "proxy/"

// classify is the decision table.
func classify(o *unstructured.Unstructured, pc pruneContext) Orphan {
	out := Orphan{Object: o}
	switch o.GetKind() {
	case render.KindHelmChartProxy:
		out.Action, out.Why = classifyProxy(o, pc)
	case kindHydraMachineTemplate, kindKubeadmConfigTemplate:
		if pc.inUse[o.GetKind()+"/"+o.GetName()] {
			out.Action, out.Why = PruneInUse, "a machine, MachineSet or the control plane still uses it; a later apply removes it"
		} else {
			out.Action, out.Why = PruneDelete, "superseded and unused"
		}
	case mdGVK.Kind:
		flag, ok := pc.poolFlag[o.GetName()]
		if !ok {
			flag = poolOf(o.GetName(), pc.cluster)
		}
		switch {
		case slices.ContainsFunc(pc.opts.Pools, func(t string) bool { return namesPool(t, o.GetName(), pc.cluster) }):
			out.Action, out.Why = PruneDelete, "pool removed from hydra.yaml; --prune-pools deletes its machines and VMs"
		case flag == "":
			out.Action, out.Why = PruneNeedsFlag, "pool removed from hydra.yaml; no --prune-pools value names it alone, so delete the MachineDeployment by hand"
		default:
			out.Action, out.Why = PruneNeedsFlag, "pool removed from hydra.yaml; deleting it deletes its machines and VMs, so it needs --prune-pools="+flag
		}
	default:
		// The Cluster's own objects and the CNI's are never pruned: they are
		// removed with the cluster, or switched by hand (cni: external).
		out.Action, out.Why = PruneNever, "apply never deletes a "+o.GetKind()
	}
	return out
}

// poolOf is the pool a MachineDeployment belongs to: its name without the
// cluster prefix.
func poolOf(md, cluster string) string { return strings.TrimPrefix(md, cluster+"-") }

// namesPool reports whether a --prune-pools value names a MachineDeployment,
// by pool name or by its own name.
func namesPool(token, md, cluster string) bool { return token == md || token == poolOf(md, cluster) }

// poolFlags picks, for each removed MachineDeployment, the --prune-pools value
// that names it and no other removed one. Pool names and MachineDeployment
// names overlap: in cluster edge, MachineDeployment edge-gpu is pool gpu, and
// edge-edge-gpu is pool edge-gpu.
func poolFlags(mds []string, cluster string) map[string]string {
	out := map[string]string{}
	for _, md := range mds {
		out[md] = ""
		for _, t := range []string{poolOf(md, cluster), md} {
			if countNamed(t, mds, cluster) == 1 {
				out[md] = t
				break
			}
		}
	}
	return out
}

// countNamed is how many of mds a --prune-pools value names.
func countNamed(token string, mds []string, cluster string) int {
	n := 0
	for _, md := range mds {
		if namesPool(token, md, cluster) {
			n++
		}
	}
	return n
}

// refuseAmbiguousPools refuses every removed MachineDeployment a
// --prune-pools value names when it names more than one, so one value never
// deletes two pools' machines and VMs.
func refuseAmbiguousPools(out []Orphan, mds []string, pc pruneContext) {
	for _, t := range pc.opts.Pools {
		if countNamed(t, mds, pc.cluster) < 2 {
			continue
		}
		var named []string
		for _, md := range mds {
			if namesPool(t, md, pc.cluster) {
				named = append(named, md)
			}
		}
		for i := range out {
			o := out[i].Object
			if o.GetKind() != mdGVK.Kind || !namesPool(t, o.GetName(), pc.cluster) {
				continue
			}
			fix := "delete the MachineDeployment by hand"
			if f := pc.poolFlag[o.GetName()]; f != "" {
				fix = "name this pool as --prune-pools=" + f
			}
			out[i].Action = PruneRefused
			out[i].Why = fmt.Sprintf("--prune-pools=%s names more than one removed pool (MachineDeployments %s), so it deletes none; %s",
				t, strings.Join(named, ", "), fix)
		}
	}
}

// classifyProxy decides a HelmChartProxy. Without a flag apply deletes only
// hydra's own proxy (named <cluster>-<addon>) for a catalogued, unprotected
// addon hydra.yaml no longer lists. Deleting a proxy uninstalls its release,
// and any proxy for the same addon shares that release, so anything else is
// removed only when named.
func classifyProxy(o *unstructured.Unstructured, pc pruneContext) (PruneAction, string) {
	name := o.GetLabels()[render.AddonLabelKey]
	byProxy := ProxyPrefix + o.GetName()
	allowedProxy := slices.Contains(pc.opts.AllowRemove, byProxy)
	hydras := name != "" && o.GetName() == render.AddonName(pc.cluster, name)
	entry, known := lookupAddon(name)
	switch {
	case allowedProxy:
		return PruneDelete, "removal allowed by --allow-remove=" + byProxy + "; deleting the proxy uninstalls its release"
	case name != "" && pc.renderedAddons[name]:
		return PruneRefused, fmt.Sprintf("another proxy for addon %s, which hydra.yaml still lists; deleting it would uninstall the release the addon uses. Remove it by hand, or pass --allow-remove=%s", name, byProxy)
	case !hydras:
		return PruneRefused, "not a proxy hydra made, so not removed on its own; pass --allow-remove=" + byProxy + " to uninstall its release"
	case known && !entry.Protected:
		return PruneDelete, "addon " + name + " is no longer in hydra.yaml; deleting its proxy uninstalls it"
	case slices.Contains(pc.opts.AllowRemove, name):
		return PruneDelete, "addon " + name + " removal allowed by --allow-remove; deleting its proxy uninstalls it"
	case known:
		return PruneRefused, fmt.Sprintf("addon %s is protected; put it back in hydra.yaml, or pass --allow-remove=%s to uninstall it", name, name)
	}
	return PruneRefused, fmt.Sprintf("addon %s is not one this hydra knows, so not removed on its own; pass --allow-remove=%s to uninstall it", name, name)
}

// lookupAddon finds a catalogue entry; tests replace it to cover protected
// addons the catalogue does not have yet.
var lookupAddon = addons.Lookup

// kindKubeadmConfigTemplate is a pool's bootstrap template; kindHydraCluster
// the cluster's infrastructure object.
const (
	kindKubeadmConfigTemplate = "KubeadmConfigTemplate"
	kindHydraCluster          = "HydraCluster"
)

// clonedFromName is the annotation Cluster API puts on an object cloned from
// a template, naming the template.
const clonedFromName = "cluster.x-k8s.io/cloned-from-name"

// PlanPrune lists the objects labelled for the cluster that objs no longer
// include, each with the action apply takes. Nothing is changed.
func (m *Manager) PlanPrune(ctx context.Context, objs []*unstructured.Unstructured, namespace, cluster string, opts PruneOptions) ([]Orphan, error) {
	rendered := map[string]bool{}
	for _, o := range objs {
		rendered[o.GetKind()+"/"+o.GetName()] = true
	}
	inUse, err := m.templatesInUse(ctx, namespace, cluster)
	if err != nil {
		return nil, err
	}
	pc := pruneContext{cluster: cluster, opts: opts, inUse: inUse, renderedAddons: map[string]bool{}}
	for _, o := range objs {
		if o.GetKind() == render.KindHelmChartProxy {
			pc.renderedAddons[o.GetLabels()[render.AddonLabelKey]] = true
		}
	}
	var candidates []*unstructured.Unstructured
	var mds []string
	for _, gvk := range ownedKinds {
		items, err := m.listLabelled(ctx, namespace, cluster, gvk)
		if err != nil {
			return nil, err
		}
		for i := range items {
			o := &items[i]
			if rendered[o.GetKind()+"/"+o.GetName()] || o.GetDeletionTimestamp() != nil {
				continue
			}
			candidates = append(candidates, o)
			if o.GetKind() == mdGVK.Kind {
				mds = append(mds, o.GetName())
			}
		}
	}
	pc.poolFlag = poolFlags(mds, cluster)
	out := make([]Orphan, 0, len(candidates))
	for _, o := range candidates {
		out = append(out, classify(o, pc))
	}
	refuseAmbiguousPools(out, mds, pc)
	slices.SortFunc(out, func(a, b Orphan) int { return strings.Compare(describe(a.Object), describe(b.Object)) })
	return out, nil
}

// Refusals returns the orphans that stop apply, as one error, or nil.
func Refusals(orphans []Orphan) error {
	var lines []string
	for _, o := range orphans {
		if o.Action == PruneRefused {
			lines = append(lines, describe(o.Object)+": "+o.Why)
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return fmt.Errorf("not applying: hydra.yaml no longer lists these, and removing them is not allowed:\n  %s", strings.Join(lines, "\n  "))
}

// Prune deletes the orphans marked PruneDelete, each only if it is still the
// version that was classified, and returns how many it deleted and how many
// it skipped because they changed after they were classified.
func (m *Manager) Prune(ctx context.Context, orphans []Orphan) (deleted, skipped int, err error) {
	n := 0
	for _, o := range orphans {
		if o.Action != PruneDelete {
			continue
		}
		switch err := m.deleteExactly(ctx, o.Object); {
		case errors.Is(err, errChanged):
			m.logf("skipped %s: it changed after it was checked; apply again to reconsider it", describe(o.Object))
			skipped++
		case err != nil:
			return n, skipped, fmt.Errorf("pruning %s: %w", describe(o.Object), err)
		default:
			m.logf("pruned %s (%s)", describe(o.Object), o.Why)
			n++
		}
	}
	return n, skipped, nil
}

// templatesInUse names, as Kind/name, every machine or bootstrap template
// the cluster's live objects reference: the control plane, each
// MachineDeployment and MachineSet, and every HydraMachine and KubeadmConfig
// cloned from one (which keeps a template until its machines are gone).
func (m *Manager) templatesInUse(ctx context.Context, namespace, cluster string) (map[string]bool, error) {
	inUse := map[string]bool{}
	add := func(kind, name string) {
		if name != "" {
			inUse[kind+"/"+name] = true
		}
	}
	hmt := schema.GroupVersionKind{Group: infraGroup, Version: infraVersion, Kind: kindHydraMachineTemplate}
	kct := schema.GroupVersionKind{Group: bootstrapGroup, Version: capiVersion, Kind: kindKubeadmConfigTemplate}
	refs := []struct {
		gvk schema.GroupVersionKind
		// protects are the template kinds this kind's objects reference.
		protects []schema.GroupVersionKind
		read     func(o *unstructured.Unstructured)
	}{
		{kcpGVK, []schema.GroupVersionKind{hmt}, func(o *unstructured.Unstructured) {
			n, _, _ := unstructured.NestedString(o.Object, "spec", "machineTemplate", "spec", "infrastructureRef", "name")
			add(kindHydraMachineTemplate, n)
		}},
		{mdGVK, []schema.GroupVersionKind{hmt, kct}, func(o *unstructured.Unstructured) { addPoolRefs(o, add) }},
		{msGVK, []schema.GroupVersionKind{hmt, kct}, func(o *unstructured.Unstructured) { addPoolRefs(o, add) }},
		{hydraMachineGVK, []schema.GroupVersionKind{hmt}, func(o *unstructured.Unstructured) { add(kindHydraMachineTemplate, o.GetAnnotations()[clonedFromName]) }},
		{kubeadmConfigGVK, []schema.GroupVersionKind{kct}, func(o *unstructured.Unstructured) { add(kindKubeadmConfigTemplate, o.GetAnnotations()[clonedFromName]) }},
	}
	for _, r := range refs {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(r.gvk.GroupVersion().WithKind(r.gvk.Kind + "List"))
		// Cluster API labels every object it makes for a cluster with its
		// name; hydra's own objects carry hydra.io/cluster. Read both.
		for _, sel := range []client.MatchingLabels{{clusterNameLabel: cluster}, {render.ClusterLabelKey: cluster}} {
			list.Items = nil
			if err := m.C.List(ctx, list, client.InNamespace(namespace), sel); err != nil {
				// A kind not served holds no references only if the
				// templates it would reference are not served either;
				// otherwise "none" is a guess, and the unsafe one.
				if meta.IsNoMatchError(err) && !m.anyServed(r.protects) {
					break
				}
				return nil, fmt.Errorf("listing %s to see which templates are in use: %w", r.gvk.Kind, err)
			}
			for i := range list.Items {
				r.read(&list.Items[i])
			}
		}
	}
	return inUse, nil
}

// anyServed reports whether the management cluster serves any of kinds.
func (m *Manager) anyServed(kinds []schema.GroupVersionKind) bool {
	for _, k := range kinds {
		if _, err := m.C.RESTMapper().RESTMapping(k.GroupKind(), k.Version); err == nil || !meta.IsNoMatchError(err) {
			return true
		}
	}
	return false
}

// addPoolRefs records the templates a MachineDeployment or MachineSet uses.
func addPoolRefs(o *unstructured.Unstructured, add func(kind, name string)) {
	infra, _, _ := unstructured.NestedString(o.Object, "spec", "template", "spec", "infrastructureRef", "name")
	boot, _, _ := unstructured.NestedString(o.Object, "spec", "template", "spec", "bootstrap", "configRef", "name")
	add(kindHydraMachineTemplate, infra)
	add(kindKubeadmConfigTemplate, boot)
}

var (
	msGVK            = schema.GroupVersionKind{Group: capiGroup, Version: capiVersion, Kind: "MachineSet"}
	hydraMachineGVK  = schema.GroupVersionKind{Group: infraGroup, Version: infraVersion, Kind: "HydraMachine"}
	kubeadmConfigGVK = schema.GroupVersionKind{Group: bootstrapGroup, Version: capiVersion, Kind: "KubeadmConfig"}
)

// bootstrapGroup holds kubeadm's bootstrap configs and their templates.
const bootstrapGroup = "bootstrap.cluster.x-k8s.io"
