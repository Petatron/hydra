package config

import (
	"strings"
	"testing"
)

const valid = `
apiVersion: hydra.io/v1alpha1
kind: ClusterConfig
metadata:
  name: c1
spec:
  storagePool: pool
  image:
    name: img.qcow2
  network:
    controlPlaneEndpoint: 10.0.0.10
    bridges: [br0]
`

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Spec
	checks := map[string][2]any{
		"namespace":   {c.Metadata.Namespace, "default"},
		"version":     {s.KubernetesVersion, DefaultKubernetesVersion},
		"bootstrap":   {s.Image.Bootstrap, BootstrapPackages},
		"podCIDR":     {s.Network.PodCIDR, DefaultPodCIDR},
		"mtu":         {s.Network.MTU, DefaultMTU},
		"vip":         {s.Network.VIPInterface, "enp1s0"},
		"replicas":    {s.ControlPlane.Replicas, int32(3)},
		"memory":      {s.ControlPlane.Memory.String(), "8Gi"},
		"schedulable": {*s.ControlPlane.Schedulable, true},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s: got %v, want %v", name, c[0], c[1])
		}
	}
	if s.KubernetesMinor() != "v1.35" || s.KubernetesPackageVersion() != "1.35.3" {
		t.Errorf("version helpers: %s %s", s.KubernetesMinor(), s.KubernetesPackageVersion())
	}
}

