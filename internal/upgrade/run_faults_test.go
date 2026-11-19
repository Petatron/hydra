package upgrade

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Petatron/hydra/internal/cluster"
	"github.com/Petatron/hydra/internal/config"
)

var errBoom = errors.New("boom")

// fault fails one verb on one kind after letting skip matching calls through.
type fault struct {
	verb, kind string
	skip       int
	calls      int
}

func (f *fault) hit(verb string, obj runtime.Object) error {
	if f.verb != verb || strings.TrimSuffix(obj.GetObjectKind().GroupVersionKind().Kind, "List") != f.kind {
		return nil
	}
	if f.calls++; f.calls <= f.skip {
		return nil
	}
	return errBoom
}

// faulty wraps the fake cluster's client so the faults fire.
func (fc *fakeCluster) faulty(faults ...*fault) client.Client {
	check := func(verb string, obj runtime.Object) error {
		for _, f := range faults {
			if err := f.hit(verb, obj); err != nil {
				return err
			}
		}
		return nil
	}
	return interceptor.NewClient(fc.c.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := check("get", obj); err != nil {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := check("list", list); err != nil {
				return err
			}
			return c.List(ctx, list, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := check("patch", obj); err != nil {
				return err
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
		Apply: func(ctx context.Context, c client.WithWatch, ac runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			for _, f := range faults {
				if f.verb == "apply" {
					return errBoom
				}
			}
			return c.Apply(ctx, ac, opts...)
		},
	})
}

// renderWithout renders, then drops every object of kind, at version only
// when version is set.
func renderWithout(kind, version string) RenderFunc {
	return func(c *config.ClusterConfig) ([]*unstructured.Unstructured, error) {
		objs, err := renderFunc(c)
		if err != nil || (version != "" && c.Spec.KubernetesVersion != version) {
			return objs, err
		}
		out := objs[:0]
		for _, o := range objs {
			if o.GetKind() != kind {
				out = append(out, o)
			}
		}
		return out, nil
	}
}

// renderFailingAt fails to render at version.
func renderFailingAt(version string) RenderFunc {
	return func(c *config.ClusterConfig) ([]*unstructured.Unstructured, error) {
		if c.Spec.KubernetesVersion == version {
			return nil, errBoom
		}
		return renderFunc(c)
	}
}

func TestUpgraderPollDefault(t *testing.T) {
	if got := (&Upgrader{}).poll(); got != 10*time.Second {
		t.Errorf("got %v", got)
	}
}

// Every read ReadLive makes is an error when it fails, never a guess that
// would let the upgrade decide from half the picture.
func TestReadLiveFailures(t *testing.T) {
	ctx := context.Background()
	cfg := loadConfig(t, "v1.35.3")
	for _, tc := range []struct {
		name string
		f    *fault
		want string
	}{
		{"Cluster unreadable", &fault{verb: "get", kind: "Cluster"}, "boom"},
		{"control plane unreadable", &fault{verb: "get", kind: kcpKind, skip: 1}, "reading the KubeadmControlPlane: boom"},
		{"control-plane Machines unlistable", &fault{verb: "list", kind: lcMachine, skip: 1}, "listing control-plane Machines: boom"},
		{"MachineDeployments unlistable", &fault{verb: "list", kind: mdKind, skip: 1}, "listing MachineDeployments: boom"},
		{"Machines unlistable", &fault{verb: "list", kind: lcMachine, skip: 2}, "listing Machines: boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFakeCluster(t, cfg, "v1.35.3", nil)
			_, err := ReadLive(ctx, fc.faulty(tc.f), cfg)
			if !errors.Is(err, errBoom) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func setField(t *testing.T, c client.Client, gvkObj *unstructured.Unstructured, value string, path ...string) {
	t.Helper()
	ctx := context.Background()
	if err := c.Get(ctx, client.ObjectKeyFromObject(gvkObj), gvkObj); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(gvkObj.Object, value, path...)
	if err := c.Update(ctx, gvkObj); err != nil {
		t.Fatal(err)
	}
}

func objRef(kind, name string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	switch kind {
	case kcpKind:
		o.SetGroupVersionKind(kcpGVK)
	case mdKind:
		o.SetGroupVersionKind(mdGVK)
	default:
		o.SetGroupVersionKind(clusterGVK)
	}
	o.SetNamespace("ns")
	o.SetName(name)
	return o
}

// A version the upgrade cannot read is refused, never treated as "older".
func TestReadLiveRefusesUnreadableVersions(t *testing.T) {
	cfg := loadConfig(t, "v1.35.3")
	fc := newFakeCluster(t, cfg, "v1.35.3", nil)
	setField(t, fc.c, objRef(kcpKind, "c-cp"), "latest", "spec", "version")
	if _, err := ReadLive(context.Background(), fc.c, cfg); err == nil || !strings.Contains(err.Error(), "the KubeadmControlPlane's spec.version") {
		t.Fatalf("got %v", err)
	}
	fc = newFakeCluster(t, cfg, "v1.35.3", nil)
	setField(t, fc.c, objRef(mdKind, "c-a"), "", "spec", "template", "spec", "version")
	if _, err := ReadLive(context.Background(), fc.c, cfg); err == nil || !strings.Contains(err.Error(), "MachineDeployment c-a's version") {
		t.Fatalf("got %v", err)
	}
}

// Leftovers without a Cluster object: nothing runs, so no drift and nothing
// live.
func TestLeftoversWithoutAClusterHaveNoDrift(t *testing.T) {
	ctx := context.Background()
	cfg := loadConfig(t, "v1.35.3")
	fc := newFakeCluster(t, cfg, "v1.36.6", nil)
	if err := fc.c.Delete(ctx, objRef("Cluster", "c")); err != nil {
		t.Fatal(err)
	}
	l, err := ReadLive(ctx, fc.c, cfg)
	if err != nil || l.Exists {
		t.Fatalf("got %+v, %v", l, err)
	}
	if drift, err := Drift(ctx, fc.c, cfg); err != nil || drift != nil {
		t.Fatalf("got %v, %v", drift, err)
	}
}

func TestDriftFailures(t *testing.T) {
	ctx := context.Background()
	cfg := loadConfig(t, "v1.35.3")
	bad := loadConfig(t, "v1.35.3")
	bad.Spec.KubernetesVersion = "one-thirty-five"
	fc := newFakeCluster(t, cfg, "v1.35.3", nil)
	if _, err := Drift(ctx, fc.c, bad); err == nil {
		t.Error("an unreadable hydra.yaml version must be an error")
	}
	if _, err := Drift(ctx, fc.faulty(&fault{verb: "get", kind: kcpKind}), cfg); err == nil || !strings.Contains(err.Error(), "reading the KubeadmControlPlane: boom") {
		t.Errorf("got %v", err)
	}
	if _, err := Drift(ctx, fc.faulty(&fault{verb: "list", kind: mdKind}), cfg); !errors.Is(err, errBoom) {
		t.Errorf("got %v", err)
	}
}

func TestMachinesAt(t *testing.T) {
	md := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"replicas": int64(2)}}}
	mc := func(v string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"version": v}}}
	}
	if !machinesAt(md, []*unstructured.Unstructured{mc("v1"), mc("v1")}, "v1") {
		t.Error("without spec.replicas, status.replicas is the count")
	}
	if machinesAt(md, []*unstructured.Unstructured{mc("v1")}, "v1") {
		t.Error("one machine short is not rolled out")
	}
}

