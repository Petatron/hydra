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

	"github.com/Petatron/hydra/internal/addons"
	"github.com/Petatron/hydra/internal/config"
)

// Versions pinned in this CLI. Changing one changes every rendered cluster,
// so each change is a reviewed release, never a runtime choice.
const (
	// KubeVIPImage holds the control-plane endpoint.
	KubeVIPImage = "ghcr.io/kube-vip/kube-vip:v0.8.9"
	// CNILabelKey on a Cluster selects the HelmChartProxy that installs its
	// CNI. The value is the cluster's name, so each cluster's proxy (which
	// carries that cluster's pod CIDR and MTU) matches only that cluster. A
	// Cluster without it is never given hydra's Cilium, and removing it from
	// a running Cluster uninstalls Cilium (PET-97): hydra never does.
	CNILabelKey = "cni.hydra.io/cilium"

	// ClusterLabelKey is on every rendered object, valued with the cluster's
	// name, so commands can find all of a cluster's objects (to delete what
	// Cluster API does not own) without needing hydra.yaml to still validate.
	ClusterLabelKey = "hydra.io/cluster"
	// AddonLabelKey names the catalogue addon a HelmChartProxy installs.
	AddonLabelKey = "hydra.io/addon"
)

// KindHelmChartProxy is CAAPH's per-addon object.
const KindHelmChartProxy = "HelmChartProxy"

// The per-cluster values in the canonical Cilium values are these sentinels
// (see the header of internal/addons/cilium-values.yaml).
const (
	sentinelClusterName = "hydra-sentinel-cluster"
	sentinelPodCIDR     = "10.255.254.0/24"
	sentinelMTU         = 65000
	sentinelOperator    = 1
)

// OperatorReplicas is the Cilium operator's replica count for the cluster: 2
// when it always has at least two nodes, so losing one node does not stop
// IPAM and identity garbage collection, and 1 otherwise, since the replicas
// keep apart and a second could never be scheduled.
func OperatorReplicas(c *config.ClusterConfig) int {
	if c.MinNodes() >= 2 {
		return 2
	}
	return 1
}

var (
	//go:embed templates/cluster.yaml.tmpl
	clusterTemplate string

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
	// PreDrainHookPath, PreDrainHookTimeout, PreDrainScriptBudget and KubectlRequestTimeout are
	// the control plane's VIP hand-over before drain (PET-88).
	PreDrainHookPath      string
	PreDrainHookTimeout   int
	PreDrainScriptBudget  int
	KubectlRequestTimeout int
	KubeVIPImage          string
	// CNILabelKey is empty when hydra installs no CNI (cni: external), so
	// the Cluster carries no label for a ClusterResourceSet to select.
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
		ClusterConfig:         c,
		Spec:                  &c.Spec,
		CLIVersion:            opts.CLIVersion,
		Name:                  c.Metadata.Name,
		Namespace:             c.Metadata.Namespace,
		CPName:                ControlPlaneName(c.Metadata.Name),
		CPSchedulable:         c.Spec.ControlPlane.Schedulable != nil && *c.Spec.ControlPlane.Schedulable,
		MHCMaxUnhealthy:       max(1, (c.Spec.ControlPlane.Replicas-1)/2),
		KubeVIPImage:          KubeVIPImage,
		PreDrainHookPath:      preDrainHookPath,
		PreDrainHookTimeout:   preDrainHookTimeout,
		PreDrainScriptBudget:  preDrainScriptBudget,
		KubectlRequestTimeout: kubectlRequestTimeout,
		ClusterLabelKey:       ClusterLabelKey,
	}
	if HydraCNI(c) {
		d.CNILabelKey = CNILabelKey
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
	d.CPTemplateName = d.CPName + "-" + contentHash(c.Metadata.Name+" "+machineShape(c.Spec.ControlPlane.MachineSize)+" "+cpPreDrainHook())
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
	if HydraCNI(c) {
		cni, err := renderCilium(c)
		if err != nil {
			return nil, err
		}
		out.Write(cni)
	}
	add, err := renderAddons(c)
	if err != nil {
		return nil, err
	}
	out.Write(add)
	return out.Bytes(), nil
}

// helmChartProxy is the part of CAAPH's HelmChartProxy render writes.
type helmChartProxy struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Metadata   objectMeta         `json:"metadata"`
	Spec       helmChartProxySpec `json:"spec"`
}

