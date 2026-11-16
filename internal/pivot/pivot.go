// Package pivot moves a cluster's Cluster API objects from the management
// cluster that built it (usually the temporary kind cluster) onto the cluster
// itself, so it manages itself (ADR-001; what PET-74 did by hand).
//
// Every run rediscovers where the objects are -- on the source, on the target,
// or split by an interrupted move -- and continues from there. Nothing is
// recorded locally. The move is verified on the target before anything on the
// source is removed. A move whose verification fails stays on the target: it
// is never moved back, and nothing is ever unpaused, automatically (see Run).
package pivot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Petatron/hydra/internal/cluster"
	"github.com/Petatron/hydra/internal/render"
)

// Annotations the Hydra provider uses to carry a machine's identity across a
// move (cluster-api-provider-hydra PET-76, docs/moving.md).
const (
	IdentityUIDAnnotation = "hydramachine.infrastructure.cluster.x-k8s.io/identity-uid"
	OwnerUIDAnnotation    = "hydramachine.infrastructure.cluster.x-k8s.io/owner-uid"

	moveLabel = "clusterctl.cluster.x-k8s.io/move"
	// pausedAnnotation is Cluster API's other way to pause a cluster.
	pausedAnnotation = "cluster.x-k8s.io/paused"
	clusterNameLabel = "cluster.x-k8s.io/cluster-name"
	capiVersion      = "v1beta2"
	clusterctlCRD    = "clusterctl.cluster.x-k8s.io"
)

var (
	clusterGVK      = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: capiVersion, Kind: "Cluster"}
	hydraMachineGVK = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha1", Kind: "HydraMachine"}
	hydraClusterGVK = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha1", Kind: "HydraCluster"}
	crsGVK          = schema.GroupVersionKind{Group: "addons.cluster.x-k8s.io", Version: capiVersion, Kind: "ClusterResourceSet"}
	hydraCRDs       = []string{
		"hydraclusters.infrastructure.cluster.x-k8s.io",
		"hydramachines.infrastructure.cluster.x-k8s.io",
		"hydramachinetemplates.infrastructure.cluster.x-k8s.io",
	}
)

// Side is one management cluster in a pivot.
type Side struct {
	Name       string // for messages: "bootstrap cluster", "pet82"
	C          client.Client
	Kubeconfig []byte
	// File is a kubeconfig path an operator can pass to kubectl or
	// clusterctl, and Prep the command that writes it first, if any. They
	// make the commands in error messages ones that can be run as printed.
	File, Prep string
}

// ref is the side's kubeconfig as a command argument.
func (s Side) ref() string { return s.File }

// steps prefixes cmds with whatever writes the kubeconfig files they use.
func steps(sides []Side, cmds ...string) string {
	var out []string
	for _, s := range sides {
		if s.Prep != "" {
			out = append(out, s.Prep)
		}
	}
	out = append(out, cmds...)
	for i, c := range out {
		out[i] = fmt.Sprintf("(%d) %s", i+1, c)
	}
	return strings.Join(out, " ")
}

// Hooks are the operations a pivot delegates, replaced in tests.
type Hooks struct {
	// EnsureProviders installs the pinned providers on the target.
	EnsureProviders func(ctx context.Context, kubeconfig []byte) error
	// Move runs clusterctl move for the namespace.
	Move func(ctx context.Context, from, to []byte, namespace string, dryRun bool) error
}

// Pivot moves one cluster from Source to Target.
type Pivot struct {
	Source, Target Side
	Namespace      string
	Name           string
	Hooks          Hooks
	Out            io.Writer
	Poll           time.Duration
}

func (p *Pivot) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(p.Out, format+"\n", args...)
}

func (p *Pivot) poll() time.Duration {
	if p.Poll > 0 {
		return p.Poll
	}
	return 10 * time.Second
}

// Where says which side holds the cluster's Cluster object.
type Where int

// Possible locations of the Cluster object.
const (
	Nowhere Where = iota
	OnSource
	OnTarget
	Split
)

func (w Where) String() string {
	return [...]string{"nowhere", "on the source", "on the target", "on both (split)"}[w]
}