func TestComposeFailures(t *testing.T) {
	cfg := loadConfig(t, "v1.35.3")
	mixed := Live{ControlPlane: Part{Version: v("v1.36.6")}, Pools: []Part{{Name: "a", Version: v("v1.35.3")}}}
	for _, tc := range []struct {
		name   string
		render RenderFunc
		want   string
	}{
		{"base render fails", renderFailingAt("v1.36.6"), "boom"},
		{"pool render fails", renderFailingAt("v1.35.3"), "boom"},
		{"no MachineDeployment", renderWithout(mdKind, "v1.35.3"), "pool a renders no MachineDeployment c-a"},
		{"no template", renderWithout(kctKind, "v1.35.3"), "pool a renders no KubeadmConfigTemplate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (&Upgrader{Config: cfg, Render: tc.render}).Compose(mixed)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestStageObjectsFailures(t *testing.T) {
	cfg := loadConfig(t, "v1.35.3")
	for _, tc := range []struct {
		name   string
		render RenderFunc
		stage  Stage
		want   string
	}{
		{"render fails", renderFailingAt("v1.36.6"), Stage{Kind: ApplyControlPlane}, "boom"},
		{"no control plane", renderWithout(kcpKind, ""), Stage{Kind: ApplyControlPlane}, "renders no KubeadmControlPlane"},
		{"no MachineDeployment", renderWithout(mdKind, ""), Stage{Kind: ApplyPool, Pool: "a"}, "pool a renders no MachineDeployment"},
		{"no template", renderWithout(kctKind, ""), Stage{Kind: ApplyPool, Pool: "a"}, "pool a renders no KubeadmConfigTemplate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (&Upgrader{Config: cfg, Render: tc.render, Target: v("v1.36.6")}).stageObjects(tc.stage)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
	// A pool stage applies the template before the deployment.
	objs, err := (&Upgrader{Config: cfg, Render: renderFunc, Target: v("v1.36.6")}).stageObjects(Stage{Kind: ApplyPool, Pool: "a"})
	if err != nil || len(objs) != 2 || objs[0].GetKind() != kctKind || objs[1].GetKind() != mdKind {
		t.Fatalf("got %v, %v", objs, err)
	}
}

// Mid-upgrade, other edits cannot be applied with create (it refuses mixed
// versions): the fix says to finish the upgrade first.
func TestOtherEditsMidUpgradeSayFinishFirst(t *testing.T) {
	cfg := loadConfig(t, "v1.35.3")
	fc := newFakeCluster(t, cfg, "v1.36.6", map[string]string{"a": "v1.35.3", "b": "v1.35.3"})
	edited := loadConfig(t, "v1.35.3")
	edited.Spec.NodePools[0].Replicas = 5
	u, out := fc.upgrader(edited, okRemote)
	_, _, err := u.Next(context.Background())
	var r *Refusal
	if !errors.As(err, &r) || !strings.Contains(r.Fix, "an upgrade is under way") {
		t.Fatalf("got %v\n%s", err, out)
	}
}

func TestNextFailures(t *testing.T) {
	ctx := context.Background()
	cfg := loadConfig(t, "v1.35.3")
	fc := newFakeCluster(t, cfg, "v1.35.3", nil)
	u, _ := fc.upgrader(cfg, okRemote)
	u.M = &cluster.Manager{C: fc.faulty(&fault{verb: "get", kind: "Cluster"}), Out: u.Out}
	if _, _, err := u.Next(ctx); !errors.Is(err, errBoom) {
		t.Errorf("got %v", err)
	}
	bad := loadConfig(t, "v1.35.3")
	bad.Spec.KubernetesVersion = "soon"
	u, _ = fc.upgrader(bad, okRemote)
	if _, _, err := u.Next(ctx); err == nil {
		t.Error("an unreadable hydra.yaml version must be an error")
	}
	// The in-sync check's dry run failing is an error, not "in sync".
	u, _ = fc.upgrader(cfg, okRemote)
	u.M = &cluster.Manager{C: fc.faulty(&fault{verb: "patch", kind: kcpKind}), Out: u.Out}
	if _, _, err := u.Next(ctx); err == nil || !strings.Contains(err.Error(), "dry-run applying") {
		t.Errorf("got %v", err)
	}
	u, _ = fc.upgrader(cfg, okRemote)
	u.Render = renderFailingAt("v1.35.3")
	if _, _, err := u.Next(ctx); !errors.Is(err, errBoom) {
		t.Errorf("got %v", err)
	}
}

func TestPlanFailuresAndNothingToDo(t *testing.T) {
	ctx := context.Background()
	cfg := loadConfig(t, "v1.35.3")
	for _, tc := range []struct {
		name   string
		remote Remote
		faults []*fault
		render RenderFunc
		want   string
	}{
		{name: "cluster unreadable", remote: okRemote, faults: []*fault{{verb: "get", kind: "Cluster"}}, want: "boom"},
		{name: "preflight refuses", remote: fakeRemote{packages: false}, want: "pkgs.k8s.io lacks"},
		{name: "target renders no control plane", remote: okRemote, render: renderWithout(kcpKind, "v1.36.6"), want: "renders no KubeadmControlPlane"},
		{name: "stage dry run fails", remote: okRemote, faults: []*fault{{verb: "patch", kind: kcpKind, skip: 1}}, want: "dry-run applying"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFakeCluster(t, cfg, "v1.35.3", nil)
			u, _ := fc.upgrader(cfg, tc.remote)
			if tc.faults != nil {
				u.M = &cluster.Manager{C: fc.faulty(tc.faults...), Out: u.Out}
			}
			if tc.render != nil {
				u.Render = tc.render
			}
			if err := u.Plan(ctx); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if len(fc.applied) != 0 {
				t.Fatalf("plan applied %v", fc.applied)
			}
		})
	}
	// At the target: plan names the stage and checks nothing remote.
	done := loadConfig(t, "v1.36.6")
	fc := newFakeCluster(t, done, "v1.36.6", nil)
	u, out := fc.upgrader(done, fakeRemote{err: errBoom})
	if err := u.Plan(ctx); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "next: ") || strings.Contains(out.String(), "update ") {
		t.Errorf("output:\n%s", out)
	}
}

func TestRunFailuresApplyNothingMore(t *testing.T) {
	ctx := context.Background()
	cfg := loadConfig(t, "v1.35.3")
	for _, tc := range []struct {
		name   string
		faults []*fault
		render RenderFunc
		want   string
	}{
		{name: "target renders no control plane", render: renderWithout(kcpKind, "v1.36.6"), want: "renders no KubeadmControlPlane"},
		// Status, ReadLive and the in-sync dry run read the control plane
		// first; the fourth read is the ownership check's.
		{name: "ownership unreadable", faults: []*fault{{verb: "get", kind: kcpKind, skip: 3}}, want: "reading KubeadmControlPlane ns/c-cp: boom"},
		{name: "apply fails", faults: []*fault{{verb: "apply"}}, want: "boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFakeCluster(t, cfg, "v1.35.3", nil)
			u, out := fc.upgrader(cfg, okRemote)
			if tc.faults != nil {
				u.M = &cluster.Manager{C: fc.faulty(tc.faults...), Out: u.Out}
			}
			if tc.render != nil {
				u.Render = tc.render
			}
			err := u.Run(ctx)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v\n%s", tc.want, err, out)
			}
			if len(fc.applied) != 0 {
				t.Fatalf("applied %v", fc.applied)
			}
		})
	}
}

