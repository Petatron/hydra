package cluster

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// ciliumAt is Cilium's agents' DaemonSet (3 nodes) and operator Deployment
// (2 replicas) with version's images, fully rolled out.
func ciliumAt(version string) (*appsv1.DaemonSet, *appsv1.Deployment) {
	pod := func(container, repo string) corev1.PodTemplateSpec {
		return corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "sidecar", Image: "quay.io/cilium/other:v0.0.0"},
			{Name: container, Image: "quay.io/cilium/" + repo + ":v" + version + "@sha256:41f1"},
		}}}
	}
	two := int32(2)
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "cilium", Generation: 4},
		Spec:       appsv1.DaemonSetSpec{Template: pod("cilium-agent", "cilium")},
		Status: appsv1.DaemonSetStatus{ObservedGeneration: 4, DesiredNumberScheduled: 3, CurrentNumberScheduled: 3,
			UpdatedNumberScheduled: 3, NumberAvailable: 3, NumberReady: 3},
	}
	op := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "cilium-operator", Generation: 2},
		Spec:       appsv1.DeploymentSpec{Replicas: &two, Template: pod("cilium-operator", "operator-generic")},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 2, UpdatedReplicas: 2, ReadyReplicas: 2, AvailableReplicas: 2},
	}
	return ds, op
}

// workloadOf is cluster c's kubeconfig Secret in ns, and the given workload
// objects (one fake stands for both clusters, through sameClient).
func workloadOf(t *testing.T, ds *appsv1.DaemonSet, op *appsv1.Deployment) []client.Object {
	t.Helper()
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "c-kubeconfig"},
		Data: map[string][]byte{"value": kubeconfigFor("https://10.0.0.5:6443", "workload-ca")}}
	return []client.Object{sec, ds, op}
}

// rolledOutWorkload is cluster c's workloadOf with Cilium 1.19.1 rolled out.
func rolledOutWorkload(t *testing.T) []client.Object {
	t.Helper()
	ds, op := ciliumAt("1.19.1")
	return workloadOf(t, ds, op)
}

func sameClient(c client.Client) func([]byte) (client.Client, error) {
	return func([]byte) (client.Client, error) { return c, nil }
}

// The rollout counts as done only as `kubectl rollout status` would have it,
// for both objects, and only at the requested version.
func TestCiliumRolledOut(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*appsv1.DaemonSet, *appsv1.Deployment)
		want   string // "" for done
	}{
		{"rolled out", func(*appsv1.DaemonSet, *appsv1.Deployment) {}, ""},
		{"agents still rolling (the PET-103 gap)", func(ds *appsv1.DaemonSet, _ *appsv1.Deployment) {
			ds.Status.UpdatedNumberScheduled, ds.Status.NumberAvailable = 1, 2
		}, "1/3"},
		{"agents updated, one not yet available", func(ds *appsv1.DaemonSet, _ *appsv1.Deployment) {
			ds.Status.NumberAvailable = 2
		}, "2/3"},
		{"update not yet observed", func(ds *appsv1.DaemonSet, _ *appsv1.Deployment) {
			ds.Generation = 5
		}, "3/3, agents' update not yet observed"},
		{"agents at the old version", func(ds *appsv1.DaemonSet, _ *appsv1.Deployment) {
			ds.Spec.Template.Spec.Containers[1].Image = "quay.io/cilium/cilium:v1.19.1@sha256:41f1"
		}, "3/3, agents at v1.19.1, want v1.19.11"},
		{"no agent container", func(ds *appsv1.DaemonSet, _ *appsv1.Deployment) {
			ds.Spec.Template.Spec.Containers = ds.Spec.Template.Spec.Containers[:1]
		}, "3/3, agents at -, want v1.19.11"},
		{"scheduled nowhere", func(ds *appsv1.DaemonSet, _ *appsv1.Deployment) {
			ds.Status = appsv1.DaemonSetStatus{ObservedGeneration: 4}
		}, "0/0"},
		{"operator still rolling", func(_ *appsv1.DaemonSet, op *appsv1.Deployment) {
			op.Status.UpdatedReplicas, op.Status.AvailableReplicas = 1, 2
		}, "3/3, operator 1/2, 1 old operator pod(s) not yet replaced"},
		{"operator's old pod not gone", func(_ *appsv1.DaemonSet, op *appsv1.Deployment) {
			op.Status.Replicas = 3
		}, "3/3, operator 2/2, 1 old operator pod(s) not yet replaced"},
		{"operator at the old version", func(_ *appsv1.DaemonSet, op *appsv1.Deployment) {
			op.Spec.Template.Spec.Containers[1].Image = "quay.io/cilium/operator-generic:v1.19.1"
		}, "3/3, operator at v1.19.1, want v1.19.11"},
		{"operator update not yet observed", func(_ *appsv1.DaemonSet, op *appsv1.Deployment) {
			op.Status.ObservedGeneration = 1
		}, "3/3, operator 2/2 (update not yet observed)"},
		{"operator stuck", func(_ *appsv1.DaemonSet, op *appsv1.Deployment) {
			op.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"}}
		}, "3/3, operator 2/2 (ProgressDeadlineExceeded)"},
		{"operator scaled to zero", func(_ *appsv1.DaemonSet, op *appsv1.Deployment) {
			zero := int32(0)
			op.Spec.Replicas = &zero
			op.Status = appsv1.DeploymentStatus{ObservedGeneration: 2}
		}, "3/3, operator 0/0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds, op := ciliumAt("1.19.11")
			tc.mutate(ds, op)
			r := CiliumRolledOut(ds, op, "1.19.11")
			if r.Done != (tc.want == "") || r.Why != tc.want {
				t.Errorf("%+v, want %q", r, tc.want)
			}
		})
	}
}

