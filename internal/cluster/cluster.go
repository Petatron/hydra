// Package cluster submits a rendered cluster to a management cluster, reports
// its progress from controller-owned status, and deletes it.
//
// Everything here reads and writes Kubernetes objects; nothing is kept
// locally (ADR-001). Create is server-side apply under the field manager
// "hydra", so rerunning it converges instead of duplicating; waiting reports
// what Cluster API's controllers report, and a timeout ends only the wait.
package cluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/Petatron/hydra/internal/addons"
	"github.com/Petatron/hydra/internal/bootstrap"
	"github.com/Petatron/hydra/internal/render"
)

const (
	// FieldManagerPrefix starts the field manager that owns a cluster's
	// objects: "hydra-<cluster>". One manager per cluster makes the API server
	// itself refuse, atomically, an apply that would take a field another
	// cluster's manager owns (see FieldManagerFor).
	FieldManagerPrefix = "hydra-"

	capiGroup                = "cluster.x-k8s.io"
	capiVersion              = "v1beta2"
	kindConfigMap            = "ConfigMap"
	kindHydraMachineTemplate = "HydraMachineTemplate"
	clusterNameLabel         = "cluster.x-k8s.io/cluster-name"
	infraGroup               = "infrastructure.cluster.x-k8s.io"
	infraVersion             = "v1alpha1"
)

// ConditionTrue is a condition's status when it holds.
const ConditionTrue = "True"

var (
	clusterGVK = schema.GroupVersionKind{Group: capiGroup, Version: capiVersion, Kind: "Cluster"}
	kcpGVK     = schema.GroupVersionKind{Group: "controlplane.cluster.x-k8s.io", Version: capiVersion, Kind: "KubeadmControlPlane"}
	machineGVK = schema.GroupVersionKind{Group: capiGroup, Version: capiVersion, Kind: "Machine"}
	// crsGVK is the ClusterResourceSet hydra rendered for its CNI before
	// PET-100; State looks for one left on a cluster.
	crsGVK = schema.GroupVersionKind{Group: addonsGroup, Version: capiVersion, Kind: "ClusterResourceSet"}
	hrpGVK = schema.GroupVersionKind{Group: addonsGroup, Version: hcpGVK.Version, Kind: "HelmReleaseProxy"}
)

// hcpNameLabel is the label CAAPH puts on a HelmReleaseProxy naming its
// HelmChartProxy; it also sets clusterNameLabel.
const hcpNameLabel = "helmreleaseproxy.addons.cluster.x-k8s.io/helmchartproxy-name"

// Manager acts on one management cluster.
type Manager struct {
	C   client.Client
	Out io.Writer
	// Poll is the interval between status reads while waiting.
	Poll time.Duration
	// Workload connects to a workload cluster from the kubeconfig in its
	// Secret, to read its CNI's rollout. Nil dials that kubeconfig
	// (ConnectWorkload); ReachVia reads a self-hosted cluster through C.
	Workload func(kubeconfig []byte) (client.Client, error)
	// SkipCNIRollout makes Status leave the workload cluster alone, for
	// callers that do not use the CNI's readiness: CNIRolledOut stays false.
	SkipCNIRollout bool
	// RequireCNIRollout makes a workload cluster that cannot be read hold the
	// CNI not ready: set when this run changed the CNI, and by the pivot.
	// Otherwise the CNI is ready once its release is, with the rollout
	// reported unverified (PET-103).
	RequireCNIRollout bool
	// UnreadableGrace is how long WaitAvailable, requiring the rollout, keeps
	// retrying a workload cluster it cannot read once nothing else is left to
	// wait for, before failing (default DefaultUnreadableGrace).
	UnreadableGrace time.Duration

	workload workloadClients
}

func (m *Manager) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(m.Out, format+"\n", args...)
}

func (m *Manager) poll() time.Duration {
	if m.Poll > 0 {
		return m.Poll
	}
	return 10 * time.Second
}

// Objects splits rendered manifests into objects, in order.
func Objects(manifests []byte) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	for doc := range bytes.SplitSeq(manifests, []byte("\n---\n")) {
		var m map[string]any
		if err := yaml.Unmarshal(doc, &m); err != nil {
			return nil, fmt.Errorf("parsing rendered manifests: %w", err)
		}
		if len(m) == 0 {
			continue // the leading comment block
		}
		out = append(out, &unstructured.Unstructured{Object: m})
	}
	return out, nil
}

func describe(o *unstructured.Unstructured) string {
	return o.GetKind() + " " + o.GetNamespace() + "/" + o.GetName()
}