// --skip-remote-checks: the run goes ahead and says it skipped them.
func TestRunWithoutRemoteChecks(t *testing.T) {
	cfg := loadConfig(t, "v1.35.3")
	fc := newFakeCluster(t, cfg, "v1.35.3", nil)
	u, out := fc.upgrader(cfg, nil)
	u.Remote = nil
	if err := u.Run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "skipping the remote checks") {
		t.Errorf("output:\n%s", out)
	}
}

type tagsErrRemote struct{}

func (tagsErrRemote) HasPackages(context.Context, Version) (bool, error) { return true, nil }
func (tagsErrRemote) AutoscalerTags(context.Context, Version) ([]string, error) {
	return nil, errBoom
}

func TestPreflightRefusesUnlistableAutoscalerImages(t *testing.T) {
	cfg := loadConfig(t, "v1.35.3")
	fc := newFakeCluster(t, cfg, "v1.35.3", nil)
	u, out := fc.upgrader(cfg, tagsErrRemote{})
	err := u.Run(context.Background())
	var r *Refusal
	if !errors.As(err, &r) || !strings.Contains(r.Reason, "could not list Cluster Autoscaler images for v1.36 (pools b are autoscaled): boom") {
		t.Fatalf("got %v\n%s", err, out)
	}
	if len(fc.applied) != 0 {
		t.Fatalf("applied %v", fc.applied)
	}
}