// A release CAAPH reports Ready, at the right version and generation, is not
// a ready CNI while the agents are still rolling out: status says how far,
// and the wait holds until they are done.
func TestReadyReleaseWithAgentsRollingOut(t *testing.T) {
	ctx := context.Background()
	ds, op := ciliumAt("1.19.1")
	ds.Status.UpdatedNumberScheduled = 1
	cl := obj(clusterGVK, "ns", "c", map[string]any{"status": map[string]any{"conditions": []any{
		map[string]any{"type": "Available", "status": "True", "reason": "Available"},
	}}})
	cl.SetLabels(map[string]string{"cni.hydra.io/cilium": "c"})
	kcp := obj(kcpGVK, "ns", "c-cp", map[string]any{"spec": map[string]any{"replicas": int64(1)}, "status": map[string]any{"replicas": int64(1), "readyReplicas": int64(1), "upToDateReplicas": int64(1)}})
	objs := append([]client.Object{cl, kcp, cniProxy("1.19.1"), cniRelease("c", "True", 1)}, workloadOf(t, ds, op)...)
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
	var out bytes.Buffer
	m := &Manager{C: c, Out: &out, Poll: time.Millisecond, Workload: sameClient(c)}

	s, err := m.Status(ctx, "ns", "c")
	if err != nil {
		t.Fatal(err)
	}
	if !s.CNIApplied || s.CNIRolledOut || s.CNIReady() {
		t.Fatalf("applied=%v rolledOut=%v ready=%v", s.CNIApplied, s.CNIRolledOut, s.CNIReady())
	}
	if !strings.HasSuffix(s.Line(), "CNI rolling out (1/3)") {
		t.Errorf("line %q", s.Line())
	}
	if _, err := m.WaitAvailable(ctx, "ns", "c", 30*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("the wait must hold while the agents roll out, got %v", err)
	}

	// The rollout finishes: now it is ready.
	ds.Status.UpdatedNumberScheduled = 3
	if err := c.Status().Update(ctx, ds); err != nil {
		t.Fatal(err)
	}
	if _, err := m.WaitAvailable(ctx, "ns", "c", time.Second); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "CNI rolling out (1/3)") || !strings.Contains(out.String(), "CNI ready") {
		t.Errorf("the wait must show the rollout, then ready:\n%s", out.String())
	}
}

