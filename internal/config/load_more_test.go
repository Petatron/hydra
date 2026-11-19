package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hydra.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	c, err := Load(writeFile(t, valid))
	if err != nil || c.Metadata.Name != "c1" || c.Spec.Network.MTU != DefaultMTU {
		t.Fatalf("got %+v, %v", c, err)
	}
	if _, err := Load(writeFile(t, strings.Replace(valid, "name: c1", "name: C1", 1))); err == nil {
		t.Error("Load must validate")
	}
	missing := filepath.Join(t.TempDir(), "absent.yaml")
	if _, err := Load(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("got %v", err)
	}
}

func TestLoadMetadata(t *testing.T) {
	m, err := LoadMetadata(writeFile(t, valid))
	if err != nil || m.Name != "c1" || m.Namespace != DefaultNamespace {
		t.Fatalf("the namespace defaults: got %+v, %v", m, err)
	}
	m, err = LoadMetadata(writeFile(t, strings.Replace(valid, "name: c1", "name: c1\n  namespace: team-a", 1)))
	if err != nil || m.Namespace != "team-a" {
		t.Fatalf("got %+v, %v", m, err)
	}
}

func TestLoadHeadFailures(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yaml")
	if _, err := LoadName(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("LoadName: got %v", err)
	}
	if _, err := LoadMetadata(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("LoadMetadata: got %v", err)
	}
	garbled := writeFile(t, "metadata: [unclosed\n")
	for name, load := range map[string]func(string) error{
		"LoadName":     func(p string) error { _, err := LoadName(p); return err },
		"LoadMetadata": func(p string) error { _, err := LoadMetadata(p); return err },
	} {
		if err := load(garbled); err == nil || !strings.Contains(err.Error(), "decoding hydra.yaml") {
			t.Errorf("%s: got %v", name, err)
		}
	}
	// A name too long for Cilium is refused even when only the name is read.
	long := writeFile(t, strings.Replace(valid, "name: c1", "name: "+strings.Repeat("a", 33), 1))
	if _, err := LoadName(long); err == nil || !strings.Contains(err.Error(), "at most 32") {
		t.Errorf("got %v", err)
	}
}

// More single-field refusals, in TestRejects' style: each must fail naming
// the field.
func TestRejectsMore(t *testing.T) {
	managed := func(subnet, start, end string) string {
		return "    managedNetwork:\n      name: n\n      subnet: " + subnet + "\n      dhcpStart: " + start + "\n      dhcpEnd: " + end + "\n"
	}
	// pool is one node pool with the given fields replacing the defaults.
	pool := func(overrides ...string) string {
		f := map[string]string{"name": "w", "replicas": "1", "vcpus": "1", "memory": "2Gi", "diskSize": "20Gi"}
		var extra []string
		for _, o := range overrides {
			k, v, _ := strings.Cut(o, ": ")
			if _, ok := f[k]; ok {
				f[k] = v
			} else {
				extra = append(extra, o)
			}
		}
		parts := make([]string, 0, 5+len(extra))
		parts = append(parts, "name: "+f["name"], "replicas: "+f["replicas"], "vcpus: "+f["vcpus"], "memory: "+f["memory"], "diskSize: "+f["diskSize"])
		return "    bridges: [br0]\n  nodePools:\n    - {" + strings.Join(append(parts, extra...), ", ") + "}\n"
	}
	cases := []struct {
		name, from, to, field string
	}{
		{"bad nodeInterface", "    bridges: [br0]", "    bridges: [br0]\n    nodeInterface: Eth0", "spec.network.nodeInterface"},
		{"bad vipInterface", "    bridges: [br0]", "    bridges: [br0]\n    vipInterface: 'eth 0'", "spec.network.vipInterface"},
		{"too many bridges", "bridges: [br0]", "bridges: [b0, b1, b2, b3, b4, b5, b6, b7, b8]", "at most 8, got 9"},
		{"managed subnet not IPv4", "    controlPlaneEndpoint: 10.0.0.10\n", managed("fd00::/64", "192.168.1.10", "192.168.1.20"), "must be an IPv4 CIDR"},
		{"managed subnet garbage", "    controlPlaneEndpoint: 10.0.0.10\n", managed("lan", "192.168.1.10", "192.168.1.20"), "must be an IPv4 CIDR"},
		{"dhcp not an address", "    controlPlaneEndpoint: 10.0.0.10\n", managed("192.168.1.0/24", "first", "192.168.1.20"), "dhcpStart: must be an IPv4 address"},
		{"dhcp on the network address", "    controlPlaneEndpoint: 10.0.0.10\n", managed("192.168.1.0/24", "192.168.1.0", "192.168.1.20"), "the subnet's network address"},
		{"dhcp on the broadcast address", "    controlPlaneEndpoint: 10.0.0.10\n", managed("192.168.1.0/24", "192.168.1.10", "192.168.1.255"), "the subnet's broadcast address"},
		{"endpoint on the gateway", "    controlPlaneEndpoint: 10.0.0.10\n", "    controlPlaneEndpoint: 192.168.1.1\n" + managed("192.168.1.0/24", "192.168.1.10", "192.168.1.20"), "the managed subnet's gateway address"},
		{"pod cidr not masked", "    bridges: [br0]", "    bridges: [br0]\n    podCIDR: 10.244.1.0/16", "spec.network.podCIDR: must be an IPv4 network in CIDR form"},
		{"service cidr IPv6", "    bridges: [br0]", "    bridges: [br0]\n    serviceCIDR: 'fd00::/108'", "spec.network.serviceCIDR: must be an IPv4 network"},
		{"long pool name", "    bridges: [br0]\n", pool("name: " + strings.Repeat("p", 62)), "is longer than 63 characters"},
		{"duplicate pools", "    bridges: [br0]\n", pool() + "    - {name: w, replicas: 1, vcpus: 1, memory: 2Gi, diskSize: 20Gi}\n", `duplicates "w"`},
		{"negative replicas", "    bridges: [br0]\n", pool("replicas: -1"), "must not be negative"},
		{"bad label value", "    bridges: [br0]\n", pool("labels: {tier: 'not ok'}"), `value "not ok"`},
		{"no vcpus", "    bridges: [br0]\n", pool("vcpus: 0"), "vcpus: must be at least 1"},
		{"tiny memory", "    bridges: [br0]\n", pool("memory: 512Mi"), "memory: must be at least 1Gi"},
		{"bad credentials Secret name", "  storagePool: pool", "  storagePool: pool\n  hypervisor:\n    address: hv\n    credentialsSecretName: Bad_Name", "spec.hypervisor.credentialsSecretName"},
		{"endpoint not a DNS name", "10.0.0.10", "api_server.example", "neither an IPv4 address nor a DNS name"},
		{"long image name", "    name: img.qcow2", "    name: " + strings.Repeat("i", 254), "at most 253 characters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(valid, tc.from) {
				t.Fatalf("test bug: %q not in valid config", tc.from)
			}
			_, err := Parse([]byte(strings.Replace(valid, tc.from, tc.to, 1)))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error does not mention %q:\n%v", tc.field, err)
			}
		})
	}
}

