package upgrade

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Petatron/hydra/internal/cluster"
	"github.com/Petatron/hydra/internal/config"
	"github.com/Petatron/hydra/internal/render"
)

const (
	kcpKind      = "KubeadmControlPlane"
	mdKind       = "MachineDeployment"
	deletingCond = "Deleting"
	capiGroup    = "cluster.x-k8s.io"
	capiVersion  = "v1beta2"
)

var (
	kcpGVK     = schema.GroupVersionKind{Group: "controlplane." + capiGroup, Version: capiVersion, Kind: kcpKind}
	mdGVK      = schema.GroupVersionKind{Group: capiGroup, Version: capiVersion, Kind: mdKind}
	machineGVK = schema.GroupVersionKind{Group: capiGroup, Version: capiVersion, Kind: "Machine"}
	kctKind    = "KubeadmConfigTemplate"
)

const (
	clusterNameLabel    = "cluster.x-k8s.io/cluster-name"
	deploymentNameLabel = "cluster.x-k8s.io/deployment-name"
	controlPlaneLabel   = "cluster.x-k8s.io/control-plane"
)

// SkewRules are the version-skew rules the upgrade enforces, printed before
// it starts.
const SkewRules = `version-skew rules enforced:
  - one minor version at a time, never down
  - the control plane first: every control-plane machine runs the target before any worker changes
  - worker pools next, one at a time in hydra.yaml's order: a kubelet is never newer than the API server
  - drains respect PodDisruptionBudgets: a blocked drain is reported and waited on, never forced`

// RenderFunc renders hydra.yaml into objects (the CLI's render, stamped with
// its version).
type RenderFunc func(*config.ClusterConfig) ([]*unstructured.Unstructured, error)

// Remote answers the preflight questions that need the internet. A nil
// Remote skips them (--skip-remote-checks).
type Remote interface {
	// HasPackages reports whether pkgs.k8s.io carries kubelet and kubeadm v.
	HasPackages(ctx context.Context, v Version) (bool, error)
	// AutoscalerTags lists Cluster Autoscaler image tags for v's minor.
	AutoscalerTags(ctx context.Context, v Version) ([]string, error)
}

// Upgrader upgrades one cluster on one management cluster.
type Upgrader struct {
	M      *cluster.Manager
	Config *config.ClusterConfig
	Render RenderFunc
	Remote Remote
	Target Version
	// Timeout bounds each stage's wait; it ends the wait, not the rollout.
	Timeout time.Duration
	Out     io.Writer
	// Poll is the interval between reads while waiting.
	Poll time.Duration
}

func (u *Upgrader) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(u.Out, format+"\n", args...)
}

func (u *Upgrader) poll() time.Duration {
	if u.Poll > 0 {
		return u.Poll
	}
	return 10 * time.Second
}

func (u *Upgrader) ns() string   { return u.Config.Metadata.Namespace }
func (u *Upgrader) name() string { return u.Config.Metadata.Name }

func (u *Upgrader) poolObjName(pool string) string { return u.name() + "-" + pool }