// CheckCredentials confirms the hypervisor credentials Secret exists and
// holds the three keys the provider reads. bootstrap writes it; create only
// checks, so a missing Secret is reported before anything is applied.
func (m *Manager) CheckCredentials(ctx context.Context, namespace, name string) error {
	var s corev1.Secret
	err := m.C.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &s)
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("hypervisor credentials Secret %s/%s does not exist in the management cluster; write it with `hydra bootstrap --hypervisor-credentials <dir>`, or create it with cacert.pem, clientcert.pem and clientkey.pem and the label clusterctl.cluster.x-k8s.io/move", namespace, name)
	}
	if err != nil {
		return fmt.Errorf("reading Secret %s/%s: %w", namespace, name, err)
	}
	var missing []string
	for _, k := range []string{bootstrap.CACertFile, bootstrap.ClientCertFile, bootstrap.ClientKeyFile} {
		if _, ok := s.Data[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("hypervisor credentials Secret %s/%s lacks %s", namespace, name, strings.Join(missing, ", "))
	}
	return nil
}

// FieldManagerFor is the server-side apply field manager for a cluster.
func FieldManagerFor(cluster string) string { return FieldManagerPrefix + cluster }

// applyOptions are the server-side apply options for a cluster's objects.
// Ownership is not forced unless asked: a field another manager set to a
// different value -- another cluster's hydra, or a hand edit -- is a conflict
// the API server reports, instead of being silently taken over. That makes
// the ownership check atomic, where CheckOwnership is only a preflight.
func applyOptions(cluster string, forceConflicts bool) []client.ApplyOption {
	opts := []client.ApplyOption{client.FieldOwner(FieldManagerFor(cluster))}
	if forceConflicts {
		opts = append(opts, client.ForceOwnership)
	}
	return opts
}

// Apply server-side applies every object as the cluster's field manager,
// after making sure their namespace exists. Reapplying the same objects
// changes nothing. forceConflicts takes over fields other managers own.
func (m *Manager) Apply(ctx context.Context, objs []*unstructured.Unstructured, cluster string, forceConflicts bool) error {
	namespaces := map[string]bool{}
	for _, o := range objs {
		if ns := o.GetNamespace(); ns != "" && !namespaces[ns] {
			namespaces[ns] = true
			if err := m.C.Apply(ctx, corev1ac.Namespace(ns), client.FieldOwner(FieldManagerFor(cluster))); err != nil {
				return fmt.Errorf("namespace %s: %w", ns, err)
			}
		}
	}
	for _, o := range objs {
		if err := m.C.Apply(ctx, client.ApplyConfigurationFromUnstructured(o.DeepCopy()), applyOptions(cluster, forceConflicts)...); err != nil {
			if f := undeclaredField(err); f != "" && o.GroupVersionKind().Group == infraGroup {
				return fmt.Errorf("applying %s: the Hydra provider on this management cluster does not know %s, which this hydra renders: it is older than %s, the oldest provider this hydra works with. Upgrade the provider first; nothing of %s was changed: %w", describe(o), f, bootstrap.MinHydraVersion, describe(o), err)
			}
			if apierrors.IsConflict(err) {
				return fmt.Errorf("applying %s: another manager owns fields hydra.yaml sets differently (another cluster, or a hand edit); nothing of theirs was changed. Inspect it, or rerun with --force-conflicts to take them over: %w", describe(o), err)
			}
			return fmt.Errorf("applying %s: %w", describe(o), err)
		}
	}
	m.logf("applied %d objects", len(objs))
	return nil
}

// UnprotectedRoll reports whether applying objs replaces the control plane's
// machines with ones whose template sets preDrainHook, while the live machines
// come from a template without it: the one roll that hands nothing over,
// because the machines it deletes have no hook (PET-88).
func (m *Manager) UnprotectedRoll(ctx context.Context, objs []*unstructured.Unstructured, namespace, name string) (bool, error) {
	cpRef := func(kcp *unstructured.Unstructured) string {
		n, _, _ := unstructured.NestedString(kcp.Object, "spec", "machineTemplate", "spec", "infrastructureRef", "name")
		return n
	}
	var rendered string
	hooked := map[string]bool{}
	for _, o := range objs {
		switch o.GetKind() {
		case kcpGVK.Kind:
			rendered = cpRef(o)
		case kindHydraMachineTemplate:
			_, has, _ := unstructured.NestedMap(o.Object, "spec", "template", "spec", "preDrainHook")
			hooked[o.GetName()] = has
		}
	}
	if rendered == "" || !hooked[rendered] {
		return false, nil
	}
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(kcpGVK)
	switch err := m.C.Get(ctx, client.ObjectKey{Namespace: namespace, Name: render.ControlPlaneName(name)}, live); {
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("reading the KubeadmControlPlane: %w", err)
	}
	current := cpRef(live)
	if current == "" || current == rendered {
		return false, nil
	}
	tmpl := &unstructured.Unstructured{}
	tmpl.SetGroupVersionKind(schema.GroupVersionKind{Group: infraGroup, Version: infraVersion, Kind: kindHydraMachineTemplate})
	switch err := m.C.Get(ctx, client.ObjectKey{Namespace: namespace, Name: current}, tmpl); {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("reading HydraMachineTemplate %s: %w", current, err)
	}
	_, has, _ := unstructured.NestedMap(tmpl.Object, "spec", "template", "spec", "preDrainHook")
	return !has, nil
}

