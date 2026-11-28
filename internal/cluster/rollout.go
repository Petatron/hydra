package cluster

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Petatron/hydra/internal/render"
)

// The Cilium objects whose rollout says the CNI is up (PET-103), as the
// chart names them in the namespace hydra's proxy installs it into.
const (
	ciliumNamespace         = "kube-system"
	ciliumDaemonSet         = "cilium"
	ciliumAgentContainer    = "cilium-agent"
	ciliumOperator          = "cilium-operator"
	ciliumOperatorContainer = "cilium-operator"
)

// WorkloadTimeout bounds each request to a workload cluster's API, so one
// that cannot be reached shows as a reason in the status, not a hang.
const WorkloadTimeout = 15 * time.Second

// ConnectWorkload builds a client for a workload cluster from the kubeconfig
// in its Secret, with every request bounded by WorkloadTimeout.
func ConnectWorkload(kubeconfig []byte) (client.Client, error) {
	rc, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	rc.Timeout = WorkloadTimeout
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return client.New(rc, client.Options{Scheme: scheme})
}

// ReachVia returns a Manager.Workload that reads a workload cluster through c
// when kubeconfig (c's own) trusts the same certificate authority as the
// workload cluster's kubeconfig: the cluster is the one c already talks to,
// perhaps through a tunnel or another address than its Secret names. Any
// other cluster is dialled from its Secret with ConnectWorkload.
func ReachVia(kubeconfig []byte, c client.Client) func([]byte) (client.Client, error) {
	return func(workload []byte) (client.Client, error) {
		if same, err := SameCA(kubeconfig, workload); err == nil && same {
			return c, nil
		}
		return ConnectWorkload(workload)
	}
}

// CNIProxyGeneration is the generation of the cluster's CNI HelmChartProxy,
// 0 when there is none (or no CAAPH). A change between two reads is a change
// to what CAAPH installs: the proxy was created, or its spec changed.
func (m *Manager) CNIProxyGeneration(ctx context.Context, namespace, name string) (int64, error) {
	hcp := &unstructured.Unstructured{}
	hcp.SetGroupVersionKind(hcpGVK)
	switch err := m.C.Get(ctx, client.ObjectKey{Namespace: namespace, Name: render.CNIName(name)}, hcp); {
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("reading the CNI HelmChartProxy: %w", err)
	}
	return hcp.GetGeneration(), nil
}

// CNIRolloutPendingAnnotation, on a cluster's CNI HelmChartProxy, records
// that a create or apply changed the CNI (its value is the proxy's
// generation then) and no one has yet seen that change roll out. While it is
// there the rollout is required, from any process: a rerun of an apply whose
// wait failed must not count the CNI ready unread (PET-103). It lives on the
// cluster, not on disk (ADR-001), and is removed once a wait has read the
// rollout complete.
const CNIRolloutPendingAnnotation = "hydra.io/cni-rollout-pending"

// cniRolloutFieldManager owns the annotation, so the cluster's own apply
// (another field manager) never removes it.
const cniRolloutFieldManager = "hydra-cni-rollout"

