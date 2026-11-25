package addons

import (
	"slices"
	"strings"
	"testing"
)

func TestCatalogueEntriesAreComplete(t *testing.T) {
	for _, a := range catalogue {
		if a.Name == "" || a.RepoURL == "" || a.Chart == "" || a.ReleaseName == "" || a.Namespace == "" || (a.Values == nil) != a.CNI {
			t.Errorf("%q: incomplete entry", a.Name)
		}
		if !slices.Contains(a.Versions, a.DefaultVersion) {
			t.Errorf("%s: default version %s is not an allowed version %v", a.Name, a.DefaultVersion, a.Versions)
		}
		for _, p := range a.Params {
			var ok bool
			switch p.Type {
			case Bool:
				_, ok = p.Default.(bool)
			case String:
				_, ok = p.Default.(string)
			case Int:
				_, ok = p.Default.(int64)
			}
			if !ok {
				t.Errorf("%s: param %s default %v (%T) is not a %s", a.Name, p.Name, p.Default, p.Default, p.Type)
			}
		}
		params, problems := a.Resolve(nil)
		if len(problems) > 0 {
			t.Errorf("%s: defaults do not resolve: %v", a.Name, problems)
		}
		if len(params) != len(a.Params) {
			t.Errorf("%s: defaults resolve to %v", a.Name, params)
		}
	}
	if !slices.IsSorted(Names()) {
		t.Errorf("Names not sorted: %v", Names())
	}
}

func TestLookup(t *testing.T) {
	if _, ok := Lookup("metrics-server"); !ok {
		t.Error("metrics-server missing")
	}
	if _, ok := Lookup("nope"); ok {
		t.Error("unknown addon found")
	}
}

func TestResolve(t *testing.T) {
	a := Addon{Name: "x", Params: []Param{
		{Name: "on", Type: Bool, Default: true},
		{Name: "s", Type: String, Default: "d"},
		{Name: "n", Type: Int, Default: int64(1)},
	}}
	got, problems := a.Resolve(map[string]any{"on": false, "s": "v", "n": float64(3)})
	if len(problems) > 0 || got["on"] != false || got["s"] != "v" || got["n"] != int64(3) {
		t.Errorf("got %v %v", got, problems)
	}
	got, _ = a.Resolve(nil)
	if got["on"] != true || got["s"] != "d" || got["n"] != int64(1) {
		t.Errorf("defaults: %v", got)
	}
	for in, want := range map[string]map[string]any{
		"params.extra: unknown; x takes on, s, n":      {"extra": 1.0},
		"params.on: must be true or false":             {"on": "yes"},
		"params.s: must be a string":                   {"s": 1.0},
		"params.s: must not contain {{":                {"s": "{{ .Cluster }}"},
		"params.n: must be a whole number":             {"n": 1.5},
		"params.n: must be a whole number, got 1e+300": {"n": 1e300},
	} {
		if _, problems := a.Resolve(want); len(problems) != 1 || !strings.HasPrefix(problems[0], in) {
			t.Errorf("%v: got %v, want %q", want, problems, in)
		}
	}
}

func TestMetricsServerValues(t *testing.T) {
	a, _ := Lookup("metrics-server")
	on, _ := a.Resolve(nil)
	if args, _ := a.Values(on)["args"].([]string); !slices.Equal(args, []string{"--kubelet-insecure-tls"}) {
		t.Errorf("default values: %v", a.Values(on))
	}
	off, _ := a.Resolve(map[string]any{"kubeletInsecureTLS": false})
	if len(a.Values(off)) != 0 {
		t.Errorf("kubeletInsecureTLS false: %v", a.Values(off))
	}
}

// The CNI is protected, never offered in spec.addons, and its values are the
// embedded canonical set: a template with every sentinel present, and nothing
// CAAPH's valuesTemplate templating would expand.
func TestCilium(t *testing.T) {
	a, ok := Lookup(Cilium)
	if !ok || !a.CNI || !a.Protected {
		t.Fatalf("cilium: %+v", a)
	}
	if slices.Contains(Names(), Cilium) {
		t.Errorf("Names lists the CNI: %v", Names())
	}
	cnis := 0
	for _, e := range catalogue {
		if e.CNI {
			cnis++
		}
	}
	if cnis != 1 {
		t.Errorf("%d CNI entries, want 1", cnis)
	}
	v := string(CiliumValues)
	for _, s := range []string{"name: hydra-sentinel-cluster", "- 10.255.254.0/24", "MTU: 65000", "replicas: 1"} {
		if !strings.Contains(v, s) {
			t.Errorf("canonical values lack the sentinel %q", s)
		}
	}
	if strings.Contains(v, "{{") {
		t.Error("canonical values contain {{, which CAAPH would template")
	}
}