// UnprotectedRollNote is what create and plan say about such a roll.
const UnprotectedRollNote = "this apply rolls the control plane onto machines that hand the control-plane VIP over before they are drained (PET-88). The machines it replaces predate that and hand nothing over, so this one roll can interrupt the API for about 5 s when it replaces the VIP holder; every roll after it is protected"

// undeclaredField returns the field an apply was refused for because the
// object's schema does not declare it ("... .spec.x: field not declared in
// schema"), or "".
func undeclaredField(err error) string {
	const marker = ": field not declared in schema"
	msg := err.Error()
	i := strings.Index(msg, marker)
	if i < 0 {
		return ""
	}
	start := strings.LastIndexAny(msg[:i], " (") + 1
	return msg[start:i]
}

// Change is one difference plan found.
type Change struct {
	Object string
	// Action is "create" or "update".
	Action string
	// Diff is a unified diff of the object, live against what apply would
	// produce, for updates.
	Diff string
	// Note qualifies what the change will do, when that is less than it looks.
	Note string
	// Result is the server's dry-run of the object, for updates.
	Result *unstructured.Unstructured
}

// Plan server-side dry-runs every object and compares the result with the
// live object. Nothing is written, and nothing local is consulted: the live
// cluster is the only "state" (ADR-001).
func (m *Manager) Plan(ctx context.Context, objs []*unstructured.Unstructured, cluster string, forceConflicts bool) ([]Change, error) {
	var changes []Change
	for _, o := range objs {
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(o.GroupVersionKind())
		err := m.C.Get(ctx, client.ObjectKeyFromObject(o), live)
		switch {
		case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
			changes = append(changes, Change{Object: describe(o), Action: "create"})
			continue
		case err != nil:
			return nil, fmt.Errorf("reading %s: %w", describe(o), err)
		}
		dry := o.DeepCopy()
		popts := []client.PatchOption{client.FieldOwner(FieldManagerFor(cluster)), client.DryRunAll}
		if forceConflicts {
			popts = append(popts, client.ForceOwnership)
		}
		if err := m.C.Patch(ctx, dry, client.Apply, popts...); err != nil { //nolint:staticcheck // Patch fills dry with the server's result; Apply does not for unstructured.
			if apierrors.IsConflict(err) {
				changes = append(changes, Change{Object: describe(o), Action: "conflict", Diff: err.Error()})
				continue
			}
			return nil, fmt.Errorf("dry-run applying %s: %w", describe(o), err)
		}
		before, after := comparable(live), comparable(dry)
		if before != after {
			changes = append(changes, Change{Object: describe(o), Action: "update", Diff: unifiedDiff(before, after), Result: dry})
		}
	}
	return changes, nil
}

// comparable renders an object without the fields the server owns, so two
// reads of the same desired state compare equal.
func comparable(o *unstructured.Unstructured) string {
	c := o.DeepCopy()
	unstructured.RemoveNestedField(c.Object, "status")
	for _, f := range []string{"managedFields", "resourceVersion", "generation", "uid", "creationTimestamp", "ownerReferences", "finalizers"} {
		unstructured.RemoveNestedField(c.Object, "metadata", f)
	}
	// A ConfigMap's values can be whole manifests (the Cilium one is): print
	// them as plain lines so a diff points at the line that changed, not at
	// one enormous escaped string.
	var data map[string]any
	if c.GetKind() == kindConfigMap {
		data, _, _ = unstructured.NestedMap(c.Object, "data")
		unstructured.RemoveNestedField(c.Object, "data")
	}
	b, err := yaml.Marshal(c.Object)
	if err != nil {
		return fmt.Sprintf("<unprintable: %v>", err)
	}
	var out strings.Builder
	out.Write(b)
	for _, k := range slices.Sorted(maps.Keys(data)) {
		fmt.Fprintf(&out, "data.%s: |\n", k)
		v, _ := data[k].(string)
		for line := range strings.SplitSeq(strings.TrimSuffix(v, "\n"), "\n") {
			out.WriteString("  " + line + "\n")
		}
	}
	return out.String()
}

