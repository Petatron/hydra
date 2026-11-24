// Package addons is hydra's curated catalogue of platform addons (ADR-002).
//
// An addon is a Helm chart every cluster may run because it is a Hydra
// cluster. hydra.yaml names addons from this catalogue only, picks a version
// from the versions listed here, and sets the params each addon declares;
// render turns each into a HelmChartProxy that Cluster API Add-on Provider
// Helm (CAAPH) reconciles. The catalogue is code: a new addon, version or
// param is a reviewed change, like any other pinned version.
package addons

import (
	_ "embed"
	"fmt"
	"math"
	"slices"
	"strings"
)

// ParamType is the type a param's value must have in hydra.yaml.
type ParamType string

// Param types.
const (
	Bool   ParamType = "bool"
	String ParamType = "string"
	Int    ParamType = "int"
)

// Param is one setting an addon exposes.
type Param struct {
	Name    string
	Type    ParamType
	Default any
	Doc     string
}

// Addon is one catalogue entry.
type Addon struct {
	// Name is how hydra.yaml refers to it.
	Name string
	// RepoURL, Chart and ReleaseName locate the chart and name its release.
	RepoURL, Chart, ReleaseName string
	// Namespace is where the release is installed on the workload cluster.
	Namespace string
	// Versions are the chart versions hydra allows; DefaultVersion is used
	// when hydra.yaml names none.
	Versions       []string
	DefaultVersion string
	Params         []Param
	// Protected marks an addon whose removal must be explicit: deleting its
	// HelmChartProxy uninstalls the release, and CAAPH has no way to keep it.
	// `hydra cluster apply` refuses to run while a protected addon is gone
	// from hydra.yaml, unless --allow-remove names it; an unprotected addon
	// dropped from hydra.yaml is uninstalled with no flag.
	Protected bool
	// CNI marks the cluster's network plugin. It is never removable, not even
	// with --allow-remove: removing it uninstalls the CNI from a running
	// cluster, and nodes still read Ready afterwards (PET-97). It is not
	// listed in spec.addons; spec.network.cni installs it, and
	// spec.network.ciliumVersion picks its version.
	CNI bool
	// Values turns resolved params (every declared param present, typed) into
	// the chart's Helm values.
	Values func(params map[string]any) map[string]any
}

// MetricsServer is the metrics-server addon's name, chart and release.
const MetricsServer = "metrics-server"

// Cilium is the CNI's name, chart and release (cni: hydra).
const Cilium = "cilium"

// CiliumValues is the canonical Cilium values for every Hydra cluster
// (PET-94): a template whose per-cluster values are sentinels, listed in its
// header, that render fills in.
//
//go:embed cilium-values.yaml
var CiliumValues []byte

var catalogue = []Addon{
	{
		Name:        Cilium,
		RepoURL:     "https://helm.cilium.io",
		Chart:       Cilium,
		ReleaseName: Cilium,
		Namespace:   "kube-system",
		// 1.19.1 is what hydra-mgmt and hydra-wl0 run (PET-94); 1.19.11 is
		// the latest 1.19 patch, the kind e2e's upgrade target. Each is
		// checked against the canonical values by hack/check-cilium.sh.
		Versions:       []string{"1.19.1", "1.19.11"},
		DefaultVersion: "1.19.1",
		Protected:      true,
		CNI:            true,
	},
	{
		Name:        MetricsServer,
		RepoURL:     "https://kubernetes-sigs.github.io/metrics-server/",
		Chart:       MetricsServer,
		ReleaseName: MetricsServer,
		Namespace:   "kube-system",
		// Both proven through CAAPH in kind, install and upgrade (PET-97).
		Versions:       []string{"3.13.1", "3.14.0"},
		DefaultVersion: "3.14.0",
		Params: []Param{{
			Name:    "kubeletInsecureTLS",
			Type:    Bool,
			Default: true,
			Doc: "skip verifying kubelets' serving certificates. kubeadm's kubelets serve self-signed " +
				"certificates unless serverTLSBootstrap is set up, so metrics-server cannot verify " +
				"them; traffic stays encrypted either way",
		}},
		Values: func(p map[string]any) map[string]any {
			if p["kubeletInsecureTLS"] == true {
				return map[string]any{"args": []string{"--kubelet-insecure-tls"}}
			}
			return map[string]any{}
		},
	},
}

// Lookup returns the named addon.
func Lookup(name string) (Addon, bool) {
	i := slices.IndexFunc(catalogue, func(a Addon) bool { return a.Name == name })
	if i < 0 {
		return Addon{}, false
	}
	return catalogue[i], true
}

// Names lists the addons spec.addons may name, sorted: the catalogue
// without the CNI.
func Names() []string {
	names := make([]string, 0, len(catalogue))
	for _, a := range catalogue {
		if !a.CNI {
			names = append(names, a.Name)
		}
	}
	slices.Sort(names)
	return names
}

// Resolve checks params against the addon's declared params and returns them
// with defaults filled in, or one message per problem. Numbers decoded from
// YAML arrive as float64; an int param must hold a whole number.
func (a Addon) Resolve(params map[string]any) (map[string]any, []string) {
	var problems []string
	out := make(map[string]any, len(a.Params))
	for k := range params {
		if !slices.ContainsFunc(a.Params, func(p Param) bool { return p.Name == k }) {
			problems = append(problems, fmt.Sprintf("params.%s: unknown; %s takes %s", k, a.Name, a.paramNames()))
		}
	}
	for _, p := range a.Params {
		v, ok := params[p.Name]
		if !ok {
			out[p.Name] = p.Default
			continue
		}
		switch p.Type {
		case Bool:
			if _, ok := v.(bool); !ok {
				problems = append(problems, fmt.Sprintf("params.%s: must be true or false, got %v", p.Name, v))
				continue
			}
		case String:
			s, ok := v.(string)
			if !ok {
				problems = append(problems, fmt.Sprintf("params.%s: must be a string, got %v", p.Name, v))
				continue
			}
			// CAAPH runs valuesTemplate through Go templating.
			if strings.Contains(s, "{{") {
				problems = append(problems, fmt.Sprintf("params.%s: must not contain {{", p.Name))
				continue
			}
		case Int:
			f, ok := v.(float64)
			// Beyond 2^53 a float64 no longer holds every whole number, and
			// converting past int64's range is implementation-defined.
			if !ok || math.Abs(f) > 1<<53 || f != math.Trunc(f) {
				problems = append(problems, fmt.Sprintf("params.%s: must be a whole number, got %v", p.Name, v))
				continue
			}
			v = int64(f)
		}
		out[p.Name] = v
	}
	slices.Sort(problems)
	return out, problems
}

func (a Addon) paramNames() string {
	if len(a.Params) == 0 {
		return "no params"
	}
	names := make([]string, 0, len(a.Params))
	for _, p := range a.Params {
		names = append(names, p.Name)
	}
	return strings.Join(names, ", ")
}