type objectMeta struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type labelSelector struct {
	MatchLabels map[string]string `json:"matchLabels"`
}

type helmChartProxySpec struct {
	ClusterSelector   labelSelector `json:"clusterSelector"`
	RepoURL           string        `json:"repoURL"`
	ChartName         string        `json:"chartName"`
	Version           string        `json:"version"`
	ReleaseName       string        `json:"releaseName"`
	Namespace         string        `json:"namespace"`
	ReconcileStrategy string        `json:"reconcileStrategy"`
	ValuesTemplate    string        `json:"valuesTemplate"`
	Options           *helmOptions  `json:"options,omitempty"`
}

// helmOptions is the part of CAAPH's HelmOptions render sets.
type helmOptions struct {
	TakeOwnership bool `json:"takeOwnership,omitempty"`
}

// AddonName is the HelmChartProxy's name for one of a cluster's addons.
func AddonName(cluster, addon string) string { return cluster + "-" + addon }

// renderAddons returns one HelmChartProxy per spec.addons entry (ADR-002).
// CAAPH installs each on the Cluster it selects: by the cluster label hydra
// puts on the Cluster, in the Cluster's namespace.
//
// Values are filled in here, not with CAAPH's valuesTemplate templating, so
// the output shows exactly what will be installed and stays deterministic.
// Continuous keeps the release matching the proxy. Helm options are left to
// CAAPH's defaults: CAAPH does not copy a later change of options onto an
// existing release (PET-97), so any option added here later reaches only
// clusters whose addon is installed after that change.
func renderAddons(c *config.ClusterConfig) ([]byte, error) {
	var out bytes.Buffer
	for _, a := range c.Spec.Addons {
		entry, ok := addons.Lookup(a.Name)
		if !ok {
			return nil, fmt.Errorf("addon %q is not in the catalogue", a.Name)
		}
		params, problems := entry.Resolve(a.Params)
		if len(problems) > 0 {
			return nil, fmt.Errorf("addon %s: %s", a.Name, strings.Join(problems, "; "))
		}
		values, err := yaml.Marshal(entry.Values(params))
		if err != nil {
			return nil, err
		}
		hcp := helmChartProxy{
			APIVersion: "addons.cluster.x-k8s.io/v1alpha1",
			Kind:       KindHelmChartProxy,
			Metadata: objectMeta{
				Name:      AddonName(c.Metadata.Name, a.Name),
				Namespace: c.Metadata.Namespace,
				Labels:    map[string]string{ClusterLabelKey: c.Metadata.Name, AddonLabelKey: a.Name},
			},
			Spec: helmChartProxySpec{
				ClusterSelector:   labelSelector{MatchLabels: map[string]string{ClusterLabelKey: c.Metadata.Name}},
				RepoURL:           entry.RepoURL,
				ChartName:         entry.Chart,
				Version:           a.Version,
				ReleaseName:       entry.ReleaseName,
				Namespace:         entry.Namespace,
				ReconcileStrategy: "Continuous",
				ValuesTemplate:    string(values),
			},
		}
		b, err := yaml.Marshal(hcp)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&out, "---\n# Addon %s %s, installed and kept in step by Cluster API Add-on Provider Helm.\n", a.Name, a.Version)
		out.Write(b)
	}
	return out.Bytes(), nil
}

// HydraCNI reports whether hydra installs c's CNI, rather than something
// else (cni: external).
func HydraCNI(c *config.ClusterConfig) bool { return c.Spec.Network.CNI == config.CNIHydra }

// ControlPlaneName is the KubeadmControlPlane's name for a cluster.
func ControlPlaneName(cluster string) string { return cluster + "-cp" }

// CNIName is the name of a cluster's CNI HelmChartProxy, and of the
// ConfigMap and ClusterResourceSet hydra rendered for it before PET-100.
func CNIName(cluster string) string { return AddonName(cluster, addons.Cilium) }

// The control plane's pre-drain hook (PET-88). The script finishes within
// preDrainScriptBudget, plus at most one kubectl request it had started
// (kubectlRequestTimeout), so the provider's timeout never releases the hook
// while it is still changing the node. The hand-over itself waits for
// kube-vip's 5 s lease to lapse and be taken: about 7 s on hardware.
const (
	preDrainHookPath      = "/usr/local/sbin/hydra-pre-drain"
	preDrainHookTimeout   = 60
	preDrainScriptBudget  = 45
	kubectlRequestTimeout = 3
)