// ReadLive reads what the management cluster shows about the cluster.
func ReadLive(ctx context.Context, c client.Client, cfg *config.ClusterConfig) (Live, error) {
	ns, name := cfg.Metadata.Namespace, cfg.Metadata.Name
	var l Live
	m := &cluster.Manager{C: c, Out: io.Discard}
	s, err := m.Status(ctx, ns, name)
	if err != nil {
		return l, err
	}
	l.Exists, l.Terminating = s.Exists, s.Terminating
	l.Available = s.Available == cluster.ConditionTrue
	l.Settled = l.Available && s.RolledOut
	if !l.Exists {
		return l, nil
	}

	kcp := &unstructured.Unstructured{}
	kcp.SetGroupVersionKind(kcpGVK)
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: render.ControlPlaneName(name)}, kcp); err != nil {
		return l, fmt.Errorf("reading the KubeadmControlPlane: %w", err)
	}
	spec, _, _ := unstructured.NestedString(kcp.Object, "spec", "version")
	if l.ControlPlane.Version, err = ParseVersion(spec); err != nil {
		return l, fmt.Errorf("the KubeadmControlPlane's spec.version: %w", err)
	}
	// status.version is the lowest version any control-plane machine runs.
	running, _, _ := unstructured.NestedString(kcp.Object, "status", "version")
	l.ControlPlane.RolledOut = cluster.UpToDate(kcp) && running == spec

	mds := &unstructured.UnstructuredList{}
	mds.SetGroupVersionKind(mdGVK.GroupVersion().WithKind("MachineDeploymentList"))
	if err := c.List(ctx, mds, client.InNamespace(ns), client.MatchingLabels{render.ClusterLabelKey: name}); err != nil {
		return l, fmt.Errorf("listing MachineDeployments: %w", err)
	}
	byName := map[string]*unstructured.Unstructured{}
	for i := range mds.Items {
		byName[mds.Items[i].GetName()] = &mds.Items[i]
	}
	for _, p := range cfg.Spec.NodePools {
		obj := name + "-" + p.Name
		md, ok := byName[obj]
		if !ok {
			// A pool hydra.yaml adds: create makes it, at hydra.yaml's version.
			continue
		}
		delete(byName, obj)
		v, _, _ := unstructured.NestedString(md.Object, "spec", "template", "spec", "version")
		pv, err := ParseVersion(v)
		if err != nil {
			return l, fmt.Errorf("MachineDeployment %s's version: %w", obj, err)
		}
		l.Pools = append(l.Pools, Part{Name: p.Name, Version: pv, RolledOut: cluster.UpToDate(md)})
	}
	l.Strays = slices.Sorted(maps.Keys(byName))
	return l, nil
}

// Drift reports, for a live cluster, the parts whose version differs from
// hydra.yaml's: create would change them all at once, which only upgrade may
// do. Empty when they agree or the cluster does not exist yet.
func Drift(ctx context.Context, c client.Client, cfg *config.ClusterConfig) ([]string, error) {
	want, err := ParseVersion(cfg.Spec.KubernetesVersion)
	if err != nil {
		return nil, err
	}
	kcp := &unstructured.Unstructured{}
	kcp.SetGroupVersionKind(kcpGVK)
	switch err := c.Get(ctx, client.ObjectKey{Namespace: cfg.Metadata.Namespace, Name: render.ControlPlaneName(cfg.Metadata.Name)}, kcp); {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("reading the KubeadmControlPlane: %w", err)
	}
	l, err := ReadLive(ctx, c, cfg)
	if err != nil {
		return nil, err
	}
	var out []string
	if l.ControlPlane.Version != want {
		out = append(out, "control plane "+l.ControlPlane.Version.String())
	}
	for _, p := range l.Pools {
		if p.Version != want {
			out = append(out, "pool "+p.Name+" "+p.Version.String())
		}
	}
	return out, nil
}

// at returns cfg with its Kubernetes version set to v.
func at(cfg *config.ClusterConfig, v Version) *config.ClusterConfig {
	c := *cfg
	c.Spec.KubernetesVersion = v.String()
	return &c
}

// pick returns the rendered object of kind and name.
func pick(objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return o
		}
	}
	return nil
}

func kctOf(md *unstructured.Unstructured) string {
	n, _, _ := unstructured.NestedString(md.Object, "spec", "template", "spec", "bootstrap", "configRef", "name")
	return n
}

// Compose renders hydra.yaml as the live cluster runs it: the control plane
// at its live version, each pool at its own. Planned against the cluster, it
// shows whether hydra.yaml matches what runs in everything but the version.
func (u *Upgrader) Compose(l Live) ([]*unstructured.Unstructured, error) {
	base, err := u.Render(at(u.Config, l.ControlPlane.Version))
	if err != nil {
		return nil, err
	}
	for _, p := range l.Pools {
		if p.Version == l.ControlPlane.Version {
			continue
		}
		other, err := u.Render(at(u.Config, p.Version))
		if err != nil {
			return nil, err
		}
		md := u.poolObjName(p.Name)
		baseMD, otherMD := pick(base, mdKind, md), pick(other, mdKind, md)
		if baseMD == nil || otherMD == nil {
			return nil, fmt.Errorf("pool %s renders no MachineDeployment %s", p.Name, md)
		}
		otherKCT := pick(other, kctKind, kctOf(otherMD))
		if otherKCT == nil {
			return nil, fmt.Errorf("pool %s renders no %s", p.Name, kctKind)
		}
		baseKCT := kctOf(baseMD)
		for i, o := range base {
			switch {
			case o.GetKind() == mdKind && o.GetName() == md:
				base[i] = otherMD
			case o.GetKind() == kctKind && o.GetName() == baseKCT:
				base[i] = otherKCT
			}
		}
	}
	return base, nil
}

