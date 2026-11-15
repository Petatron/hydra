// Package render translates a ClusterConfig into Cluster API manifests.
//
// It is a pure function of its input and the constants pinned below: no
// cluster, no network, no clock, no randomness. Same hydra.yaml and same CLI
// version give byte-identical output (ADR-001 decision 3).
package render

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"sigs.k8s.io/yaml"

	"github.com/Petatron/hydra/internal/config"
)

// Versions pinned in this CLI. Changing one changes every rendered cluster,
// so each change is a reviewed release, never a runtime choice.
const (
	// KubeVIPImage holds the control-plane endpoint.
	KubeVIPImage = "ghcr.io/kube-vip/kube-vip:v0.8.9"
	// CiliumVersion is the chart hack/gen-cilium.sh rendered assets/cilium.yaml from.
	CiliumVersion = "1.19.1"

	// CNILabelKey on a Cluster selects the ClusterResourceSet that installs
	// its CNI. The value is the cluster's name, so each cluster's set (which
	// carries that cluster's pod CIDR and MTU) matches only that cluster.
	CNILabelKey = "cni.hydra.io/cilium"

	// ClusterLabelKey is on every rendered object, valued with the cluster's
	// name, so commands can find all of a cluster's objects (to delete what
	// Cluster API does not own) without needing hydra.yaml to still validate.
	ClusterLabelKey = "hydra.io/cluster"
)

const kindConfigMap = "ConfigMap"

// Sentinels hack/gen-cilium.sh renders in place of the per-cluster values.
const (
	sentinelClusterName = "hydra-sentinel-cluster"
	sentinelPodCIDR     = "10.255.254.0/24"
	sentinelMTU         = `mtu: "65000"`
)

var (
	//go:embed templates/cluster.yaml.tmpl
	clusterTemplate string
	//go:embed assets/cilium.yaml
	ciliumManifest string

	tmpl = template.Must(template.New("cluster").
		Funcs(template.FuncMap{"q": strconv.Quote, "nodeSetupArgs": newNodeSetupArgs}).
		Option("missingkey=error").
		Parse(clusterTemplate))
)

// Options carries what is not part of hydra.yaml.
type Options struct {
	// CLIVersion is stamped into the output header.
	CLIVersion string
}

type data struct {
	*config.ClusterConfig
	Spec       *config.ClusterSpec
	CLIVersion string
	Name       string
	Namespace  string
	CPName     string
	// CPTemplateName is the control plane's HydraMachineTemplate, named after
	// its content.
	CPTemplateName string
	HasEndpoint    bool
	EndpointHost   string
	EndpointPort   int
	CPSchedulable  bool
	// MHCMaxUnhealthy is how many control-plane machines may be remediated
	// at once: what etcd can lose and keep quorum, at least 1.
	MHCMaxUnhealthy int32
	KubeVIPImage    string
	CNILabelKey     string
	ClusterLabelKey string
	Pools           []pool
}

type pool struct {
	Root       *data
	Spec       config.NodePoolSpec
	ObjName    string
	NodeLabels string
	// HMTName and KCTName carry a hash of their own content. Both templates
	// are effectively immutable (HydraMachineTemplate's spec by CEL rule; a
	// KubeadmConfigTemplate edited in place reaches only machines created
	// later), so a changed shape or bootstrap must be a new template that the
	// MachineDeployment rolls onto.
	HMTName string
	KCTName string
}

type nodeSetupArgs struct {
	Spec         *config.ClusterSpec
	ControlPlane bool
	Indent       string
}

func newNodeSetupArgs(d *data, controlPlane bool) nodeSetupArgs {
	indent := strings.Repeat(" ", 8)
	if controlPlane {
		indent = strings.Repeat(" ", 6)
	}
	return nodeSetupArgs{Spec: d.Spec, ControlPlane: controlPlane, Indent: indent}
}

// Render returns the multi-document YAML for c. c must have come from
// config.Parse (defaulted and validated).
func Render(c *config.ClusterConfig, opts Options) ([]byte, error) {
	d := &data{
		ClusterConfig:   c,
		Spec:            &c.Spec,
		CLIVersion:      opts.CLIVersion,
		Name:            c.Metadata.Name,
		Namespace:       c.Metadata.Namespace,
		CPName:          ControlPlaneName(c.Metadata.Name),
		CPSchedulable:   c.Spec.ControlPlane.Schedulable != nil && *c.Spec.ControlPlane.Schedulable,
		MHCMaxUnhealthy: max(1, (c.Spec.ControlPlane.Replicas-1)/2),
		KubeVIPImage:    KubeVIPImage,
		CNILabelKey:     CNILabelKey,
		ClusterLabelKey: ClusterLabelKey,
	}
	if ep := c.Spec.Network.ControlPlaneEndpoint; ep != "" {
		host, port, err := config.SplitEndpoint(ep)
		if err != nil {
			return nil, err
		}
		d.HasEndpoint, d.EndpointHost, d.EndpointPort = true, host, port
	}
	// The hash inputs are part of the naming contract: changing them renames
	// every template, which rolls every existing cluster's machines on its next
	// create. Change them only with a migration note in the release.
	d.CPTemplateName = d.CPName + "-" + contentHash(c.Metadata.Name+" "+machineShape(c.Spec.ControlPlane.MachineSize))
	for _, p := range c.Spec.NodePools {
		pl := pool{
			Root:       d,
			Spec:       p,
			ObjName:    c.Metadata.Name + "-" + p.Name,
			NodeLabels: joinLabels(p.Labels),
		}
		// The cluster name is in the hash, so two clusters whose object names
		// happen to collide never share a template.
		pl.HMTName = pl.ObjName + "-" + contentHash(c.Metadata.Name+" "+machineShape(p.MachineSize))
		var kct bytes.Buffer
		if err := tmpl.ExecuteTemplate(&kct, "kctSpec", pl); err != nil {
			return nil, fmt.Errorf("rendering node pool %s: %w", p.Name, err)
		}
		// Hash the object, not the text: a comment-only change in a later CLI
		// release must not roll every pool.
		obj, err := yaml.YAMLToJSON(kct.Bytes())
		if err != nil {
			return nil, fmt.Errorf("node pool %s: rendered KubeadmConfigTemplate is not valid YAML: %w", p.Name, err)
		}
		pl.KCTName = pl.ObjName + "-" + contentHash(string(obj))
		d.Pools = append(d.Pools, pl)
	}

	var out bytes.Buffer
	if err := tmpl.Execute(&out, d); err != nil {
		return nil, fmt.Errorf("rendering cluster manifests: %w", err)
	}
	cni, err := renderCNI(c)
	if err != nil {
		return nil, err
	}
	out.Write(cni)
	return out.Bytes(), nil
}

