package pivot

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Petatron/hydra/internal/cluster"
)

// TombstoneName is the ConfigMap Teardown leaves in the bootstrap kind
// cluster's copy of the namespace before it moves anything. It is the proof
// that a teardown of this cluster is under way, so an interrupted teardown can
// always be finished, and it records the cluster's certificate authority so
// the cluster's kubeconfig file is removed only when it is this cluster's.
// State lives in Kubernetes, as ADR-001 requires.
const TombstoneName = "hydra-teardown"

const (
	phaseMoving = "moving"
	phaseMoved  = "moved"
)

// TeardownHooks are the operations a teardown delegates; tests replace them.
type TeardownHooks struct {
	// EnsureKind brings the bootstrap kind cluster up with the pinned
	// providers and without hypervisor credentials (they arrive with the
	// move), and returns it, and whether it had to create it.
	EnsureKind func(ctx context.Context) (kind Side, created bool, err error)
	// EnsureProviders and Move are as for Pivot.
	EnsureProviders func(ctx context.Context, kubeconfig []byte) error
	Move            func(ctx context.Context, from, to []byte, namespace string, dryRun bool) error
	// DeleteCluster deletes the cluster from the bootstrap kind cluster:
	// label-scoped, with its ownership checks.
	DeleteCluster func(ctx context.Context, kind Side) error
	// DeleteKind removes the bootstrap kind cluster; it refuses while kind
	// still manages a Cluster.
	DeleteKind func(ctx context.Context) error
	// RemoveKubeconfig removes the cluster's kubeconfig file.
	RemoveKubeconfig func() error
}

// Teardown removes a self-hosted cluster: it moves the cluster from itself
// into the bootstrap kind cluster, waits until kind's provider has taken
// every VM over, deletes the cluster there, then removes the cluster's
// kubeconfig file and kind. Each run reads where things are and does the next
// safe step, so an interrupted teardown is finished by running it again.
type Teardown struct {
	// Self is the cluster, reached with its own kubeconfig. Self.C is nil
	// when there is no kubeconfig file; SelfErr says why it could not be
	// connected to, if it could not.
	Self    Side
	SelfErr error
	// Kind is the bootstrap kind cluster; Kind.C is nil when it does not
	// exist.
	Kind            Side
	Namespace, Name string
	// Adopt deletes a Cluster hydra did not label (as `cluster delete
	// --adopt`).
	Adopt bool
	// FinishSplit completes a move interrupted part way, after proving from
	// the takeover evidence that the cluster itself holds the source copy.
	FinishSplit     bool
	TakeoverTimeout time.Duration
	Hooks           TeardownHooks
	Out             io.Writer
	Poll            time.Duration
	// createdKind records that EnsureKind created kind in this run.
	createdKind bool
	// steps counts state transitions within one Run, so a step that does
	// not change anything stops instead of looping.
	steps int
}

// holds is a three-valued read: unreachable is never "does not hold".
type holds int

const (
	holdsNo holds = iota
	holdsYes
	holdsUnknown
)

func (t *Teardown) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(t.Out, format+"\n", args...)
}

func (t *Teardown) poll() time.Duration {
	if t.Poll > 0 {
		return t.Poll
	}
	return 5 * time.Second
}

// pivot is a Pivot from the cluster itself to kind, for the helpers the
// two share: preflight, move, split handling.
func (t *Teardown) pivot() *Pivot {
	return &Pivot{
		Source: t.Self, Target: t.Kind, Namespace: t.Namespace, Name: t.Name,
		Hooks:          Hooks{EnsureProviders: t.Hooks.EnsureProviders, Move: t.Hooks.Move},
		allowUnsettled: true,
		Rerun:          "`hydra cluster delete --self-hosted`",
		Out:            t.Out, Poll: t.Poll,
	}
}

