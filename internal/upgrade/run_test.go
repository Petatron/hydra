package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Petatron/hydra/internal/cluster"
	"github.com/Petatron/hydra/internal/config"
	"github.com/Petatron/hydra/internal/render"
)

const testConfig = `apiVersion: hydra.io/v1alpha1
kind: ClusterConfig
metadata:
  name: c
  namespace: ns
spec:
  kubernetesVersion: %s
  storagePool: pool
  image:
    name: base.img
  network:
    controlPlaneEndpoint: 192.168.1.10
    bridges: [br0]
  controlPlane:
    replicas: 3
    vcpus: 2
    memory: 4Gi
    diskSize: 40Gi
  nodePools:
  - name: a
    replicas: 2
    vcpus: 2
    memory: 4Gi
    diskSize: 40Gi
  - name: b
    vcpus: 2
    memory: 4Gi
    diskSize: 40Gi
    autoscaling:
      min: 1
      max: 3
`

func loadConfig(t *testing.T, version string) *config.ClusterConfig {
	t.Helper()
	c, err := config.Parse([]byte(strings.Replace(testConfig, "%s", version, 1)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func renderFunc(c *config.ClusterConfig) ([]*unstructured.Unstructured, error) {
	out, err := render.Render(c, render.Options{CLIVersion: "test"})
	if err != nil {
		return nil, err
	}
	return cluster.Objects(out)
}

var clusterGVK = schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Cluster"}

// rolledOut gives a KubeadmControlPlane or MachineDeployment the status of a
// finished rollout at its spec's version.
func rolledOut(o *unstructured.Unstructured) {
	replicas := int64(1)
	switch r := o.Object["spec"].(map[string]any)["replicas"].(type) {
	case int64:
		replicas = r
	case float64:
		replicas = int64(r)
	}
	st := map[string]any{
		"observedGeneration": o.GetGeneration(),
		"replicas":           replicas, "upToDateReplicas": replicas, "readyReplicas": replicas,
	}
	if o.GetKind() == kcpKind {
		st["version"], _, _ = unstructured.NestedString(o.Object, "spec", "version")
	}
	o.Object["status"] = st
}

// poolMachines are a MachineDeployment's Machines: its desired count, at its
// version.
func poolMachines(md *unstructured.Unstructured) []*unstructured.Unstructured {
	n := int64(1)
	switch r := md.Object["spec"].(map[string]any)["replicas"].(type) {
	case int64:
		n = r
	case float64:
		n = int64(r)
	}
	ver, _, _ := unstructured.NestedString(md.Object, "spec", "template", "spec", "version")
	labels := map[string]string{clusterNameLabel: "c", deploymentNameLabel: md.GetName()}
	if md.GetKind() == kcpKind {
		ver, _, _ = unstructured.NestedString(md.Object, "spec", "version")
		labels = map[string]string{clusterNameLabel: "c", controlPlaneLabel: ""}
	}
	out := make([]*unstructured.Unstructured, 0, n)
	for i := range n {
		mc := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"version": ver}}}
		mc.SetGroupVersionKind(machineGVK)
		mc.SetNamespace(md.GetNamespace())
		mc.SetName(fmt.Sprintf("%s-%d", md.GetName(), i))
		mc.SetLabels(labels)
		out = append(out, mc)
	}
	return out
}