// Summary is a cluster's progress as its controllers report it.
type Summary struct {
	Exists           bool
	Available        string // condition status, "" if not yet reported
	AvailableReason  string
	AvailableMessage string
	ControlPlane     string // "ready/desired"
	Machines         map[string]int
	// CNIApplied: CAAPH reports hydra's Cilium installed at the version the
	// proxy asks for. CNIProxyPresent: CAAPH has a HelmReleaseProxy for it,
	// installed or not. CNIRolledOut: once applied, the agents' DaemonSet and
	// the operator's Deployment in the workload cluster have rolled out at
	// that version (CAAPH's Ready does not wait for pods, PET-103).
	// CNIRolloutUnreadable: the workload cluster could not be read, so the
	// rollout is unknown; CNIRolloutRequired: that holds the CNI not ready
	// (Manager.RequireCNIRollout). CNIWhy is why it is not ready yet: the
	// release's reason, the rollout's progress ("n/m" agents first), or why
	// the workload cluster could not be read.
	CNIApplied           bool
	CNIProxyPresent      bool
	CNIRolledOut         bool
	CNIRolloutUnreadable bool
	CNIRolloutRequired   bool
	CNIWhy               string
	// CNIExternal is true when the Cluster carries no CNI label, so hydra's
	// Cilium proxy does not select it (cni: external): something else
	// installs the CNI, and the other CNI fields say nothing.
	CNIExternal bool
	// CNILabelMissing: hydra's Cilium proxy exists but the Cluster lost the
	// label it selects by, so CAAPH has uninstalled Cilium (PET-97). Not
	// ready, whatever the nodes say.
	CNILabelMissing bool
	// Addons are the cluster's HelmChartProxies, as CAAPH reports them.
	Addons []AddonState
	// AddonsError is why the proxies could not be read, if they could not.
	AddonsError string
	// RolledOut is true when the control plane and every MachineDeployment
	// have observed their latest spec and have all replicas up to date, so a
	// rerun after a change does not report success before the rollout.
	RolledOut bool
	// Terminating and DeletingMessage report a Cluster being deleted, with
	// what Cluster API says it is waiting for.
	Terminating     bool
	DeletingMessage string
}

// CNIReady reports whether the CNI no longer holds the cluster up as far as
// hydra can tell: CAAPH installed hydra's Cilium and its agents and operator
// have rolled out at that version (or, not required, their rollout could not
// be read), or hydra does not install it. An external CNI that is missing
// shows as Machines that never become Ready, which RolledOut already waits
// for.
func (s Summary) CNIReady() bool {
	return s.CNIExternal || (s.CNIApplied && (s.CNIRolledOut || s.CNIRolloutUnverified()))
}

// CNIRolloutUnverified reports a CNI counted ready on its release alone: the
// workload cluster could not be read, and the rollout was not required.
func (s Summary) CNIRolloutUnverified() bool {
	return s.CNIApplied && !s.CNIRolledOut && s.CNIRolloutUnreadable && !s.CNIRolloutRequired
}

// Line is the summary as one line, printed whenever it changes.
func (s Summary) Line() string {
	if !s.Exists {
		return "Cluster does not exist"
	}
	if s.Terminating {
		msg := "Cluster is being deleted"
		if s.DeletingMessage != "" {
			msg += ": " + strings.Join(strings.Fields(s.DeletingMessage), " ")
		}
		return msg
	}
	parts := []string{"Available=" + orDash(s.Available)}
	if s.AvailableReason != "" {
		parts[0] += " (" + s.AvailableReason + ")"
	}
	if s.Available == ConditionTrue && !s.RolledOut {
		parts = append(parts, "rolling out")
	}
	parts = append(parts, "control plane "+orDash(s.ControlPlane))
	var phases []string
	for _, p := range slices.Sorted(maps.Keys(s.Machines)) {
		phases = append(phases, fmt.Sprintf("%d %s", s.Machines[p], p))
	}
	if len(phases) == 0 {
		phases = []string{"none"}
	}
	parts = append(parts, "machines: "+strings.Join(phases, ", "))
	switch {
	case s.CNILabelMissing:
		parts = append(parts, "CNI uninstalled: the Cluster lost its "+render.CNILabelKey+" label")
	case s.CNIExternal:
		parts = append(parts, "CNI external (not installed by hydra)")
	case s.CNIApplied && s.CNIRolledOut:
		parts = append(parts, "CNI ready")
	case s.CNIRolloutUnverified():
		parts = append(parts, "CNI ready (rollout unverified: "+s.CNIWhy+")")
	case s.CNIApplied && s.CNIRolloutUnreadable:
		parts = append(parts, "CNI rollout unknown: "+s.CNIWhy)
	case s.CNIApplied:
		parts = append(parts, "CNI rolling out ("+orDash(s.CNIWhy)+")")
	case s.CNIProxyPresent && s.CNIWhy != "":
		parts = append(parts, "CNI installing ("+s.CNIWhy+")")
	case s.CNIProxyPresent:
		parts = append(parts, "CNI installing")
	default:
		parts = append(parts, "CNI waiting for the API")
	}
	if s.AddonsError != "" {
		parts = append(parts, "addons: unreadable")
	}
	if len(s.Addons) > 0 {
		var ad []string
		for _, a := range s.Addons {
			state := "ready"
			if a.Ready != ConditionTrue {
				state = "not ready"
			}
			ad = append(ad, a.Name+" "+a.Version+" "+state)
		}
		parts = append(parts, "addons: "+strings.Join(ad, ", "))
	}
	return strings.Join(parts, " · ")
}

// AddonState is one addon's progress, as CAAPH reports it on its
// HelmChartProxy.
type AddonState struct {
	Name, Version string
	// Ready is the proxy's Ready condition status ("" if not yet reported).
	Ready string
	// Reason and Message are the Ready condition's, kept when it is not
	// True; CAAPH may set either alone.
	Reason, Message string
}

