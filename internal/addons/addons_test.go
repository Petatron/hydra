package addons

import (
	"slices"
	"strings"
	"testing"
)

func TestCatalogueEntriesAreComplete(t *testing.T) {
	for _, a := range catalogue {
		if a.Name == "" || a.RepoURL == "" || a.Chart == "" || a.ReleaseName == "" || a.Namespace == "" || a.Values == nil {
			t.Errorf("%q: incomplete entry", a.Name)
		}
		if !slices.Contains(a.Versions, a.DefaultVersion) {
			t.Errorf("%s: default version %s is not an allowed version %v", a.Name, a.DefaultVersion, a.Versions)
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
		"params.extra: unknown; x takes on, s, n": {"extra": 1.0},
		"params.on: must be true or false":        {"on": "yes"},
		"params.s: must be a string":              {"s": 1.0},
		"params.s: must not contain {{":           {"s": "{{ .Cluster }}"},
		"params.n: must be a whole number":        {"n": 1.5},
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
