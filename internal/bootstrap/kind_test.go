package bootstrap

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// fakeDocker is a stand-in `docker` CLI on PATH, which is all kind's docker
// provider talks to. It knows the kind clusters named in $FAKE_KIND_CLUSTERS
// (one control-plane node each), logs every invocation to $FAKE_DOCKER_LOG,
// and fails anything it does not recognise, so no test can create a cluster.
const fakeDocker = `#!/bin/sh
echo "$*" >> "$FAKE_DOCKER_LOG"
case "$1" in
-v) echo "Docker version 99.0.0, build fake"; exit 0 ;;
ps)
  filter=""; prev=""
  for a in "$@"; do
    [ "$prev" = "--filter" ] && filter="$a"
    prev="$a"
  done
  case "$filter" in
  label=io.x-k8s.kind.cluster) for c in $FAKE_KIND_CLUSTERS; do echo "$c"; done ;;
  label=io.x-k8s.kind.cluster=*)
    want="${filter#label=io.x-k8s.kind.cluster=}"
    for c in $FAKE_KIND_CLUSTERS; do [ "$c" = "$want" ] && echo "$c-control-plane"; done ;;
  esac
  exit 0 ;;
rm) exit 0 ;;
esac
exit 1
`

// withFakeDocker puts only the fake docker on PATH and returns its log path.
func withFakeDocker(t *testing.T, clusters ...string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake docker is a shell script")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "docker.log")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")
	t.Setenv("FAKE_DOCKER_LOG", log)
	t.Setenv("FAKE_KIND_CLUSTERS", strings.Join(clusters, " "))
	return log
}

func dockerCalls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestNewKindProviderNeedsAContainerRuntime(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no docker, podman or nerdctl
	if _, err := NewKindProvider(); err == nil {
		t.Error("want an error when no container runtime is available")
	}
}

func TestKindExists(t *testing.T) {
	withFakeDocker(t, "hydra-bootstrap-lab", "other")
	k, err := NewKindProvider()
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"hydra-bootstrap-lab": true, "other": true, "hydra-bootstrap-gone": false} {
		got, err := k.Exists(name)
		if err != nil || got != want {
			t.Errorf("Exists(%s) = %v %v, want %v", name, got, err, want)
		}
	}
}

// Delete removes the cluster's nodes and leaves the user's own kubeconfig
// untouched, even though kind would otherwise edit it.
func TestKindDeleteNeverTouchesUserKubeconfig(t *testing.T) {
	log := withFakeDocker(t, "hydra-bootstrap-lab")
	userKC := filepath.Join(t.TempDir(), "config")
	const content = "apiVersion: v1\nkind: Config\ncurrent-context: kind-hydra-bootstrap-lab\n"
	if err := os.WriteFile(userKC, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", userKC)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	k, err := NewKindProvider()
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Delete("hydra-bootstrap-lab"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(dockerCalls(t, log), "rm -f -v hydra-bootstrap-lab-control-plane") {
		t.Errorf("node not removed; docker calls:\n%s", strings.Join(dockerCalls(t, log), "\n"))
	}
	if b, err := os.ReadFile(userKC); err != nil || string(b) != content {
		t.Errorf("the user's kubeconfig changed: %q %v", b, err)
	}
	assertEmptyDir(t, tmp) // the scratch kubeconfig is removed
}

func TestKindListFailureIsAnError(t *testing.T) {
	withFakeDocker(t)
	k, err := NewKindProvider()
	if err != nil {
		t.Fatal(err)
	}
	// The runtime vanishing after detection: listing must fail, not report
	// "no such cluster" (which would make bootstrap create a second one).
	t.Setenv("PATH", t.TempDir())
	if _, err := k.Exists("hydra-bootstrap-lab"); err == nil {
		t.Error("a failed list must be an error")
	}
}

func TestWithScratchKubeconfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	k := &KindProvider{}
	var seen string
	err := k.withScratchKubeconfig(func(path string) error {
		seen = path
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the scratch kubeconfig must exist while in use: %v", err)
		}
		return os.ErrClosed
	})
	if err != os.ErrClosed {
		t.Errorf("fn's error must be returned, got %v", err)
	}
	if seen == "" || filepath.Dir(seen) != tmp {
		t.Errorf("scratch path %q not in %s", seen, tmp)
	}
	assertEmptyDir(t, tmp)

	t.Setenv("TMPDIR", filepath.Join(tmp, "missing"))
	if err := k.withScratchKubeconfig(func(string) error { t.Error("fn must not run"); return nil }); err == nil {
		t.Error("an unwritable temp dir must be an error")
	}
}