// Locate finds the Cluster object on the two sides.
func (p *Pivot) Locate(ctx context.Context) (Where, error) {
	src, err := hasCluster(ctx, p.Source.C, p.Namespace, p.Name)
	if err != nil {
		return Nowhere, fmt.Errorf("reading the %s: %w", p.Source.Name, err)
	}
	tgt, err := hasCluster(ctx, p.Target.C, p.Namespace, p.Name)
	if err != nil {
		return Nowhere, fmt.Errorf("reading %s: %w", p.Target.Name, err)
	}
	switch {
	case src && tgt:
		return Split, nil
	case src:
		return OnSource, nil
	case tgt:
		return OnTarget, nil
	}
	return Nowhere, nil
}

func hasCluster(ctx context.Context, c client.Client, namespace, name string) (bool, error) {
	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(clusterGVK)
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cl)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return false, nil
	}
	return false, err
}

// ErrAlreadySelfHosted is returned when source and target are one cluster:
// the cluster already manages itself, and there is nothing to move. Moving a
// cluster onto itself would delete the only copy of its objects.
var ErrAlreadySelfHosted = errors.New("the source and target are the same cluster: it already manages itself")

// ErrSplit is returned when an interrupted move left objects on both sides.
var ErrSplit = errors.New("the Cluster object exists on both management clusters: a move was interrupted part way")

// Run takes the cluster from wherever it is to verified on the target. It
// returns nil only when the target manages the cluster and the source no
// longer does; the caller may then remove the source.
//
// It never moves a cluster back. Once moved, a machine's VM may already have
// been claimed by the target's provider: the provider claims it in the
// backend record first and records that on the HydraMachine (owner-uid)
// afterwards, in a patch that can fail and be retried. Nothing visible from
// here proves that no VM was claimed, and moving a claimed machine back
// strands it. So a cluster that does not verify stays on the target and the
// error says how to inspect it, and how to undo the move by hand.
//
// It never unpauses anything either: a pause left by an interrupted move and
// one an operator set look the same, so it says what to run instead.
func (p *Pivot) Run(ctx context.Context, verifyTimeout time.Duration) error {
	// Both kubeconfigs reaching one cluster looks like a split to Locate, and
	// clusterctl move has no same-cluster check: it would delete the only
	// copy. Tell them apart by certificate authority before anything else.
	same, err := cluster.SameCA(p.Source.Kubeconfig, p.Target.Kubeconfig)
	if err != nil {
		return fmt.Errorf("telling the %s and %s apart: %w", p.Source.Name, p.Target.Name, err)
	}
	if same {
		return ErrAlreadySelfHosted
	}
	where, err := p.Locate(ctx)
	if err != nil {
		return err
	}
	p.logf("Cluster %s/%s is %s", p.Namespace, p.Name, where)
	switch where {
	case Nowhere:
		return fmt.Errorf("the Cluster %s/%s exists on neither management cluster", p.Namespace, p.Name)
	case Split:
		return fmt.Errorf("%w. Do not delete either copy. clusterctl paused the source copy and has not finished creating the target. To finish the move, unpause the source copy and run the move again with clusterctl itself, which updates the objects already on the target: %s. Then rerun this command to verify", ErrSplit,
			steps([]Side{p.Source, p.Target},
				unpauseCommand(p.Source.ref(), p.Namespace, p.Name),
				fmt.Sprintf("clusterctl move --kubeconfig %s --to-kubeconfig %s --namespace %s", p.Source.ref(), p.Target.ref(), p.Namespace)))
	case OnTarget:
		cl, err := p.getCluster(ctx, p.Target.C)
		if err != nil {
			return err
		}
		if why := pauseReason(cl); why != "" {
			return fmt.Errorf("the Cluster on %s is paused (%s). A move interrupted after it deleted the source copy leaves it so, but so does an operator's pause, and pivot cannot tell which. If no one paused it on purpose, %s, then rerun to verify", p.Target.Name, why, unpauseAdvice(cl, p.Target, p.Namespace, p.Name))
		}
		p.logf("already on %s; verifying only", p.Target.Name)
		if err := p.verify(ctx, verifyTimeout); err != nil {
			return fmt.Errorf("%s holds the cluster but it does not verify: %w. Nothing was moved; inspect it with `hydra cluster status --management-kubeconfig %s`", p.Target.Name, err, p.Target.ref())
		}
		return nil
	}

	// OnSource.
	if err := p.preflight(ctx); err != nil {
		return err
	}
	if err := p.move(ctx); err != nil {
		return err
	}
	if err := p.verify(ctx, verifyTimeout); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("interrupted before %s verified (%w). The cluster has moved and stays there; rerun pivot to keep verifying", p.Target.Name, err)
		}
		return fmt.Errorf("the cluster moved to %s but did not verify there: %w. It stays on %s, which manages it now; pivot never moves a cluster back automatically, because a VM may already have been taken over and moving it back would strand that machine. Inspect it with `hydra cluster status --management-kubeconfig %s` and rerun pivot to keep verifying. To undo the move by hand, first confirm in the target provider's log that no machine was taken over (\"Took over a machine moved from another management cluster\"), then: %s", p.Target.Name, err, p.Target.Name, p.Target.ref(),
			steps([]Side{p.Source}, fmt.Sprintf("clusterctl move --kubeconfig %s --to-kubeconfig %s --namespace %s", p.Target.ref(), p.Source.ref(), p.Namespace)))
	}
	return nil
}

