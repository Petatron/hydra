package upgrade

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func v(s string) Version {
	x, err := ParseVersion(s)
	if err != nil {
		panic(err)
	}
	return x
}

// settled is a cluster at version on control plane and both pools.
func settled(version string) Live {
	return Live{
		Exists: true, Available: true, Settled: true,
		ControlPlane: Part{Version: v(version), RolledOut: true},
		Pools: []Part{
			{Name: "a", Version: v(version), RolledOut: true},
			{Name: "b", Version: v(version), RolledOut: true},
		},
	}
}

func TestParseVersion(t *testing.T) {
	if got := v("v1.36.6"); got != (Version{Minor: 36, Patch: 6}) || got.String() != "v1.36.6" || got.MinorString() != "v1.36" {
		t.Errorf("got %+v", got)
	}
	for _, bad := range []string{"1.36.6", "v1.36", "v2.0.0", "v1.036.1", ""} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("%q must not parse", bad)
		}
	}
	if !v("v1.35.9").Less(v("v1.36.0")) || v("v1.36.1").Less(v("v1.36.0")) || v("v1.36.1").Less(v("v1.36.1")) {
		t.Error("Less orders by minor, then patch")
	}
}

// The decision table: each row is a live state, the target and hydra.yaml's
// version, and the stage that must follow (or the refusal's key words).
func TestDecide(t *testing.T) {
	cpRolling := settled("v1.35.3")
	cpRolling.ControlPlane = Part{Version: v("v1.36.6"), RolledOut: false}
	cpRolling.Settled = false

	cpDone := settled("v1.35.3")
	cpDone.ControlPlane = Part{Version: v("v1.36.6"), RolledOut: true}

	poolARolling := cpDone
	poolARolling.Pools = []Part{{Name: "a", Version: v("v1.36.6"), RolledOut: false}, {Name: "b", Version: v("v1.35.3"), RolledOut: true}}

	poolADone := cpDone
	poolADone.Pools = []Part{{Name: "a", Version: v("v1.36.6"), RolledOut: true}, {Name: "b", Version: v("v1.35.3"), RolledOut: true}}

	unavailableMidway := poolADone
	unavailableMidway.Available = false
	unavailableMidway.Settled = false

	notSettled := settled("v1.35.3")
	notSettled.Settled = false

	poolBehind := settled("v1.35.3")
	poolBehind.Pools[1].Version = v("v1.34.2")

	poolAhead := settled("v1.35.3")
	poolAhead.Pools[0].Version = v("v1.36.6")

	poolTwoBehind := cpDone
	poolTwoBehind.Pools = []Part{{Name: "a", Version: v("v1.34.1"), RolledOut: true}}

	strays := settled("v1.35.3")
	strays.Strays = []string{"c-old"}

	gone := Live{}
	deleting := settled("v1.35.3")
	deleting.Terminating = true

	for _, tc := range []struct {
		name         string
		live         Live
		target, file string
		want         Stage
		refuse       string
	}{
		{name: "settled, one minor up: control plane first", live: settled("v1.35.3"), target: "v1.36.6", file: "v1.35.3", want: Stage{Kind: ApplyControlPlane, From: v("v1.35.3")}},
		{name: "hydra.yaml may already say the target", live: settled("v1.35.3"), target: "v1.36.6", file: "v1.36.6", want: Stage{Kind: ApplyControlPlane, From: v("v1.35.3")}},
		{name: "a patch upgrade is allowed", live: settled("v1.35.3"), target: "v1.35.9", file: "v1.35.3", want: Stage{Kind: ApplyControlPlane, From: v("v1.35.3")}},
		{name: "control plane rolling: wait, never re-apply", live: cpRolling, target: "v1.36.6", file: "v1.35.3", want: Stage{Kind: WaitControlPlane}},
		{name: "control plane done: first pool", live: cpDone, target: "v1.36.6", file: "v1.35.3", want: Stage{Kind: ApplyPool, Pool: "a", From: v("v1.35.3")}},
		{name: "first pool rolling: wait for it, not the next", live: poolARolling, target: "v1.36.6", file: "v1.35.3", want: Stage{Kind: WaitPool, Pool: "a"}},
		{name: "first pool done: the next", live: poolADone, target: "v1.36.6", file: "v1.36.6", want: Stage{Kind: ApplyPool, Pool: "b", From: v("v1.35.3")}},
		{name: "everything at the target", live: settled("v1.36.6"), target: "v1.36.6", file: "v1.36.6", want: Stage{Kind: Done}},
		{name: "everything at the target, hydra.yaml not yet updated", live: settled("v1.36.6"), target: "v1.36.6", file: "v1.35.3", want: Stage{Kind: Done}},

		{name: "two minors", live: settled("v1.35.3"), target: "v1.37.0", file: "v1.35.3", refuse: "one minor version at a time"},
		{name: "downgrade a minor", live: settled("v1.36.6"), target: "v1.35.3", file: "v1.36.6", refuse: "does not support downgrading"},
		{name: "downgrade a patch", live: settled("v1.36.6"), target: "v1.36.2", file: "v1.36.6", refuse: "does not support downgrading"},
		{name: "not settled", live: notSettled, target: "v1.36.6", file: "v1.35.3", refuse: "not settled"},
		{name: "a pool left behind by an earlier upgrade", live: poolBehind, target: "v1.36.6", file: "v1.34.2", refuse: "earlier upgrade has not finished"},
		{name: "a pool already at the target, one minor ahead: the control plane catches up", live: poolAhead, target: "v1.36.6", file: "v1.35.3", want: Stage{Kind: ApplyControlPlane, From: v("v1.35.3")}},
		{name: "a pool one minor ahead, not at the target", live: poolAhead, target: "v1.36.9", file: "v1.35.3", refuse: "--to v1.36.6"},
		{name: "a pool two minors ahead", live: func() Live { l := settled("v1.35.3"); l.Pools[0].Version = v("v1.37.0"); return l }(), target: "v1.37.0", file: "v1.35.3", refuse: "back to v1.35.3 by hand"},
		{name: "a pool newer than the target", live: func() Live { l := settled("v1.35.3"); l.Pools[0].Version = v("v1.36.6"); return l }(), target: "v1.36.2", file: "v1.35.3", refuse: "--to v1.36.6"},
		{name: "a pool two minors behind once the control plane is done", live: poolTwoBehind, target: "v1.36.6", file: "v1.34.1", refuse: "2 minor versions behind"},
		{name: "unavailable between pools", live: unavailableMidway, target: "v1.36.6", file: "v1.35.3", refuse: "not Available"},
		{name: "hydra.yaml says something else", live: settled("v1.35.3"), target: "v1.36.6", file: "v1.34.0", refuse: "hydra.yaml says"},
		{name: "a pool a patch ahead is no skew violation", live: func() Live { l := settled("v1.35.2"); l.Pools[0].Version = v("v1.35.3"); return l }(), target: "v1.36.6", file: "v1.35.2", refuse: "--to v1.35.3"},
		{name: "stray MachineDeployments", live: strays, target: "v1.36.6", file: "v1.35.3", refuse: "not pools in hydra.yaml"},
		{name: "no cluster", live: gone, target: "v1.36.6", file: "v1.35.3", refuse: "does not exist"},
		{name: "being deleted", live: deleting, target: "v1.36.6", file: "v1.35.3", refuse: "being deleted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Decide(tc.live, v(tc.target), v(tc.file))
			if tc.refuse != "" {
				var r *Refusal
				if !errors.As(err, &r) || !strings.Contains(err.Error(), tc.refuse) {
					t.Fatalf("want a refusal containing %q, got %v (stage %v)", tc.refuse, err, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Every refusal the ticket names carries a fix line.
func TestRefusalsCarryAFix(t *testing.T) {
	poolAhead := settled("v1.35.3")
	poolAhead.Pools[0].Version = v("v1.36.6")
	for _, tc := range []struct {
		live         Live
		target, file string
	}{
		{settled("v1.35.3"), "v1.37.0", "v1.35.3"},
		{settled("v1.36.6"), "v1.35.3", "v1.36.6"},
		{poolAhead, "v1.36.9", "v1.35.3"},
		{poolAhead, "v1.36.2", "v1.35.3"},
	} {
		_, err := Decide(tc.live, v(tc.target), v(tc.file))
		if err == nil || !strings.Contains(err.Error(), "fix: ") {
			t.Errorf("%s -> %s: want a fix line, got %v", tc.live.ControlPlane.Version, tc.target, err)
		}
	}
}

var toFlag = regexp.MustCompile("upgrade --to (v1\\.[0-9]+\\.[0-9]+)`")

// A refusal that suggests `hydra cluster upgrade --to X` must be answered by
// that command: Decide with X proceeds, never refusing again the same way.
func TestSuggestedUpgradeCommandsProceed(t *testing.T) {
	patchAhead := settled("v1.35.2")
	patchAhead.Pools[0].Version = v("v1.35.3")
	poolBehind := settled("v1.35.3")
	poolBehind.Pools[1].Version = v("v1.34.2")
	minorAhead := settled("v1.35.3")
	minorAhead.Pools[0].Version = v("v1.36.6")
	for _, tc := range []struct {
		name         string
		live         Live
		target, file string
	}{
		{"a pool a patch ahead", patchAhead, "v1.36.6", "v1.35.2"},
		{"a pool behind", poolBehind, "v1.36.6", "v1.34.2"},
		{"a pool a minor ahead, other target", minorAhead, "v1.36.9", "v1.35.3"},
		{"a pool newer than the target", minorAhead, "v1.36.2", "v1.35.3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decide(tc.live, v(tc.target), v(tc.file))
			m := toFlag.FindStringSubmatch(fmt.Sprint(err))
			if m == nil {
				t.Fatalf("want a refusal suggesting upgrade --to, got %v", err)
			}
			got, err := Decide(tc.live, v(m[1]), v(tc.file))
			if err != nil {
				t.Fatalf("the suggested --to %s is refused too: %v", m[1], err)
			}
			if got.Kind == Done {
				t.Fatalf("the suggested --to %s does nothing", m[1])
			}
		})
	}
}