// stageObjects are what a stage applies: the KubeadmControlPlane at the
// target, or one pool's KubeadmConfigTemplate and MachineDeployment at it
// (the template first, so the deployment never points at a missing one).
func (u *Upgrader) stageObjects(s Stage) ([]*unstructured.Unstructured, error) {
	objs, err := u.Render(at(u.Config, u.Target))
	if err != nil {
		return nil, err
	}
	if s.Kind == ApplyControlPlane {
		kcp := pick(objs, kcpKind, render.ControlPlaneName(u.name()))
		if kcp == nil {
			return nil, errors.New("hydra.yaml renders no KubeadmControlPlane")
		}
		return []*unstructured.Unstructured{kcp}, nil
	}
	md := pick(objs, mdKind, u.poolObjName(s.Pool))
	if md == nil {
		return nil, fmt.Errorf("pool %s renders no MachineDeployment", s.Pool)
	}
	kct := pick(objs, kctKind, kctOf(md))
	if kct == nil {
		return nil, fmt.Errorf("pool %s renders no %s", s.Pool, kctKind)
	}
	return []*unstructured.Unstructured{kct, md}, nil
}

// checkInSync refuses when hydra.yaml differs from the live cluster in
// anything but the version: upgrade applies only the version, and must not
// carry other edits along, or skip ones create has not applied.
func (u *Upgrader) checkInSync(ctx context.Context, l Live) error {
	objs, err := u.Compose(l)
	if err != nil {
		return err
	}
	changes, err := u.M.Plan(ctx, objs, u.name(), false)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return nil
	}
	var what []string
	for _, ch := range changes {
		what = append(what, ch.Action+" "+ch.Object)
	}
	return &Refusal{
		Reason: fmt.Sprintf("hydra.yaml differs from the live cluster in more than the version (%s)", strings.Join(what, ", ")),
		Fix:    fmt.Sprintf("apply hydra.yaml first with `hydra cluster create` (kubernetesVersion %s, what runs), or revert the edits; `hydra cluster plan` shows them", l.Lowest()),
	}
}

// preflight runs the checks that need the internet, before a stage that
// creates machines at the target.
func (u *Upgrader) preflight(ctx context.Context, s Stage) error {
	if u.Config.Spec.Image.Bootstrap == config.BootstrapPrebaked {
		return &Refusal{
			Reason: "this cluster boots a prebaked image, which carries one Kubernetes version; hydra cannot yet swap the image per stage",
			Fix:    "build an image carrying " + u.Target.String() + " and upgrade by hand, or switch the cluster to bootstrap: packages",
		}
	}
	if u.Remote == nil {
		u.logf("skipping the remote checks (pkgs.k8s.io, Cluster Autoscaler images), as asked")
		return nil
	}
	ok, err := u.Remote.HasPackages(ctx, u.Target)
	switch {
	case err != nil:
		return &Refusal{
			Reason: fmt.Sprintf("could not check pkgs.k8s.io for kubelet and kubeadm %s: %v", u.Target, err),
			Fix:    "machines install them from there at boot; check it is reachable, or pass --skip-remote-checks once you have checked by hand",
		}
	case !ok:
		return &Refusal{
			Reason: fmt.Sprintf("pkgs.k8s.io has no kubelet and kubeadm %s, which new machines install at boot", u.Target),
			Fix:    fmt.Sprintf("pick a released %s patch (https://kubernetes.io/releases/)", u.Target.MinorString()),
		}
	}
	u.logf("pkgs.k8s.io carries kubelet and kubeadm %s", u.Target)
	if s.Kind != ApplyControlPlane {
		return nil
	}
	var autoscaled []string
	for _, p := range u.Config.Spec.NodePools {
		if p.Autoscaling != nil {
			autoscaled = append(autoscaled, p.Name)
		}
	}
	if len(autoscaled) == 0 {
		return nil
	}
	tags, err := u.Remote.AutoscalerTags(ctx, u.Target)
	switch {
	case err != nil:
		return &Refusal{
			Reason: fmt.Sprintf("could not list Cluster Autoscaler images for %s (pools %s are autoscaled): %v", u.Target.MinorString(), strings.Join(autoscaled, ", "), err),
			Fix:    "check registry.k8s.io is reachable, or pass --skip-remote-checks once you have checked by hand",
		}
	case len(tags) == 0:
		return &Refusal{
			Reason: fmt.Sprintf("there is no Cluster Autoscaler release for %s yet, and pools %s are autoscaled: Cluster Autoscaler is pinned to the Kubernetes minor", u.Target.MinorString(), strings.Join(autoscaled, ", ")),
			Fix:    "wait for a Cluster Autoscaler " + u.Target.MinorString() + ".x release, or stay on the current minor",
		}
	}
	u.logf("Cluster Autoscaler has %s for %s: upgrade it to that minor once the control plane runs %s", tags[len(tags)-1], u.Target.MinorString(), u.Target)
	return nil
}