// setMachines replaces a pool's Machines with ones at its MachineDeployment's
// version, as a finished rollout leaves them.
func setMachines(ctx context.Context, c client.Client, md *unstructured.Unstructured) error {
	sel := client.MatchingLabels{deploymentNameLabel: md.GetName()}
	if md.GetKind() == kcpKind {
		sel = client.MatchingLabels{controlPlaneLabel: ""}
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(machineGVK.GroupVersion().WithKind("MachineList"))
	if err := c.List(ctx, list, sel); err != nil {
		return err
	}
	for i := range list.Items {
		if err := c.Delete(ctx, &list.Items[i]); err != nil {
			return err
		}
	}
	for _, mc := range poolMachines(md) {
		if err := c.Create(ctx, mc); err != nil {
			return err
		}
	}
	return nil
}

// rolling gives it the status of a rollout under way.
func rolling(o *unstructured.Unstructured) {
	rolledOut(o)
	_ = unstructured.SetNestedField(o.Object, int64(0), "status", "upToDateReplicas")
	if o.GetKind() == kcpKind {
		_ = unstructured.SetNestedField(o.Object, "v1.0.0", "status", "version")
	}
}

// fakeCluster is a management cluster holding cfg rendered with the control
// plane at cp and each pool at pools[name] (default cp), every part rolled
// out unless listed in rollingParts ("cp" or a pool name).
type fakeCluster struct {
	t       *testing.T
	c       client.Client
	mu      sync.Mutex
	applied []string
	// rollout, when true, makes an applied part roll out at once, as if the
	// controllers were instant; otherwise it stays rolling.
	rollout bool
}

func newFakeCluster(t *testing.T, cfg *config.ClusterConfig, cp string, pools map[string]string, rollingParts ...string) *fakeCluster {
	t.Helper()
	u := &Upgrader{Config: cfg, Render: renderFunc}
	l := Live{ControlPlane: Part{Version: v(cp)}}
	for _, p := range cfg.Spec.NodePools {
		pv := cp
		if x, ok := pools[p.Name]; ok {
			pv = x
		}
		l.Pools = append(l.Pools, Part{Name: p.Name, Version: v(pv)})
	}
	objs, err := u.Compose(l)
	if err != nil {
		t.Fatal(err)
	}
	isRolling := map[string]bool{}
	for _, r := range rollingParts {
		isRolling[r] = true
	}
	seed := make([]client.Object, 0, len(objs))
	for _, o := range objs {
		o = o.DeepCopy()
		o.SetGeneration(1)
		switch o.GetKind() {
		case kcpKind:
			if isRolling["cp"] {
				rolling(o)
			} else {
				rolledOut(o)
			}
			for _, mc := range poolMachines(o) {
				seed = append(seed, mc)
			}
		case mdKind:
			if isRolling[strings.TrimPrefix(o.GetName(), "c-")] {
				rolling(o)
			} else {
				rolledOut(o)
			}
			for _, mc := range poolMachines(o) {
				seed = append(seed, mc)
			}
		case "Cluster":
			o.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Available", "status": "True"}}}
		}
		seed = append(seed, o)
	}
	fc := &fakeCluster{t: t, rollout: true}
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	fc.c = fake.NewClientBuilder().WithScheme(s).WithObjects(seed...).WithInterceptorFuncs(interceptor.Funcs{
		// Plan's server-side dry run: the server's result is the object as sent.
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return nil
		},
		Apply: fc.apply,
	}).Build()
	return fc
}

// apply stands in for server-side apply and the controllers behind it.
func (fc *fakeCluster) apply(ctx context.Context, c client.WithWatch, ac runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
	b, err := json.Marshal(ac)
	if err != nil {
		return err
	}
	o := &unstructured.Unstructured{}
	if err := json.Unmarshal(b, &o.Object); err != nil {
		return err
	}
	if o.GetKind() == "Namespace" {
		return nil
	}
	fc.mu.Lock()
	fc.applied = append(fc.applied, o.GetKind()+"/"+o.GetName())
	fc.mu.Unlock()
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(o.GroupVersionKind())
	err = c.Get(ctx, client.ObjectKeyFromObject(o), live)
	if apierrors.IsNotFound(err) {
		return c.Create(ctx, o)
	}
	if err != nil {
		return err
	}
	o.SetResourceVersion(live.GetResourceVersion())
	o.SetGeneration(live.GetGeneration() + 1)
	if fc.rollout {
		rolledOut(o)
	} else {
		rolling(o)
	}
	if err := c.Update(ctx, o); err != nil {
		return err
	}
	if fc.rollout && (o.GetKind() == mdKind || o.GetKind() == kcpKind) {
		return setMachines(ctx, c, o)
	}
	return nil
}

func (fc *fakeCluster) upgrader(cfg *config.ClusterConfig, remote Remote) (*Upgrader, *bytes.Buffer) {
	var out bytes.Buffer
	return &Upgrader{
		M: &cluster.Manager{C: fc.c, Out: &out}, Config: cfg, Render: renderFunc, Remote: remote,
		Target: v("v1.36.6"), Timeout: 2 * time.Second, Out: &out, Poll: 10 * time.Millisecond,
	}, &out
}

type fakeRemote struct {
	packages bool
	tags     []string
	err      error
}