// Ctrl-C while waiting ends the run without claiming a timeout; the rollout
// itself goes on in the management cluster.
func TestWaitInterrupted(t *testing.T) {
	cfg := loadConfig(t, "v1.35.3")
	fc := newFakeCluster(t, cfg, "v1.35.3", nil)
	fc.rollout = false
	u, out := fc.upgrader(cfg, okRemote)
	u.Timeout = time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	err := u.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "interrupted; the rollout continues") || errors.Is(err, cluster.ErrTimeout) {
		t.Fatalf("got %v\n%s", err, out)
	}
}

// A failed progress read is shown and waited out, never taken as done.
func TestWaitShowsUnreadableProgress(t *testing.T) {
	cfg := loadConfig(t, "v1.36.6")
	fc := newFakeCluster(t, cfg, "v1.36.6", nil)
	u, out := fc.upgrader(cfg, okRemote)
	u.M = &cluster.Manager{C: fc.faulty(&fault{verb: "get", kind: "Cluster"}), Out: u.Out}
	u.Timeout = 50 * time.Millisecond
	err := u.wait(context.Background(), Stage{Kind: WaitControlPlane})
	if !errors.Is(err, cluster.ErrTimeout) || !strings.Contains(out.String(), "cannot read progress: boom") {
		t.Fatalf("got %v\n%s", err, out)
	}
}

