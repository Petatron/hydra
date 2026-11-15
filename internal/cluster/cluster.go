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

	"github.com/Petatron/hydra/internal/bootstrap"
	"github.com/Petatron/hydra/internal/render"
)

const (
	// FieldManager owns everything this CLI applies.
	FieldManager = "hydra"

	capiGroup        = "cluster.x-k8s.io"
	capiVersion      = "v1beta2"
	kindConfigMap    = "ConfigMap"
	clusterNameLabel = "cluster.x-k8s.io/cluster-name"
	infraGroup       = "infrastructure.cluster.x-k8s.io"
	infraVersion     = "v1alpha1"
)

// ConditionTrue is a condition's status when it holds.
const ConditionTrue = "True"

var (
	clusterGVK = schema.GroupVersionKind{Group: capiGroup, Version: capiVersion, Kind: "Cluster"}
	kcpGVK     = schema.GroupVersionKind{Group: "controlplane.cluster.x-k8s.io", Version: capiVersion, Kind: "KubeadmControlPlane"}
	machineGVK = schema.GroupVersionKind{Group: capiGroup, Version: capiVersion, Kind: "Machine"}
	bindingGVK = schema.GroupVersionKind{Group: "addons.cluster.x-k8s.io", Version: capiVersion, Kind: "ClusterResourceSetBinding"}
)