// Next reads the cluster and decides the next stage, refusing when the
// cluster or hydra.yaml is not in a state to upgrade from.
func (u *Upgrader) Next(ctx context.Context) (Live, Stage, error) {
	l, err := ReadLive(ctx, u.M.C, u.Config)
	if err != nil {
		return l, Stage{}, err
	}
	file, err := ParseVersion(u.Config.Spec.KubernetesVersion)
	if err != nil {
		return l, Stage{}, err
	}
	s, err := Decide(l, u.Target, file)
	if err != nil {
		return l, s, err
	}
	if s.Kind != Done {
		if err := u.checkInSync(ctx, l); err != nil {
			return l, s, err
		}
	}
	return l, s, nil
}

// Plan prints the next stage and what it would change, without changing
// anything.
func (u *Upgrader) Plan(ctx context.Context) error {
	_, s, err := u.Next(ctx)
	if err != nil {
		return err
	}
	u.logf("next: %s", s)
	if s.Kind != ApplyControlPlane && s.Kind != ApplyPool {
		return nil
	}
	if err := u.preflight(ctx, s); err != nil {
		return err
	}
	objs, err := u.stageObjects(s)
	if err != nil {
		return err
	}
	changes, err := u.M.Plan(ctx, objs, u.name(), false)
	if err != nil {
		return err
	}
	for _, ch := range changes {
		u.logf("%s %s", ch.Action, ch.Object)
		if ch.Diff != "" {
			u.logf("%s", ch.Diff)
		}
	}
	return nil
}

// Run upgrades stage by stage until the control plane and every pool run the
// target. Each stage is decided afresh from the live objects.
func (u *Upgrader) Run(ctx context.Context) error {
	u.logf("%s", SkewRules)
	start := time.Now()
	preflighted := false
	for range 64 {
		_, s, err := u.Next(ctx)
		if err != nil {
			return err
		}
		u.logf("next: %s", s)
		switch s.Kind {
		case Done:
			u.finish(start)
			return nil
		case ApplyControlPlane, ApplyPool:
			if !preflighted {
				if err := u.preflight(ctx, s); err != nil {
					return err
				}
				preflighted = true
			}
			objs, err := u.stageObjects(s)
			if err != nil {
				return err
			}
			if err := u.M.CheckOwnership(ctx, objs, u.name(), false); err != nil {
				return err
			}
			if err := u.M.Apply(ctx, objs, u.name(), false); err != nil {
				return err
			}
		}
		if err := u.wait(ctx, s); err != nil {
			return err
		}
	}
	return errors.New("the upgrade made no progress after many stages; inspect the cluster with `hydra cluster status`")
}