// Why is the not-Ready addon's reason and message, as one line.
func (a AddonState) Why() string {
	msg := strings.Join(strings.Fields(a.Message), " ")
	switch {
	case a.Reason != "" && msg != "":
		return a.Reason + ": " + msg
	case a.Reason != "":
		return a.Reason
	}
	return msg
}

func addonState(hcp *unstructured.Unstructured) AddonState {
	a := AddonState{Name: hcp.GetLabels()[render.AddonLabelKey]}
	if a.Name == "" {
		a.Name = hcp.GetName()
	}
	a.Version, _, _ = unstructured.NestedString(hcp.Object, "spec", "version")
	if c := condition(hcp, "Ready"); c != nil {
		a.Ready, _ = c["status"].(string)
		if a.Ready != ConditionTrue {
			a.Reason, _ = c["reason"].(string)
			a.Message, _ = c["message"].(string)
		}
	}
	return a
}

// AddonsReady reports whether every addon's proxy is Ready (false when they
// could not be read).
func (s Summary) AddonsReady() bool {
	if s.AddonsError != "" {
		return false
	}
	for _, a := range s.Addons {
		if a.Ready != ConditionTrue {
			return false
		}
	}
	return true
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Status reads the cluster's progress.
func (m *Manager) Status(ctx context.Context, namespace, name string) (Summary, error) {
	s := Summary{Machines: map[string]int{}, CNIRolloutRequired: m.RequireCNIRollout}
	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(clusterGVK)
	err := m.C.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cl)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	s.Exists = true
	s.CNIExternal = !HydraCNISelected(cl)
	if cl.GetDeletionTimestamp() != nil {
		s.Terminating = true
		if c := condition(cl, "Deleting"); c != nil {
			s.DeletingMessage, _ = c["message"].(string)
		}
	}
	s.RolledOut = true
	if c := condition(cl, "Available"); c != nil {
		s.Available, _ = c["status"].(string)
		s.AvailableReason, _ = c["reason"].(string)
		s.AvailableMessage, _ = c["message"].(string)
	}

	kcp := &unstructured.Unstructured{}
	kcp.SetGroupVersionKind(kcpGVK)
	if err := m.C.Get(ctx, client.ObjectKey{Namespace: namespace, Name: render.ControlPlaneName(name)}, kcp); err == nil {
		desired, _, _ := unstructured.NestedInt64(kcp.Object, "spec", "replicas")
		ready, _, _ := unstructured.NestedInt64(kcp.Object, "status", "readyReplicas")
		s.ControlPlane = fmt.Sprintf("%d/%d ready", ready, desired)
		s.RolledOut = s.RolledOut && UpToDate(kcp)
	} else if apierrors.IsNotFound(err) {
		s.RolledOut = false
	} else {
		return s, fmt.Errorf("reading KubeadmControlPlane: %w", err)
	}
	mds := &unstructured.UnstructuredList{}
	mds.SetGroupVersionKind(mdGVK.GroupVersion().WithKind("MachineDeploymentList"))
	// A failed read is an error, never "nothing to wait for": one refused
	// list must not let the wait succeed before the workers are known.
	if err := m.C.List(ctx, mds, client.InNamespace(namespace), client.MatchingLabels{render.ClusterLabelKey: name}); err != nil {
		return s, fmt.Errorf("listing MachineDeployments: %w", err)
	}
	for i := range mds.Items {
		s.RolledOut = s.RolledOut && UpToDate(&mds.Items[i])
	}

	machines := &unstructured.UnstructuredList{}
	machines.SetGroupVersionKind(machineGVK.GroupVersion().WithKind("MachineList"))
	if err := m.C.List(ctx, machines, client.InNamespace(namespace), client.MatchingLabels{clusterNameLabel: name}); err != nil {
		return s, fmt.Errorf("listing Machines: %w", err)
	}
	for _, mc := range machines.Items {
		phase, _, _ := unstructured.NestedString(mc.Object, "status", "phase")
		s.Machines[orDash(phase)]++
	}

	// Addons never make status fail: the cluster's own state is what create
	// waits on, and a kubeconfig that may not list proxies must not hang it.
	if hcps, err := m.listLabelled(ctx, namespace, name, hcpGVK); err != nil {
		s.AddonsError = err.Error()
	} else {
		for i := range hcps {
			// The CNI's proxy is reported as the CNI, not as an addon.
			if hcps[i].GetLabels()[render.AddonLabelKey] == addons.Cilium {
				continue
			}
			s.Addons = append(s.Addons, addonState(&hcps[i]))
		}
	}

	if s.CNIExternal {
		// No label, but hydra's proxy for it: the label was removed, and
		// CAAPH uninstalled Cilium. That is no external CNI.
		proxy, err := m.hydraCNIProxy(ctx, namespace, name)
		if err != nil {
			return s, fmt.Errorf("reading the CNI HelmChartProxy: %w", err)
		}
		if proxy {
			s.CNIExternal, s.CNILabelMissing = false, true
		}
		return s, nil
	}
	if err := m.cniStatus(ctx, namespace, name, &s); err != nil {
		return s, err
	}
	return s, nil
}

