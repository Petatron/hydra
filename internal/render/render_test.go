package render

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/Petatron/hydra/internal/addons"
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

// ciliumProxy is the rendered CNI HelmChartProxy, or nil.
func ciliumProxy(t *testing.T, out []byte) map[string]any {
	t.Helper()
	var found map[string]any
	for d := range strings.SplitSeq(string(out), "\n---\n") {
		var o map[string]any
		if err := yaml.Unmarshal([]byte(d), &o); err != nil || o["kind"] != KindHelmChartProxy {
			continue
		}
		md, _ := o["metadata"].(map[string]any)
		labels, _ := md["labels"].(map[string]any)
		if labels[AddonLabelKey] == addons.Cilium {
			if found != nil {
				t.Fatal("two Cilium HelmChartProxies")
			}
			found = o
		}
	}
	return found
}

// proxyValues is a HelmChartProxy's valuesTemplate, parsed.
func proxyValues(t *testing.T, hcp map[string]any) map[string]any {
	t.Helper()
	spec, _ := hcp["spec"].(map[string]any)
	v, _ := spec["valuesTemplate"].(string)
	var out map[string]any
	if err := yaml.Unmarshal([]byte(v), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// cni: hydra renders one HelmChartProxy that selects the Cluster by its CNI
// label, takes over from the first render, and carries the canonical values
// with exactly the four per-cluster values changed.
func TestCiliumCarriesClusterValues(t *testing.T) {
	c := load(t, "../../examples/hydra-mgmt.yaml")
	c.Spec.Network.PodCIDR = "10.50.0.0/16"
	c.Spec.Network.MTU = 1450
	out, err := Render(c, Options{})
	if err != nil {
		t.Fatal(err)
	}
	hcp := ciliumProxy(t, out)
	if hcp == nil {
		t.Fatal("no Cilium HelmChartProxy")
	}
	md, _ := hcp["metadata"].(map[string]any)
	spec, _ := hcp["spec"].(map[string]any)
	labels, _ := md["labels"].(map[string]any)
	sel, _ := spec["clusterSelector"].(map[string]any)
	ml, _ := sel["matchLabels"].(map[string]any)
	opts, _ := spec["options"].(map[string]any)
	if md["name"] != "hydra-mgmt-cilium" || labels[ClusterLabelKey] != "hydra-mgmt" {
		t.Errorf("metadata: %v", md)
	}
	if len(ml) != 1 || ml[CNILabelKey] != "hydra-mgmt" {
		t.Errorf("selector must be the CNI label alone: %v", sel)
	}
	if spec["version"] != "1.19.1" || spec["chartName"] != "cilium" || spec["releaseName"] != "cilium" ||
		spec["namespace"] != "kube-system" || spec["reconcileStrategy"] != "Continuous" || spec["repoURL"] != "https://helm.cilium.io" {
		t.Errorf("spec: %v", spec)
	}
	if opts["takeOwnership"] != true {
		t.Errorf("options: %v", opts)
	}

	got := proxyValues(t, hcp)
	var want map[string]any
	if err := yaml.Unmarshal(addons.CiliumValues, &want); err != nil {
		t.Fatal(err)
	}
	want["cluster"].(map[string]any)["name"] = "hydra-mgmt"
	want["ipam"].(map[string]any)["operator"].(map[string]any)["clusterPoolIPv4PodCIDRList"] = []any{"10.50.0.0/16"}
	want["MTU"] = float64(1450)
	want["operator"].(map[string]any)["replicas"] = float64(2)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("values differ from the canonical set with the cluster's filled in:\n got %v\nwant %v", got, want)
	}

	cluster := false
	for _, o := range documents(t, out) {
		if o.Kind == "Cluster" {
			cluster = o.Metadata.Labels[CNILabelKey] == "hydra-mgmt"
		}
	}
	if !cluster {
		t.Error("the Cluster is not labelled for its CNI proxy")
	}

	c.Spec.Network.CiliumVersion = "1.19.11"
	out, err = Render(c, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if spec, _ := ciliumProxy(t, out)["spec"].(map[string]any); spec["version"] != "1.19.11" {
		t.Errorf("ciliumVersion not rendered: %v", spec["version"])
	}
}

// A canonical values file whose sentinels moved fails render, rather than
// installing a sentinel.
func TestCiliumValuesNeedTheirSentinels(t *testing.T) {
	c := load(t, "../../examples/hydra-mgmt.yaml")
	orig := addons.CiliumValues
	t.Cleanup(func() { addons.CiliumValues = orig })
	for _, tc := range []struct{ from, to string }{
		{"name: hydra-sentinel-cluster", "name: someone"},
		{"- 10.255.254.0/24", "- 10.1.0.0/16"},
		{"MTU: 65000", "MTU: 1500"},
		{"  replicas: 1", "  replicas: 3"},
	} {
		if !bytes.Contains(orig, []byte(tc.from)) {
			t.Fatalf("canonical values lack %q", tc.from)
		}
		addons.CiliumValues = bytes.Replace(orig, []byte(tc.from), []byte(tc.to), 1)
		if _, err := Render(c, Options{}); err == nil || !strings.Contains(err.Error(), "sentinel") {
			t.Errorf("%s: got %v", tc.to, err)
		}
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

func TestTemplateNamesFollowContent(t *testing.T) {
	// names returns the control plane's and the system pool's template names.
	names := func(mutate func(c *config.ClusterConfig)) (cp, pool []string) {
		c := load(t, "../../examples/workload-managed-network.yaml")
		mutate(c)
		out, err := Render(c, Options{})
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range documents(t, out) {
			switch {
			case strings.HasPrefix(o.Metadata.Name, "demo-cp-") && o.Kind == "HydraMachineTemplate":
				cp = append(cp, o.Metadata.Name)
			case strings.HasPrefix(o.Metadata.Name, "demo-system-"):
				pool = append(pool, o.Kind+"/"+o.Metadata.Name)
			}
		}
		return cp, pool
	}
	cp0, sys0 := names(func(*config.ClusterConfig) {})
	cp1, sys1 := names(func(c *config.ClusterConfig) { c.Spec.ControlPlane.VCPUs++ })
	if len(cp0) != 1 || cp0[0] == cp1[0] {
		t.Errorf("a new control-plane shape must get a new HydraMachineTemplate name: %v %v", cp0, cp1)
	}
	if strings.Join(sys0, ",") != strings.Join(sys1, ",") {
		t.Error("an unrelated change must not rename a pool's templates")
	}
	_, sys2 := names(func(c *config.ClusterConfig) { c.Spec.NodePools[0].Labels["hydra.io/tier"] = "x" })
	if strings.Join(sys0, ",") == strings.Join(sys2, ",") {
		t.Error("a pool label change must rename its KubeadmConfigTemplate so the pool rolls")
	}
}

func TestReferencesResolve(t *testing.T) {
	out, err := Render(load(t, "../../examples/workload-managed-network.yaml"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defined := map[string]bool{}
	for _, o := range documents(t, out) {
		defined[o.Kind+"/"+o.Metadata.Name] = true
	}
	// Every templateRef/configRef/infrastructureRef name must be an object we render.
	s := string(out)
	for _, kind := range []string{"HydraMachineTemplate", "KubeadmConfigTemplate"} {
		marker := "kind: " + kind + "\n"
		for rest := s; ; {
			i := strings.Index(rest, marker)
			if i < 0 {
				break
			}
			rest = rest[i+len(marker):]
			line := strings.TrimSpace(rest[:strings.IndexByte(rest, '\n')])
			if after, ok := strings.CutPrefix(line, "name: "); ok {
				name := strings.Trim(after, `"`)
				if !defined[kind+"/"+name] {
					t.Errorf("reference to %s %q, which is not rendered", kind, name)
				}
			}
		}
	}
}

func TestNodeInterfaceWarningOnlyWhenShared(t *testing.T) {
	wl, err := Render(load(t, "../../examples/workload-managed-network.yaml"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wl), "MUST be the same interface") {
		t.Error("with the VIP on another NIC, the same-interface warning is false")
	}
	if !strings.Contains(string(wl), "--node-ip=%s") || strings.Count(string(wl), "--node-ip=%s") != 3 {
		t.Errorf("on a managed network the control plane and both pools must pin --node-ip, found %d", strings.Count(string(wl), "--node-ip=%s"))
	}
}

func TestWorkersPinNodeIPWithSeveralBridges(t *testing.T) {
	c := load(t, "../../examples/workload-managed-network.yaml")
	c.Spec.Network.ManagedNetwork = nil
	c.Spec.Network.ControlPlaneEndpoint = "10.0.0.10"
	c.Spec.Network.VIPInterface = "enp1s0"
	c.Spec.Network.Bridges = []string{"br0", "br1"}
	out, err := Render(c, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(out), "--node-ip=%s"); n != 3 {
		t.Errorf("two bridges: the control plane and both pools must pin --node-ip, found %d", n)
	}
	c.Spec.Network.Bridges = []string{"br0"}
	out, err = Render(c, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(out), "--node-ip=%s"); n != 1 {
		t.Errorf("one NIC: only the control plane pins --node-ip (the proven pool template), found %d", n)
	}
}

func TestTemplateHashIgnoresComments(t *testing.T) {
	c := load(t, "../../examples/workload-managed-network.yaml")
	pl := pool{Spec: c.Spec.NodePools[0], ObjName: "x"}
	d := &data{ClusterConfig: c, Spec: &c.Spec}
	pl.Root = d
	var a bytes.Buffer
	if err := tmpl.ExecuteTemplate(&a, "kctSpec", pl); err != nil {
		t.Fatal(err)
	}
	commented := strings.Replace(a.String(), "      files:\n", "      # a new comment\n      files:\n", 1)
	ja, err1 := yaml.YAMLToJSON(a.Bytes())
	jb, err2 := yaml.YAMLToJSON([]byte(commented))
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if contentHash(string(ja)) != contentHash(string(jb)) {
		t.Error("a comment changed the template hash")
	}
}

func TestEveryObjectCarriesTheClusterLabel(t *testing.T) {
	for _, name := range examples {
		c := load(t, "../../examples/"+name+".yaml")
		out, err := Render(c, Options{})
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range documents(t, out) {
			if o.Metadata.Labels[ClusterLabelKey] != c.Metadata.Name {
				t.Errorf("%s: %s %s lacks %s=%s", name, o.Kind, o.Metadata.Name, ClusterLabelKey, c.Metadata.Name)
			}
		}
	}
}

// cni: external renders exactly the default output minus hydra's CNI: no
// Cilium ConfigMap, no ClusterResourceSet, and no Cluster label for one to
// select. Anything else differing would mean the option changes more than
// who installs the CNI.
func TestExternalCNIRendersNoCNI(t *testing.T) {
	for _, name := range examples {
		c := load(t, "../../examples/"+name+".yaml")
		withCNI, err := Render(c, Options{})
		if err != nil {
			t.Fatal(err)
		}
		c.Spec.Network.CNI, c.Spec.Network.MTU = config.CNIExternal, 0
		if HydraCNI(c) {
			t.Fatal("HydraCNI is true for cni: external")
		}
		external, err := Render(c, Options{})
		if err != nil {
			t.Fatal(err)
		}
		cut := strings.Index(string(withCNI), "---\n# The CNI: Cilium ")
		if cut < 0 {
			t.Fatalf("%s: default render has no Cilium section", name)
		}
		// The Cilium section runs to the first addon, if any, or the end.
		end := len(withCNI)
		if i := strings.Index(string(withCNI[cut:]), "---\n# Addon "); i >= 0 {
			end = cut + i
		}
		labelLines := "\n    # Selects the HelmChartProxy that installs Cilium. Never remove it from a\n    # running cluster: CAAPH would uninstall Cilium.\n    " + CNILabelKey + ": " + strconv.Quote(c.Metadata.Name)
		want := strings.Replace(string(withCNI[:cut]), labelLines, "", 1) + string(withCNI[end:])
		if want == string(withCNI[:cut])+string(withCNI[end:]) {
			t.Fatalf("%s: default render has no CNI label on the Cluster", name)
		}
		if string(external) != want {
			t.Errorf("%s: cni: external changes more than the CNI objects and label", name)
		}
		for _, o := range documents(t, external) {
			if o.Kind == "ClusterResourceSet" || o.Metadata.Labels[CNILabelKey] != "" || o.Metadata.Labels[AddonLabelKey] == addons.Cilium {
				t.Errorf("%s: %s %s is hydra's CNI", name, o.Kind, o.Metadata.Name)
			}
		}
	}
}

// The Cilium operator runs 2 replicas when the cluster always has at least
// two nodes, and 1 when it can shrink to one.
func TestOperatorReplicasFollowMinimumNodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(c *config.ClusterConfig)
		want int
	}{
		{"3 control-plane nodes", func(*config.ClusterConfig) {}, 2},
		{"1 control-plane node, no pools", func(c *config.ClusterConfig) { c.Spec.ControlPlane.Replicas = 1 }, 1},
		{"1 control-plane node, a pool that scales to 0", func(c *config.ClusterConfig) {
			c.Spec.ControlPlane.Replicas = 1
			c.Spec.NodePools = []config.NodePoolSpec{{Name: "p", Autoscaling: &config.AutoscalingSpec{Min: 0, Max: 3}}}
		}, 1},
		{"1 control-plane node, a fixed pool", func(c *config.ClusterConfig) {
			c.Spec.ControlPlane.Replicas = 1
			c.Spec.NodePools = []config.NodePoolSpec{{Name: "p", Replicas: 1}}
		}, 2},
	} {
		c := load(t, "../../examples/hydra-mgmt.yaml")
		tc.edit(c)
		if got := OperatorReplicas(c); got != tc.want {
			t.Errorf("%s: OperatorReplicas = %d, want %d", tc.name, got, tc.want)
		}
		v, err := CiliumValues(c)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := yaml.Unmarshal([]byte(v), &m); err != nil {
			t.Fatal(err)
		}
		if got := m["operator"].(map[string]any)["replicas"]; got != float64(tc.want) {
			t.Errorf("%s: operator.replicas %v, want %d", tc.name, got, tc.want)
		}
	}
}

// Each spec.addons entry becomes one HelmChartProxy selecting this cluster by
// its label, with hydra-filled values that CAAPH's templating cannot touch.
func TestAddonsRenderHelmChartProxies(t *testing.T) {
	c := load(t, "../../examples/hydra-mgmt.yaml")
	c.Spec.Addons = []config.AddonSpec{{Name: "metrics-server", Version: "3.13.1", Params: map[string]any{"kubeletInsecureTLS": false}}}
	out, err := Render(c, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var hcps []map[string]any
	for d := range strings.SplitSeq(string(out), "\n---\n") {
		var o map[string]any
		if err := yaml.Unmarshal([]byte(d), &o); err == nil && o["kind"] == KindHelmChartProxy {
			if md, _ := o["metadata"].(map[string]any); md["name"] != "hydra-mgmt-cilium" {
				hcps = append(hcps, o)
			}
		}
	}
	if len(hcps) != 1 {
		t.Fatalf("want one addon HelmChartProxy besides the CNI's, got %d", len(hcps))
	}
	h := hcps[0]
	md, _ := h["metadata"].(map[string]any)
	spec, _ := h["spec"].(map[string]any)
	labels, _ := md["labels"].(map[string]any)
	sel, _ := spec["clusterSelector"].(map[string]any)
	ml, _ := sel["matchLabels"].(map[string]any)
	if md["name"] != "hydra-mgmt-metrics-server" || labels[ClusterLabelKey] != "hydra-mgmt" || labels[AddonLabelKey] != "metrics-server" {
		t.Errorf("metadata: %v", md)
	}
	if ml[ClusterLabelKey] != "hydra-mgmt" || len(ml) != 1 {
		t.Errorf("selector: %v", sel)
	}
	if spec["version"] != "3.13.1" || spec["reconcileStrategy"] != "Continuous" || spec["namespace"] != "kube-system" {
		t.Errorf("spec: %v", spec)
	}
	if v, _ := spec["valuesTemplate"].(string); strings.Contains(v, "kubelet-insecure-tls") || strings.Contains(v, "{{") {
		t.Errorf("values: %q", v)
	}
}

// With no addons and cni: external, no HelmChartProxy is rendered.
func TestNoAddonsNoHelmChartProxy(t *testing.T) {
	c := load(t, "../../examples/hydra-mgmt.yaml")
	c.Spec.Network.CNI, c.Spec.Network.MTU, c.Spec.Network.CiliumVersion = config.CNIExternal, 0, ""
	out, err := Render(c, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), KindHelmChartProxy) {
		t.Error("a HelmChartProxy rendered without spec.addons or hydra's CNI")
	}

}