func (u *Upgrader) finish(start time.Time) {
	u.logf("cluster %s/%s runs %s: control plane and every pool (%s)", u.ns(), u.name(), u.Target, time.Since(start).Round(time.Second))
	if u.Config.Spec.KubernetesVersion != u.Target.String() {
		u.logf("now set kubernetesVersion: %s in hydra.yaml; until then create refuses, and plan shows the difference", u.Target)
	}
	for _, p := range u.Config.Spec.NodePools {
		if p.Autoscaling != nil {
			u.logf("autoscaled pools: run a Cluster Autoscaler %s.x release to match", u.Target.MinorString())
			break
		}
	}
	u.logf("each upgraded pool's previous KubeadmConfigTemplate is left in place for its old MachineSets; plan lists it as orphaned")
}

// wait polls until stage s's part runs the target, rolled out, with the
// Cluster Available, printing progress (and drains blocked by a
// PodDisruptionBudget) whenever it changes.
func (u *Upgrader) wait(ctx context.Context, s Stage) error {
	ctx, cancel := context.WithTimeout(ctx, u.Timeout)
	defer cancel()
	last := ""
	for {
		line, done, err := u.progress(ctx, s)
		if err != nil {
			line = "cannot read progress: " + err.Error()
		}
		if line != last {
			u.logf("  %s", line)
			last = line
		}
		if err == nil && done {
			return nil
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("%w (the rollout continues: rerun `hydra cluster upgrade --to %s` to keep going)", cluster.ErrTimeout, u.Target)
			}
			return fmt.Errorf("interrupted; the rollout continues in the management cluster: rerun `hydra cluster upgrade --to %s` to keep going", u.Target)
		case <-time.After(u.poll()):
		}
	}
}

// progress is one reading of stage s: a line to show, and whether it is done.
func (u *Upgrader) progress(ctx context.Context, s Stage) (string, bool, error) {
	l, err := ReadLive(ctx, u.M.C, u.Config)
	if err != nil {
		return "", false, err
	}
	sel := client.MatchingLabels{clusterNameLabel: u.name()}
	what, done := "", false
	if s.Kind == ApplyControlPlane || s.Kind == WaitControlPlane {
		sel[controlPlaneLabel] = ""
		what = "control plane"
		done = l.ControlPlane.Version == u.Target && l.ControlPlane.RolledOut
	} else {
		sel[deploymentNameLabel] = u.poolObjName(s.Pool)
		what = "pool " + s.Pool
		for _, p := range l.Pools {
			if p.Name == s.Pool {
				done = p.Version == u.Target && p.RolledOut
			}
		}
	}
	machines := &unstructured.UnstructuredList{}
	machines.SetGroupVersionKind(machineGVK.GroupVersion().WithKind("MachineList"))
	if err := u.M.C.List(ctx, machines, client.InNamespace(u.ns()), sel); err != nil {
		return "", false, fmt.Errorf("listing Machines: %w", err)
	}
	atTarget, total := 0, 0
	var blocked []string
	for i := range machines.Items {
		mc := &machines.Items[i]
		total++
		if v, _, _ := unstructured.NestedString(mc.Object, "spec", "version"); v == u.Target.String() {
			atTarget++
		}
		if mc.GetDeletionTimestamp() != nil {
			if msg := drainMessage(mc); msg != "" {
				blocked = append(blocked, mc.GetName()+": "+msg)
			}
		}
	}
	line := fmt.Sprintf("%s: %d/%d machines at %s · cluster Available=%t", what, atTarget, total, u.Target, l.Available)
	if len(blocked) > 0 {
		line += "\n    draining: " + strings.Join(blocked, "\n    draining: ")
	}
	return line, done && l.Available, nil
}

// drainMessage is what Cluster API says about a deleting Machine's drain:
// its Deleting condition while it drains the Node, which names the pods a
// PodDisruptionBudget keeps from being evicted.
func drainMessage(mc *unstructured.Unstructured) string {
	conds, _, _ := unstructured.NestedSlice(mc.Object, "status", "conditions")
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok || cm["type"] != deletingCond {
			continue
		}
		reason, _ := cm["reason"].(string)
		msg, _ := cm["message"].(string)
		if reason != "DrainingNode" && !strings.Contains(msg, "disruption budget") {
			return ""
		}
		return strings.Join(strings.Fields(msg), " ")
	}
	return ""
}
