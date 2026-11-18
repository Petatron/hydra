package upgrade

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// pinnedPackages are the packages the rendered node setup installs at the
// exact Kubernetes version (templates/cluster.yaml.tmpl, bootstrap: packages).
var pinnedPackages = []string{"kubelet", "kubeadm", "kubectl"}

// HTTPRemote answers the remote preflight questions from pkgs.k8s.io and
// registry.k8s.io.
type HTTPRemote struct {
	Client *http.Client
	// PackagesURL and TagsURL override the endpoints, for tests.
	PackagesURL func(Version) string
	TagsURL     string
}

func (r *HTTPRemote) client() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (r *HTTPRemote) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.client().Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// HasPackages reads the minor's Debian package index on pkgs.k8s.io, which
// the rendered node setup installs from, for every package it pins to v:
// kubelet, kubeadm and (on control-plane machines) kubectl.
func (r *HTTPRemote) HasPackages(ctx context.Context, v Version) (bool, error) {
	url := fmt.Sprintf("https://pkgs.k8s.io/core:/stable:/%s/deb/Packages", v.MinorString())
	if r.PackagesURL != nil {
		url = r.PackagesURL(v)
	}
	resp, err := r.get(ctx, url)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden:
		// pkgs.k8s.io has no repository for an unreleased minor.
		return false, nil
	case resp.StatusCode != http.StatusOK:
		return false, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	want := strings.TrimPrefix(v.String(), "v") + "-"
	found := map[string]bool{}
	pkg := ""
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "Package: "):
			pkg = strings.TrimPrefix(line, "Package: ")
		case strings.HasPrefix(line, "Version: ") && slices.Contains(pinnedPackages, pkg):
			if strings.HasPrefix(strings.TrimPrefix(line, "Version: "), want) {
				found[pkg] = true
			}
		}
	}
	if err := sc.Err(); err != nil {
		return false, fmt.Errorf("reading %s: %w", url, err)
	}
	for _, p := range pinnedPackages {
		if !found[p] {
			return false, nil
		}
	}
	return true, nil
}

// AutoscalerTags lists registry.k8s.io's Cluster Autoscaler tags for v's
// minor, oldest first.
func (r *HTTPRemote) AutoscalerTags(ctx context.Context, v Version) ([]string, error) {
	url := r.TagsURL
	if url == "" {
		url = "https://registry.k8s.io/v2/autoscaling/cluster-autoscaler/tags/list"
	}
	resp, err := r.get(ctx, url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	var body struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("reading %s: %w", url, err)
	}
	var out []Version
	for _, t := range body.Tags {
		if tv, err := ParseVersion(t); err == nil && tv.Minor == v.Minor {
			out = append(out, tv)
		}
	}
	slices.SortFunc(out, func(a, b Version) int {
		switch {
		case a.Less(b):
			return -1
		case b.Less(a):
			return 1
		}
		return 0
	})
	tags := make([]string, len(out))
	for i, tv := range out {
		tags[i] = tv.String()
	}
	return tags, nil
}