func (r fakeRemote) HasPackages(context.Context, Version) (bool, error) { return r.packages, r.err }
func (r fakeRemote) AutoscalerTags(context.Context, Version) ([]string, error) {
	return r.tags, r.err
}

var okRemote = fakeRemote{packages: true, tags: []string{"v1.36.2"}}

func TestComposeRendersEachPartAtItsVersion(t *testing.T) {
	cfg := loadConfig(t, "v1.35.3")
	u := &Upgrader{Config: cfg, Render: renderFunc}
	objs, err := u.Compose(Live{ControlPlane: Part{Version: v("v1.36.6")}, Pools: []Part{{Name: "a", Version: v("v1.36.6")}, {Name: "b", Version: v("v1.35.3")}}})
	if err != nil {
		t.Fatal(err)
	}
	versions := map[string]string{}
	kcts := map[string]bool{}
	for _, o := range objs {
		switch o.GetKind() {
		case kcpKind:
			versions["cp"], _, _ = unstructured.NestedString(o.Object, "spec", "version")
		case mdKind:
			versions[o.GetName()], _, _ = unstructured.NestedString(o.Object, "spec", "template", "spec", "version")
			if !kcts[kctOf(o)] {
				kcts[kctOf(o)] = true
			}
		}
	}
	want := map[string]string{"cp": "v1.36.6", "c-a": "v1.36.6", "c-b": "v1.35.3"}
	for k, w := range want {
		if versions[k] != w {
			t.Errorf("%s at %s, want %s", k, versions[k], w)
		}
	}
	// Each MachineDeployment's template is rendered alongside it.
	for name := range kcts {
		if pick(objs, kctKind, name) == nil {
			t.Errorf("KubeadmConfigTemplate %s is referenced but not rendered", name)
		}
	}
}

// A full run applies the control plane, then pool a, then pool b, each only
// after the one before rolled out, and nothing else.
func TestRunUpgradesInOrder(t *testing.T) {
	cfg := loadConfig(t, "v1.35.3")
	fc := newFakeCluster(t, cfg, "v1.35.3", nil)
	u, out := fc.upgrader(cfg, okRemote)
	if err := u.Run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	kinds := make([]string, 0, len(fc.applied))
	for _, a := range fc.applied {
		// A template's name ends in its content hash; drop it.
		if strings.HasPrefix(a, kctKind+"/") {
			a = a[:strings.LastIndex(a, "-")+1]
		}
		kinds = append(kinds, a)
	}
	want := "KubeadmControlPlane/c-cp KubeadmConfigTemplate/c-a- MachineDeployment/c-a KubeadmConfigTemplate/c-b- MachineDeployment/c-b"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("applied %s\nwant    %s\n%s", got, want, out)
	}
	for _, s := range []string{"version-skew rules", "set kubernetesVersion: v1.36.6", "Cluster Autoscaler has v1.36.2"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
	l, err := ReadLive(context.Background(), fc.c, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !l.at(v("v1.36.6")) {
		t.Errorf("not at the target: %+v", l)
	}
}

// While the control plane rolls, no pool is touched: the wait ends the run
// (here by its timeout) before any MachineDeployment is applied.
func TestRunNeverTouchesPoolsBeforeTheControlPlaneFinishes(t *testing.T) {
	cfg := loadConfig(t, "v1.35.3")
	fc := newFakeCluster(t, cfg, "v1.35.3", nil)
	fc.rollout = false
	u, out := fc.upgrader(cfg, okRemote)
	u.Timeout = 100 * time.Millisecond
	err := u.Run(context.Background())
	if !errors.Is(err, cluster.ErrTimeout) {
		t.Fatalf("want a timeout, got %v\n%s", err, out)
	}
	if strings.Join(fc.applied, " ") != "KubeadmControlPlane/c-cp" {
		t.Fatalf("applied %v", fc.applied)
	}
}

// A rerun after an interrupt mid-control-plane waits, re-applying nothing,
// then carries on with the pools.
func TestRerunResumesAControlPlaneRollout(t *testing.T) {
	cfg := loadConfig(t, "v1.35.3")
	fc := newFakeCluster(t, cfg, "v1.36.6", map[string]string{"a": "v1.35.3", "b": "v1.35.3"}, "cp")
	u, out := fc.upgrader(cfg, okRemote)
	go func() {
		// The controllers finish the control plane a little later.
		time.Sleep(50 * time.Millisecond)
		kcp := &unstructured.Unstructured{}
		kcp.SetGroupVersionKind(kcpGVK)
		if err := fc.c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "c-cp"}, kcp); err == nil {
			rolledOut(kcp)
			_ = fc.c.Update(context.Background(), kcp)
			_ = setMachines(context.Background(), fc.c, kcp)
		}
	}()
	if err := u.Run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if fc.applied[0] == "KubeadmControlPlane/c-cp" {
		t.Fatalf("re-applied the control plane: %v", fc.applied)
	}
	if !strings.Contains(out.String(), "wait for the control plane's rollout") {
		t.Errorf("should say it is waiting:\n%s", out)
	}
	if len(fc.applied) != 4 {
		t.Errorf("want both pools applied, got %v", fc.applied)
	}
}