// Run tears the cluster down from wherever the last run left it.
func (t *Teardown) Run(ctx context.Context) error {
	if t.steps++; t.steps > 3 {
		return errors.New("the teardown made no progress: a step reported success but the cluster is where it was. Rerun, and inspect both sides if it persists")
	}
	self, selfErr := t.selfHolds(ctx)
	kindHolds := false
	if t.Kind.C != nil {
		var err error
		if kindHolds, err = hasCluster(ctx, t.Kind.C, t.Namespace, t.Name); err != nil {
			return fmt.Errorf("reading the %s: %w", t.Kind.Name, err)
		}
	}
	tomb, err := t.tombstone(ctx)
	if err != nil {
		return err
	}
	// A record left by an earlier cluster of this name in a kept kind is not
	// evidence for this one: its CA differs from this kubeconfig's.
	if tomb != nil && t.Self.Kubeconfig != nil && tomb.Data["ca"] != "" &&
		tomb.Data["ca"] != base64.StdEncoding.EncodeToString(cluster.KubeconfigCA(t.Self.Kubeconfig)) {
		return fmt.Errorf("the teardown record in the %s was left by another cluster named %s (a different certificate authority than %s's). Remove the bootstrap cluster (`hydra bootstrap delete --force`) once you have checked it manages nothing, then rerun", t.Kind.Name, t.Name, t.Self.File)
	}
	switch {
	case self == holdsYes && kindHolds:
		return t.split(ctx, tomb)
	case self == holdsYes:
		if err := t.moveToKind(ctx, tomb); err != nil {
			return err
		}
		return t.Run(ctx)
	case kindHolds:
		if self == holdsUnknown && (tomb == nil || tomb.Data["phase"] != phaseMoved) {
			return fmt.Errorf("the %s holds Cluster %s/%s, but %s cannot be reached (%v) to show it no longer holds its own copy, and no finished move is recorded. Restore access to it and rerun", t.Kind.Name, t.Namespace, t.Name, t.Self.Name, selfErr)
		}
		return t.finishInKind(ctx, tomb)
	}
	// Neither side holds the Cluster. With the cluster itself unseen, only a
	// finished move proves its copy is gone: a move that paused it and then
	// failed leaves it running, paused, with nothing in kind.
	if tomb != nil && (self != holdsUnknown || tomb.Data["phase"] == phaseMoved) {
		return t.cleanup(ctx, tomb.Data["ca"], tomb.Data["createdKind"] == recordTrue)
	}
	if self == holdsUnknown {
		return fmt.Errorf("%s cannot be reached (%v), and nothing shows a teardown under way: it may have been deleted already, or it may be down and never moved. hydra cannot tell which, so it changes nothing. If the cluster is gone, remove its kubeconfig file yourself", t.Self.Name, selfErr)
	}
	if t.Self.C == nil {
		return ErrNothingToTearDown
	}
	return fmt.Errorf("%s answers but holds no Cluster %s/%s, and the %s holds none either: this is not that cluster managing itself. Check the kubeconfig and namespace", t.Self.Name, t.Namespace, t.Name, t.kindName())
}

// ErrNothingToTearDown says no trace of the cluster was found: no kubeconfig
// file, no bootstrap cluster holding it, no teardown under way.
var ErrNothingToTearDown = errors.New("found nothing to tear down")

func (t *Teardown) kindName() string {
	if t.Kind.Name != "" {
		return t.Kind.Name
	}
	return defaultKindName
}

// defaultKindName names the bootstrap kind cluster in messages when the
// caller gave it no name.
const defaultKindName = "bootstrap cluster"

func (t *Teardown) selfHolds(ctx context.Context) (holds, error) {
	if t.Self.C == nil {
		switch {
		case t.SelfErr != nil:
			return holdsUnknown, t.SelfErr
		case t.Kind.C != nil:
			// No kubeconfig for the cluster is no evidence it holds nothing:
			// with a bootstrap cluster around, a teardown may be half done.
			return holdsUnknown, fmt.Errorf("no kubeconfig at %s for the cluster itself", t.Self.File)
		}
		return holdsNo, nil
	}
	ok, err := hasCluster(ctx, t.Self.C, t.Namespace, t.Name)
	switch {
	case err != nil:
		return holdsUnknown, err
	case ok:
		return holdsYes, nil
	}
	return holdsNo, nil
}