// A workload cluster that cannot be read is a reason, never an error, so the
// rest of the status is still reported: the CNI counts ready on its release
// alone, unverified, unless the rollout is required. An object not there yet
// is a rollout in progress, required or not.
func TestCNIRolloutUnreadable(t *testing.T) {
	ctx := context.Background()
	ds, op := ciliumAt("1.19.1")
	base := append([]client.Object{cniProxy("1.19.1"), cniRelease("c", "True", 1)}, workloadOf(t, ds, op)...)
	refused := func([]byte) (client.Client, error) {
		return nil, errors.New("dial tcp 10.0.0.5:6443:\n  connect: connection refused")
	}
	for _, tc := range []struct {
		name     string
		objs     []client.Object
		workload func(c client.Client) func([]byte) (client.Client, error)
		funcs    interceptor.Funcs
		want     string
		readable bool
	}{
		{"no kubeconfig Secret", base[:2], sameClient, interceptor.Funcs{},
			`reading Secret ns/c-kubeconfig: secrets "c-kubeconfig" not found`, false},
		{"workload API unreachable", base, func(client.Client) func([]byte) (client.Client, error) { return refused }, interceptor.Funcs{},
			"connecting to the workload cluster: dial tcp 10.0.0.5:6443: connect: connection refused", false},
		{"no DaemonSet yet", base[:3], sameClient, interceptor.Funcs{},
			"no DaemonSet kube-system/cilium in the workload cluster yet", true},
		{"no operator yet", base[:4], sameClient, interceptor.Funcs{},
			"no Deployment kube-system/cilium-operator in the workload cluster yet", true},
		{"read refused", base, sameClient, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			if _, ok := o.(*appsv1.DaemonSet); ok {
				return errors.New("forbidden")
			}
			return c.Get(ctx, key, o, opts...)
		}}, "reading DaemonSet kube-system/cilium in the workload cluster: forbidden", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(tc.objs...).WithInterceptorFuncs(tc.funcs).Build()
			for _, required := range []bool{false, true} {
				s := Summary{CNIRolloutRequired: required}
				if err := (&Manager{C: c, Workload: tc.workload(c)}).cniStatus(ctx, "ns", "c", &s); err != nil {
					t.Fatalf("an unreadable workload cluster is a reason, not an error: %v", err)
				}
				if !s.CNIApplied || s.CNIRolledOut || s.CNIRolloutUnreadable == tc.readable || s.CNIWhy != tc.want {
					t.Errorf("applied=%v rolledOut=%v unreadable=%v why=%q\nwant %q", s.CNIApplied, s.CNIRolledOut, s.CNIRolloutUnreadable, s.CNIWhy, tc.want)
				}
				if want := !tc.readable && !required; s.CNIReady() != want || s.CNIRolloutUnverified() != want {
					t.Errorf("required=%v: ready=%v unverified=%v, want %v", required, s.CNIReady(), s.CNIRolloutUnverified(), want)
				}
			}
		})
	}
}

// One connection per workload kubeconfig for the Manager's life, not one per
// poll; a different kubeconfig connects again.
func TestWorkloadClientReused(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme(t)).Build()
	n := 0
	m := &Manager{C: c, Workload: func([]byte) (client.Client, error) { n++; return c, nil }}
	for _, kc := range []string{"a", "a", "b", "b"} {
		if _, err := m.workloadClient([]byte(kc)); err != nil {
			t.Fatal(err)
		}
	}
	if n != 2 {
		t.Errorf("connected %d times, want 2", n)
	}
	m.Workload = func([]byte) (client.Client, error) { return nil, errors.New("refused") }
	if _, err := m.workloadClient([]byte("c")); err == nil {
		t.Error("a failed connection must be an error")
	}
	if got, err := m.workloadClient([]byte("c")); err == nil || got != nil {
		t.Error("a failed connection must not be cached")
	}
}

