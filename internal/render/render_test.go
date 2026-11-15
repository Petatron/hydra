package render

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/Petatron/hydra/internal/config"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/")

// examples are rendered against testdata/<name>.golden.yaml. The examples
// directory is the user-facing documentation, so testing it keeps it true.
var examples = []string{"hydra-mgmt", "workload-managed-network"}

func load(t *testing.T, path string) *config.ClusterConfig {
	t.Helper()
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGolden(t *testing.T) {
	for _, name := range examples {
		t.Run(name, func(t *testing.T) {
			got, err := Render(load(t, filepath.Join("..", "..", "examples", name+".yaml")), Options{CLIVersion: "test"})
			if err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join("testdata", name+".golden.yaml")
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run go test ./internal/render -update)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("render output differs from %s; if the change is intended, run go test ./internal/render -update and review the diff", golden)
			}
		})
	}
}

func TestDeterministic(t *testing.T) {
	c := load(t, "../../examples/workload-managed-network.yaml")
	first, err := Render(c, Options{CLIVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		again, err := Render(c, Options{CLIVersion: "test"})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, again) {
			t.Fatal("two renders of the same input differ")
		}
	}
}

type object struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Data map[string]string `json:"data"`
}

func documents(t *testing.T, out []byte) []object {
	t.Helper()
	var objs []object
	for doc := range bytes.SplitSeq(out, []byte("\n---\n")) {
		var o object
		if err := yaml.UnmarshalStrict(doc, &o); err != nil {
			// Strict only to catch syntax errors; unknown fields are expected.
			if err := yaml.Unmarshal(doc, &o); err != nil {
				t.Fatalf("rendered document is not valid YAML: %v\n%s", err, doc)
			}
		}
		if o.Kind == "" {
			continue
		}
		objs = append(objs, o)
	}
	return objs
}

func TestEveryDocumentIsNamespacedAndNamed(t *testing.T) {
	for _, name := range examples {
		c := load(t, "../../examples/"+name+".yaml")
		out, err := Render(c, Options{})
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range documents(t, out) {
			if o.Metadata.Name == "" || o.Metadata.Namespace != c.Metadata.Namespace {
				t.Errorf("%s: %s %q has namespace %q, want %q", name, o.Kind, o.Metadata.Name, o.Metadata.Namespace, c.Metadata.Namespace)
			}
		}
	}
}

func TestNoSecretsOrSentinels(t *testing.T) {
	for _, name := range examples {
		out, err := Render(load(t, "../../examples/"+name+".yaml"), Options{})
		if err != nil {
			t.Fatal(err)
		}
		s := string(out)
		for _, bad := range []string{"kind: Secret", "PRIVATE KEY", "BEGIN CERTIFICATE", sentinelClusterName, sentinelPodCIDR, "65000"} {
			if strings.Contains(s, bad) {
				t.Errorf("%s: output contains %q", name, bad)
			}
		}
	}
}

func TestCiliumCarriesClusterValues(t *testing.T) {
	c := load(t, "../../examples/hydra-mgmt.yaml")
	c.Spec.Network.PodCIDR = "10.50.0.0/16"
	c.Spec.Network.MTU = 1450
	out, err := Render(c, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var cm, crs *object
	var cluster *object
	objs := documents(t, out)
	for i := range objs {
		switch objs[i].Kind {
		case "ConfigMap":
			cm = &objs[i]
		case "ClusterResourceSet":
			crs = &objs[i]
		case "Cluster":
			cluster = &objs[i]
		}
	}
	if cm == nil || crs == nil || cluster == nil {
		t.Fatal("missing ConfigMap, ClusterResourceSet or Cluster")
	}
	manifest := cm.Data["cilium.yaml"]
	for _, want := range []string{`cluster-name: "hydra-mgmt"`, `cluster-pool-ipv4-cidr: "10.50.0.0/16"`, `mtu: "1450"`} {
		if !strings.Contains(manifest, want) {
			t.Errorf("Cilium manifest lacks %s", want)
		}
	}
	if cluster.Metadata.Labels[CNILabelKey] != "hydra-mgmt" {
		t.Errorf("Cluster is not labelled for its ClusterResourceSet: %v", cluster.Metadata.Labels)
	}
	if len(manifest) > 900*1024 {
		t.Errorf("Cilium manifest is %d bytes; a ConfigMap holds at most 1MiB", len(manifest))
	}
}

func TestControlPlaneTaintFollowsNodePools(t *testing.T) {
	mgmt, err := Render(load(t, "../../examples/hydra-mgmt.yaml"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(mgmt), "taints: []"); n != 2 {
		t.Errorf("no node pools: want taints: [] on init and join, found %d", n)
	}
	wl, err := Render(load(t, "../../examples/workload-managed-network.yaml"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wl), "taints: []") {
		t.Error("with node pools the control plane must keep its taint")
	}
}

func TestBootstrapModes(t *testing.T) {
	c := load(t, "../../examples/hydra-mgmt.yaml")
	packages, err := Render(c, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(packages), "apt-get install -y kubelet=1.35.3-*") {
		t.Error("bootstrap: packages should install the pinned Kubernetes packages")
	}
	c.Spec.Image.Bootstrap = config.BootstrapPrebaked
	prebaked, err := Render(c, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"apt-get", "pkgs.k8s.io", "curl "} {
		if strings.Contains(string(prebaked), forbidden) {
			t.Errorf("bootstrap: prebaked must contact no package mirror, found %q", forbidden)
		}
	}
	if !strings.Contains(string(prebaked), "systemctl enable kubelet") {
		t.Error("bootstrap: prebaked still has to enable the kubelet")
	}
}

func TestManagedNetworkLeavesEndpointToHydra(t *testing.T) {
	out, err := Render(load(t, "../../examples/workload-managed-network.yaml"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "controlPlaneEndpoint:\n") {
		t.Error("with a managed network and no endpoint, HydraCluster must leave controlPlaneEndpoint for Hydra to choose")
	}
	if !strings.Contains(s, `value: "enp2s0"`) {
		t.Error("kube-vip should use the managed network's NIC (enp2s0)")
	}
	if !strings.Contains(s, "show enp1s0") {
		t.Error("the node should still register its site-network address (enp1s0)")
	}
}

func TestMHCQuorumBound(t *testing.T) {
	for replicas, want := range map[int32]string{1: "1", 3: "1", 5: "2"} {
		c := load(t, "../../examples/hydra-mgmt.yaml")
		c.Spec.ControlPlane.Replicas = replicas
		out, err := Render(c, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), "unhealthyLessThanOrEqualTo: "+want+"\n") {
			t.Errorf("replicas %d: want unhealthyLessThanOrEqualTo %s", replicas, want)
		}
	}
}
