package pivot

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// deleteForMoveAnnotation tells providers a deletion is clusterctl move's,
// not a real one: skip external cleanup (clusterctl's DeleteForMoveAnnotation).
const deleteForMoveAnnotation = "clusterctl.cluster.x-k8s.io/delete-for-move"

// takenOver counts the HydraMachines in namespace on c, every one of them,
// and how many have taken their VM over: owner-uid equal to their own UID.
func takenOver(ctx context.Context, c client.Client, namespace string) (owned, total int, err error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(hydraMachineGVK.GroupVersion().WithKind("HydraMachineList"))
	if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return 0, 0, err
	}
	for _, m := range list.Items {
		// An absent record is never "owned", whatever the UID reads as.
		if o := m.GetAnnotations()[OwnerUIDAnnotation]; o != "" && o == string(m.GetUID()) {
			owned++
		}
	}
	return owned, len(list.Items), nil
}

// SplitPlan is what the evidence says about a split (a move interrupted
// part way): which side is the move's source, and how far the move got.
type SplitPlan struct {
	Source, Target Side
	// Deleting means the move was interrupted while clusterctl deleted the
	// source: the target is complete, the source depleted. Such a move is
	// past the point of no return: the source must never be resumed.
	Deleting bool
}

// ClassifySplit reads, from evidence, never from which command is running,
// which of two sides holding the Cluster is the source of an interrupted
// move, and how far it got.
//
// clusterctl pauses the source, creates every object on the target (new
// UIDs, paused, the source's annotations copied), then deletes the source's
// objects, the Cluster last, and only then resumes the target. A paused
// provider takes nothing over. So:
//   - interrupted while creating: every HydraMachine on the source holds its
//     VM (owner-uid equal to its own UID), the target holds none, and the
//     source has at least as many machines as the target;
//   - interrupted while deleting: the target has the full set and holds
//     none, the source has fewer (perhaps none), all still holding theirs.
//
// Anything else is refused: guessing wrong would resume a partial copy,
// whose machines would claim VMs from the complete one, or a depleted
// source, whose controllers would recreate the machines already deleted.
func ClassifySplit(ctx context.Context, a, b Side, namespace string) (SplitPlan, error) {
	ao, at, err := takenOver(ctx, a.C, namespace)
	if err != nil {
		return SplitPlan{}, fmt.Errorf("reading HydraMachines on the %s: %w", a.Name, err)
	}
	bo, bt, err := takenOver(ctx, b.C, namespace)
	if err != nil {
		return SplitPlan{}, fmt.Errorf("reading HydraMachines on the %s: %w", b.Name, err)
	}
	switch {
	case at > 0 && ao == at && bo == 0 && at >= bt:
		return SplitPlan{Source: a, Target: b}, nil
	case bt > 0 && bo == bt && ao == 0 && bt >= at:
		return SplitPlan{Source: b, Target: a}, nil
	case bt > 0 && bo == 0 && ao == at && at < bt:
		return SplitPlan{Source: a, Target: b, Deleting: true}, nil
	case at > 0 && ao == 0 && bo == bt && bt < at:
		return SplitPlan{Source: b, Target: a, Deleting: true}, nil
	}
	return SplitPlan{}, fmt.Errorf("cannot tell which copy is the source of the interrupted move: the %s has %d of %d HydraMachines holding their VM, the %s %d of %d. A move interrupted while creating leaves a complete source holding all its machines and a target holding none; one interrupted while deleting leaves the target complete and holding none, and the source with fewer. Inspect both and finish the move by hand", a.Name, ao, at, b.Name, bo, bt)
}

// SplitSource is ClassifySplit's source and target, for a split interrupted
// while creating; a split interrupted while deleting, or an unclear one, is
// an error here.
func SplitSource(ctx context.Context, a, b Side, namespace string) (source, target Side, err error) {
	plan, err := ClassifySplit(ctx, a, b, namespace)
	if err != nil {
		return Side{}, Side{}, err
	}
	if plan.Deleting {
		return Side{}, Side{}, fmt.Errorf("the move from the %s was interrupted while deleting it; the source must not be resumed", plan.Source.Name)
	}
	return plan.Source, plan.Target, nil
}

// manualFinish is the by-hand finish of a split interrupted while creating.
func manualFinish(source, target Side, namespace, name string) string {
	return steps([]Side{source, target},
		unpauseCommand(source.ref(), namespace, name),
		fmt.Sprintf("clusterctl move --kubeconfig %s --to-kubeconfig %s --namespace %s", source.ref(), target.ref(), namespace))
}

// splitAdvice says what to do about a split without --finish-split.
func splitAdvice(plan SplitPlan, err error, rerun, manual string) string {
	switch {
	case err != nil:
		return err.Error()
	case plan.Deleting:
		return fmt.Sprintf("The move from the %s to %s was interrupted while clusterctl deleted the source: %s is complete. Never unpause the source (its controllers would recreate the machines already deleted). Rerun %s with --finish-split: it removes the source's leftover objects as clusterctl does (finalizers stripped, so nothing is torn down) and resumes the complete copy", plan.Source.Name, plan.Target.Name, plan.Target.Name, rerun)
	}
	return fmt.Sprintf("The move from the %s to %s was interrupted while creating: the source is intact. Rerun %s with --finish-split: it unpauses the source and finishes the move. Or by hand, with clusterctl: %s. Then rerun this command to verify", plan.Source.Name, plan.Target.Name, rerun, manual)
}

