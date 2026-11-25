// Package config defines hydra.yaml, the CLI's only input (ADR-001).
//
// hydra.yaml is a user-facing artifact, never a state system: the CLI reads it,
// translates it into Cluster API resources, and keeps nothing about it anywhere.
package config

import "k8s.io/apimachinery/pkg/api/resource"

const (
	// APIVersion is the only apiVersion this CLI accepts.
	APIVersion = "hydra.io/v1alpha1"
	// Kind is the only kind this CLI accepts.
	Kind = "ClusterConfig"
)

// CNI modes. See NetworkSpec.CNI.
const (
	CNIHydra    = "hydra"
	CNIExternal = "external"
)

// Node bootstrap modes. See ImageSpec.Bootstrap.
const (
	BootstrapPackages = "packages"
	BootstrapPrebaked = "prebaked"
)

// ClusterConfig describes one Hydra cluster.
type ClusterConfig struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Metadata   Metadata    `json:"metadata"`
	Spec       ClusterSpec `json:"spec"`
}

// Metadata names the cluster.
type Metadata struct {
	// Name is the cluster's name. It becomes the Cluster API object names and
	// Cilium's cluster name, so it must be a DNS label of at most 32 characters.
	Name string `json:"name"`
	// Namespace is the management-cluster namespace the objects are created in.
	// Defaults to "default".
	Namespace string `json:"namespace,omitempty"`
}