// Accepted edge cases the validators must not refuse.
func TestAcceptsEdgeCases(t *testing.T) {
	for _, tc := range []struct{ name, from, to string }{
		{"bracketed IPv6 hypervisor", "  storagePool: pool", "  storagePool: pool\n  hypervisor:\n    address: '[fd00::1]'"},
		{"hypervisor with port", "  storagePool: pool", "  storagePool: pool\n  hypervisor:\n    address: hv.example:16514"},
		{"DNS endpoint", "10.0.0.10", "api.example.com"},
		{"allowed kubelet label", "    bridges: [br0]", "    bridges: [br0]\n  nodePools:\n    - {name: w, replicas: 1, vcpus: 1, memory: 2Gi, diskSize: 20Gi, labels: {node.kubernetes.io/pool: w, example.com/tier: web}}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(strings.Replace(valid, tc.from, tc.to, 1))); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestKubeletRefusesLabel(t *testing.T) {
	for key, refused := range map[string]bool{
		"tier":                            false,
		"example.com/tier":                false,
		"kubernetes.io/role":              true,
		"k8s.io/x":                        true,
		"node-role.kubernetes.io/worker":  true,
		"foo.k8s.io/x":                    true,
		"kubelet.kubernetes.io/x":         false,
		"node.kubernetes.io/pool":         false,
		"sub.node.kubernetes.io/pool":     false,
		"notkubernetes.io/x":              false,
		"example.com/kubernetes.io-style": false,
	} {
		if got := kubeletRefusesLabel(key); got != refused {
			t.Errorf("%s: refused %v, want %v", key, got, refused)
		}
	}
}

func TestValidPortSuffix(t *testing.T) {
	for addr, ok := range map[string]bool{
		"hv":             true,
		"hv:16514":       true,
		"hv:0":           false,
		"hv:65536":       false,
		"hv:x":           false,
		"[fd00::1]":      true,
		"[fd00::1]:1234": true,
	} {
		if got := validPortSuffix(addr); got != ok {
			t.Errorf("%s: %v, want %v", addr, got, ok)
		}
	}
}

func TestMultipleNICs(t *testing.T) {
	if (&NetworkSpec{Bridges: []string{"br0"}}).MultipleNICs() {
		t.Error("one bridge is one NIC")
	}
	if !(&NetworkSpec{Bridges: []string{"br0", "br1"}}).MultipleNICs() {
		t.Error("two bridges are two NICs")
	}
	if !(&NetworkSpec{Bridges: []string{"br0"}, ManagedNetwork: &ManagedNetworkSpec{}}).MultipleNICs() {
		t.Error("a bridge and a managed network are two NICs")
	}
}

func TestEndpointOnManagedNetworkWithoutOne(t *testing.T) {
	if (&NetworkSpec{ControlPlaneEndpoint: "10.0.0.10"}).endpointOnManagedNetwork() {
		t.Error("no managed network: the endpoint cannot be on it")
	}
}