// Interrupted mid-workers (pool a rolling, b untouched), a rerun waits for a
// and then upgrades b.
func TestRerunResumesMidWorkers(t *testing.T) {
	cfg := loadConfig(t, "v1.36.6")
	fc := newFakeCluster(t, cfg, "v1.36.6", map[string]string{"a": "v1.36.6", "b": "v1.35.3"}, "a")
	u, out := fc.upgrader(cfg, okRemote)
	go func() {
		time.Sleep(50 * time.Millisecond)
		md := &unstructured.Unstructured{}
		md.SetGroupVersionKind(mdGVK)
		if err := fc.c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "c-a"}, md); err == nil {
			rolledOut(md)
			_ = fc.c.Update(context.Background(), md)
			_ = setMachines(context.Background(), fc.c, md)
		}
	}()
	if err := u.Run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := strings.Join(fc.applied, " "); !strings.HasSuffix(got, "MachineDeployment/c-b") || strings.Contains(got, "MachineDeployment/c-a") || strings.Contains(got, "KubeadmControlPlane") {
		t.Fatalf("applied %s", got)
	}
}

// hydra.yaml edits beyond the version are refused before anything is
// applied: upgrade carries only the version.
func TestRunRefusesOtherEdits(t *testing.T) {
	cfg := loadConfig(t, "v1.35.3")
	fc := newFakeCluster(t, cfg, "v1.35.3", nil)
	edited := loadConfig(t, "v1.35.3")
	edited.Spec.NodePools[0].Replicas = 5
	u, out := fc.upgrader(edited, okRemote)
	err := u.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "more than the version") {
		t.Fatalf("got %v\n%s", err, out)
	}
	if len(fc.applied) != 0 {
		t.Fatalf("applied %v", fc.applied)
	}
}

func TestPreflightRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remote Remote
		edit   func(*config.ClusterConfig)
		refuse string
	}{
		{name: "no packages", remote: fakeRemote{packages: false, tags: []string{"v1.36.0"}}, refuse: "pkgs.k8s.io lacks kubelet, kubeadm or kubectl"},
		{name: "no autoscaler release", remote: fakeRemote{packages: true}, refuse: "no Cluster Autoscaler release"},
		{name: "unreachable", remote: fakeRemote{err: errors.New("dial tcp: no route")}, refuse: "--skip-remote-checks"},
		{name: "prebaked", remote: okRemote, edit: func(c *config.ClusterConfig) { c.Spec.Image.Bootstrap = config.BootstrapPrebaked }, refuse: "prebaked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadConfig(t, "v1.35.3")
			if tc.edit != nil {
				tc.edit(cfg)
			}
			fc := newFakeCluster(t, cfg, "v1.35.3", nil)
			u, out := fc.upgrader(cfg, tc.remote)
			err := u.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.refuse) || !strings.Contains(err.Error(), "fix: ") {
				t.Fatalf("want a refusal with %q and a fix, got %v\n%s", tc.refuse, err, out)
			}
			if len(fc.applied) != 0 {
				t.Fatalf("applied %v", fc.applied)
			}
		})
	}
}

