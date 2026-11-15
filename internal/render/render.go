// Package render translates a ClusterConfig into Cluster API manifests.
//
// It is a pure function of its input and the constants pinned below: no
// cluster, no network, no clock, no randomness. Same hydra.yaml and same CLI
// version give byte-identical output (ADR-001 decision 3).
package render

import (
	"bytes"
	_ "embed"
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
	Spec          *config.ClusterSpec
	CLIVersion    string
	Name          string
	Namespace     string
	CPName        string
	HasEndpoint   bool
	EndpointHost  string
	EndpointPort  int
	CPSchedulable bool
	// MHCMaxUnhealthy is how many control-plane machines may be remediated
	// at once: what etcd can lose and keep quorum, at least 1.
	MHCMaxUnhealthy int32
	KubeVIPImage    string
	CNILabelKey     string
	Pools           []pool
}

type pool struct {
	Spec       config.NodePoolSpec
	ObjName    string
	NodeLabels string
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
		CPName:          c.Metadata.Name + "-cp",
		CPSchedulable:   c.Spec.ControlPlane.Schedulable != nil && *c.Spec.ControlPlane.Schedulable,
		MHCMaxUnhealthy: max(1, (c.Spec.ControlPlane.Replicas-1)/2),
		KubeVIPImage:    KubeVIPImage,
		CNILabelKey:     CNILabelKey,
	}
	if ep := c.Spec.Network.ControlPlaneEndpoint; ep != "" {
		host, port, err := config.SplitEndpoint(ep)
		if err != nil {
			return nil, err
		}
		d.HasEndpoint, d.EndpointHost, d.EndpointPort = true, host, port
	}
	for _, p := range c.Spec.NodePools {
		d.Pools = append(d.Pools, pool{
			Spec:       p,
			ObjName:    c.Metadata.Name + "-" + p.Name,
			NodeLabels: joinLabels(p.Labels),
		})
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
	name := c.Metadata.Name + "-cilium"
	cm := map[string]any{
		"apiVersion": "v1",
		"kind":       kindConfigMap,
		"metadata": map[string]any{
			"name":      name,
			"namespace": c.Metadata.Namespace,
			"labels":    map[string]string{CNILabelKey: c.Metadata.Name},
		},
		"data": map[string]string{"cilium.yaml": manifest},
	}
	crs := map[string]any{
		"apiVersion": "addons.cluster.x-k8s.io/v1beta2",
		"kind":       "ClusterResourceSet",
		"metadata": map[string]any{
			"name":      name,
			"namespace": c.Metadata.Namespace,
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