// ControlPlaneName is the KubeadmControlPlane's name for a cluster.
func ControlPlaneName(cluster string) string { return cluster + "-cp" }

// CNIName is the CNI ConfigMap's and ClusterResourceSet's name for a cluster.
func CNIName(cluster string) string { return cluster + "-cilium" }

// machineShape is the content of a HydraMachineTemplate's spec.
func machineShape(m config.MachineSize) string {
	return fmt.Sprintf("vcpus=%d memory=%s diskSize=%s", m.VCPUs, m.Memory.String(), m.DiskSize.String())
}

// contentHash is a short, stable digest for naming objects after their content.
func contentHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// joinLabels renders a label map as k=v,k=v in key order.
func joinLabels(labels map[string]string) string {
	parts := make([]string, 0, len(labels))
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, ",")
}

// renderCNI returns the ConfigMap carrying this cluster's Cilium manifest and
// the ClusterResourceSet that applies it once the cluster's API is up
// (ADR-001 decision 6).
//
// The set uses ApplyOnce. Reconcile would re-apply the manifest whenever the
// ConfigMap changes, but the manifest contains a Job (Hubble certificate
// generation), whose pod template is immutable, so a Cilium version change
// would fail to apply part-way. Upgrading Cilium is left to the upgrade work,
// not to an implicit re-apply.
func renderCNI(c *config.ClusterConfig) ([]byte, error) {
	manifest, err := ciliumFor(c)
	if err != nil {
		return nil, err
	}
	name := CNIName(c.Metadata.Name)
	cm := map[string]any{
		"apiVersion": "v1",
		"kind":       kindConfigMap,
		"metadata": map[string]any{
			"name":      name,
			"namespace": c.Metadata.Namespace,
			"labels":    map[string]string{CNILabelKey: c.Metadata.Name, ClusterLabelKey: c.Metadata.Name},
		},
		"data": map[string]string{"cilium.yaml": manifest},
	}
	crs := map[string]any{
		"apiVersion": "addons.cluster.x-k8s.io/v1beta2",
		"kind":       "ClusterResourceSet",
		"metadata": map[string]any{
			"name":      name,
			"namespace": c.Metadata.Namespace,
			"labels":    map[string]string{ClusterLabelKey: c.Metadata.Name},
		},
		"spec": map[string]any{
			"strategy": "ApplyOnce",
			"clusterSelector": map[string]any{
				"matchLabels": map[string]string{CNILabelKey: c.Metadata.Name},
			},
			"resources": []map[string]string{{"kind": kindConfigMap, "name": name}},
		},
	}
	var out bytes.Buffer
	for _, header := range []struct {
		comment string
		obj     any
	}{
		{"# Cilium " + CiliumVersion + " for this cluster, applied by the ClusterResourceSet below.", cm},
		{"# Installs the CNI once the cluster's API is reachable. ApplyOnce: see the hydra source.", crs},
	} {
		b, err := yaml.Marshal(header.obj)
		if err != nil {
			return nil, err
		}
		out.WriteString("---\n" + header.comment + "\n")
		out.Write(b)
	}
	return out.Bytes(), nil
}

// ciliumFor substitutes this cluster's values into the embedded manifest.
func ciliumFor(c *config.ClusterConfig) (string, error) {
	m := ciliumManifest
	// Drop the generator's header line; the ConfigMap comment says where it came from.
	if i := strings.IndexByte(m, '\n'); i >= 0 && strings.HasPrefix(m, "# Code generated") {
		m = m[i+1:]
	}
	for _, s := range []struct{ from, to string }{
		{sentinelClusterName, c.Metadata.Name},
		{sentinelPodCIDR, c.Spec.Network.PodCIDR},
		{sentinelMTU, fmt.Sprintf("mtu: %q", strconv.Itoa(c.Spec.Network.MTU))},
	} {
		if !strings.Contains(m, s.from) {
			return "", fmt.Errorf("embedded Cilium manifest has no %q: regenerate it with hack/gen-cilium.sh", s.from)
		}
		m = strings.ReplaceAll(m, s.from, s.to)
	}
	return m, nil
}
