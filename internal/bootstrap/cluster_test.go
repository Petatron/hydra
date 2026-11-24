package bootstrap

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterctl "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
	clusterctlconfig "sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// listResult is what the stub returns for one kind's List: objects, or an error.
type listResult struct {
	items []string // names in namespace "ns"
	err   error
}

// stubCluster builds a KubeCluster whose List of each managed kind and Get of
// each CRD return exactly what the test says.
func stubCluster(t *testing.T, lists map[string]listResult, crds map[string]error) *KubeCluster {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
			u, ok := list.(*unstructured.UnstructuredList)
			if !ok {
				return nil // typed lists (Deployments) are empty
			}
			kind := strings.TrimSuffix(u.GroupVersionKind().Kind, "List")
			r, ok := lists[kind]
			if !ok {
				t.Fatalf("unexpected List of %s", kind)
			}
			if r.err != nil {
				return r.err
			}
			for _, n := range r.items {
				o := unstructured.Unstructured{}
				o.SetNamespace("ns")
				o.SetName(n)
				u.Items = append(u.Items, o)
			}
			return nil
		},
		Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
			if _, ok := obj.(*apiextensionsv1.CustomResourceDefinition); !ok {
				return errors.New("unexpected Get")
			}
			if err, ok := crds[key.Name]; ok {
				return err
			}
			return apierrors.NewNotFound(schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}, key.Name)
		},
	}).Build()
	return newKubeCluster(c)
}

func noKind(kind string) error {
	return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Kind: kind}}
}

// discoveryNoMatch is what controller-runtime's lazy mapper returns the first
// time a group-version is missing under aggregated discovery.
func discoveryNoMatch() error {
	return &apiutil.ErrResourceDiscoveryFailed{
		schema.GroupVersion{Group: "cluster.x-k8s.io", Version: "v1beta2"}: &meta.NoResourceMatchError{},
	}
}

func allAbsent() map[string]listResult {
	return map[string]listResult{
		"Cluster": {err: noKind("Cluster")}, "Machine": {err: noKind("Machine")},
		"HydraCluster": {err: noKind("HydraCluster")}, "HydraMachine": {err: noKind("HydraMachine")},
	}
}