// moveToKind moves the cluster from itself into kind, after proving it is
// the cluster itself and it is this CLI's to delete, and leaving the
// tombstone first.
func (t *Teardown) moveToKind(ctx context.Context, tomb *corev1.ConfigMap) error {
	self := &cluster.Manager{C: t.Self.C, Out: io.Discard}
	st, err := self.State(ctx, t.Namespace, t.Name)
	if err != nil {
		return err
	}
	isSelf, _, err := self.SelfHosted(ctx, t.Namespace, t.Name, t.Self.Kubeconfig, st)
	if err != nil {
		return fmt.Errorf("cannot prove %s is cluster %s managing itself: %w", t.Self.Name, t.Name, err)
	}
	if !isSelf {
		return fmt.Errorf("%s holds Cluster %s/%s but is not that cluster (its certificate authority differs from the cluster's own): this is not a self-hosted cluster. Delete it from its management cluster with `hydra cluster delete --management-kubeconfig`", t.Self.Name, t.Namespace, t.Name)
	}
	// Ownership before anything moves: a Cluster moved into kind that
	// Delete then refuses would be stranded there.
	if err := st.CheckOwner(t.Name, t.Adopt); err != nil {
		return err
	}
	if tomb != nil && tomb.Data["phase"] == phaseMoving {
		// This teardown started a move that paused the cluster and failed
		// before creating anything in kind (kind holds no Cluster, or this
		// would be a split): the pause is clusterctl's, so lift it and retry.
		if err := t.resumeOwnPause(ctx); err != nil {
			return err
		}
	}
	kind, created, err := t.Hooks.EnsureKind(ctx)
	if err != nil {
		return err
	}
	t.Kind, t.createdKind = kind, created
	if err := t.kindNamespaceClean(ctx); err != nil {
		return err
	}
	p := t.pivot()
	if err := p.preflight(ctx); err != nil {
		return err
	}
	if err := t.writeTombstone(ctx, phaseMoving); err != nil {
		return err
	}
	if err := t.pauseHealthChecks(ctx); err != nil {
		return err
	}
	if err := p.move(ctx); err != nil {
		return err
	}
	return t.writeTombstone(ctx, phaseMoved)
}