func otherClusters(ctx context.Context, c client.Client, namespace, name string) ([]string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(clusterGVK.GroupVersion().WithKind("ClusterList"))
	if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var out []string
	for _, cl := range list.Items {
		if cl.GetName() != name {
			out = append(out, "Cluster "+cl.GetName())
		}
	}
	return out, nil
}

// machines lists this cluster's HydraMachines (by Cluster API's cluster-name
// label), never another cluster's in the same namespace.
func (p *Pivot) machines(ctx context.Context, c client.Client) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(hydraMachineGVK.GroupVersion().WithKind("HydraMachineList"))
	if err := c.List(ctx, list, client.InNamespace(p.Namespace), client.MatchingLabels{clusterNameLabel: p.Name}); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func paused(cl *unstructured.Unstructured) bool {
	return pauseReason(cl) != ""
}

// pauseReason says how a Cluster is paused, or "" when it is not.
func pauseReason(cl *unstructured.Unstructured) string {
	var why []string
	if p, _, _ := unstructured.NestedBool(cl.Object, "spec", "paused"); p {
		why = append(why, "spec.paused is true")
	}
	if _, ok := cl.GetAnnotations()[pausedAnnotation]; ok {
		why = append(why, "it carries the "+pausedAnnotation+" annotation")
	}
	return strings.Join(why, ", and ")
}

func unpauseCommand(kubeconfig, namespace, name string) string {
	return fmt.Sprintf(`kubectl --kubeconfig %s -n %s patch cluster %s --type=merge -p '{"spec":{"paused":false}}'`, kubeconfig, namespace, name)
}

// unpauseAdvice names the commands that clear every pause cl has.
func unpauseAdvice(cl *unstructured.Unstructured, side Side, namespace, name string) string {
	var cmds []string
	if p, _, _ := unstructured.NestedBool(cl.Object, "spec", "paused"); p {
		cmds = append(cmds, unpauseCommand(side.ref(), namespace, name))
	}
	if _, ok := cl.GetAnnotations()[pausedAnnotation]; ok {
		cmds = append(cmds, fmt.Sprintf("kubectl --kubeconfig %s -n %s annotate cluster %s %s-", side.ref(), namespace, name, pausedAnnotation))
	}
	return "run: " + steps([]Side{side}, cmds...)
}

func (p *Pivot) getCluster(ctx context.Context, c client.Client) (*unstructured.Unstructured, error) {
	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(clusterGVK)
	return cl, c.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: p.Name}, cl)
}