// finishDeletion completes a move interrupted while clusterctl deleted the
// source, without ever resuming the source: it removes the source's leftover
// objects as clusterctl's deleteSourceObject does (the delete-for-move
// annotation, so providers skip their external cleanup, then delete, then
// strip finalizers), the Cluster last, and resumes the complete target.
func (p *Pivot) finishDeletion(ctx context.Context, plan SplitPlan) error {
	tcl, err := p.getCluster(ctx, plan.Target.C)
	if err != nil {
		return fmt.Errorf("reading the Cluster on %s: %w", plan.Target.Name, err)
	}
	if _, ok := tcl.GetAnnotations()[pausedAnnotation]; ok {
		return fmt.Errorf("the complete copy on %s carries the %s annotation, which clusterctl does not set: someone paused it on purpose. Remove it yourself, then rerun", plan.Target.Name, pausedAnnotation)
	}
	p.logf("finishing the move interrupted while deleting the %s: removing its leftover objects, then resuming %s", plan.Source.Name, plan.Target.Name)
	for _, gvk := range append(append([]schema.GroupVersionKind{}, movableKinds...), clusterGVK) {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := plan.Source.C.List(ctx, list, client.InNamespace(p.Namespace)); err != nil {
			if meta.IsNoMatchError(err) {
				continue
			}
			return fmt.Errorf("listing %s on the %s: %w", gvk.Kind, plan.Source.Name, err)
		}
		for i := range list.Items {
			if err := deleteForMove(ctx, plan.Source.C, &list.Items[i]); err != nil {
				return fmt.Errorf("removing %s %s from the %s: %w", gvk.Kind, list.Items[i].GetName(), plan.Source.Name, err)
			}
		}
	}
	// The rest of what the move carries, as clusterctl's graph does: the
	// cluster's Secrets (and any labelled for move, the credentials Secret),
	// its ConfigMaps (the CNI one) and MachineHealthChecks.
	if err := p.deleteLeftoverExtras(ctx, plan.Source); err != nil {
		return err
	}
	if specPaused, _, _ := unstructured.NestedBool(tcl.Object, "spec", "paused"); specPaused {
		patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"paused":false}}`))
		if err := plan.Target.C.Patch(ctx, tcl, patch); err != nil {
			return fmt.Errorf("resuming the Cluster on %s: %w", plan.Target.Name, err)
		}
	}
	return nil
}

// deleteLeftoverExtras removes from the source the objects the move carries
// beyond movableKinds: this cluster's Secrets and ConfigMaps (by its labels or
// an owner reference to its Cluster), Secrets labelled for move, and
// MachineHealthChecks.
func (p *Pivot) deleteLeftoverExtras(ctx context.Context, source Side) error {
	for _, kind := range []schema.GroupVersionKind{
		secretGVK, configMapGVK, mhcGVK,
	} {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(kind.GroupVersion().WithKind(kind.Kind + "List"))
		if err := source.C.List(ctx, list, client.InNamespace(p.Namespace)); err != nil {
			if meta.IsNoMatchError(err) {
				continue
			}
			return fmt.Errorf("listing %s on the %s: %w", kind.Kind, source.Name, err)
		}
		for i := range list.Items {
			o := &list.Items[i]
			_, moveLabelled := o.GetLabels()[moveLabel]
			if o.GetName() == TombstoneName || (!p.belongs(o) && (kind != secretGVK || !moveLabelled)) {
				continue
			}
			if err := deleteForMove(ctx, source.C, o); err != nil {
				return fmt.Errorf("removing %s %s from the %s: %w", kind.Kind, o.GetName(), source.Name, err)
			}
		}
	}
	return nil
}

// deleteForMove removes one object the way clusterctl move removes a moved
// object from its source.
func deleteForMove(ctx context.Context, c client.Client, o *unstructured.Unstructured) error {
	annotate := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"`+deleteForMoveAnnotation+`":""}}}`))
	if err := c.Patch(ctx, o, annotate); err != nil {
		return client.IgnoreNotFound(err)
	}
	if err := c.Delete(ctx, o); err != nil {
		return client.IgnoreNotFound(err)
	}
	if len(o.GetFinalizers()) > 0 {
		strip := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":[]}}`))
		if err := c.Patch(ctx, o, strip); err != nil {
			return client.IgnoreNotFound(err)
		}
	}
	return nil
}

// finishSplit completes an interrupted move from source to target: it
// unpauses the source copy (clusterctl will not move a paused cluster) and
// runs the move again, which updates the objects already on the target.
// The caller has established source by SplitSource.
func (p *Pivot) finishSplit(ctx context.Context, source, target Side) error {
	cl, err := p.getCluster(ctx, source.C)
	if err != nil {
		return fmt.Errorf("reading the Cluster on the %s: %w", source.Name, err)
	}
	if _, ok := cl.GetAnnotations()[pausedAnnotation]; ok {
		return fmt.Errorf("the source copy on the %s carries the %s annotation, which clusterctl does not set: someone paused it on purpose. Remove it yourself if that pause is over, then rerun", source.Name, pausedAnnotation)
	}
	if specPaused, _, _ := unstructured.NestedBool(cl.Object, "spec", "paused"); specPaused {
		patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"paused":false}}`))
		if err := source.C.Patch(ctx, cl, patch); err != nil {
			return fmt.Errorf("unpausing the source copy on the %s: %w", source.Name, err)
		}
	}
	p.logf("finishing the interrupted move from the %s to %s", source.Name, target.Name)
	if err := p.Hooks.Move(ctx, source.Kubeconfig, target.Kubeconfig, p.Namespace, false); err != nil {
		return fmt.Errorf("clusterctl move: %w (rerun: it finds where the objects are and continues)", err)
	}
	return nil
}