// resumeOwnPause lifts the spec.paused clusterctl set on the cluster itself
// when this teardown's own move failed before creating anything in kind. A
// pause by annotation is someone's deliberate pause and is left alone.
func (t *Teardown) resumeOwnPause(ctx context.Context) error {
	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(clusterGVK)
	if err := t.Self.C.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: t.Name}, cl); err != nil {
		return err
	}
	if specPaused, _, _ := unstructured.NestedBool(cl.Object, "spec", "paused"); !specPaused {
		return nil
	}
	t.logf("the cluster is paused by this teardown's earlier move, which failed before reaching the %s; resuming it to move again", t.kindName())
	patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"paused":false}}`))
	return t.Self.C.Patch(ctx, cl, patch)
}

// split handles both sides holding the Cluster: a move interrupted part way.
func (t *Teardown) split(ctx context.Context, tomb *corev1.ConfigMap) error {
	plan, cerr := ClassifySplit(ctx, t.Self, t.Kind, t.Namespace)
	if cerr == nil && plan.Source.Name != t.Self.Name {
		return fmt.Errorf("%w. The evidence says the source copy is on the %s: it was left by an interrupted pivot, not by this teardown. Finish it with `hydra cluster pivot --finish-split`, never this command", ErrSplit, plan.Source.Name)
	}
	if !t.FinishSplit {
		return fmt.Errorf("%w. Do not delete either copy. %s", ErrSplit, splitAdvice(plan, cerr, "`hydra cluster delete --self-hosted`", manualFinish(t.Self, t.Kind, t.Namespace, t.Name)))
	}
	if cerr != nil {
		return cerr
	}
	if tomb == nil {
		// A split with the cluster as its source but no record was not
		// started by a teardown.
		return fmt.Errorf("both the cluster and the %s hold Cluster %s/%s, with the cluster as the source, but no teardown is recorded in the %s. Something else moved it; finish that move by hand", t.Kind.Name, t.Namespace, t.Name, t.Kind.Name)
	}
	var err error
	if plan.Deleting {
		err = t.pivot().finishDeletion(ctx, plan)
	} else {
		err = t.pivot().finishSplit(ctx, t.Self, t.Kind)
	}
	if err != nil {
		return err
	}
	if err := t.writeTombstone(ctx, phaseMoved); err != nil {
		return err
	}
	return t.Run(ctx) // the split is gone, or the step guard stops it
}

// finishInKind waits for kind's provider to take every VM over, deletes the
// cluster from kind, then cleans up.
func (t *Teardown) finishInKind(ctx context.Context, tomb *corev1.ConfigMap) error {
	if tomb == nil {
		// No record of a teardown: the kubeconfig answered and holds no
		// Cluster, but nothing yet shows it is this cluster's. Its CA must
		// match the cluster's own kubeconfig Secret, now in kind.
		kc, err := (&cluster.Manager{C: t.Kind.C, Out: io.Discard}).Kubeconfig(ctx, t.Namespace, t.Name)
		if err != nil {
			return fmt.Errorf("reading the cluster's kubeconfig Secret in the %s: %w", t.Kind.Name, err)
		}
		if same, err := cluster.SameCA(kc, t.Self.Kubeconfig); err != nil || !same {
			return fmt.Errorf("%s is not cluster %s's kubeconfig (its certificate authority differs from the cluster's own Secret in the %s), so nothing shows where the cluster's own copy is. Pass --cluster-kubeconfig with the right file", t.Self.File, t.Name, t.Kind.Name)
		}
	}
	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(clusterGVK)
	if err := t.Kind.C.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: t.Name}, cl); err != nil {
		return fmt.Errorf("reading the Cluster in the %s: %w", t.Kind.Name, err)
	}
	if _, ok := cl.GetAnnotations()[pausedAnnotation]; ok {
		return fmt.Errorf("the Cluster in the %s carries the %s annotation, which clusterctl does not set: someone paused it on purpose. Remove it yourself, then rerun", t.Kind.Name, pausedAnnotation)
	}
	if specPaused, _, _ := unstructured.NestedBool(cl.Object, "spec", "paused"); specPaused && cl.GetDeletionTimestamp() == nil {
		// spec.paused is also an operator's pause. Only this teardown's
		// record shows the pause is its own interrupted move's.
		if tomb == nil {
			return fmt.Errorf("the Cluster in the %s is paused, and no teardown record shows this teardown paused it: it may be an operator's pause. If it is not, %s, then rerun", t.Kind.Name, unpauseAdvice(cl, t.Kind, t.Namespace, t.Name))
		}
		// A move interrupted after it deleted the source copy and before it
		// resumed this one. The cluster itself is shown not to hold the
		// Cluster (or the move is recorded as finished), so this is the
		// only copy: resume it, or nothing is ever taken over.
		t.logf("the Cluster in the %s is paused, left so by an interrupted move; resuming it", t.Kind.Name)
		patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"paused":false}}`))
		if err := t.Kind.C.Patch(ctx, cl, patch); err != nil {
			return fmt.Errorf("resuming the Cluster in the %s: %w", t.Kind.Name, err)
		}
	}
	if err := t.waitTakenOver(ctx); err != nil {
		return err
	}
	// The cluster's CA, for removing its kubeconfig file only if it is this
	// cluster's: from the tombstone, or else from the cluster's own
	// kubeconfig Secret, read before Delete removes it.
	ca := ""
	if tomb != nil {
		ca = tomb.Data["ca"]
	} else if kc, err := (&cluster.Manager{C: t.Kind.C, Out: io.Discard}).Kubeconfig(ctx, t.Namespace, t.Name); err == nil {
		ca = base64.StdEncoding.EncodeToString(cluster.KubeconfigCA(kc))
	}
	t.logf("deleting cluster %s/%s from the %s", t.Namespace, t.Name, t.Kind.Name)
	if err := t.Hooks.DeleteCluster(ctx, t.Kind); err != nil {
		return err
	}
	return t.cleanup(ctx, ca, tomb != nil && tomb.Data["createdKind"] == recordTrue)
}