// The delete decision table, against the real KubeCluster.Clusters.
func TestClustersDecisionTable(t *testing.T) {
	ctx := context.Background()
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "clusters"}, "", errors.New("rbac"))
	cases := []struct {
		name    string
		lists   map[string]listResult
		crds    map[string]error
		want    []string
		wantErr string
	}{
		{name: "no CAPI at all is none", lists: allAbsent()},
		{name: "discovery-failed NoMatch with CRD absent is none", lists: func() map[string]listResult {
			l := allAbsent()
			l["Cluster"] = listResult{err: discoveryNoMatch()}
			return l
		}()},
		{name: "objects are listed", lists: map[string]listResult{
			"Cluster": {items: []string{"a"}}, "Machine": {items: []string{"a-cp-1"}},
			"HydraCluster": {items: []string{"a"}}, "HydraMachine": {},
		}, want: []string{"Cluster ns/a", "Machine ns/a-cp-1", "HydraCluster ns/a"}},
		{name: "machines left after the Cluster is gone still count", lists: map[string]listResult{
			"Cluster": {}, "Machine": {}, "HydraCluster": {}, "HydraMachine": {items: []string{"a-cp-1"}},
		}, want: []string{"HydraMachine ns/a-cp-1"}},
		{name: "NoMatch while the CRD exists refuses", lists: allAbsent(),
			crds: map[string]error{"machines.cluster.x-k8s.io": nil}, wantErr: "does not serve"},
		{name: "NoMatch and the CRD check fails refuses", lists: allAbsent(),
			crds: map[string]error{"clusters.cluster.x-k8s.io": errors.New("boom")}, wantErr: "checking CRD"},
		{name: "forbidden refuses", lists: map[string]listResult{"Cluster": {err: forbidden}}, wantErr: "listing clusters.cluster.x-k8s.io"},
		{name: "server error refuses", lists: map[string]listResult{"Cluster": {err: apierrors.NewInternalError(errors.New("x"))}}, wantErr: "listing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := stubCluster(t, tc.lists, tc.crds).Clusters(ctx)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v (%v)", tc.wantErr, err, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// Delete must never call Kind.Delete in any refusing row.
func TestDeleteNeverDeletesWhenItCannotTell(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(k *fakeKind, b *Bootstrapper)
	}{
		{"exists check fails", func(k *fakeKind, _ *Bootstrapper) { k.err = errors.New("docker down") }},
		{"connect fails", func(_ *fakeKind, b *Bootstrapper) {
			b.Connect = func(string) (Cluster, error) { return nil, errors.New("unreachable") }
		}},
		{"listing fails", func(_ *fakeKind, b *Bootstrapper) {
			b.Connect = func(string) (Cluster, error) {
				return stubCluster(t, map[string]listResult{"Cluster": {err: errors.New("timeout")}}, nil), nil
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, _, b, _ := newFakes()
			k.clusters["hydra-bootstrap-lab"] = true
			tc.setup(k, b)
			if err := b.Delete(ctx, "lab", false); err == nil {
				t.Fatal("want a refusal")
			}
			if len(k.deleted) != 0 {
				t.Error("deleted a cluster it could not check")
			}
		})
	}
}

func TestDeploymentAvailable(t *testing.T) {
	cond := func(s corev1.ConditionStatus) []appsv1.DeploymentCondition {
		return []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: s}}
	}
	for _, tc := range []struct {
		name string
		d    appsv1.Deployment
		want bool
	}{
		{"available", appsv1.Deployment{Status: appsv1.DeploymentStatus{Conditions: cond(corev1.ConditionTrue)}}, true},
		{"unavailable", appsv1.Deployment{Status: appsv1.DeploymentStatus{Conditions: cond(corev1.ConditionFalse)}}, false},
		{"no condition yet", appsv1.Deployment{}, false},
		{"stale status", appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Generation: 2},
			Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Conditions: cond(corev1.ConditionTrue)}}, false},
	} {
		if got := deploymentAvailable(&tc.d); got != tc.want {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
}

func TestCertManagerVersionChecked(t *testing.T) {
	k, c, b, _ := newFakes()
	k.clusters["hydra-bootstrap-lab"] = true
	for _, p := range Providers() {
		c.installed[p.Name] = p.Version
	}
	c.installed[CertManagerName] = "v1.17.0"
	err := b.Create(context.Background(), cfg(t, cfgYAML), Options{Credentials: testCredentials(t)})
	if err == nil || !strings.Contains(err.Error(), "cert-manager is v1.17.0") {
		t.Fatalf("got %v", err)
	}
}

func TestFirstRunNeedsCredentialsBeforeCreating(t *testing.T) {
	k, _, b, _ := newFakes()
	if err := b.Create(context.Background(), cfg(t, cfgYAML), Options{}); err == nil {
		t.Fatal("want an error")
	}
	if len(k.created) != 0 {
		t.Error("must fail before creating the kind cluster")
	}
}

func TestComponentsURLIsPinned(t *testing.T) {
	// clusterctl's repository form: .../releases/{tag}/{file}. Not "latest"
	// (unpinned), and not "releases/download/" (a download link, which
	// clusterctl would parse as version "download").
	if !strings.HasPrefix(HydraComponentsURL, "https://github.com/Petatron/cluster-api-provider-hydra/releases/"+HydraVersion+"/") ||
		strings.Contains(HydraComponentsURL, "latest") || strings.Contains(HydraComponentsURL, "/download/") {
		t.Errorf("components URL must be clusterctl's repository URL for the pinned release: %s", HydraComponentsURL)
	}
}

func TestClusterctlReaderIsolatesConfig(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	r, cleanup, err := clusterctlReader()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := r.Get(overridesFolderKey)
	if err != nil || dir == "" {
		t.Fatalf("overrides folder not set: %q %v", dir, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("overrides folder must exist and be empty: %v %v", entries, err)
	}
	if tok, err := r.Get(clusterctlconfig.GitHubTokenVariable); err != nil || tok != "test-token" {
		t.Errorf("GitHub token must be set under clusterctl's key: %q %v", tok, err)
	}
	if cm, err := r.Get(clusterctlconfig.CertManagerConfigKey); err != nil || !strings.Contains(cm, CertManagerVersion) {
		t.Errorf("cert-manager pin: %q %v", cm, err)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("cleanup must remove the overrides folder")
	}
}

func TestGetAndLabelSecret(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pki"},
		Data:       map[string][]byte{CACertFile: []byte("a"), ClientCertFile: []byte("b"), ClientKeyFile: []byte("c")},
	}
	k := newKubeCluster(fake.NewClientBuilder().WithScheme(scheme).WithObjects(sec).Build())

	if info, err := k.GetSecret(ctx, "ns", "absent"); err != nil || info != nil {
		t.Fatalf("absent Secret: %v %v", info, err)
	}
	info, err := k.GetSecret(ctx, "ns", "pki")
	if err != nil || strings.Join(info.Keys, ",") != "cacert.pem,clientcert.pem,clientkey.pem" || info.Terminating {
		t.Fatalf("got %+v %v", info, err)
	}
	if err := k.LabelSecret(ctx, "ns", "pki", MoveLabel, ""); err != nil {
		t.Fatal(err)
	}
	var got corev1.Secret
	if err := k.c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "pki"}, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Labels[MoveLabel]; !ok || string(got.Data[ClientKeyFile]) != "c" {
		t.Errorf("label not added, or data changed: %v %v", got.Labels, got.Data)
	}
}

func TestInitOptionsNeverLetClusterctlDefault(t *testing.T) {
	// Resuming with only the core provider missing: clusterctl would add the
	// latest kubeadm providers to empty lists. The no-op entry stops that.
	opts, err := initOptions("kc", []Provider{Providers()[0]})
	if err != nil {
		t.Fatal(err)
	}
	if opts.CoreProvider != "cluster-api:"+CAPIVersion {
		t.Errorf("core: %q", opts.CoreProvider)
	}
	for name, l := range map[string][]string{"bootstrap": opts.BootstrapProviders, "control-plane": opts.ControlPlaneProviders} {
		if len(l) != 1 || l[0] != clusterctl.NoopProvider {
			t.Errorf("%s list must be only the no-op entry, got %v", name, l)
		}
	}
	all, err := initOptions("kc", Providers())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(all.BootstrapProviders, ",") != clusterctl.NoopProvider+",kubeadm:"+CAPIVersion ||
		strings.Join(all.InfrastructureProviders, ",") != "hydra:"+HydraVersion {
		t.Errorf("pinned providers must follow the no-op entry: %+v", all)
	}
}

func TestCertManagerCheckedWithoutInventory(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cm := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "cert-manager", Name: "cert-manager",
		Annotations: map[string]string{"cert-manager.clusterctl.cluster.x-k8s.io/version": "v1.17.0"}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*unstructured.UnstructuredList); ok {
				return noKind("Provider") // clusterctl has never run
			}
			return cl.List(ctx, list, opts...)
		},
	}).Build()
	got, err := newKubeCluster(c).InstalledProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got[CertManagerName] != "v1.17.0" {
		t.Errorf("cert-manager must be reported even without clusterctl's inventory: %v", got)
	}
}