// Manager acts on one management cluster.
type Manager struct {
	C   client.Client
	Out io.Writer
	// Poll is the interval between status reads while waiting.
	Poll time.Duration
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

// Apply server-side applies every object, after making sure their namespace
// exists. Reapplying the same objects changes nothing.
func (m *Manager) Apply(ctx context.Context, objs []*unstructured.Unstructured) error {
	namespaces := map[string]bool{}
	for _, o := range objs {
		if ns := o.GetNamespace(); ns != "" && !namespaces[ns] {
			namespaces[ns] = true
			if err := m.C.Apply(ctx, corev1ac.Namespace(ns), client.FieldOwner(FieldManager)); err != nil {
				return fmt.Errorf("namespace %s: %w", ns, err)
			}
		}
	}
	for _, o := range objs {
		if err := m.C.Apply(ctx, client.ApplyConfigurationFromUnstructured(o.DeepCopy()), client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
			return fmt.Errorf("applying %s: %w", describe(o), err)
		}
	}
	m.logf("applied %d objects", len(objs))
	return nil
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
}

// cniNote explains that a changed CNI ConfigMap does not reach a running
// cluster: its ClusterResourceSet uses ApplyOnce (see render).
const cniNote = "the CNI ClusterResourceSet applies once: this updates the ConfigMap, but Cilium on an existing cluster is not changed"

// Plan server-side dry-runs every object and compares the result with the
// live object. Nothing is written, and nothing local is consulted: the live
// cluster is the only "state" (ADR-001).
func (m *Manager) Plan(ctx context.Context, objs []*unstructured.Unstructured) ([]Change, error) {
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
		if err := m.C.Patch(ctx, dry, client.Apply, client.FieldOwner(FieldManager), client.ForceOwnership, client.DryRunAll); err != nil { //nolint:staticcheck // Patch fills dry with the server's result; Apply does not for unstructured.
			return nil, fmt.Errorf("dry-run applying %s: %w", describe(o), err)
		}
		before, after := comparable(live), comparable(dry)
		if before != after {
			ch := Change{Object: describe(o), Action: "update", Diff: unifiedDiff(before, after)}
			if o.GetKind() == kindConfigMap && o.GetLabels()[render.CNILabelKey] != "" {
				ch.Note = cniNote
			}
			changes = append(changes, ch)
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
	Exists            bool
	Available         string // condition status, "" if not yet reported
	AvailableReason   string
	AvailableMessage  string
	ControlPlane      string // "ready/desired"
	Machines          map[string]int
	CNIApplied        bool
	CNIBindingPresent bool
	// RolledOut is true when the control plane and every MachineDeployment
	// have observed their latest spec and have all replicas up to date, so a
	// rerun after a change does not report success before the rollout.
	RolledOut bool
	// Terminating and DeletingMessage report a Cluster being deleted, with
	// what Cluster API says it is waiting for.
	Terminating     bool
	DeletingMessage string
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
	case s.CNIApplied:
		parts = append(parts, "CNI applied")
	case s.CNIBindingPresent:
		parts = append(parts, "CNI pending")
	default:
		parts = append(parts, "CNI waiting for the API")
	}
	return strings.Join(parts, " · ")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Status reads the cluster's progress.
func (m *Manager) Status(ctx context.Context, namespace, name string) (Summary, error) {
	s := Summary{Machines: map[string]int{}}
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
		s.RolledOut = s.RolledOut && upToDate(kcp)
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
		s.RolledOut = s.RolledOut && upToDate(&mds.Items[i])
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

	binding := &unstructured.Unstructured{}
	binding.SetGroupVersionKind(bindingGVK)
	switch err := m.C.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, binding); {
	case err == nil:
		s.CNIBindingPresent = true
		s.CNIApplied = bindingApplied(binding, render.CNIName(name))
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		// No binding yet: Cluster API makes it once the API is reachable.
	default:
		return s, fmt.Errorf("reading ClusterResourceSetBinding: %w", err)
	}
	return s, nil
}

// upToDate reports whether a KubeadmControlPlane or MachineDeployment has
// observed its latest generation and has every desired replica both up to
// date and Ready. upToDateReplicas alone is not enough: in Cluster API v1.14 a
// Machine is UpToDate as soon as its spec matches, before its Node is Ready.
// spec.replicas is the desired count; for an autoscaled pool Cluster API
// defaults it to the min-size annotation when the pool is created, so that
// minimum is what is waited for. Without spec.replicas, status.replicas is
// used.
func upToDate(o *unstructured.Unstructured) bool {
	observed, _, _ := unstructured.NestedInt64(o.Object, "status", "observedGeneration")
	if observed < o.GetGeneration() {
		return false
	}
	desired, found, _ := unstructured.NestedInt64(o.Object, "spec", "replicas")
	if !found {
		desired, _, _ = unstructured.NestedInt64(o.Object, "status", "replicas")
	}
	upto, _, _ := unstructured.NestedInt64(o.Object, "status", "upToDateReplicas")
	ready, _, _ := unstructured.NestedInt64(o.Object, "status", "readyReplicas")
	return upto >= desired && ready >= desired
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

// bindingApplied reports whether the ClusterResourceSet named crs has
// applied all its resources to the cluster.
func bindingApplied(b *unstructured.Unstructured, crs string) bool {
	bindings, _, _ := unstructured.NestedSlice(b.Object, "spec", "bindings")
	for _, x := range bindings {
		bm, ok := x.(map[string]any)
		if !ok || bm["clusterResourceSetName"] != crs {
			continue
		}
		res, _ := bm["resources"].([]any)
		if len(res) == 0 {
			return false
		}
		for _, r := range res {
			if rm, ok := r.(map[string]any); !ok || rm["applied"] != true {
				return false
			}
		}
		return true
	}
	return false
}

// ErrTimeout is returned when a wait ends before the cluster gets there.
// The controllers keep working; the timeout only ends the CLI's wait.
var ErrTimeout = errors.New("timed out waiting; the controllers keep working, so run `hydra cluster status` or the same command again")

// ErrInterrupted is returned when the user interrupts a wait. Like a timeout,
// it ends only the wait: the management cluster carries on.
var ErrInterrupted = errors.New("interrupted; the operation continues in the management cluster, so run `hydra cluster status` or the same command again")

// WaitAvailable polls until the Cluster is Available and the CNI is applied,
// printing the summary whenever it changes.
func (m *Manager) WaitAvailable(ctx context.Context, namespace, name string, timeout time.Duration) error {
	return m.wait(ctx, namespace, name, timeout, func(s Summary) bool {
		return s.Exists && !s.Terminating && s.Available == ConditionTrue && s.CNIApplied && s.RolledOut
	})
}

func (m *Manager) wait(ctx context.Context, namespace, name string, timeout time.Duration, done func(Summary) bool) error {
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
		if err == nil && done(s) {
			return nil
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
func (m *Manager) Delete(ctx context.Context, namespace, name string, timeout time.Duration) error {
	// One deadline for the whole command, not a fresh timeout per phase.
	deadline := time.Now().Add(timeout)
	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(clusterGVK)
	cl.SetNamespace(namespace)
	cl.SetName(name)
	err := m.C.Delete(ctx, cl)
	switch {
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		m.logf("Cluster %s/%s does not exist", namespace, name)
	case err != nil:
		return fmt.Errorf("deleting Cluster %s/%s: %w", namespace, name, err)
	default:
		m.logf("deleting Cluster %s/%s; waiting for its machines and infrastructure to be removed", namespace, name)
		if err := m.wait(ctx, namespace, name, time.Until(deadline), func(s Summary) bool { return !s.Exists }); err != nil {
			return err
		}
	}
	// Phase 1: objects with machines or infrastructure behind them.
	if err := m.deleteLabelled(ctx, namespace, name, ownedKinds[:3]); err != nil {
		return err
	}
	if err := m.waitLabelledGone(ctx, namespace, name, ownedKinds[:3], time.Until(deadline)); err != nil {
		return err
	}
	// Phase 2: nothing references these any more.
	return m.deleteLabelled(ctx, namespace, name, ownedKinds[3:])
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
			if err := m.C.Delete(ctx, o); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting %s: %w", describe(o), err)
			}
			m.logf("deleted %s", describe(o))
		}
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