// waitTakenOver waits until every HydraMachine in the namespace in kind has
// taken its VM over. Deleting one before that wedges its finalizer: the
// provider refuses to delete a VM it does not own.
func (t *Teardown) waitTakenOver(ctx context.Context) error {
	timeout := t.TakeoverTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		owned, total, err := takenOver(ctx, t.Kind.C, t.Namespace)
		if err != nil {
			return fmt.Errorf("reading HydraMachines in the %s: %w", t.Kind.Name, err)
		}
		if owned == total {
			t.logf("the %s holds all %d machines' VMs", t.Kind.Name, total)
			return nil
		}
		if msg := fmt.Sprintf("%d of %d machines taken over by the %s", owned, total, t.Kind.Name); msg != last {
			t.logf("  waiting: %s", msg)
			last = msg
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("after %s, %s; deleting now would wedge the rest (the provider refuses to delete a VM it does not own). Check the %s's Hydra provider log, then rerun", timeout, last, t.Kind.Name)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("interrupted while waiting for the takeover (%s); rerun to continue", last)
		case <-time.After(t.poll()):
		}
	}
}

// cleanup removes the cluster's kubeconfig file, when it is this cluster's,
// and the bootstrap kind cluster.
func (t *Teardown) cleanup(ctx context.Context, caB64 string, deleteKind bool) error {
	if t.Self.Kubeconfig != nil && t.Hooks.RemoveKubeconfig != nil {
		ca, _ := base64.StdEncoding.DecodeString(caB64)
		switch {
		case len(ca) == 0:
			t.logf("leaving %s: nothing recorded says it is this cluster's", t.Self.File)
		case !bytes.Equal(ca, cluster.KubeconfigCA(t.Self.Kubeconfig)):
			t.logf("leaving %s: it trusts another certificate authority than cluster %s did", t.Self.File, t.Name)
		default:
			if err := t.Hooks.RemoveKubeconfig(); err != nil {
				return fmt.Errorf("removing %s: %w", t.Self.File, err)
			}
			t.logf("removed %s: cluster %s is gone", t.Self.File, t.Name)
		}
	}
	if t.Kind.C == nil {
		return nil
	}
	if !deleteKind {
		t.logf("the %s existed before this teardown; leaving it", t.Kind.Name)
		return nil
	}
	return t.Hooks.DeleteKind(ctx)
}

var (
	mhcGVK       = schema.GroupVersionKind{Group: clusterGVK.Group, Version: capiVersion, Kind: "MachineHealthCheck"}
	secretGVK    = schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
	configMapGVK = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
)

// recordTrue is the record's value for a fact that holds.
const recordTrue = "true"

func (t *Teardown) tombstone(ctx context.Context) (*corev1.ConfigMap, error) {
	if t.Kind.C == nil {
		return nil, nil
	}
	cm := &corev1.ConfigMap{}
	err := t.Kind.C.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: TombstoneName}, cm)
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("reading the teardown record in the %s: %w", t.Kind.Name, err)
	}
	if cm.Data["cluster"] != t.Name {
		return nil, fmt.Errorf("the teardown record in the %s names cluster %q, not %s", t.Kind.Name, cm.Data["cluster"], t.Name)
	}
	return cm, nil
}

