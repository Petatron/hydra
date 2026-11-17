package pivot

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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

// SplitSource decides which of two sides that both hold the Cluster is the
// source of an interrupted move, from evidence, never from which command is
// running.
//
// clusterctl creates the target's objects with new UIDs and paused, and
// copies the source's annotations; a paused provider takes nothing over. So
// on the source every HydraMachine has owner-uid equal to its own UID, and
// on the target copy none does. Anything else -- no machines, a mix, both
// sides owning -- is refused: guessing wrong would unpause a partial copy,
// whose machines would then claim VMs from the complete one.
func SplitSource(ctx context.Context, a, b Side, namespace string) (source, target Side, err error) {
	ao, at, err := takenOver(ctx, a.C, namespace)
	if err != nil {
		return Side{}, Side{}, fmt.Errorf("reading HydraMachines on the %s: %w", a.Name, err)
	}
	bo, bt, err := takenOver(ctx, b.C, namespace)
	if err != nil {
		return Side{}, Side{}, fmt.Errorf("reading HydraMachines on the %s: %w", b.Name, err)
	}
	// The source must also hold at least as many machines as the target: a
	// source with fewer was interrupted while clusterctl deleted it, and
	// unpausing it would let its controllers recreate the deleted machines.
	switch {
	case at > 0 && ao == at && bo == 0 && at >= bt:
		return a, b, nil
	case bt > 0 && bo == bt && ao == 0 && bt >= at:
		return b, a, nil
	}
	return Side{}, Side{}, fmt.Errorf("cannot tell which copy is the source of the interrupted move: the %s has %d of %d HydraMachines holding their VM, the %s %d of %d. A move interrupted while creating leaves a complete source holding all its machines and a target holding none; one interrupted while deleting leaves the target complete and the source with fewer, or none, and must not be resumed from the source. Inspect both and finish the move by hand", a.Name, ao, at, b.Name, bo, bt)
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
