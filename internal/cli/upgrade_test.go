package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Petatron/hydra/internal/upgrade"
)

// cluRemote is an upgrade.Remote that records it was made and answers
// nothing; the cluster commands' tests never reach the internet.
type cluRemote struct{}

func (cluRemote) HasPackages(context.Context, upgrade.Version) (bool, error) {
	return false, errors.New("no network in tests")
}

func (cluRemote) AutoscalerTags(context.Context, upgrade.Version) ([]string, error) {
	return nil, errors.New("no network in tests")
}

// cluWithRemote replaces the upgrade preflight's internet access and counts
// how often it is made.
func cluWithRemote(t *testing.T) *int {
	t.Helper()
	made := 0
	orig := newRemote
	newRemote = func() upgrade.Remote { made++; return cluRemote{} }
	t.Cleanup(func() { newRemote = orig })
	return &made
}

func TestCluUpgradeRefusesBadArguments(t *testing.T) {
	made := cluWithRemote(t)
	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no target", nil, "--to is required"},
		{"zero timeout", []string{"--to", "v1.36.0", "--timeout", "0s"}, "--timeout must be positive"},
		{"bad version", []string{"--to", "latest"}, "latest"},
		{"missing file", []string{"--to", "v1.36.0", "-f", "/nonexistent/hydra.yaml"}, "no such file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := run(t, append([]string{"cluster", lcUpgrade}, tc.args...)...)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
		})
	}
	if *made != 0 {
		t.Errorf("the remote was made %d times for refused arguments", *made)
	}
}

func TestCluUpgradeManagementUnreachable(t *testing.T) {
	cluWithRemote(t)
	orig := managementTarget
	t.Cleanup(func() { managementTarget = orig })
	var got string
	managementTarget = func(path, name string) ([]byte, string, error) {
		got = path + "|" + name
		return nil, "", errors.New("no bootstrap cluster")
	}
	_, _, err := run(t, "cluster", lcUpgrade, "-f", "../../examples/hydra-mgmt.yaml", "--to", "v1.36.0", "--management-kubeconfig", "/m")
	if err == nil || !strings.Contains(err.Error(), "no bootstrap cluster") {
		t.Fatalf("got %v", err)
	}
	if got != "/m|hydra-mgmt" {
		t.Errorf("target %q", got)
	}
}

// Against a management cluster without the cluster, every mode refuses; what
// differs is whether the remote checks are wired in and whether it is a dry
// run (which never prints the run's skew-rule banner).
func TestCluUpgradeModes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		flags      []string
		wantRemote int
		wantBanner bool
	}{
		{"run", nil, 1, true},
		{"run without remote checks", []string{"--skip-remote-checks"}, 0, true},
		{"dry run", []string{"--dry-run"}, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			made := cluWithRemote(t)
			withFakeManagement(t)
			args := append([]string{"cluster", lcUpgrade, "-f", "../../examples/hydra-mgmt.yaml", "--to", "v1.36.0"}, tc.flags...)
			_, errOut, err := run(t, args...)
			if err == nil {
				t.Fatal("a cluster that does not exist cannot be upgraded")
			}
			if *made != tc.wantRemote {
				t.Errorf("remote made %d times, want %d", *made, tc.wantRemote)
			}
			if banner := strings.Contains(errOut, "version-skew rules enforced"); banner != tc.wantBanner {
				t.Errorf("banner shown=%v, want %v:\n%s", banner, tc.wantBanner, errOut)
			}
			if !strings.Contains(errOut, "management cluster: test-mgmt") {
				t.Errorf("the target must be shown:\n%s", errOut)
			}
		})
	}
}

func TestCluUpgradeDefaultRemoteIsHTTP(t *testing.T) {
	if _, ok := newRemote().(*upgrade.HTTPRemote); !ok {
		t.Errorf("the default remote is %T, want *upgrade.HTTPRemote", newRemote())
	}
}
