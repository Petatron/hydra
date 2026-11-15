package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

// Defaults applied when hydra.yaml leaves a field empty.
const (
	DefaultNamespace         = "default"
	DefaultKubernetesVersion = "v1.35.3"
	DefaultPodCIDR           = "10.244.0.0/16"
	DefaultServiceCIDR       = "10.96.0.0/12"
	DefaultMTU               = 1500
	DefaultNodeInterface     = "enp1s0"
	DefaultEndpointPort      = 6443
	DefaultHypervisorPort    = 16514
	DefaultControlPlaneCount = 3

	// maxClusterName is Cilium's limit on cluster.name, which is the cluster's
	// name here; a longer name renders but Cilium refuses to start.
	maxClusterName = 32
)

var (
	defaultCPVCPUs    int32 = 4
	defaultCPMemory         = resource.MustParse("8Gi")
	defaultCPDiskSize       = resource.MustParse("40Gi")

	versionRE   = regexp.MustCompile(`^v1\.(\d+)\.(\d+)$`)
	interfaceRE = regexp.MustCompile(`^[a-z][a-z0-9]{0,14}$`)
	// The provider's HydraHypervisor.address pattern.
	hypervisorRE = regexp.MustCompile(`^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9]([-A-Za-z0-9.]*[A-Za-z0-9])?)(:[0-9]{1,5})?$`)
)