func TestProgressMachinesUnlistable(t *testing.T) {
	cfg := loadConfig(t, "v1.36.6")
	fc := newFakeCluster(t, cfg, "v1.36.6", nil)
	u, _ := fc.upgrader(cfg, okRemote)
	// Status and ReadLive list Machines three times; the fourth is progress's.
	u.M = &cluster.Manager{C: fc.faulty(&fault{verb: "list", kind: lcMachine, skip: 3}), Out: u.Out}
	if _, _, err := u.progress(context.Background(), Stage{Kind: WaitPool, Pool: "a"}); err == nil || !strings.Contains(err.Error(), "listing Machines: boom") {
		t.Fatalf("got %v", err)
	}
}

// A drain a PodDisruptionBudget blocks is shown with the machine's name.
func TestProgressShowsBlockedDrains(t *testing.T) {
	ctx := context.Background()
	cfg := loadConfig(t, "v1.35.3")
	fc := newFakeCluster(t, cfg, "v1.35.3", nil)
	mc := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"version": "v1.35.3"},
		"status": map[string]any{"conditions": []any{
			"garbage",
			map[string]any{"type": "Ready", "status": "True"},
			map[string]any{"type": "Deleting", "status": "True", "reason": lcDrainingNode, "message": "Drain not completed:\n  * Pod default/web-1: would violate the pod's disruption budget."},
		}},
	}}
	mc.SetGroupVersionKind(machineGVK)
	mc.SetNamespace("ns")
	mc.SetName("c-cp-old")
	mc.SetLabels(map[string]string{clusterNameLabel: "c", controlPlaneLabel: ""})
	mc.SetFinalizers([]string{"machine.cluster.x-k8s.io"})
	if err := fc.c.Create(ctx, mc); err != nil {
		t.Fatal(err)
	}
	if err := fc.c.Delete(ctx, mc); err != nil {
		t.Fatal(err)
	}
	u, _ := fc.upgrader(cfg, okRemote)
	line, done, err := u.progress(ctx, Stage{Kind: WaitControlPlane})
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Error("the control plane is not at the target")
	}
	if !strings.Contains(line, "draining: c-cp-old: Drain not completed: * Pod default/web-1: would violate the pod's disruption budget.") {
		t.Errorf("line %q", line)
	}
	if !strings.Contains(line, "0/4 machines at v1.36.6") {
		t.Errorf("line %q", line)
	}
}

func TestDrainMessageWithoutDeletingCondition(t *testing.T) {
	mc := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"conditions": []any{
		"garbage", map[string]any{"type": "Ready", "status": "True"},
	}}}}
	if got := drainMessage(mc); got != "" {
		t.Errorf("got %q", got)
	}
}