func TestHypervisorSecretDefault(t *testing.T) {
	c, err := Parse([]byte(valid + "  hypervisor:\n    address: hv.example:16514\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Spec.Hypervisor.CredentialsSecretName; got != "c1-libvirt-pki" {
		t.Errorf("credentialsSecretName default: %q", got)
	}
}

func TestSchedulableWithPools(t *testing.T) {
	c, err := Parse([]byte(valid + `  nodePools:
    - name: w
      replicas: 1
      vcpus: 2
      memory: 4Gi
      diskSize: 40Gi
`))
	if err != nil {
		t.Fatal(err)
	}
	if *c.Spec.ControlPlane.Schedulable {
		t.Error("with node pools the control plane should default to tainted")
	}
}

// Each case replaces one line of the valid config (or appends) and must fail
// naming the field.
func TestRejects(t *testing.T) {
	cases := []struct {
		name, from, to, field string
	}{
		{"unknown field", "  storagePool: pool", "  storagePool: pool\n  stroagePool: x", "unknown field"},
		{"wrong apiVersion", "hydra.io/v1alpha1", "hydra.io/v2", "apiVersion"},
		{"uppercase name", "name: c1", "name: C1", "metadata.name"},
		{"long name", "name: c1", "name: " + strings.Repeat("a", 33), "Cilium"},
		{"bad version", "  storagePool: pool", "  storagePool: pool\n  kubernetesVersion: 1.35", "spec.kubernetesVersion"},
		{"no pool", "  storagePool: pool\n", "", "spec.storagePool"},
		{"bad bootstrap", "    name: img.qcow2", "    name: img.qcow2\n    bootstrap: magic", "spec.image.bootstrap"},
		{"no endpoint", "    controlPlaneEndpoint: 10.0.0.10\n", "", "spec.network.controlPlaneEndpoint"},
		{"ipv6 endpoint", "10.0.0.10", "'[fd00::1]:6443'", "IPv4"},
		{"bad port", "10.0.0.10", "10.0.0.10:99999", "port"},
		{"no bridges", "bridges: [br0]", "bridges: []", "spec.network.bridges"},
		{"dup bridges", "bridges: [br0]", "bridges: [br0, br0]", "duplicates"},
		{"overlap", "    bridges: [br0]", "    bridges: [br0]\n    podCIDR: 10.96.0.0/16", "overlaps"},
		{"bad mtu", "    bridges: [br0]", "    bridges: [br0]\n    mtu: 100", "spec.network.mtu"},
		{"even cp", "    bridges: [br0]", "    bridges: [br0]\n  controlPlane:\n    replicas: 2", "odd"},
		{"tiny disk", "    bridges: [br0]", "    bridges: [br0]\n  controlPlane:\n    diskSize: 1Gi", "diskSize"},
		{"managed dhcp outside", "    controlPlaneEndpoint: 10.0.0.10\n", "    managedNetwork:\n      name: n\n      subnet: 192.168.1.0/24\n      dhcpStart: 192.168.2.10\n      dhcpEnd: 192.168.1.20\n", "outside"},
		{"managed two bridges", "    controlPlaneEndpoint: 10.0.0.10\n    bridges: [br0]", "    bridges: [br0, br1]\n    managedNetwork:\n      name: n\n      subnet: 192.168.1.0/24\n      dhcpStart: 192.168.1.10\n      dhcpEnd: 192.168.1.20", "vipInterface"},
		{"reserved pool name", "    bridges: [br0]", "    bridges: [br0]\n  nodePools:\n    - {name: cp, replicas: 1, vcpus: 1, memory: 2Gi, diskSize: 20Gi}", "reserved"},
		{"autoscaled with replicas", "    bridges: [br0]", "    bridges: [br0]\n  nodePools:\n    - {name: w, replicas: 2, vcpus: 1, memory: 2Gi, diskSize: 20Gi, autoscaling: {min: 0, max: 3}}", "omitted with autoscaling"},
		{"bad bounds", "    bridges: [br0]", "    bridges: [br0]\n  nodePools:\n    - {name: w, vcpus: 1, memory: 2Gi, diskSize: 20Gi, autoscaling: {min: 4, max: 3}}", "autoscaling"},
		{"bad label", "    bridges: [br0]", "    bridges: [br0]\n  nodePools:\n    - {name: w, replicas: 1, vcpus: 1, memory: 2Gi, diskSize: 20Gi, labels: {'a b': x}}", "labels"},
		{"bad hypervisor", "  storagePool: pool", "  storagePool: pool\n  hypervisor:\n    address: 'bad host'", "spec.hypervisor.address"},
		{"hypervisor port range", "  storagePool: pool", "  storagePool: pool\n  hypervisor:\n    address: hv:99999", "spec.hypervisor.address"},
		{"pool whitespace", "storagePool: pool", "storagePool: 'my pool'", "whitespace"},
		{"leading-zero version", "  storagePool: pool", "  storagePool: pool\n  kubernetesVersion: v1.035.3", "spec.kubernetesVersion"},
		{"endpoint not ipv4", "10.0.0.10", "300.1.1.1", "not a valid IPv4"},
		{"endpoint port", "10.0.0.10", "10.0.0.10:443", "only 6443"},
		{"pod cidr too small", "    bridges: [br0]", "    bridges: [br0]\n    podCIDR: 10.244.0.0/24", "per-node"},
		{"pod cidr smaller than node", "    bridges: [br0]", "    bridges: [br0]\n    podCIDR: 10.244.0.0/25", "smaller than one node"},
		{"service cidr too big", "    bridges: [br0]", "    bridges: [br0]\n    serviceCIDR: 172.0.0.0/8", "/12"},
		{"kubelet label", "    bridges: [br0]", "    bridges: [br0]\n  nodePools:\n    - {name: w, replicas: 1, vcpus: 1, memory: 2Gi, diskSize: 20Gi, labels: {node-role.kubernetes.io/worker: ''}}", "your own domain"},
		{"dhcp reversed", "    controlPlaneEndpoint: 10.0.0.10\n", "    managedNetwork:\n      name: n\n      subnet: 192.168.1.0/24\n      dhcpStart: 192.168.1.200\n      dhcpEnd: 192.168.1.20\n", "after dhcpEnd"},
		{"dhcp on gateway", "    controlPlaneEndpoint: 10.0.0.10\n", "    managedNetwork:\n      name: n\n      subnet: 192.168.1.0/24\n      dhcpStart: 192.168.1.1\n      dhcpEnd: 192.168.1.20\n", "gateway"},
		{"endpoint in dhcp range", "    controlPlaneEndpoint: 10.0.0.10\n", "    controlPlaneEndpoint: 192.168.1.50\n    managedNetwork:\n      name: n\n      subnet: 192.168.1.0/24\n      dhcpStart: 192.168.1.10\n      dhcpEnd: 192.168.1.100\n", "inside the DHCP range"},
		{"pod overlaps managed", "    controlPlaneEndpoint: 10.0.0.10\n", "    podCIDR: 192.168.0.0/16\n    managedNetwork:\n      name: n\n      subnet: 192.168.1.0/24\n      dhcpStart: 192.168.1.10\n      dhcpEnd: 192.168.1.100\n", "overlaps the managed subnet"},
		{"endpoint in service cidr", "10.0.0.10", "10.100.0.5", "inside serviceCIDR"},
		{"endpoint in pod cidr", "10.0.0.10", "10.244.3.4", "inside podCIDR"},
		{"dns endpoint with managed", "    controlPlaneEndpoint: 10.0.0.10\n", "    controlPlaneEndpoint: api.example.com\n    managedNetwork:\n      name: n\n      subnet: 192.168.1.0/24\n      dhcpStart: 192.168.1.10\n      dhcpEnd: 192.168.1.100\n", "vipInterface"},
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

func TestReportsEveryError(t *testing.T) {
	_, err := Parse([]byte("apiVersion: x\nkind: y\nmetadata: {name: A}\nspec: {}\n"))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, f := range []string{"apiVersion", "kind", "metadata.name", "spec.storagePool", "spec.image.name", "spec.network.bridges"} {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("missing %s in:\n%v", f, err)
		}
	}
}

func TestEmpty(t *testing.T) {
	if _, err := Parse([]byte("  \n")); err == nil {
		t.Error("empty input accepted")
	}
}

func TestVIPInterfaceFollowsEndpoint(t *testing.T) {
	managed := "    managedNetwork:\n      name: n\n      subnet: 192.168.1.0/24\n      dhcpStart: 192.168.1.10\n      dhcpEnd: 192.168.1.100\n"
	for _, tc := range []struct{ endpoint, want string }{
		{"", "enp2s0"}, // Hydra chooses it on the managed network
		{"    controlPlaneEndpoint: 192.168.1.2\n", "enp2s0"}, // stated, on the managed network
		{"    controlPlaneEndpoint: 10.0.0.5\n", "enp1s0"},    // stated, on the site network
	} {
		cfg := strings.Replace(valid, "    controlPlaneEndpoint: 10.0.0.10\n", tc.endpoint+managed, 1)
		c, err := Parse([]byte(cfg))
		if err != nil {
			t.Fatalf("%q: %v", tc.endpoint, err)
		}
		if got := c.Spec.Network.VIPInterface; got != tc.want {
			t.Errorf("endpoint %q: vipInterface %q, want %q", tc.endpoint, got, tc.want)
		}
	}
}

func TestPodCIDRSizedForAutoscaledPools(t *testing.T) {
	cfg := valid + "  nodePools:\n    - {name: w, vcpus: 1, memory: 2Gi, diskSize: 20Gi, autoscaling: {min: 0, max: 300}}\n"
	if _, err := Parse([]byte(cfg)); err == nil || !strings.Contains(err.Error(), "303 nodes") {
		t.Errorf("a /16 holds 256 nodes; 3 + 300 must be refused, got %v", err)
	}
}