// writeTombstone records, in kind, that this cluster's teardown is under
// way and how far the move got.
func (t *Teardown) writeTombstone(ctx context.Context, phase string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: t.Namespace}}
	if err := t.Kind.C.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating namespace %s in the %s: %w", t.Namespace, t.Kind.Name, err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: t.Namespace, Name: TombstoneName}}
	err := t.Kind.C.Get(ctx, client.ObjectKeyFromObject(cm), cm)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("reading the teardown record: %w", err)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data["cluster"] = t.Name
	cm.Data["phase"] = phase
	// Whether this teardown created kind, kept once true: a later run finds
	// kind already there, and only the first run's word counts.
	if t.createdKind {
		cm.Data["createdKind"] = recordTrue
	} else if _, ok := cm.Data["createdKind"]; !ok {
		cm.Data["createdKind"] = "false"
	}
	if t.Self.Kubeconfig != nil {
		cm.Data["ca"] = base64.StdEncoding.EncodeToString(cluster.KubeconfigCA(t.Self.Kubeconfig))
	}
	if apierrors.IsNotFound(err) {
		err = t.Kind.C.Create(ctx, cm)
	} else {
		err = t.Kind.C.Update(ctx, cm)
	}
	if err != nil {
		return fmt.Errorf("writing the teardown record in the %s: %w", t.Kind.Name, err)
	}
	return nil
}

// kindNamespaceClean refuses a kind whose copy of the namespace holds
// anything but this teardown's own record: stale objects would survive the
// move (clusterctl updates what exists and leaves the rest) and then be
// deleted, or be taken for a split.
func (t *Teardown) kindNamespaceClean(ctx context.Context) error {
	var found []string
	// MachineHealthChecks too: clusterctl carries one owned by this Cluster,
	// and a stale one left here, not paused, could remediate moved machines.
	for _, gvk := range append([]schema.GroupVersionKind{clusterGVK, mhcGVK}, movableKinds...) {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := t.Kind.C.List(ctx, list, client.InNamespace(t.Namespace)); err != nil {
			if meta.IsNoMatchError(err) {
				continue
			}
			return fmt.Errorf("listing %s in the %s: %w", gvk.Kind, t.Kind.Name, err)
		}
		for _, o := range list.Items {
			found = append(found, gvk.Kind+" "+o.GetName())
		}
	}
	// Secrets and ConfigMaps too: the move carries the credentials Secret
	// and the CNI ConfigMap, and a stale one would be mixed in.
	secrets := &corev1.SecretList{}
	if err := t.Kind.C.List(ctx, secrets, client.InNamespace(t.Namespace)); err != nil {
		return fmt.Errorf("listing Secrets in the %s: %w", t.Kind.Name, err)
	}
	for _, o := range secrets.Items {
		found = append(found, "Secret "+o.Name)
	}
	cms := &corev1.ConfigMapList{}
	if err := t.Kind.C.List(ctx, cms, client.InNamespace(t.Namespace)); err != nil {
		return fmt.Errorf("listing ConfigMaps in the %s: %w", t.Kind.Name, err)
	}
	for _, o := range cms.Items {
		// Kubernetes puts kube-root-ca.crt in every namespace; the record is
		// this teardown's own.
		if o.Name != "kube-root-ca.crt" && o.Name != TombstoneName {
			found = append(found, "ConfigMap "+o.Name)
		}
	}
	if len(found) > 0 {
		return fmt.Errorf("namespace %s in the %s already holds %v, left from an earlier run; the move would mix it with this cluster's objects. Remove the bootstrap cluster (`hydra bootstrap delete --force`) and rerun", t.Namespace, t.Kind.Name, found)
	}
	return nil
}

// pauseHealthChecks pauses every MachineHealthCheck in the namespace on the
// cluster itself before the move: one remediating a moved Machine before
// kind's provider has taken its VM over would wedge its finalizer. They move
// paused, and are deleted with the cluster.
func (t *Teardown) pauseHealthChecks(ctx context.Context) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(mhcGVK.GroupVersion().WithKind("MachineHealthCheckList"))
	if err := t.Self.C.List(ctx, list, client.InNamespace(t.Namespace)); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("listing MachineHealthChecks: %w", err)
	}
	for i := range list.Items {
		m := &list.Items[i]
		if _, ok := m.GetAnnotations()[pausedAnnotation]; ok {
			continue
		}
		patch := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"`+pausedAnnotation+`":""}}}`))
		if err := t.Self.C.Patch(ctx, m, patch); err != nil {
			return fmt.Errorf("pausing MachineHealthCheck %s: %w", m.GetName(), err)
		}
		t.logf("paused MachineHealthCheck %s for the move", m.GetName())
	}
	return nil
}