// cpPreDrainHook is the control-plane template's preDrainHook, as it enters
// the template's content hash.
func cpPreDrainHook() string {
	return fmt.Sprintf("preDrainHook=%s,%d", preDrainHookPath, preDrainHookTimeout)
}

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

// renderCilium returns the HelmChartProxy that installs and upgrades this
// cluster's Cilium (ADR-002 decision 5): the canonical values with the
// cluster's own filled in, at spec.network.ciliumVersion.
//
// It selects the Cluster by the CNI label, not the cluster label, so a Cluster
// that hydra did not label for its CNI never gets hydra's Cilium on top of
// another one. takeOwnership is set from the first render: CAAPH copies
// options onto a HelmReleaseProxy only when it creates it (PET-97), and it
// lets an existing Cilium with no Helm release record be taken over (PET-101)
// rather than fail. On a fresh cluster there is nothing to take over.
func renderCilium(c *config.ClusterConfig) ([]byte, error) {
	entry, ok := addons.Lookup(addons.Cilium)
	if !ok {
		return nil, fmt.Errorf("the catalogue has no %s", addons.Cilium)
	}
	values, err := CiliumValues(c)
	if err != nil {
		return nil, err
	}
	hcp := helmChartProxy{
		APIVersion: "addons.cluster.x-k8s.io/v1alpha1",
		Kind:       KindHelmChartProxy,
		Metadata: objectMeta{
			Name:      CNIName(c.Metadata.Name),
			Namespace: c.Metadata.Namespace,
			Labels:    map[string]string{ClusterLabelKey: c.Metadata.Name, AddonLabelKey: addons.Cilium},
		},
		Spec: helmChartProxySpec{
			ClusterSelector:   labelSelector{MatchLabels: map[string]string{CNILabelKey: c.Metadata.Name}},
			RepoURL:           entry.RepoURL,
			ChartName:         entry.Chart,
			Version:           c.Spec.Network.CiliumVersion,
			ReleaseName:       entry.ReleaseName,
			Namespace:         entry.Namespace,
			ReconcileStrategy: "Continuous",
			ValuesTemplate:    values,
			Options:           &helmOptions{TakeOwnership: true},
		},
	}
	b, err := yaml.Marshal(hcp)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	fmt.Fprintf(&out, "---\n# The CNI: Cilium %s, installed and kept in step by Cluster API Add-on Provider Helm.\n# Never removable: removing it uninstalls Cilium from the running cluster.\n", c.Spec.Network.CiliumVersion)
	out.Write(b)
	return out.Bytes(), nil
}

// CiliumValues is the canonical values with this cluster's filled in for the
// sentinels, as YAML. Each sentinel must be where the canonical file puts it,
// so a reshaped file fails here rather than installing a sentinel.
func CiliumValues(c *config.ClusterConfig) (string, error) {
	var v map[string]any
	if err := yaml.Unmarshal(addons.CiliumValues, &v); err != nil {
		return "", fmt.Errorf("canonical Cilium values: %w", err)
	}
	for _, s := range []struct {
		path []string
		want any
		to   any
	}{
		{[]string{"cluster", "name"}, sentinelClusterName, c.Metadata.Name},
		{[]string{"ipam", "operator", "clusterPoolIPv4PodCIDRList"}, []any{sentinelPodCIDR}, []any{c.Spec.Network.PodCIDR}},
		{[]string{"MTU"}, sentinelMTU, c.Spec.Network.MTU},
		{[]string{"operator", "replicas"}, sentinelOperator, OperatorReplicas(c)},
	} {
		parent, key := v, s.path[len(s.path)-1]
		for _, p := range s.path[:len(s.path)-1] {
			next, ok := parent[p].(map[string]any)
			if !ok {
				return "", fmt.Errorf("canonical Cilium values have no %s", strings.Join(s.path, "."))
			}
			parent = next
		}
		if got := parent[key]; fmt.Sprint(got) != fmt.Sprint(s.want) {
			return "", fmt.Errorf("canonical Cilium values: %s is %v, want the sentinel %v", strings.Join(s.path, "."), got, s.want)
		}
		parent[key] = s.to
	}
	b, err := yaml.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