// cniStatus reads how far CAAPH has got installing hydra's Cilium: the
// HelmReleaseProxy it made for the cluster from the CNI proxy. Applied means
// Ready for the proxy's current version and the generation CAAPH last
// reconciled, so a version change is not reported done by the old release.
// Once applied, the rollout is read from the workload cluster (cniRollout).
// A read that fails is an error, never "installed"; a workload cluster that
// cannot be read is a reason, so status still reports the rest.
func (m *Manager) cniStatus(ctx context.Context, namespace, name string, s *Summary) error {
	proxy := render.CNIName(name)
	hcp := &unstructured.Unstructured{}
	hcp.SetGroupVersionKind(hcpGVK)
	switch err := m.C.Get(ctx, client.ObjectKey{Namespace: namespace, Name: proxy}, hcp); {
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return nil // not applied yet, or no CAAPH: nothing installs it
	case err != nil:
		return fmt.Errorf("reading the CNI HelmChartProxy: %w", err)
	}
	if hcp.GetDeletionTimestamp() != nil {
		// CAAPH uninstalls the release while it cleans the proxy up; the
		// HelmReleaseProxy may still say Ready until it is gone.
		s.CNIProxyPresent, s.CNIWhy = true, "its HelmChartProxy is being deleted, which uninstalls Cilium"
		return nil
	}
	want, _, _ := unstructured.NestedString(hcp.Object, "spec", "version")
	hrps := &unstructured.UnstructuredList{}
	hrps.SetGroupVersionKind(hrpGVK.GroupVersion().WithKind(hrpGVK.Kind + "List"))
	err := m.C.List(ctx, hrps, client.InNamespace(namespace), client.MatchingLabels{clusterNameLabel: name, hcpNameLabel: proxy})
	switch {
	case meta.IsNoMatchError(err):
		return nil
	case err != nil:
		return fmt.Errorf("listing the CNI HelmReleaseProxy: %w", err)
	}
	for i := range hrps.Items {
		h := &hrps.Items[i]
		if h.GetDeletionTimestamp() != nil {
			continue
		}
		s.CNIProxyPresent = true
		version, _, _ := unstructured.NestedString(h.Object, "spec", "version")
		c := condition(h, "Ready")
		switch {
		case version != want:
			s.CNIWhy = "moving to " + want
		case c == nil:
		case c["status"] == ConditionTrue && observedCurrent(h, c):
			s.CNIApplied = true
			s.CNIWhy = ""
			// A cluster being deleted is not read: its API is going away.
			if !m.SkipCNIRollout && !s.Terminating {
				m.cniRollout(ctx, namespace, name, want, s)
			}
			return nil
		case c["status"] == ConditionTrue:
			// Ready for an older generation: CAAPH has not caught up yet.
		default:
			reason, _ := c["reason"].(string)
			msg, _ := c["message"].(string)
			s.CNIWhy = strings.TrimSpace(reason + ": " + strings.Join(strings.Fields(msg), " "))
			s.CNIWhy = strings.TrimSuffix(strings.TrimPrefix(s.CNIWhy, ": "), ":")
		}
	}
	return nil
}

// observedCurrent reports whether a condition was set for the object's
// current generation. CAAPH sets observedGeneration on every condition (Cluster
// API's condition setter does), so one without it is not trusted.
func observedCurrent(o *unstructured.Unstructured, c map[string]any) bool {
	g, ok := c["observedGeneration"]
	if !ok {
		return false
	}
	switch v := g.(type) {
	case int64:
		return v >= o.GetGeneration()
	case float64:
		return int64(v) >= o.GetGeneration()
	}
	return false
}

// UpToDate reports whether a KubeadmControlPlane or MachineDeployment has
// observed its latest generation, has every desired replica both up to date
// and Ready, and has no more replicas than desired: a scale-down is not done
// while the surplus Machines are still being deleted. upToDateReplicas alone is not enough: in Cluster API v1.14 a
// Machine is UpToDate as soon as its spec matches, before its Node is Ready.
// spec.replicas is the desired count; for an autoscaled pool Cluster API
// defaults it to the min-size annotation when the pool is created, so that
// minimum is what is waited for. Without spec.replicas, status.replicas is
// used.
func UpToDate(o *unstructured.Unstructured) bool {
	observed, _, _ := unstructured.NestedInt64(o.Object, "status", "observedGeneration")
	if observed < o.GetGeneration() {
		return false
	}
	desired, found, _ := unstructured.NestedInt64(o.Object, "spec", "replicas")
	if !found {
		desired, _, _ = unstructured.NestedInt64(o.Object, "status", "replicas")
	}
	replicas, _, _ := unstructured.NestedInt64(o.Object, "status", "replicas")
	upto, _, _ := unstructured.NestedInt64(o.Object, "status", "upToDateReplicas")
	ready, _, _ := unstructured.NestedInt64(o.Object, "status", "readyReplicas")
	return replicas == desired && upto >= desired && ready >= desired
}