// Load reads, defaults and validates a hydra.yaml file.
func Load(path string) (*ClusterConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes, defaults and validates hydra.yaml content. Unknown fields are
// errors: a misspelt field silently ignored would render a different cluster
// from the one the user described.
func Parse(data []byte) (*ClusterConfig, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("hydra.yaml is empty")
	}
	var c ClusterConfig
	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		return nil, fmt.Errorf("decoding hydra.yaml: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *ClusterConfig) applyDefaults() {
	s := &c.Spec
	if c.Metadata.Namespace == "" {
		c.Metadata.Namespace = DefaultNamespace
	}
	if s.KubernetesVersion == "" {
		s.KubernetesVersion = DefaultKubernetesVersion
	}
	if s.Hypervisor != nil && s.Hypervisor.CredentialsSecretName == "" {
		s.Hypervisor.CredentialsSecretName = c.Metadata.Name + "-libvirt-pki"
	}
	if s.Image.Bootstrap == "" {
		s.Image.Bootstrap = BootstrapPackages
	}
	n := &s.Network
	if n.PodCIDR == "" {
		n.PodCIDR = DefaultPodCIDR
	}
	if n.ServiceCIDR == "" {
		n.ServiceCIDR = DefaultServiceCIDR
	}
	if n.MTU == 0 {
		n.MTU = DefaultMTU
	}
	if n.NodeInterface == "" {
		n.NodeInterface = DefaultNodeInterface
	}
	if n.VIPInterface == "" {
		switch {
		case n.ManagedNetwork == nil:
			n.VIPInterface = n.NodeInterface
		case len(n.Bridges) == 1 && n.NodeInterface == DefaultNodeInterface:
			// The managed network is attached after the bridges, so behind one
			// bridge it is the second virtio NIC.
			n.VIPInterface = "enp2s0"
		}
	}
	cp := &s.ControlPlane
	if cp.Replicas == 0 {
		cp.Replicas = DefaultControlPlaneCount
	}
	if cp.VCPUs == 0 {
		cp.VCPUs = defaultCPVCPUs
	}
	if cp.Memory.IsZero() {
		cp.Memory = defaultCPMemory.DeepCopy()
	}
	if cp.DiskSize.IsZero() {
		cp.DiskSize = defaultCPDiskSize.DeepCopy()
	}
	if cp.Schedulable == nil {
		schedulable := len(s.NodePools) == 0
		cp.Schedulable = &schedulable
	}
}

// fieldErrors collects every problem, so one run reports them all.
type fieldErrors []string

func (e *fieldErrors) add(field, format string, args ...any) {
	*e = append(*e, field+": "+fmt.Sprintf(format, args...))
}

func (e fieldErrors) err() error {
	if len(e) == 0 {
		return nil
	}
	return fmt.Errorf("invalid hydra.yaml:\n  %s", strings.Join(e, "\n  "))
}

// Validate checks a defaulted config.
func (c *ClusterConfig) Validate() error {
	var errs fieldErrors
	if c.APIVersion != APIVersion {
		errs.add("apiVersion", "must be %q, got %q", APIVersion, c.APIVersion)
	}
	if c.Kind != Kind {
		errs.add("kind", "must be %q, got %q", Kind, c.Kind)
	}
	dnsLabel(&errs, "metadata.name", c.Metadata.Name)
	if len(c.Metadata.Name) > maxClusterName {
		errs.add("metadata.name", "must be at most %d characters (Cilium's cluster name limit)", maxClusterName)
	}
	dnsLabel(&errs, "metadata.namespace", c.Metadata.Namespace)

	s := &c.Spec
	if !versionRE.MatchString(s.KubernetesVersion) {
		errs.add("spec.kubernetesVersion", "must look like v1.35.3, got %q", s.KubernetesVersion)
	}
	if h := s.Hypervisor; h != nil {
		if h.Address == "" || !hypervisorRE.MatchString(h.Address) || len(h.Address) > 259 {
			errs.add("spec.hypervisor.address", "must be host or host:port, got %q", h.Address)
		}
		dnsSubdomain(&errs, "spec.hypervisor.credentialsSecretName", h.CredentialsSecretName)
	}
	if s.StoragePool == "" {
		errs.add("spec.storagePool", "is required")
	}
	if s.Image.Name == "" {
		errs.add("spec.image.name", "is required")
	}
	if s.Image.Bootstrap != BootstrapPackages && s.Image.Bootstrap != BootstrapPrebaked {
		errs.add("spec.image.bootstrap", "must be %q or %q, got %q", BootstrapPackages, BootstrapPrebaked, s.Image.Bootstrap)
	}
	c.validateNetwork(&errs)
	c.validateMachines(&errs)
	return errs.err()
}

func (c *ClusterConfig) validateNetwork(errs *fieldErrors) {
	n := &c.Spec.Network
	if len(n.Bridges) == 0 {
		errs.add("spec.network.bridges", "needs at least one site network: it gives machines their Node address and internet access")
	}
	seen := map[string]bool{}
	for i, b := range n.Bridges {
		f := fmt.Sprintf("spec.network.bridges[%d]", i)
		if b == "" {
			errs.add(f, "is empty")
		}
		if seen[b] {
			errs.add(f, "duplicates %q", b)
		}
		seen[b] = true
	}
	if n.ControlPlaneEndpoint == "" && n.ManagedNetwork == nil {
		errs.add("spec.network.controlPlaneEndpoint", "is required unless a managedNetwork lets Hydra choose it")
	}
	if n.ControlPlaneEndpoint != "" {
		if _, _, err := SplitEndpoint(n.ControlPlaneEndpoint); err != nil {
			errs.add("spec.network.controlPlaneEndpoint", "%v", err)
		}
	}
	if m := n.ManagedNetwork; m != nil {
		dnsLabel(errs, "spec.network.managedNetwork.name", m.Name)
		if len(m.Name) > 63 {
			errs.add("spec.network.managedNetwork.name", "must be at most 63 characters")
		}
		prefix, err := netip.ParsePrefix(m.Subnet)
		if err != nil || !prefix.Addr().Is4() {
			errs.add("spec.network.managedNetwork.subnet", "must be an IPv4 CIDR, got %q", m.Subnet)
		}
		for _, f := range []struct{ name, v string }{{"dhcpStart", m.DHCPStart}, {"dhcpEnd", m.DHCPEnd}} {
			a, err := netip.ParseAddr(f.v)
			if err != nil || !a.Is4() {
				errs.add("spec.network.managedNetwork."+f.name, "must be an IPv4 address, got %q", f.v)
			} else if prefix.IsValid() && !prefix.Contains(a) {
				errs.add("spec.network.managedNetwork."+f.name, "%s is outside %s", f.v, m.Subnet)
			}
		}
	}
	for _, f := range []struct{ name, v string }{{"podCIDR", n.PodCIDR}, {"serviceCIDR", n.ServiceCIDR}} {
		if p, err := netip.ParsePrefix(f.v); err != nil || !p.Addr().Is4() || p.Masked() != p {
			errs.add("spec.network."+f.name, "must be an IPv4 network in CIDR form, got %q", f.v)
		}
	}
	if pod, err1 := netip.ParsePrefix(n.PodCIDR); err1 == nil {
		if svc, err2 := netip.ParsePrefix(n.ServiceCIDR); err2 == nil && pod.Overlaps(svc) {
			errs.add("spec.network.podCIDR", "%s overlaps serviceCIDR %s", n.PodCIDR, n.ServiceCIDR)
		}
	}
	if n.MTU < 1280 || n.MTU > 9000 {
		errs.add("spec.network.mtu", "must be between 1280 and 9000, got %d", n.MTU)
	}
	if !interfaceRE.MatchString(n.NodeInterface) {
		errs.add("spec.network.nodeInterface", "must be a Linux interface name, got %q", n.NodeInterface)
	}
	switch {
	case n.VIPInterface == "":
		errs.add("spec.network.vipInterface", "must be set: with a managedNetwork behind %d bridges Hydra cannot tell which guest interface it is", len(n.Bridges))
	case !interfaceRE.MatchString(n.VIPInterface):
		errs.add("spec.network.vipInterface", "must be a Linux interface name, got %q", n.VIPInterface)
	}
}

func (c *ClusterConfig) validateMachines(errs *fieldErrors) {
	cp := &c.Spec.ControlPlane
	if cp.Replicas != 1 && cp.Replicas != 3 && cp.Replicas != 5 {
		errs.add("spec.controlPlane.replicas", "must be 1, 3 or 5 (etcd needs an odd member count), got %d", cp.Replicas)
	}
	machineSize(errs, "spec.controlPlane", cp.MachineSize)

	names := map[string]bool{}
	for i, p := range c.Spec.NodePools {
		f := fmt.Sprintf("spec.nodePools[%d]", i)
		dnsLabel(errs, f+".name", p.Name)
		// <cluster>-<pool> names the MachineDeployment and its templates.
		if len(c.Metadata.Name)+1+len(p.Name) > 63 {
			errs.add(f+".name", "%s-%s is longer than 63 characters", c.Metadata.Name, p.Name)
		}
		if p.Name == "cp" {
			errs.add(f+".name", `"cp" is reserved for the control plane's objects`)
		}
		if names[p.Name] {
			errs.add(f+".name", "duplicates %q", p.Name)
		}
		names[p.Name] = true
		if p.Replicas < 0 {
			errs.add(f+".replicas", "must not be negative")
		}
		machineSize(errs, f, p.MachineSize)
		for k, v := range p.Labels {
			for _, msg := range validation.IsQualifiedName(k) {
				errs.add(f+".labels", "key %q: %s", k, msg)
			}
			for _, msg := range validation.IsValidLabelValue(v) {
				errs.add(f+".labels", "value %q: %s", v, msg)
			}
		}
		if a := p.Autoscaling; a != nil {
			if a.Min < 0 || a.Max < 1 || a.Min > a.Max {
				errs.add(f+".autoscaling", "needs 0 <= min <= max and max >= 1, got min %d max %d", a.Min, a.Max)
			}
			if p.Replicas != 0 {
				errs.add(f+".replicas", "must be omitted with autoscaling: Cluster Autoscaler owns the count, and the pool starts at min")
			}
		}
	}
}

func machineSize(errs *fieldErrors, field string, m MachineSize) {
	if m.VCPUs < 1 {
		errs.add(field+".vcpus", "must be at least 1")
	}
	if m.Memory.Cmp(resource.MustParse("1Gi")) < 0 {
		errs.add(field+".memory", "must be at least 1Gi, got %s", m.Memory.String())
	}
	if m.DiskSize.Cmp(resource.MustParse("10Gi")) < 0 {
		errs.add(field+".diskSize", "must be at least 10Gi, got %s", m.DiskSize.String())
	}
}

func dnsLabel(errs *fieldErrors, field, v string) {
	for _, msg := range validation.IsDNS1123Label(v) {
		errs.add(field, "%q: %s", v, msg)
	}
}

func dnsSubdomain(errs *fieldErrors, field, v string) {
	for _, msg := range validation.IsDNS1123Subdomain(v) {
		errs.add(field, "%q: %s", v, msg)
	}
}

// SplitEndpoint splits host[:port], defaulting the port to 6443.
func SplitEndpoint(ep string) (string, int, error) {
	host, port := ep, DefaultEndpointPort
	if h, p, err := net.SplitHostPort(ep); err == nil {
		n, perr := strconv.Atoi(p)
		if perr != nil || n < 1 || n > 65535 {
			return "", 0, fmt.Errorf("port %q is not 1-65535", p)
		}
		host, port = h, n
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if !a.Is4() {
			return "", 0, fmt.Errorf("%q: only IPv4 endpoints are supported (kube-vip is rendered with a /32)", host)
		}
		return host, port, nil
	}
	if msgs := validation.IsDNS1123Subdomain(host); len(msgs) > 0 {
		return "", 0, fmt.Errorf("%q is neither an IPv4 address nor a DNS name", host)
	}
	return host, port, nil
}

// KubernetesMinor returns "v1.35" for "v1.35.3", for the pkgs.k8s.io repository path.
func (s *ClusterSpec) KubernetesMinor() string {
	m := versionRE.FindStringSubmatch(s.KubernetesVersion)
	return "v1." + m[1]
}

// KubernetesPackageVersion returns "1.35.3" for "v1.35.3".
func (s *ClusterSpec) KubernetesPackageVersion() string {
	return strings.TrimPrefix(s.KubernetesVersion, "v")
}
