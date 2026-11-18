package upgrade

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// index is a pkgs.k8s.io-style Packages file holding the given
// package=version entries.
func index(entries ...string) string {
	var b strings.Builder
	for _, e := range entries {
		name, ver, _ := strings.Cut(e, "=")
		b.WriteString("Package: " + name + "\nVersion: " + ver + "\nArchitecture: amd64\n\n")
	}
	return b.String()
}

func packagesServer(t *testing.T, status int, body string) *HTTPRemote {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &HTTPRemote{PackagesURL: func(Version) string { return srv.URL }, TagsURL: srv.URL}
}

func TestHasPackages(t *testing.T) {
	all := []string{"kubelet=1.36.6-1.1", "kubeadm=1.36.6-1.1", "kubectl=1.36.6-1.1"}
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"all three at the target", http.StatusOK, index(append([]string{"cri-tools=1.36.0-1.1"}, all...)...), true},
		// The control-plane bootstrap pins kubectl too.
		{"kubectl missing", http.StatusOK, index(all[:2]...), false},
		{"kubeadm missing", http.StatusOK, index(all[0], all[2]), false},
		// 1.36.6- must not match 1.36.60, nor another patch.
		{"only other patches", http.StatusOK, index("kubelet=1.36.60-1.1", "kubeadm=1.36.5-1.1", "kubectl=1.36.7-1.1"), false},
		{"an unreleased minor (403)", http.StatusForbidden, "", false},
		{"an unreleased minor (404)", http.StatusNotFound, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := packagesServer(t, tc.status, tc.body).HasPackages(context.Background(), v("v1.36.6"))
			if err != nil || got != tc.want {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	if _, err := packagesServer(t, http.StatusInternalServerError, "").HasPackages(context.Background(), v("v1.36.6")); err == nil {
		t.Error("a server error must be an error, never \"not published\"")
	}
}

func TestAutoscalerTags(t *testing.T) {
	r := packagesServer(t, http.StatusOK, `{"tags":["v1.35.3","v1.36.2","v1.36.0","v1.36.10","v1.37.0","latest","v1.36.1-rc.0"]}`)
	got, err := r.AutoscalerTags(context.Background(), v("v1.36.6"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "v1.36.0 v1.36.2 v1.36.10" {
		t.Errorf("got %v", got)
	}
}