// condition returns the named entry of status.conditions.
func condition(o *unstructured.Unstructured, typ string) map[string]any {
	conds, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, c := range conds {
		if cm, ok := c.(map[string]any); ok && cm["type"] == typ {
			return cm
		}
	}
	return nil
}

// ErrTimeout is returned when a wait ends before the cluster gets there.
// The controllers keep working; the timeout only ends the CLI's wait.
var ErrTimeout = errors.New("timed out waiting; the controllers keep working, so run `hydra cluster status` or the same command again")

// ErrInterrupted is returned when the user interrupts a wait. Like a timeout,
// it ends only the wait: the management cluster carries on.
var ErrInterrupted = errors.New("interrupted; the operation continues in the management cluster, so run `hydra cluster status` or the same command again")

// DefaultUnreadableGrace is Manager.UnreadableGrace's default: long enough
// to ride out a blip, far shorter than any wait.
const DefaultUnreadableGrace = 2 * time.Minute

// ErrCNIRolloutUnknown is returned by WaitAvailable, requiring the CNI's
// rollout, when everything else is done but the workload cluster could not
// be read for UnreadableGrace.
var ErrCNIRolloutUnknown = errors.New("cannot verify the CNI's rollout")

// WaitAvailable polls until the Cluster is Available, rolled out and its CNI
// ready (or not hydra's to apply), printing the summary whenever it changes.
// With RequireCNIRollout, a workload cluster that stays unreadable once
// nothing else is left fails the wait after UnreadableGrace rather than at
// the timeout: it is not a rollout in progress.
func (m *Manager) WaitAvailable(ctx context.Context, namespace, name string, timeout time.Duration) error {
	grace := m.UnreadableGrace
	if grace <= 0 {
		grace = DefaultUnreadableGrace
	}
	var since time.Time
	return m.wait(ctx, namespace, name, timeout, func(s Summary) (bool, error) {
		settled := s.Exists && !s.Terminating && s.Available == ConditionTrue && s.RolledOut
		if settled && s.CNIReady() {
			return true, nil
		}
		if !settled || !s.CNIApplied || !s.CNIRolloutUnreadable {
			since = time.Time{}
			return false, nil
		}
		if since.IsZero() {
			since = time.Now()
		} else if time.Since(since) >= grace {
			return false, fmt.Errorf("%w: this run changed the CNI, its release is applied and the cluster is Available, but the workload cluster could not be read for %s: %s. Cilium may well be rolling out; check from where its API is reachable (`hydra cluster status`), or rerun from there", ErrCNIRolloutUnknown, grace, s.CNIWhy)
		}
		return false, nil
	})
}

func (m *Manager) wait(ctx context.Context, namespace, name string, timeout time.Duration, done func(Summary) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	last := ""
	for {
		s, err := m.Status(ctx, namespace, name)
		line := ""
		if err != nil {
			// Shown, not swallowed: an expired credential or a refused
			// connection must not look like slow progress for 30 minutes.
			line = "cannot read status: " + err.Error()
		} else {
			line = s.Line()
		}
		if line != last {
			m.logf("  %s", line)
			last = line
		}
		if err == nil {
			ok, derr := done(s)
			if derr != nil {
				return derr
			}
			if ok {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return ErrTimeout
			}
			return ErrInterrupted
		case <-time.After(m.poll()):
		}
	}
}

// Kubeconfig returns the workload cluster's admin kubeconfig from the Secret
// Cluster API writes.
func (m *Manager) Kubeconfig(ctx context.Context, namespace, name string) ([]byte, error) {
	var s corev1.Secret
	if err := m.C.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name + "-kubeconfig"}, &s); err != nil {
		return nil, fmt.Errorf("reading Secret %s/%s-kubeconfig: %w", namespace, name, err)
	}
	v, ok := s.Data["value"]
	if !ok || len(v) == 0 {
		return nil, fmt.Errorf("the kubeconfig Secret %s/%s-kubeconfig is empty", namespace, name)
	}
	return v, nil
}

