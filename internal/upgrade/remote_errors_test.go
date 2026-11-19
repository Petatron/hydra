package upgrade

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// roundTrip answers every request in-process, recording its URL.
type roundTrip struct {
	urls   []string
	status int
	body   string
}

func (r *roundTrip) RoundTrip(req *http.Request) (*http.Response, error) {
	r.urls = append(r.urls, req.URL.String())
	return &http.Response{StatusCode: r.status, Status: http.StatusText(r.status), Body: io.NopCloser(strings.NewReader(r.body)), Request: req}, nil
}

// Without overrides, the real endpoints are asked, through the given client.
func TestHTTPRemoteDefaultEndpoints(t *testing.T) {
	ctx := context.Background()
	rt := &roundTrip{status: http.StatusOK, body: index("kubelet=1.36.6-1.1", "kubeadm=1.36.6-1.1", "kubectl=1.36.6-1.1")}
	r := &HTTPRemote{Client: &http.Client{Transport: rt}}
	if ok, err := r.HasPackages(ctx, v("v1.36.6")); err != nil || !ok {
		t.Fatalf("got %v, %v", ok, err)
	}
	rt.body = `{"tags":["v1.36.2","v1.36.2","v1.36.0"]}`
	tags, err := r.AutoscalerTags(ctx, v("v1.36.6"))
	if err != nil || strings.Join(tags, " ") != "v1.36.0 v1.36.2 v1.36.2" {
		t.Fatalf("got %v, %v", tags, err)
	}
	want := []string{
		"https://pkgs.k8s.io/core:/stable:/v1.36/deb/Packages",
		"https://registry.k8s.io/v2/autoscaling/cluster-autoscaler/tags/list",
	}
	if strings.Join(rt.urls, " ") != strings.Join(want, " ") {
		t.Errorf("asked %v, want %v", rt.urls, want)
	}
	if (&HTTPRemote{}).client() == nil {
		t.Error("a default client is used when none is given")
	}
}

// Any failure to ask is an error, never "not published".
func TestHTTPRemoteFailures(t *testing.T) {
	ctx := context.Background()
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	for _, tc := range []struct {
		name string
		r    *HTTPRemote
	}{
		{"unreachable", &HTTPRemote{PackagesURL: func(Version) string { return closed.URL }, TagsURL: closed.URL}},
		{"malformed URL", &HTTPRemote{PackagesURL: func(Version) string { return "http://bad host/" }, TagsURL: "http://bad host/"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if ok, err := tc.r.HasPackages(ctx, v("v1.36.6")); err == nil || ok {
				t.Errorf("packages: got %v, %v", ok, err)
			}
			if tags, err := tc.r.AutoscalerTags(ctx, v("v1.36.6")); err == nil || tags != nil {
				t.Errorf("tags: got %v, %v", tags, err)
			}
		})
	}
}

func TestAutoscalerTagsBadResponses(t *testing.T) {
	ctx := context.Background()
	if _, err := packagesServer(t, http.StatusInternalServerError, "").AutoscalerTags(ctx, v("v1.36.6")); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("got %v", err)
	}
	if _, err := packagesServer(t, http.StatusOK, "{not json").AutoscalerTags(ctx, v("v1.36.6")); err == nil || !strings.Contains(err.Error(), "reading ") {
		t.Errorf("got %v", err)
	}
}

// A Packages line beyond the scanner's limit is an error, not a short index.
func TestHasPackagesUnreadableIndex(t *testing.T) {
	huge := "Description: " + strings.Repeat("x", 2*1024*1024) + "\n"
	ok, err := packagesServer(t, http.StatusOK, huge).HasPackages(context.Background(), v("v1.36.6"))
	if err == nil || ok || !strings.Contains(err.Error(), "reading ") {
		t.Fatalf("got %v, %v", ok, err)
	}
}