// ReachVia reads a cluster that trusts the management kubeconfig's CA (one
// managing itself) through the management connection, and dials any other.
func TestReachVia(t *testing.T) {
	mgmt := fake.NewClientBuilder().WithScheme(scheme(t)).Build()
	reach := ReachVia(kubeconfigFor("https://127.0.0.1:40000", "mgmt-ca"), mgmt)
	got, err := reach(kubeconfigFor("https://10.0.0.11:6443", "mgmt-ca"))
	if err != nil || got != mgmt {
		t.Errorf("self-hosted: got %v, %v; want the management client", got, err)
	}
	// No CA to compare: never assumed to be the management cluster. Nothing
	// is dialled until a request is made.
	other := []byte(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://10.0.0.12:6443", insecure-skip-tls-verify: true}}]
contexts: [{name: c, context: {cluster: c, user: u}}]
current-context: c
users: [{name: u, user: {token: x}}]
`)
	got, err = reach(other)
	if err != nil || got == mgmt || got == nil {
		t.Errorf("another cluster: got %v, %v; want its own client", got, err)
	}
	if _, err := reach([]byte("not a kubeconfig")); err == nil {
		t.Error("an unreadable kubeconfig must be an error")
	}
}

func TestImageTag(t *testing.T) {
	for image, want := range map[string]string{
		"quay.io/cilium/cilium:v1.19.11@sha256:645c": "v1.19.11",
		"quay.io/cilium/cilium:v1.19.11":             "v1.19.11",
		"quay.io/cilium/cilium@sha256:645c":          "",
		"registry:5000/cilium/cilium":                "",
		"registry:5000/cilium/cilium:v1.19.1":        "v1.19.1",
		"":                                           "",
	} {
		if got := imageTag(image); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", image, got, want)
		}
	}
}

// Requiring the rollout, a workload cluster that stays unreadable once
// everything else is done fails the wait after the grace, not at the
// timeout; not requiring it, the wait passes on the release alone. A rollout
// in progress is waited for either way.
func TestWaitAvailableUnreadableWorkload(t *testing.T) {
	ctx := context.Background()
	ds, op := ciliumAt("1.19.1")
	cl := obj(clusterGVK, "ns", "c", map[string]any{"status": map[string]any{"conditions": []any{
		map[string]any{"type": "Available", "status": "True"},
	}}})
	cl.SetLabels(map[string]string{"cni.hydra.io/cilium": "c"})
	kcp := obj(kcpGVK, "ns", "c-cp", map[string]any{"spec": map[string]any{"replicas": int64(1)}, "status": map[string]any{"replicas": int64(1), "readyReplicas": int64(1), "upToDateReplicas": int64(1)}})
	objs := append([]client.Object{cl, kcp, cniProxy("1.19.1"), cniRelease("c", "True", 1)}, workloadOf(t, ds, op)...)
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
	refused := func([]byte) (client.Client, error) { return nil, errors.New("connection refused") }

	var out bytes.Buffer
	strict := &Manager{C: c, Out: &out, Poll: time.Millisecond, Workload: refused, RequireCNIRollout: true, UnreadableGrace: 20 * time.Millisecond}
	start := time.Now()
	_, err := strict.WaitAvailable(ctx, "ns", "c", time.Minute)
	if !errors.Is(err, ErrCNIRolloutUnknown) || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("an unreadable workload cluster must fail fast, not at the timeout")
	}
	if !strings.Contains(out.String(), "CNI rollout unknown: connecting to the workload cluster: connection refused") {
		t.Errorf("the wait must say why:\n%s", out.String())
	}

	out.Reset()
	lenient := &Manager{C: c, Out: &out, Poll: time.Millisecond, Workload: refused}
	if _, err := lenient.WaitAvailable(ctx, "ns", "c", time.Second); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "CNI ready (rollout unverified: connecting to the workload cluster: connection refused)") {
		t.Errorf("an unverified rollout must say so:\n%s", out.String())
	}

	// Readable and rolling: both wait (no grace, no early pass).
	ds.Status.UpdatedNumberScheduled = 1
	if err := c.Status().Update(ctx, ds); err != nil {
		t.Fatal(err)
	}
	for _, required := range []bool{false, true} {
		m := &Manager{C: c, Out: &bytes.Buffer{}, Poll: time.Millisecond, Workload: sameClient(c), RequireCNIRollout: required, UnreadableGrace: time.Millisecond}
		if _, err := m.WaitAvailable(ctx, "ns", "c", 50*time.Millisecond); !errors.Is(err, ErrTimeout) {
			t.Errorf("required=%v: a rollout in progress must hold the wait, got %v", required, err)
		}
	}
}

func TestCNIProxyGeneration(t *testing.T) {
	ctx := context.Background()
	hcp := cniProxy("1.19.1")
	hcp.SetGeneration(3)
	m := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(hcp).Build()}
	if g, err := m.CNIProxyGeneration(ctx, "ns", "c"); err != nil || g != 3 {
		t.Errorf("got %d, %v", g, err)
	}
	if g, err := m.CNIProxyGeneration(ctx, "ns", "other"); err != nil || g != 0 {
		t.Errorf("no proxy: got %d, %v", g, err)
	}
	broken := &Manager{C: fake.NewClientBuilder().WithScheme(scheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("boom")
		}}).Build()}
	if _, err := broken.CNIProxyGeneration(ctx, "ns", "c"); err == nil {
		t.Error("an unreadable proxy must be an error")
	}
}

// A cluster being deleted is not read: its API is going away, and the
// delete's wait polls Status until it is gone.
func TestTerminatingClusterNotRead(t *testing.T) {
	cl := obj(clusterGVK, "ns", "c", nil)
	cl.SetLabels(map[string]string{"cni.hydra.io/cilium": "c"})
	now := metav1.Now()
	cl.SetDeletionTimestamp(&now)
	cl.SetFinalizers([]string{"cluster.cluster.x-k8s.io"})
	objs := append([]client.Object{cl, cniProxy("1.19.1"), cniRelease("c", "True", 1)}, rolledOutWorkload(t)...)
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
	m := &Manager{C: c, Workload: func([]byte) (client.Client, error) {
		t.Error("a cluster being deleted must not be read")
		return c, nil
	}}
	if _, err := m.Status(context.Background(), "ns", "c"); err != nil {
		t.Fatal(err)
	}
}

// settledCluster is Available, rolled out, with hydra's CNI applied at
// 1.19.1 and the workload objects given.
func settledCluster(t *testing.T, ds *appsv1.DaemonSet, op *appsv1.Deployment, hcpAnnotations map[string]string) []client.Object {
	t.Helper()
	cl := obj(clusterGVK, "ns", "c", map[string]any{"status": map[string]any{"conditions": []any{
		map[string]any{"type": "Available", "status": "True"},
	}}})
	cl.SetLabels(map[string]string{"cni.hydra.io/cilium": "c"})
	kcp := obj(kcpGVK, "ns", "c-cp", map[string]any{"spec": map[string]any{"replicas": int64(1)}, "status": map[string]any{"replicas": int64(1), "readyReplicas": int64(1), "upToDateReplicas": int64(1)}})
	hcp := cniProxy("1.19.1")
	hcp.SetAnnotations(hcpAnnotations)
	return append([]client.Object{cl, kcp, hcp, cniRelease("c", "True", 1)}, workloadOf(t, ds, op)...)
}

// The wait's own context ending during the workload read is an interrupt,
// never an unreadable cluster counted ready: a lenient wait must not succeed
// on Ctrl-C (review, PR #36).
func TestWaitInterruptedDuringWorkloadRead(t *testing.T) {
	ds, op := ciliumAt("1.19.1")
	ds.Status.UpdatedNumberScheduled = 1 // rolling: the wait would hold
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(settledCluster(t, ds, op, nil)...).Build()
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	m := &Manager{C: c, Out: &out, Poll: time.Millisecond, Workload: func([]byte) (client.Client, error) {
		cancel() // Ctrl-C while dialling
		return nil, context.Canceled
	}}
	if _, err := m.WaitAvailable(ctx, "ns", "c", time.Minute); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("want ErrInterrupted, got %v", err)
	}
	if strings.Contains(out.String(), "rollout unverified") || strings.Contains(out.String(), "cannot read status") {
		t.Errorf("an interrupt is not a status:\n%s", out.String())
	}
}

// An operator rollout past its progress deadline is shown, but holds the wait
// rather than failing it: the deployment controller keeps trying, and the
// condition clears once it progresses (review, PR #36).
func TestStuckOperatorHoldsTheWait(t *testing.T) {
	ds, op := ciliumAt("1.19.1")
	op.Status.UpdatedReplicas = 1
	op.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"}}
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(settledCluster(t, ds, op, nil)...).Build()
	var out bytes.Buffer
	m := &Manager{C: c, Out: &out, Poll: time.Millisecond, Workload: sameClient(c)}
	if _, err := m.WaitAvailable(context.Background(), "ns", "c", 30*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(out.String(), "CNI rolling out (3/3, operator 1/2, 1 old operator pod(s) not yet replaced (ProgressDeadlineExceeded))") {
		t.Errorf("the deadline must be shown:\n%s", out.String())
	}
}

// A CNI change no one has seen roll out is recorded on the cluster, so the
// rollout is required from any process, not only the one that made the change
// (review, PR #36). A wait that reads it complete removes the record, but
// only the record it read.
func TestCNIRolloutPendingAnnotation(t *testing.T) {
	ctx := context.Background()
	ds, op := ciliumAt("1.19.1")
	pending := map[string]string{CNIRolloutPendingAnnotation: "1"}
	refused := func([]byte) (client.Client, error) { return nil, errors.New("connection refused") }

	// Unreadable, recorded: strict even though this Manager changed nothing.
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(settledCluster(t, ds, op, pending)...).Build()
	var out bytes.Buffer
	m := &Manager{C: c, Out: &out, Poll: time.Millisecond, Workload: refused, UnreadableGrace: 10 * time.Millisecond}
	s, err := m.Status(ctx, "ns", "c")
	if err != nil || !s.CNIRolloutRequired || s.CNIReady() || !strings.HasSuffix(s.Line(), "CNI rollout unknown, change pending verification since generation 1: connecting to the workload cluster: connection refused") {
		t.Fatalf("status %q required=%v err=%v", s.Line(), s.CNIRolloutRequired, err)
	}
	if _, err := m.WaitAvailable(ctx, "ns", "c", time.Minute); !errors.Is(err, ErrCNIRolloutUnknown) {
		t.Fatalf("a recorded change must keep a rerun strict, got %v", err)
	}

	// Readable and rolled out: the wait passes and removes the record.
	m = &Manager{C: c, Out: &out, Poll: time.Millisecond, Workload: sameClient(c)}
	if _, err := m.WaitAvailable(ctx, "ns", "c", time.Second); err != nil {
		t.Fatal(err)
	}
	if g := proxyAnnotations(t, c)[CNIRolloutPendingAnnotation]; g != "" {
		t.Errorf("the record must be removed once the rollout is seen, still %q", g)
	}

	// Marking sets it; a removal that read an older value leaves a newer one.
	if err := m.MarkCNIRolloutPending(ctx, "ns", "c", 2); err != nil {
		t.Fatal(err)
	}
	if err := m.clearCNIRolloutPending(ctx, "ns", "c", "1"); err == nil {
		t.Error("removing a record that changed since it was read must fail")
	}
	if g := proxyAnnotations(t, c)[CNIRolloutPendingAnnotation]; g != "2" {
		t.Errorf("want the newer record kept, got %q", g)
	}
	if err := m.clearCNIRolloutPending(ctx, "ns", "c", "2"); err != nil {
		t.Fatal(err)
	}
}

func proxyAnnotations(t *testing.T, c client.Client) map[string]string {
	t.Helper()
	hcp := cniProxy("")
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "c-cilium"}, hcp); err != nil {
		t.Fatal(err)
	}
	return hcp.GetAnnotations()
}

// A release counts only once CAAPH has carried the proxy's current spec into
// it: a values-only change keeps the version, and the old release would read
// Ready and current (review, PR #36).
func TestCNIProxyBehind(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		mutate func(*unstructured.Unstructured)
		want   string // "" when the release is trusted
	}{
		{"reconciled", func(*unstructured.Unstructured) {}, ""},
		{"spec changed, not yet observed", func(h *unstructured.Unstructured) { h.SetGeneration(3) },
			proxyNotReconciled},
		{"no condition", func(h *unstructured.Unstructured) { unstructured.RemoveNestedField(h.Object, "status", "conditions") },
			proxyNotReconciled},
		{"updating the release failed", func(h *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(h.Object, []any{map[string]any{"type": "HelmReleaseProxySpecsUpToDate", "status": "False",
				"reason": "HelmReleaseProxyCreationFailed", "message": "conflict\n  on update", "observedGeneration": int64(2)}}, "status", "conditions")
		}, "HelmReleaseProxyCreationFailed: conflict on update"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hcp := cniProxy("1.19.1")
			tc.mutate(hcp)
			objs := append([]client.Object{hcp, cniRelease("c", "True", 1)}, rolledOutWorkload(t)...)
			c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
			var s Summary
			if err := (&Manager{C: c, Workload: sameClient(c)}).cniStatus(ctx, "ns", "c", &s); err != nil {
				t.Fatal(err)
			}
			if s.CNIApplied != (tc.want == "") || s.CNIWhy != tc.want {
				t.Errorf("applied=%v why=%q, want %q", s.CNIApplied, s.CNIWhy, tc.want)
			}
		})
	}
}

// The record is hydra's, not hydra.yaml's: plan never shows it as a change.
func TestComparableIgnoresThePendingRecord(t *testing.T) {
	a, b := cniProxy("1.19.1"), cniProxy("1.19.1")
	b.SetAnnotations(map[string]string{CNIRolloutPendingAnnotation: "2"})
	if comparable(a) != comparable(b) {
		t.Errorf("differs:\n%s\n%s", comparable(a), comparable(b))
	}
	b.SetAnnotations(map[string]string{CNIRolloutPendingAnnotation: "2", "other": "x"})
	if !strings.Contains(comparable(b), "other: x") || strings.Contains(comparable(b), CNIRolloutPendingAnnotation) {
		t.Errorf("only the record is dropped:\n%s", comparable(b))
	}
}
