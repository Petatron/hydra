// Package upgrade drives a cluster's Kubernetes upgrade in the order the
// version-skew policy requires: the control plane first, then each worker
// pool, one minor version at a time.
//
// It keeps nothing (ADR-001): every run reads the KubeadmControlPlane's and
// MachineDeployments' versions and rollout state, decides the next stage from
// them alone, and so continues an interrupted upgrade wherever it stopped.
package upgrade

import (
	"fmt"
	"regexp"
	"strconv"
)

// Version is a parsed Kubernetes version, v1.<Minor>.<Patch>.
type Version struct {
	Minor, Patch int
}

var versionRE = regexp.MustCompile(`^v1\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// ParseVersion parses "v1.35.3".
func ParseVersion(s string) (Version, error) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("%q is not a Kubernetes version like v1.36.2", s)
	}
	minor, _ := strconv.Atoi(m[1])
	patch, _ := strconv.Atoi(m[2])
	return Version{Minor: minor, Patch: patch}, nil
}

func (v Version) String() string { return fmt.Sprintf("v1.%d.%d", v.Minor, v.Patch) }

// MinorString is "v1.36".
func (v Version) MinorString() string { return fmt.Sprintf("v1.%d", v.Minor) }

// Less orders versions.
func (v Version) Less(o Version) bool {
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

// Part is a control plane or one worker pool as the live objects show it.
type Part struct {
	// Name is the pool's name in hydra.yaml; empty for the control plane.
	Name string
	// Version is the version its spec asks for (KubeadmControlPlane
	// spec.version, MachineDeployment spec.template.spec.version).
	Version Version
	// RolledOut is true when every machine runs Version, the spec is
	// observed, and every desired replica is up to date and Ready.
	RolledOut bool
}

// Live is what the management cluster shows about the cluster.
type Live struct {
	Exists, Terminating bool
	// Available is the Cluster's Available condition holding.
	Available bool
	// Settled is Available with the control plane and every pool rolled out.
	Settled      bool
	ControlPlane Part
	// Pools are in hydra.yaml's order.
	Pools []Part
	// Missing are pools hydra.yaml names that have no MachineDeployment yet.
	Missing []string
	// Strays are MachineDeployments labelled for this cluster that hydra.yaml
	// does not name: an upgrade would leave them behind.
	Strays []string
}

// StageKind is the next thing the upgrade does.
type StageKind int

const (
	// ApplyControlPlane sets the target version on the KubeadmControlPlane.
	ApplyControlPlane StageKind = iota
	// WaitControlPlane waits for a control-plane rollout already under way.
	WaitControlPlane
	// ApplyPool sets the target version on one pool's MachineDeployment.
	ApplyPool
	// WaitPool waits for one pool's rollout already under way.
	WaitPool
	// Done means the control plane and every pool run the target.
	Done
)

// Stage is the decision: what to do next, and to which pool.
type Stage struct {
	Kind StageKind
	// Pool names the pool for ApplyPool and WaitPool.
	Pool string
	// From is the version the part runs now, for messages.
	From Version
}

func (s Stage) String() string {
	switch s.Kind {
	case ApplyControlPlane:
		return fmt.Sprintf("upgrade the control plane from %s", s.From)
	case WaitControlPlane:
		return "wait for the control plane's rollout to finish"
	case ApplyPool:
		return fmt.Sprintf("upgrade pool %s from %s", s.Pool, s.From)
	case WaitPool:
		return fmt.Sprintf("wait for pool %s's rollout to finish", s.Pool)
	}
	return "nothing: the control plane and every pool run the target"
}

// Refusal is a preflight decision not to proceed, with what to do instead.
type Refusal struct {
	Reason, Fix string
}

func (r *Refusal) Error() string {
	if r.Fix == "" {
		return r.Reason
	}
	return r.Reason + "\n  fix: " + r.Fix
}

// Lowest is the lowest version any part runs: the version hydra.yaml says
// before the upgrade finishes, and after it, the target.
func (l Live) Lowest() Version {
	low := l.ControlPlane.Version
	for _, p := range l.Pools {
		if p.Version.Less(low) {
			low = p.Version
		}
	}
	return low
}

// at reports whether the control plane and every pool run v, rolled out.
func (l Live) at(v Version) bool {
	if l.ControlPlane.Version != v || !l.ControlPlane.RolledOut {
		return false
	}
	for _, p := range l.Pools {
		if p.Version != v || !p.RolledOut {
			return false
		}
	}
	return true
}

// checkShape refuses a cluster no upgrade should start from or continue:
// gone, being deleted, pools missing or stray, or versions that a downgrade
// or a skew violation would follow from.
func checkShape(l Live, target Version) error {
	cp := l.ControlPlane
	switch {
	case !l.Exists:
		return &Refusal{Reason: "the cluster does not exist on this management cluster", Fix: "check --management-kubeconfig; create the cluster with `hydra cluster create`"}
	case l.Terminating:
		return &Refusal{Reason: "the cluster is being deleted"}
	case len(l.Missing) > 0:
		fix := "create them first with `hydra cluster create`, then rerun"
		if l.ControlPlane.Version != l.Lowest() {
			fix = "an upgrade is under way: take them out of hydra.yaml, finish the upgrade, then add them back and run `hydra cluster create`"
		}
		return &Refusal{
			Reason: fmt.Sprintf("pools in hydra.yaml have no MachineDeployment yet: %v. The upgrade would report them done without creating them", l.Missing),
			Fix:    fix,
		}
	case len(l.Strays) > 0:
		return &Refusal{
			Reason: fmt.Sprintf("MachineDeployments labelled for this cluster are not pools in hydra.yaml: %v. The upgrade would leave them on the old version", l.Strays),
			Fix:    "add them back to hydra.yaml, or delete them, then rerun",
		}
	case target.Less(cp.Version):
		return &Refusal{
			Reason: fmt.Sprintf("the control plane runs %s, newer than %s: Kubernetes does not support downgrading", cp.Version, target),
			Fix:    "recovering an older version means restoring etcd from a backup taken before the upgrade",
		}
	}
	for _, p := range l.Pools {
		if target.Less(p.Version) {
			fix := fmt.Sprintf("set pool %s back to %s by hand (it was moved outside upgrade), or restore etcd from a backup taken before it moved", p.Name, cp.Version)
			if p.Version.Minor-cp.Version.Minor <= 1 {
				fix = fmt.Sprintf("upgrade to the pool's version instead: `hydra cluster upgrade --to %s`", p.Version)
			}
			return &Refusal{
				Reason: fmt.Sprintf("pool %s runs %s, newer than %s: Kubernetes does not support downgrading", p.Name, p.Version, target),
				Fix:    fix,
			}
		}
		// A pool ahead of the control plane breaks the skew policy. When it
		// is at most one minor ahead and already at the target, bringing the
		// control plane up to it is the repair, so let that through.
		if p.Version.Minor > cp.Version.Minor && (p.Version != target || p.Version.Minor-cp.Version.Minor > 1) {
			fix := fmt.Sprintf("bring the control plane up to it: `hydra cluster upgrade --to %s`", p.Version)
			if p.Version.Minor-cp.Version.Minor > 1 {
				fix = fmt.Sprintf("set pool %s back to %s by hand: it is too far ahead to catch up in one step", p.Name, cp.Version)
			}
			return &Refusal{
				Reason: fmt.Sprintf("pool %s runs %s, newer than the control plane's %s, which the version-skew policy forbids", p.Name, p.Version, cp.Version),
				Fix:    fix,
			}
		}
	}
	return nil
}

// Decide returns the next stage of upgrading to target, or why not. file is
// hydra.yaml's kubernetesVersion. It reads nothing but its arguments.
//
// The control plane goes first, and only from a settled cluster whose pools
// all run what it runs; pools follow in hydra.yaml's order, each only once
// the control plane and the pools before it are rolled out. A rollout already
// under way at the target is waited for, never re-applied, so a rerun after
// an interrupt continues where the last run stopped.
func Decide(l Live, target, file Version) (Stage, error) {
	cp := l.ControlPlane
	if err := checkShape(l, target); err != nil {
		return Stage{}, err
	}
	if l.at(target) {
		if !l.Available {
			return Stage{}, &Refusal{
				Reason: "every part runs " + target.String() + ", but the cluster is not Available: the upgrade is not done until it is",
				Fix:    "check `hydra cluster status`, then rerun to confirm",
			}
		}
		// Whatever hydra.yaml says: the run that finished the last pool
		// started from the old version, and says to set the new one.
		return Stage{Kind: Done}, nil
	}
	if low := l.Lowest(); file != target && file != low {
		return Stage{}, &Refusal{
			Reason: fmt.Sprintf("hydra.yaml says kubernetesVersion %s, but the cluster runs %s and the target is %s", file, low, target),
			Fix:    fmt.Sprintf("set kubernetesVersion to %s (what runs) or %s (the target) in hydra.yaml", low, target),
		}
	}

	if cp.Version.Less(target) {
		if target.Minor-cp.Version.Minor > 1 {
			return Stage{}, &Refusal{
				Reason: fmt.Sprintf("%s is %d minor versions past the control plane's %s: Kubernetes upgrades one minor version at a time", target, target.Minor-cp.Version.Minor, cp.Version),
				Fix:    fmt.Sprintf("upgrade to the latest v1.%d patch first, then to %s", cp.Version.Minor+1, target),
			}
		}
		for _, p := range l.Pools {
			// A pool already at the target is done for this upgrade; any
			// other version means an earlier change has not finished. The
			// fix names a version this function then lets through: the
			// control plane's (the pools catch up), or the pool's when it
			// is ahead (the control plane catches up).
			if p.Version != cp.Version && p.Version != target {
				finish := cp.Version
				if finish.Less(p.Version) {
					finish = p.Version
				}
				return Stage{}, &Refusal{
					Reason: fmt.Sprintf("pool %s runs %s while the control plane runs %s: an earlier upgrade has not finished", p.Name, p.Version, cp.Version),
					Fix:    fmt.Sprintf("bring every part to one version first: `hydra cluster upgrade --to %s`", finish),
				}
			}
		}
		if !l.Settled {
			return Stage{}, &Refusal{
				Reason: "the cluster is not settled (not Available, or a rollout is under way): an upgrade would start on top of it",
				Fix:    "wait for `hydra cluster status` to show it Available with nothing rolling out, then rerun",
			}
		}
		return Stage{Kind: ApplyControlPlane, From: cp.Version}, nil
	}

	// The control plane asks for the target.
	if !cp.RolledOut {
		return Stage{Kind: WaitControlPlane}, nil
	}
	for _, p := range l.Pools {
		switch {
		case p.Version == target && !p.RolledOut:
			return Stage{Kind: WaitPool, Pool: p.Name}, nil
		case p.Version == target:
			continue
		case target.Minor-p.Version.Minor > 1:
			return Stage{}, &Refusal{
				Reason: fmt.Sprintf("pool %s runs %s, %d minor versions behind %s", p.Name, p.Version, target.Minor-p.Version.Minor, target),
				Fix:    fmt.Sprintf("upgrade it by hand to the latest v1.%d patch first", p.Version.Minor+1),
			}
		case !l.Available:
			return Stage{}, &Refusal{
				Reason: "the cluster is not Available: no pool is upgraded until it is",
				Fix:    "check `hydra cluster status`, then rerun",
			}
		}
		return Stage{Kind: ApplyPool, Pool: p.Name, From: p.Version}, nil
	}
	return Stage{Kind: Done}, nil
}