// ClusterSpec is the desired cluster.
type ClusterSpec struct {
	// KubernetesVersion, e.g. v1.35.3. Defaults to the version this CLI is
	// tested with.
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`

	// Hypervisor names the libvirt TLS endpoint for this cluster. When omitted,
	// the provider's manager-wide hypervisor configuration is used.
	Hypervisor *HypervisorSpec `json:"hypervisor,omitempty"`

	// StoragePool is the libvirt pool machine disks are created in, and where
	// the base image must already be.
	StoragePool string `json:"storagePool"`

	Image        ImageSpec        `json:"image"`
	Network      NetworkSpec      `json:"network"`
	ControlPlane ControlPlaneSpec `json:"controlPlane"`
	NodePools    []NodePoolSpec   `json:"nodePools,omitempty"`

	// Addons are platform addons from hydra's catalogue (internal/addons),
	// each installed by Cluster API Add-on Provider Helm (ADR-002).
	Addons []AddonSpec `json:"addons,omitempty"`
}

// AddonSpec selects one catalogue addon.
type AddonSpec struct {
	// Name is the catalogue entry, e.g. metrics-server.
	Name string `json:"name"`
	// Version is the chart version, one the catalogue allows. Defaults to the
	// catalogue's default for this addon.
	Version string `json:"version,omitempty"`
	// Params are the addon's declared settings; unset ones take their
	// defaults. Raw Helm values are not accepted.
	Params map[string]any `json:"params,omitempty"`
}

// HypervisorSpec points the cluster at a hypervisor.
type HypervisorSpec struct {
	// Address is the libvirt TLS listener, host or host:port (default port 16514).
	Address string `json:"address"`
	// CredentialsSecretName names the Secret, in the cluster's namespace, that
	// holds cacert.pem, clientcert.pem and clientkey.pem. render references it
	// and never emits it: key material does not belong in rendered output.
	// Defaults to <cluster>-libvirt-pki.
	CredentialsSecretName string `json:"credentialsSecretName,omitempty"`
}

// ImageSpec selects what machines boot from.
type ImageSpec struct {
	// Name of the image volume in StoragePool.
	Name string `json:"name"`
	// Bootstrap is how a booted machine gets its container runtime and
	// Kubernetes packages:
	//   packages  install them from the distribution and pkgs.k8s.io at boot
	//             (the default; needs the package mirrors to be reachable)
	//   prebaked  the image already carries them (PET-77): containerd
	//             configured for the systemd cgroup driver, kubelet, kubeadm
	//             and kubectl at KubernetesVersion, qemu-guest-agent enabled
	Bootstrap string `json:"bootstrap,omitempty"`
}

// NetworkSpec is the cluster's addressing.
type NetworkSpec struct {
	// ControlPlaneEndpoint is the API address kube-vip holds: an IPv4 address
	// or DNS name, optionally with :6443 (the only port supported, since the
	// API server binds 6443 and kube-vip moves the address, not the port). It
	// must be outside every DHCP range on its network. Leave it empty with a
	// ManagedNetwork to let Hydra choose one on that network.
	ControlPlaneEndpoint string `json:"controlPlaneEndpoint,omitempty"`
	// Bridges are the site networks every machine is attached to, in order.
	Bridges []string `json:"bridges"`
	// ManagedNetwork asks Hydra to create and own a network for the cluster.
	// Machines get an interface on it in addition to Bridges.
	ManagedNetwork *ManagedNetworkSpec `json:"managedNetwork,omitempty"`
	// PodCIDR is shared by kubeadm and Cilium's IPAM pool, which gives each
	// node a /24 from it, so it must hold one /24 per node the cluster can
	// reach. Default 10.244.0.0/16.
	PodCIDR string `json:"podCIDR,omitempty"`
	// ServiceCIDR. Default 10.96.0.0/12.
	ServiceCIDR string `json:"serviceCIDR,omitempty"`
	// CNI is who installs the cluster's network plugin:
	//   hydra     hydra renders a Cilium HelmChartProxy, which Cluster API
	//             Add-on Provider Helm installs once the API is up and keeps
	//             at CiliumVersion (the default)
	//   external  hydra installs no CNI. Something else must (Argo CD, Helm, a
	//             person), with a pod CIDR matching PodCIDR. Nodes stay
	//             NotReady, and create keeps waiting, until it does.
	CNI string `json:"cni,omitempty"`
	// MTU for hydra's Cilium. Default 1500. Only valid with CNI hydra: an
	// external CNI takes its MTU from its own configuration.
	MTU int `json:"mtu,omitempty"`
	// CiliumVersion is hydra's Cilium chart version, one of those the
	// catalogue allows. Default: the catalogue's. Changing it upgrades
	// Cilium on the next apply. Only valid with CNI hydra.
	CiliumVersion string `json:"ciliumVersion,omitempty"`
	// NodeInterface is the guest interface whose address a node registers
	// with. Default enp1s0, the first virtio NIC.
	NodeInterface string `json:"nodeInterface,omitempty"`
	// VIPInterface is the guest interface kube-vip puts the endpoint on.
	// Defaults to NodeInterface when the endpoint is on the site network, or
	// to enp2s0 when it is on a ManagedNetwork behind a single bridge (the
	// managed NIC comes after the bridges). Must be set explicitly otherwise.
	VIPInterface string `json:"vipInterface,omitempty"`
}

// ManagedNetworkSpec mirrors HydraCluster.spec.managedNetwork.
type ManagedNetworkSpec struct {
	Name      string `json:"name"`
	Subnet    string `json:"subnet"`
	DHCPStart string `json:"dhcpStart"`
	DHCPEnd   string `json:"dhcpEnd"`
}

// MachineSize is the shape of one VM.
type MachineSize struct {
	VCPUs    int32             `json:"vcpus,omitempty"`
	Memory   resource.Quantity `json:"memory,omitzero"`
	DiskSize resource.Quantity `json:"diskSize,omitzero"`
}

// ControlPlaneSpec sizes the control plane.
type ControlPlaneSpec struct {
	// Replicas must be odd: 1, 3 or 5. Default 3.
	Replicas    int32 `json:"replicas,omitempty"`
	MachineSize `json:",inline"`
	// Schedulable removes the control-plane taint so workloads run on the
	// control plane. Defaults to true when there are no node pools.
	Schedulable *bool `json:"schedulable,omitempty"`
}

// NodePoolSpec is one MachineDeployment of workers.
type NodePoolSpec struct {
	Name string `json:"name"`
	// Replicas is the fixed node count. Omit it when Autoscaling is set.
	Replicas    int32 `json:"replicas,omitempty"`
	MachineSize `json:",inline"`
	// Labels are applied to the pool's Nodes through the kubelet.
	Labels map[string]string `json:"labels,omitempty"`
	// Autoscaling, when set, gives Cluster Autoscaler the pool's bounds.
	Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`
}

// AutoscalingSpec bounds a pool for Cluster Autoscaler.
type AutoscalingSpec struct {
	Min int32 `json:"min"`
	Max int32 `json:"max"`
}