// MarkCNIRolloutPending sets CNIRolloutPendingAnnotation to generation.
func (m *Manager) MarkCNIRolloutPending(ctx context.Context, namespace, name string, generation int64) error {
	return m.patchCNIProxy(ctx, namespace, name, types.MergePatchType,
		fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%q}}}`, CNIRolloutPendingAnnotation, strconv.FormatInt(generation, 10)))
}

// clearCNIRolloutPending removes the annotation if it still holds value: one
// set by a later change while this wait read the rollout stays.
func (m *Manager) clearCNIRolloutPending(ctx context.Context, namespace, name, value string) error {
	path := "/metadata/annotations/" + strings.ReplaceAll(CNIRolloutPendingAnnotation, "/", "~1")
	return m.patchCNIProxy(ctx, namespace, name, types.JSONPatchType,
		fmt.Appendf(nil, `[{"op":"test","path":%q,"value":%q},{"op":"remove","path":%q}]`, path, value, path))
}

func (m *Manager) patchCNIProxy(ctx context.Context, namespace, name string, pt types.PatchType, patch []byte) error {
	hcp := &unstructured.Unstructured{}
	hcp.SetGroupVersionKind(hcpGVK)
	hcp.SetNamespace(namespace)
	hcp.SetName(render.CNIName(name))
	if err := m.C.Patch(ctx, hcp, client.RawPatch(pt, patch), client.FieldOwner(cniRolloutFieldManager)); err != nil {
		return fmt.Errorf("annotating the CNI HelmChartProxy %s/%s: %w", namespace, render.CNIName(name), err)
	}
	return nil
}

// workloadClients keeps one client per workload kubeconfig for a Manager's
// lifetime, so a wait does not open a new connection on every poll. Nothing
// outlives the process (ADR-001).
type workloadClients struct {
	mu sync.Mutex
	kc string
	c  client.Client
}

func (m *Manager) workloadClient(kubeconfig []byte) (client.Client, error) {
	m.workload.mu.Lock()
	defer m.workload.mu.Unlock()
	if m.workload.c != nil && m.workload.kc == string(kubeconfig) {
		return m.workload.c, nil
	}
	connect := m.Workload
	if connect == nil {
		connect = ConnectWorkload
	}
	c, err := connect(kubeconfig)
	if err != nil {
		return nil, err
	}
	m.workload.kc, m.workload.c = string(kubeconfig), c
	return c, nil
}

// cniRollout reads, through the workload cluster's own API, whether Cilium's
// agents and operator have rolled out at version: CAAPH reports a release
// Ready once Helm has applied it, before any pod has restarted. An object not
// there yet is a rollout in progress; a cluster that cannot be read is
// CNIRolloutUnreadable, with why in CNIWhy. The caller's context ending is
// an error, never "unreadable": an interrupted or timed-out wait must not
// read as a CNI counted ready unverified.
func (m *Manager) cniRollout(ctx context.Context, namespace, name, version string, s *Summary) error {
	s.CNIRolledOut, s.CNIRolloutUnreadable = false, false
	unreadable := func(why string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.CNIRolloutUnreadable, s.CNIWhy = true, why
		return nil
	}
	kc, err := m.Kubeconfig(ctx, namespace, name)
	if err != nil {
		return unreadable(oneLine(err.Error()))
	}
	c, err := m.workloadClient(kc)
	if err != nil {
		return unreadable("connecting to the workload cluster: " + oneLine(err.Error()))
	}
	ds := &appsv1.DaemonSet{}
	op := &appsv1.Deployment{}
	for _, o := range []struct {
		kind, name string
		obj        client.Object
	}{{"DaemonSet", ciliumDaemonSet, ds}, {"Deployment", ciliumOperator, op}} {
		err := c.Get(ctx, client.ObjectKey{Namespace: ciliumNamespace, Name: o.name}, o.obj)
		switch {
		case apierrors.IsNotFound(err):
			s.CNIWhy = fmt.Sprintf("no %s %s/%s in the workload cluster yet", o.kind, ciliumNamespace, o.name)
			return nil
		case err != nil:
			return unreadable(fmt.Sprintf("reading %s %s/%s in the workload cluster: %s", o.kind, ciliumNamespace, o.name, oneLine(err.Error())))
		}
	}
	r := CiliumRolledOut(ds, op, version)
	s.CNIRolledOut, s.CNIWhy = r.Done, r.Why
	return nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// progressDeadlineExceeded is the Progressing condition's reason on a
// Deployment whose rollout stopped making progress.
const progressDeadlineExceeded = "ProgressDeadlineExceeded"

// Rollout is how far Cilium's agents and operator have rolled out.
type Rollout struct {
	// Done: both have finished rolling out with the version's images.
	Done bool
	// Why says how far it is when not Done, starting with the agents as
	// "n/m" (updated and available, of those desired).
	Why string
}

// CiliumRolledOut reads whether the agents' DaemonSet and the operator's
// Deployment have finished rolling out with version's images, in the sense
// of `kubectl rollout status`: the controller has observed the latest spec,
// and every desired pod is updated and available, with no old pod left.
func CiliumRolledOut(ds *appsv1.DaemonSet, op *appsv1.Deployment, version string) Rollout {
	want := "v" + version
	agents := min(ds.Status.UpdatedNumberScheduled, ds.Status.NumberAvailable)
	desired := ds.Status.DesiredNumberScheduled
	progress := fmt.Sprintf("%d/%d", max(agents, 0), desired)
	if tag := imageTag(containerImage(ds.Spec.Template.Spec.Containers, ciliumAgentContainer)); tag != want {
		return Rollout{Why: fmt.Sprintf("%s, agents at %s, want %s", progress, orDash(tag), want)}
	}
	if ds.Status.ObservedGeneration < ds.Generation {
		return Rollout{Why: progress + ", agents' update not yet observed"}
	}
	// No desired pod is no rollout: a DaemonSet that schedules nowhere has
	// no agent running at all.
	if desired == 0 || ds.Status.UpdatedNumberScheduled < desired || ds.Status.NumberAvailable < desired {
		return Rollout{Why: progress}
	}
	replicas := int32(1)
	if op.Spec.Replicas != nil {
		replicas = *op.Spec.Replicas
	}
	opProgress := fmt.Sprintf("%s, operator %d/%d", progress, min(op.Status.UpdatedReplicas, op.Status.AvailableReplicas), replicas)
	if old := op.Status.Replicas - op.Status.UpdatedReplicas; old > 0 {
		opProgress += fmt.Sprintf(", %d old operator pod(s) not yet replaced", old)
	}
	if tag := imageTag(containerImage(op.Spec.Template.Spec.Containers, ciliumOperatorContainer)); tag != want {
		return Rollout{Why: fmt.Sprintf("%s, operator at %s, want %s", progress, orDash(tag), want)}
	}
	if op.Status.ObservedGeneration < op.Generation {
		return Rollout{Why: opProgress + " (update not yet observed)"}
	}
	for _, c := range op.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Reason == progressDeadlineExceeded {
			// Shown, not fatal: the deployment controller keeps trying, and
			// the condition clears once it progresses (an operator replica
			// waiting for a second node to join, say).
			return Rollout{Why: opProgress + " (" + progressDeadlineExceeded + ")"}
		}
	}
	// As `kubectl rollout status`: every updated pod available, not only as
	// many as desired (a surge can leave more updated pods than replicas).
	if replicas == 0 || op.Status.UpdatedReplicas < replicas || op.Status.Replicas > op.Status.UpdatedReplicas ||
		op.Status.AvailableReplicas < max(replicas, op.Status.UpdatedReplicas) {
		return Rollout{Why: opProgress}
	}
	return Rollout{Done: true}
}

// containerImage is the image of the named container, "" if there is none.
func containerImage(cs []corev1.Container, name string) string {
	for _, c := range cs {
		if c.Name == name {
			return c.Image
		}
	}
	return ""
}

// imageTag is an image reference's tag, without any digest: "v1.19.11" for
// quay.io/cilium/cilium:v1.19.11@sha256:…, "" when it has none.
func imageTag(image string) string {
	image, _, _ = strings.Cut(image, "@")
	i := strings.LastIndex(image, ":")
	if i < 0 || strings.Contains(image[i:], "/") {
		return "" // a registry port, not a tag
	}
	return image[i+1:]
}
