package render

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/Petatron/hydra/internal/config"
)

// preDrainScript renders the example management cluster and returns the
// control plane's pre-drain script.
func preDrainScript(t *testing.T) string {
	t.Helper()
	c, err := config.Load("../../examples/hydra-mgmt.yaml")
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(c, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for doc := range bytes.SplitSeq(out, []byte("\n---\n")) {
		var obj struct {
			Kind string `json:"kind"`
			Spec struct {
				KubeadmConfigSpec struct {
					Files []struct {
						Path    string `json:"path"`
						Content string `json:"content"`
					} `json:"files"`
				} `json:"kubeadmConfigSpec"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(doc, &obj); err != nil || obj.Kind != "KubeadmControlPlane" {
			continue
		}
		for _, f := range obj.Spec.KubeadmConfigSpec.Files {
			if f.Path == preDrainHookPath {
				return f.Content
			}
		}
	}
	t.Fatal("no pre-drain script rendered")
	return ""
}

// guest is a fake control-plane node for the script: kubectl, ip and
// hostname are shell stubs whose answers come from files in dir.
type guest struct {
	t   *testing.T
	dir string
}

const (
	vip      = "192.168.15.11"
	thisNode = "cp-a"
)

func newGuest(t *testing.T) *guest {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	g := &guest{t: t, dir: t.TempDir()}
	bin := filepath.Join(g.dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d := g.dir
	stub("hostname", "echo "+thisNode+"\n")
	stub("sleep", ":\n")
	// kubectl: "get nodes" prints nodes.out or fails when nodes.fail exists;
	// "get lease" prints the first line of lease (then drops it, so a
	// sequence of holders can be scripted).
	stub("kubectl", `case "$*" in
*"get nodes"*) [ -f `+d+`/nodes.fail ] && exit 1; cat `+d+`/nodes.out ;;
*"get lease"*) head -1 `+d+`/lease; tail -n +2 `+d+`/lease > `+d+`/lease.new; [ -s `+d+`/lease.new ] && mv `+d+`/lease.new `+d+`/lease; true ;;
esac
`)
	// ip: "addr show" prints the VIP line while vip.on exists; "addr del"
	// removes it unless del.fail exists.
	stub("ip", `case "$*" in
*"addr show"*) [ -f `+d+`/vip.on ] && echo "2: enp1s0    inet `+vip+`/32 scope global enp1s0" ; true ;;
*"addr del"*) [ -f `+d+`/del.fail ] && exit 2; rm -f `+d+`/vip.on ;;
esac
`)
	g.write("manifest", "    env:\n    - name: address\n      value: \""+vip+"\"\n")
	return g
}

func (g *guest) write(name, content string) {
	g.t.Helper()
	if err := os.WriteFile(filepath.Join(g.dir, name), []byte(content), 0o644); err != nil {
		g.t.Fatal(err)
	}
}

func (g *guest) exists(name string) bool {
	_, err := os.Stat(filepath.Join(g.dir, name))
	return err == nil
}

// run runs the script and returns its exit code and output.
func (g *guest) run(script string) (int, string) {
	g.t.Helper()
	path := filepath.Join(g.dir, "hook")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		g.t.Fatal(err)
	}
	cmd := exec.Command("sh", path)
	cmd.Env = append(os.Environ(),
		"PATH="+filepath.Join(g.dir, "bin")+":"+os.Getenv("PATH"),
		"MANIFEST="+filepath.Join(g.dir, "manifest"),
		"PARKED="+filepath.Join(g.dir, "parked"),
		"HANDOVER=1",
	)
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	}
	if err != nil {
		g.t.Fatal(err)
	}
	return 0, string(out)
}

func TestPreDrainScript(t *testing.T) {
	script := preDrainScript(t)
	readyPeer := thisNode + "=True\ncp-b=True\n"

	t.Run("hands the VIP over and drops it", func(t *testing.T) {
		g := newGuest(t)
		g.write("vip.on", "")
		g.write("nodes.out", readyPeer)
		g.write("lease", thisNode+"\n"+thisNode+"\ncp-b\n")
		code, out := g.run(script)
		if code != 0 || !strings.Contains(out, "handed "+vip+" to cp-b") {
			t.Fatalf("exit %d: %s", code, out)
		}
		if g.exists("vip.on") || g.exists("manifest") || !g.exists("parked") {
			t.Fatal("want the VIP dropped and kube-vip parked")
		}
	})

	t.Run("does nothing on a node without the VIP", func(t *testing.T) {
		g := newGuest(t)
		g.write("nodes.out", readyPeer)
		code, out := g.run(script)
		if code != 0 || !strings.Contains(out, "does not hold") || !g.exists("manifest") {
			t.Fatalf("exit %d: %s", code, out)
		}
	})

	t.Run("keeps the VIP when no other control-plane node is Ready", func(t *testing.T) {
		g := newGuest(t)
		g.write("vip.on", "")
		g.write("nodes.out", thisNode+"=True\ncp-b=False\n")
		code, out := g.run(script)
		if code != 0 || !g.exists("manifest") || !g.exists("vip.on") {
			t.Fatalf("exit %d, kube-vip must stay: %s", code, out)
		}
	})

	t.Run("a node list that keeps failing is a failure, never nobody to hand over to", func(t *testing.T) {
		g := newGuest(t)
		g.write("vip.on", "")
		g.write("nodes.fail", "")
		code, out := g.run(script)
		if code == 0 || !strings.Contains(out, "cannot list") || !g.exists("manifest") || !g.exists("vip.on") {
			t.Fatalf("exit %d, kube-vip and the VIP must be untouched: %s", code, out)
		}
	})

	t.Run("a lease nobody takes restores kube-vip", func(t *testing.T) {
		g := newGuest(t)
		g.write("vip.on", "")
		g.write("nodes.out", readyPeer)
		g.write("lease", thisNode+"\n")
		code, out := g.run(script)
		if code == 0 || !strings.Contains(out, "kube-vip restored") {
			t.Fatalf("exit %d: %s", code, out)
		}
		if !g.exists("manifest") || g.exists("parked") || !g.exists("vip.on") {
			t.Fatal("kube-vip must be back in place, still holding the VIP")
		}
	})

	t.Run("an address that cannot be removed is a failure", func(t *testing.T) {
		g := newGuest(t)
		g.write("vip.on", "")
		g.write("del.fail", "")
		g.write("nodes.out", readyPeer)
		g.write("lease", "cp-b\n")
		code, out := g.run(script)
		if code == 0 || !strings.Contains(out, "could not be removed") {
			t.Fatalf("exit %d: %s", code, out)
		}
	})

	t.Run("a rerun after a handover does nothing", func(t *testing.T) {
		g := newGuest(t)
		g.write("vip.on", "")
		g.write("nodes.out", readyPeer)
		g.write("lease", "cp-b\n")
		if code, out := g.run(script); code != 0 {
			t.Fatalf("first run: %d %s", code, out)
		}
		code, out := g.run(script)
		if code != 0 || !strings.Contains(out, "does not hold") {
			t.Fatalf("rerun: %d %s", code, out)
		}
	})
}