// Without an autoscaled pool, a missing Cluster Autoscaler release is no
// reason to stop.
func TestNoAutoscalerCheckWithoutAutoscaledPools(t *testing.T) {
	cfg := loadConfig(t, "v1.35.3")
	cfg.Spec.NodePools = cfg.Spec.NodePools[:1]
	fc := newFakeCluster(t, cfg, "v1.35.3", nil)
	u, out := fc.upgrader(cfg, fakeRemote{packages: true})
	if err := u.Run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	cfg := loadConfig(t, "v1.35.3")
	fc := newFakeCluster(t, cfg, "v1.35.3", nil)
	u, out := fc.upgrader(cfg, okRemote)
	if err := u.Plan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fc.applied) != 0 {
		t.Fatalf("applied %v", fc.applied)
	}
	if !strings.Contains(out.String(), "next: upgrade the control plane from v1.35.3") || !strings.Contains(out.String(), "update KubeadmControlPlane ns/c-cp") {
		t.Errorf("output:\n%s", out)
	}
}

func TestDrift(t *testing.T) {
	cfg := loadConfig(t, "v1.35.3")
	fc := newFakeCluster(t, cfg, "v1.36.6", map[string]string{"b": "v1.35.3"})
	drift, err := Drift(context.Background(), fc.c, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(drift, ", "); got != "control plane v1.36.6, pool a v1.36.6" {
		t.Errorf("drift %q", got)
	}
	same := newFakeCluster(t, cfg, "v1.35.3", nil)
	if drift, err := Drift(context.Background(), same.c, cfg); err != nil || len(drift) != 0 {
		t.Errorf("no drift expected, got %v %v", drift, err)
	}
	empty := fake.NewClientBuilder().Build()
	if drift, err := Drift(context.Background(), empty, cfg); err != nil || len(drift) != 0 {
		t.Errorf("a cluster not yet created has no drift, got %v %v", drift, err)
	}
	_ = clusterGVK
}

func TestDrainMessage(t *testing.T) {
	mc := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"conditions": []any{
		map[string]any{"type": "Deleting", "status": "True", "reason": "DrainingNode", "message": "Drain not completed yet:\n* Pod default/web-1: cannot evict pod as it would violate the pod's disruption budget."},
	}}}}
	if got := drainMessage(mc); !strings.Contains(got, "disruption budget") || strings.Contains(got, "\n") {
		t.Errorf("got %q", got)
	}
	mc.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Deleting", "reason": "WaitingForInfrastructure"}}}
	if got := drainMessage(mc); got != "" {
		t.Errorf("not draining, got %q", got)
	}
}

// Just after a pool's template changes, its MachineDeployment's status can
// still count the old machines as up to date. The pool is not rolled out
// until its Machines run the new version.
func TestPoolNotRolledOutWhileItsMachinesRunTheOldVersion(t *testing.T) {
	cfg := loadConfig(t, "v1.35.3")
	fc := newFakeCluster(t, cfg, "v1.36.6", nil)
	md := &unstructured.Unstructured{}
	md.SetGroupVersionKind(mdGVK)
	ctx := context.Background()
	if err := fc.c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "c-a"}, md); err != nil {
		t.Fatal(err)
	}
	// The machines were made at v1.35.3; the spec now says v1.36.6 and the
	// status looks complete.
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(machineGVK.GroupVersion().WithKind("MachineList"))
	if err := fc.c.List(ctx, list, client.MatchingLabels{deploymentNameLabel: "c-a"}); err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		_ = unstructured.SetNestedField(list.Items[i].Object, "v1.35.3", "spec", "version")
		if err := fc.c.Update(ctx, &list.Items[i]); err != nil {
			t.Fatal(err)
		}
	}
	l, err := ReadLive(ctx, fc.c, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if l.Pools[0].RolledOut {
		t.Fatal("pool a must not count as rolled out while its machines run v1.35.3")
	}
	if !l.Pools[1].RolledOut {
		t.Fatal("pool b's machines run its version")
	}
}

// A run that resumes on a pool (the control plane already at the target)
// still stops for a missing Cluster Autoscaler release.
func TestAutoscalerCheckedWhenResumingOnAPool(t *testing.T) {
	cfg := loadConfig(t, "v1.35.3")
	fc := newFakeCluster(t, cfg, "v1.36.6", map[string]string{"a": "v1.35.3", "b": "v1.35.3"})
	u, out := fc.upgrader(cfg, fakeRemote{packages: true})
	err := u.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no Cluster Autoscaler release") {
		t.Fatalf("want the autoscaler refusal, got %v\n%s", err, out)
	}
	if len(fc.applied) != 0 {
		t.Fatalf("applied %v", fc.applied)
	}
}