// preflight is docs/moving.md's checklist, run before anything moves.
func (p *Pivot) preflight(ctx context.Context) error {
	src := &cluster.Manager{C: p.Source.C, Out: io.Discard}
	st, err := src.Status(ctx, p.Namespace, p.Name)
	if err != nil {
		return err
	}
	if st.Terminating {
		return fmt.Errorf("the Cluster %s/%s is being deleted; not moving it", p.Namespace, p.Name)
	}
	cl, err := p.getCluster(ctx, p.Source.C)
	if err != nil {
		return err
	}
	if why := pauseReason(cl); why != "" {
		return fmt.Errorf("the Cluster %s/%s is paused on the %s (%s), and clusterctl refuses to move a paused cluster. An earlier move that failed before reaching the target can leave it so. If no move is in progress and no one paused it on purpose, %s, then rerun pivot", p.Namespace, p.Name, p.Source.Name, why, unpauseAdvice(cl, p.Source, p.Namespace, p.Name))
	}
	if st.Available != cluster.ConditionTrue || !st.RolledOut {
		return fmt.Errorf("the Cluster %s/%s is not settled (%s); move only an Available cluster with no rollout in progress", p.Namespace, p.Name, st.Line())
	}

	var problems []string
	// clusterctl moves the whole namespace.
	others, err := otherClusters(ctx, p.Source.C, p.Namespace, p.Name)
	if err != nil {
		return fmt.Errorf("listing Clusters: %w", err)
	}
	for _, o := range others {
		problems = append(problems, fmt.Sprintf("namespace %s also holds %s, and clusterctl move would take it too", p.Namespace, o))
	}
	// Every machine must carry its identity across (PET-76).
	machines, err := p.machines(ctx, p.Source.C)
	if err != nil {
		return fmt.Errorf("listing HydraMachines: %w", err)
	}
	if len(machines) == 0 {
		problems = append(problems, "no HydraMachines found; nothing would be taken over")
	}
	for _, m := range machines {
		a := m.GetAnnotations()
		if a[IdentityUIDAnnotation] == "" || a[OwnerUIDAnnotation] != string(m.GetUID()) {
			problems = append(problems, fmt.Sprintf("HydraMachine %s has not recorded its identity (%s, %s=own UID); it could not take its VM over after the move", m.GetName(), IdentityUIDAnnotation, OwnerUIDAnnotation))
		}
	}
	// The hypervisor credentials must go with the cluster. The live
	// HydraCluster says which Secret it uses; a cluster that names no
	// hypervisor depends on the source manager's own settings, which a move
	// cannot carry.
	hc := &unstructured.Unstructured{}
	hc.SetGroupVersionKind(hydraClusterGVK)
	if err := p.Source.C.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: p.Name}, hc); err != nil {
		return fmt.Errorf("reading HydraCluster %s/%s: %w", p.Namespace, p.Name, err)
	}
	secret, _, _ := unstructured.NestedString(hc.Object, "spec", "hypervisor", "credentialsSecretRef", "name")
	if secret == "" {
		problems = append(problems, "the HydraCluster names no hypervisor (spec.hypervisor); it relies on the source manager's libvirt-config, which does not move, so the target could not reach its VMs")
	} else {
		// The same completeness check create makes: the target provider
		// reads these keys, and once the bootstrap cluster is gone nothing
		// else can reach the VMs.
		sec := &unstructured.Unstructured{}
		sec.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Secret"})
		if err := src.CheckCredentials(ctx, p.Namespace, secret); err != nil {
			problems = append(problems, err.Error())
		} else if err := p.Source.C.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: secret}, sec); err != nil {
			problems = append(problems, fmt.Sprintf("reading credentials Secret %s: %v", secret, err))
		} else if sec.GetDeletionTimestamp() != nil {
			problems = append(problems, fmt.Sprintf("credentials Secret %s is being deleted", secret))
		} else if _, ok := sec.GetLabels()[moveLabel]; !ok {
			problems = append(problems, fmt.Sprintf("credentials Secret %s lacks the %s label, so the move would leave it behind", secret, moveLabel))
		}
	}
	if err := crdsLabelled(ctx, p.Source.C); err != nil {
		problems = append(problems, "source: "+err.Error())
	}
	if len(problems) > 0 {
		return fmt.Errorf("not moving; fix these first:\n  %s", strings.Join(problems, "\n  "))
	}

	p.logf("installing the pinned providers on %s", p.Target.Name)
	if err := p.Hooks.EnsureProviders(ctx, p.Target.Kubeconfig); err != nil {
		return fmt.Errorf("installing providers on %s: %w", p.Target.Name, err)
	}
	if err := crdsLabelled(ctx, p.Target.C); err != nil {
		return fmt.Errorf("%s: %w", p.Target.Name, err)
	}
	p.logf("checking the move (dry run)")
	if err := p.Hooks.Move(ctx, p.Source.Kubeconfig, p.Target.Kubeconfig, p.Namespace, true); err != nil {
		return fmt.Errorf("clusterctl move dry run: %w", err)
	}
	return nil
}