// Delete deletes the Cluster and waits for Cluster API to tear it down:
// machines, then the control plane, then the infrastructure (Hydra removes a
// network it created once nothing uses it). It then deletes, by the
// hydra.io/cluster label, anything left: first the HydraCluster, control
// plane and MachineDeployments a Cluster never came to own (a create
// interrupted between applies), waiting for their finalizers; then the
// templates and the CNI objects. A timeout ends the wait, not the deletion.
//
// The Cluster must be labelled for this cluster, or unlabelled with adopt
// (one built by hand); one labelled for another cluster is never deleted, and
// the delete carries the UID and resourceVersion it checked, so an object
// changed in between is not deleted either.
func (m *Manager) Delete(ctx context.Context, namespace, name string, timeout time.Duration, adopt bool) error {
	// One deadline for the whole command, not a fresh timeout per phase.
	deadline := time.Now().Add(timeout)
	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(clusterGVK)
	err := m.C.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cl)
	switch {
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		m.logf("Cluster %s/%s does not exist", namespace, name)
	case err != nil:
		return fmt.Errorf("reading Cluster %s/%s: %w", namespace, name, err)
	default:
		if err := checkOwner(cl, name, adopt); err != nil {
			return err
		}
		if err := m.deleteExactly(ctx, cl); err != nil {
			return err
		}
		m.logf("deleting Cluster %s/%s; waiting for its machines and infrastructure to be removed", namespace, name)
		if err := m.wait(ctx, namespace, name, time.Until(deadline), func(s Summary) (bool, error) { return !s.Exists, nil }); err != nil {
			return err
		}
	}
	// Phase 1: objects with machines or infrastructure behind them.
	if err := m.deleteLabelled(ctx, namespace, name, withMachines); err != nil {
		return err
	}
	if err := m.waitLabelledGone(ctx, namespace, name, withMachines, time.Until(deadline)); err != nil {
		return err
	}
	// Phase 2: nothing references these any more.
	return m.deleteLabelled(ctx, namespace, name, afterMachines)
}

// Labelled lists every object labelled for the cluster, as delete would find
// them, so the CLI can tell whether a delete would remove anything.
func (m *Manager) Labelled(ctx context.Context, namespace, name string) ([]string, error) {
	var out []string
	for _, gvk := range ownedKinds {
		items, err := m.listLabelled(ctx, namespace, name, gvk)
		if err != nil {
			return nil, err
		}
		for i := range items {
			out = append(out, describe(&items[i]))
		}
	}
	return out, nil
}

func (m *Manager) listLabelled(ctx context.Context, namespace, name string, gvk schema.GroupVersionKind) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	err := m.C.List(ctx, list, client.InNamespace(namespace), client.MatchingLabels{render.ClusterLabelKey: name})
	if meta.IsNoMatchError(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", gvk.Kind, err)
	}
	return list.Items, nil
}

func (m *Manager) deleteLabelled(ctx context.Context, namespace, name string, kinds []schema.GroupVersionKind) error {
	for _, gvk := range kinds {
		items, err := m.listLabelled(ctx, namespace, name, gvk)
		if err != nil {
			return err
		}
		for i := range items {
			o := &items[i]
			if o.GetDeletionTimestamp() != nil {
				continue
			}
			switch err := m.deleteExactly(ctx, o); {
			case errors.Is(err, errChanged):
				m.logf("skipped %s: it changed after it was listed", describe(o))
			case err != nil:
				return err
			default:
				m.logf("deleted %s", describe(o))
			}
		}
	}
	return nil
}

// errChanged means an object was modified between being checked and deleted.
var errChanged = errors.New("object changed since it was checked")

// deleteExactly deletes the object only if it is still the version that was
// read (same UID and resourceVersion), so ownership checked on that version
// still holds. An object already gone is not an error.
func (m *Manager) deleteExactly(ctx context.Context, o *unstructured.Unstructured) error {
	uid, rv := o.GetUID(), o.GetResourceVersion()
	err := m.C.Delete(ctx, o, client.Preconditions{UID: &uid, ResourceVersion: &rv})
	switch {
	case err == nil, apierrors.IsNotFound(err):
		return nil
	case apierrors.IsConflict(err):
		return fmt.Errorf("%s: %w", describe(o), errChanged)
	}
	return fmt.Errorf("deleting %s: %w", describe(o), err)
}

// checkOwner applies the ownership rule to a live object about to be deleted.
func checkOwner(o *unstructured.Unstructured, cluster string, adopt bool) error {
	owner, labelled := o.GetLabels()[render.ClusterLabelKey]
	switch {
	case labelled && owner != cluster:
		return fmt.Errorf("refusing to delete %s: it belongs to cluster %q", describe(o), owner)
	case !labelled && !adopt:
		return fmt.Errorf("refusing to delete %s: hydra did not make it (no %s label); pass --adopt to delete a cluster built some other way", describe(o), render.ClusterLabelKey)
	}
	return nil
}

func (m *Manager) waitLabelledGone(ctx context.Context, namespace, name string, kinds []schema.GroupVersionKind, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	last := ""
	for {
		var left []string
		for _, gvk := range kinds {
			items, err := m.listLabelled(ctx, namespace, name, gvk)
			if err != nil {
				return err
			}
			for i := range items {
				left = append(left, describe(&items[i]))
			}
		}
		if len(left) == 0 {
			return nil
		}
		if line := strings.Join(left, ", "); line != last {
			m.logf("  waiting for %s to be removed", line)
			last = line
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return ErrTimeout
			}
			return ErrInterrupted
		case <-time.After(m.poll()):
		}
	}
}