// The control plane is not rolled out while a control-plane Machine runs the
// old version, even when the deprecated status.version is not set.
func TestControlPlaneNotRolledOutWhileAMachineRunsTheOldVersion(t *testing.T) {
	cfg := loadConfig(t, "v1.35.3")
	fc := newFakeCluster(t, cfg, "v1.36.6", nil)
	ctx := context.Background()
	kcp := &unstructured.Unstructured{}
	kcp.SetGroupVersionKind(kcpGVK)
	if err := fc.c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "c-cp"}, kcp); err != nil {
		t.Fatal(err)
	}
	unstructured.RemoveNestedField(kcp.Object, "status", "version")
	if err := fc.c.Update(ctx, kcp); err != nil {
		t.Fatal(err)
	}
	if l, err := ReadLive(ctx, fc.c, cfg); err != nil || !l.ControlPlane.RolledOut {
		t.Fatalf("all machines at the target without status.version: want rolled out, got %+v %v", l.ControlPlane, err)
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(machineGVK.GroupVersion().WithKind("MachineList"))
	if err := fc.c.List(ctx, list, client.MatchingLabels{controlPlaneLabel: ""}); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(list.Items[0].Object, "v1.35.3", "spec", "version")
	if err := fc.c.Update(ctx, &list.Items[0]); err != nil {
		t.Fatal(err)
	}
	if l, err := ReadLive(ctx, fc.c, cfg); err != nil || l.ControlPlane.RolledOut {
		t.Fatalf("a machine at v1.35.3: want not rolled out, got %+v %v", l.ControlPlane, err)
	}
}

// Settled comes from the machines too: a pool whose status looks finished
// while its Machines run another version keeps the control plane waiting.
func TestNotSettledWhileAPoolsMachinesLag(t *testing.T) {
	cfg := loadConfig(t, "v1.35.3")
	fc := newFakeCluster(t, cfg, "v1.35.3", nil)
	ctx := context.Background()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(machineGVK.GroupVersion().WithKind("MachineList"))
	if err := fc.c.List(ctx, list, client.MatchingLabels{deploymentNameLabel: "c-a"}); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(list.Items[0].Object, "v1.35.1", "spec", "version")
	if err := fc.c.Update(ctx, &list.Items[0]); err != nil {
		t.Fatal(err)
	}
	u, out := fc.upgrader(cfg, okRemote)
	err := u.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "not settled") {
		t.Fatalf("want a not-settled refusal, got %v\n%s", err, out)
	}
	if len(fc.applied) != 0 {
		t.Fatalf("applied %v", fc.applied)
	}
}

// A pool hydra.yaml names but the cluster lacks is refused, never reported
// done.
func TestMissingPoolIsRefused(t *testing.T) {
	cfg := loadConfig(t, "v1.36.6")
	live := loadConfig(t, "v1.36.6")
	live.Spec.NodePools = live.Spec.NodePools[:1]
	fc := newFakeCluster(t, live, "v1.36.6", nil)
	u, out := fc.upgrader(cfg, okRemote)
	err := u.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no MachineDeployment yet: [b]") {
		t.Fatalf("want a missing-pool refusal, got %v\n%s", err, out)
	}
}

// A finished upgrade with other hydra.yaml edits says so, instead of
// implying the file matches.
func TestDoneNotesOtherEdits(t *testing.T) {
	cfg := loadConfig(t, "v1.36.6")
	fc := newFakeCluster(t, cfg, "v1.36.6", nil)
	edited := loadConfig(t, "v1.36.6")
	edited.Spec.NodePools[0].Replicas = 5
	u, out := fc.upgrader(edited, okRemote)
	if err := u.Run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "note: hydra.yaml differs from the live cluster in more than the version") {
		t.Errorf("want a note about the other edits:\n%s", out)
	}
}

// The stage loop's bound grows with the pools: every part of a cluster with
// many pools gets its passes (the control plane and each pool may need two:
// a resumed wait, then their own).
func TestMaxPassesFollowsThePools(t *testing.T) {
	for _, n := range []int{0, 2, 63, 64, 500} {
		if need := 2 * (n + 1); maxPasses(n) < need {
			t.Errorf("%d pools: bound %d, need at least %d", n, maxPasses(n), need)
		}
	}
}