// crdsLabelled checks the Hydra CRDs carry the label clusterctl move looks
// types up by; without it the move silently skips every Hydra object.
func crdsLabelled(ctx context.Context, c client.Client) error {
	for _, name := range hydraCRDs {
		crd := &unstructured.Unstructured{}
		crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
		if err := c.Get(ctx, client.ObjectKey{Name: name}, crd); err != nil {
			return fmt.Errorf("reading CRD %s: %w", name, err)
		}
		if _, ok := crd.GetLabels()[clusterctlCRD]; !ok {
			return fmt.Errorf("CRD %s lacks the %s label, so clusterctl move would skip its objects", name, clusterctlCRD)
		}
	}
	return nil
}

func (p *Pivot) move(ctx context.Context) error {
	p.logf("moving namespace %s from the %s to %s", p.Namespace, p.Source.Name, p.Target.Name)
	if err := p.Hooks.Move(ctx, p.Source.Kubeconfig, p.Target.Kubeconfig, p.Namespace, false); err != nil {
		return fmt.Errorf("clusterctl move: %w (rerun pivot: it finds where the objects are and continues)", err)
	}
	return nil
}

// verify waits until the target manages the cluster: the Cluster is Available
// and rolled out there, every HydraMachine has taken its VM over (owner-uid
// rewritten to its new UID) and is Ready, the CNI ClusterResourceSet came
// along, and the source no longer holds the Cluster.
func (p *Pivot) verify(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	last := ""
	for {
		err := p.verifyOnce(ctx)
		if err == nil {
			p.logf("verified: %s manages cluster %s/%s", p.Target.Name, p.Namespace, p.Name)
			return nil
		}
		if msg := err.Error(); msg != last {
			p.logf("  waiting: %s", msg)
			last = msg
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not verified within %s: %w", timeout, err)
		case <-time.After(p.poll()):
		}
	}
}

func (p *Pivot) verifyOnce(ctx context.Context) error {
	onSource, err := hasCluster(ctx, p.Source.C, p.Namespace, p.Name)
	if err != nil {
		return fmt.Errorf("reading the %s: %w", p.Source.Name, err)
	}
	if onSource {
		return fmt.Errorf("the %s still holds the Cluster", p.Source.Name)
	}
	tgt := &cluster.Manager{C: p.Target.C, Out: io.Discard}
	st, err := tgt.Status(ctx, p.Namespace, p.Name)
	if err != nil {
		return err
	}
	// Being deleted, or a CNI only present as the moved ClusterResourceSet
	// without its binding applied, is no management path to leave the
	// cluster on: the bootstrap cluster is deleted once this passes.
	if !st.Exists || st.Terminating || st.Available != cluster.ConditionTrue || !st.RolledOut || !st.CNIApplied {
		return fmt.Errorf("the Cluster on %s: %s", p.Target.Name, st.Line())
	}
	if cl, err := p.getCluster(ctx, p.Target.C); err != nil {
		return err
	} else if paused(cl) {
		return fmt.Errorf("the Cluster on %s is still paused", p.Target.Name)
	}
	machines, err := p.machines(ctx, p.Target.C)
	if err != nil {
		return fmt.Errorf("listing HydraMachines on %s: %w", p.Target.Name, err)
	}
	if len(machines) == 0 {
		return fmt.Errorf("no HydraMachines on %s yet", p.Target.Name)
	}
	for _, m := range machines {
		if m.GetAnnotations()[OwnerUIDAnnotation] != string(m.GetUID()) {
			return fmt.Errorf("HydraMachine %s has not taken its VM over yet", m.GetName())
		}
		if !conditionTrue(&m, "Ready") {
			return fmt.Errorf("HydraMachine %s is not Ready", m.GetName())
		}
	}
	crs := &unstructured.Unstructured{}
	crs.SetGroupVersionKind(crsGVK)
	if err := p.Target.C.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: render.CNIName(p.Name)}, crs); err != nil {
		return fmt.Errorf("the CNI ClusterResourceSet did not arrive on %s: %w", p.Target.Name, err)
	}
	return nil
}

func conditionTrue(o *unstructured.Unstructured, typ string) bool {
	conds, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, c := range conds {
		if cm, ok := c.(map[string]any); ok && cm["type"] == typ {
			return cm["status"] == cluster.ConditionTrue
		}
	}
	return false
}
