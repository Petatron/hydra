package bootstrap

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// testKubeconfig points at an address nothing is dialled on: building a
// client must not contact the server.
const testKubeconfig = `apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:1"}}]
contexts: [{name: c, context: {cluster: c, user: u}}]
current-context: c
users: [{name: u, user: {token: x}}]
`

func coreScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := apiextensionsv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// CAAPH installed without clusterctl has its CRDs but no inventory entry: it
// is reported at an unknown version, so bootstrap refuses rather than letting
// clusterctl init install over it.
func TestCAAPHNotInstalledByClusterctl(t *testing.T) {
	crd := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: caaphCRD}}
	got, err := newKubeCluster(providerClient(t, nil, nil, crd)).InstalledProviders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got[caaphInventoryName] != notByClusterctl {
		t.Errorf("got %v", got)
	}
	// The case that matters most: clusterctl never ran, so its inventory
	// kind does not exist at all, yet CAAPH's CRD does.
	noInventory := &meta.NoKindMatchError{GroupKind: providerListGVK.GroupKind()}
	got, err = newKubeCluster(providerClient(t, nil, noInventory, crd)).InstalledProviders(context.Background())
	if err != nil || got[caaphInventoryName] != notByClusterctl {
		t.Errorf("no clusterctl inventory: %v %v", got, err)
	}
	if got, err := newKubeCluster(providerClient(t, nil, noInventory)).InstalledProviders(context.Background()); err != nil || len(got) != 0 {
		t.Errorf("nothing installed at all: %v %v", got, err)
	}
	inv := []*unstructured.Unstructured{provider("caaph-system", "AddonProvider", "helm", CAAPHVersion)}
	got, err = newKubeCluster(providerClient(t, inv, nil, crd)).InstalledProviders(context.Background())
	if err != nil || got[caaphInventoryName] != CAAPHVersion {
		t.Errorf("clusterctl-installed CAAPH: %v %v", got, err)
	}
}

func provider(ns, typ, name, version string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"providerName": name, "type": typ, "version": version}}
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

// providerClient serves the given clusterctl inventory for the ProviderList
// and the real fake store for everything else (Deployments).
func providerClient(t *testing.T, inventory []*unstructured.Unstructured, listErr error, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(coreScheme(t)).WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			u, ok := list.(*unstructured.UnstructuredList)
			if !ok {
				return cl.List(ctx, list, opts...)
			}
			if u.GroupVersionKind() != providerListGVK {
				t.Fatalf("unexpected List of %s", u.GroupVersionKind())
			}
			if listErr != nil {
				return listErr
			}
			for _, p := range inventory {
				u.Items = append(u.Items, *p.DeepCopy())
			}
			return nil
		},
	}).Build()
}

func deployment(ns, name string, available bool) *appsv1.Deployment {
	status := corev1.ConditionFalse
	if available {
		status = corev1.ConditionTrue
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status:     appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: status}}},
	}
}

func TestConnectKube(t *testing.T) {
	c, err := ConnectKube(testKubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	kc, ok := c.(*KubeCluster)
	if !ok || kc.kubeconfig != testKubeconfig || kc.c == nil {
		t.Errorf("want a KubeCluster keeping its kubeconfig for clusterctl, got %#v", c)
	}
	if _, err := ConnectKube("not: [a kubeconfig"); err == nil {
		t.Error("an unparseable kubeconfig must be an error")
	}
}

func TestInstalledProvidersReadsInventory(t *testing.T) {
	ctx := context.Background()
	cm := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: certManagerNamespace, Name: "cert-manager",
		Annotations: map[string]string{"cert-manager.clusterctl.cluster.x-k8s.io/version": CertManagerVersion}}}
	inv := []*unstructured.Unstructured{
		provider("capi-system", "CoreProvider", "cluster-api", CAPIVersion),
		provider("capi-kubeadm-bootstrap-system", "BootstrapProvider", "kubeadm", CAPIVersion),
		provider("capi-kubeadm-control-plane-system", "ControlPlaneProvider", "kubeadm", CAPIVersion),
		provider("hydra-system", "InfrastructureProvider", "hydra", HydraVersion),
		provider("caaph-system", "AddonProvider", "helm", CAAPHVersion),
	}
	got, err := newKubeCluster(providerClient(t, inv, nil, cm)).InstalledProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{CertManagerName: CertManagerVersion}
	for _, p := range Providers() {
		want[p.Name] = p.Version
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q, want %q", k, got[k], v)
		}
	}
}

func TestInstalledProvidersErrors(t *testing.T) {
	ctx := context.Background()
	// A refused inventory list is an error, never "nothing installed": that
	// would make bootstrap reinstall over whatever is there.
	if _, err := newKubeCluster(providerClient(t, nil, errors.New("forbidden"))).InstalledProviders(ctx); err == nil {
		t.Error("a failed inventory list must be an error")
	}
	// The same for cert-manager's Deployments.
	c := fake.NewClientBuilder().WithScheme(coreScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("timeout")
		},
	}).Build()
	if _, err := newKubeCluster(c).InstalledProviders(ctx); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Errorf("a failed cert-manager read must be an error, got %v", err)
	}
}

func TestCertManagerNotInstalledByClusterctl(t *testing.T) {
	// A cert-manager without clusterctl's annotation is reported, so the
	// version check refuses it instead of installing a second one.
	cm := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: certManagerNamespace, Name: "cert-manager"}}
	got, err := newKubeCluster(providerClient(t, nil, nil, cm)).InstalledProviders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got[CertManagerName], "unknown") {
		t.Errorf("want an unknown cert-manager version, got %v", got)
	}
}

func TestUnavailableDeployments(t *testing.T) {
	ctx := context.Background()
	inv := []*unstructured.Unstructured{
		provider("capi-system", "CoreProvider", "cluster-api", CAPIVersion),
		provider("hydra-system", "InfrastructureProvider", "hydra", HydraVersion),
	}
	k := newKubeCluster(providerClient(t, inv, nil,
		deployment(certManagerNamespace, "cert-manager", true),
		deployment("capi-system", "capi-controller-manager", false),
		deployment("unrelated", "app", false), // not a provider namespace
	))
	got, err := k.unavailableDeployments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := "capi-system/capi-controller-manager,hydra-system/(no deployments yet)"; strings.Join(got, ",") != want {
		t.Errorf("got %v, want %s", got, want)
	}
	if _, err := newKubeCluster(providerClient(t, nil, errors.New("forbidden"))).unavailableDeployments(ctx); err == nil {
		t.Error("a failed inventory list must be an error")
	}
}

func TestWaitProvidersAvailable(t *testing.T) {
	inv := []*unstructured.Unstructured{provider("capi-system", "CoreProvider", "cluster-api", CAPIVersion)}
	ready := newKubeCluster(providerClient(t, inv, nil,
		deployment(certManagerNamespace, "cert-manager", true),
		deployment("capi-system", "capi-controller-manager", true)))
	if err := ready.WaitProvidersAvailable(context.Background()); err != nil {
		t.Errorf("all Available must return at once: %v", err)
	}

	// An ended wait names what is still pending...
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pending := newKubeCluster(providerClient(t, inv, nil, deployment(certManagerNamespace, "cert-manager", true),
		deployment("capi-system", "capi-controller-manager", false)))
	err := pending.WaitProvidersAvailable(ctx)
	if err == nil || !strings.Contains(err.Error(), "capi-system/capi-controller-manager") {
		t.Errorf("want the pending Deployment named, got %v", err)
	}
	// ...or the last read error, not a misleading "not Available".
	broken := newKubeCluster(providerClient(t, nil, errors.New("connection refused")))
	if err := broken.WaitProvidersAvailable(ctx); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("want the read error, got %v", err)
	}
}

func TestApplySecret(t *testing.T) {
	ctx := context.Background()
	type call struct{ kind, name, owner string }
	var calls []call
	forced := map[string]bool{}
	c := fake.NewClientBuilder().WithScheme(coreScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			o := &client.ApplyOptions{}
			o.ApplyOptions(opts)
			u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
			if err != nil {
				return err
			}
			kind, _ := u["kind"].(string)
			name, _, _ := unstructured.NestedString(u, "metadata", "name")
			calls = append(calls, call{kind, name, o.FieldManager})
			forced[kind] = o.Force != nil && *o.Force
			return cl.Apply(ctx, obj, opts...)
		},
	}).Build()
	k := newKubeCluster(c)
	data := map[string][]byte{CACertFile: []byte("ca")}
	if err := k.ApplySecret(ctx, "clusters", "pki", data, map[string]string{MoveLabel: ""}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0] != (call{"Namespace", "clusters", FieldManager}) || calls[1] != (call{"Secret", "pki", FieldManager}) {
		t.Fatalf("want the namespace then the Secret applied as %s, got %+v", FieldManager, calls)
	}
	if !forced["Secret"] || forced["Namespace"] {
		t.Errorf("only the Secret takes ownership by force: %v", forced)
	}
	var sec corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: "clusters", Name: "pki"}, &sec); err != nil {
		t.Fatal(err)
	}
	if string(sec.Data[CACertFile]) != "ca" || sec.Type != corev1.SecretTypeOpaque {
		t.Errorf("Secret data/type: %v %s", sec.Data, sec.Type)
	}
	if _, ok := sec.Labels[MoveLabel]; !ok {
		t.Errorf("Secret must carry the move label: %v", sec.Labels)
	}

	failing := newKubeCluster(fake.NewClientBuilder().WithScheme(coreScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
			return errors.New("forbidden")
		},
	}).Build())
	if err := failing.ApplySecret(ctx, "clusters", "pki", data, nil); err == nil || !strings.Contains(err.Error(), "namespace clusters") {
		t.Errorf("a refused namespace must stop before the Secret: %v", err)
	}
}

func TestGetSecretReportsTerminatingAndErrors(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pki", DeletionTimestamp: &now, Finalizers: []string{"x"}}}
	k := newKubeCluster(fake.NewClientBuilder().WithScheme(coreScheme(t)).WithObjects(sec).Build())
	info, err := k.GetSecret(ctx, "ns", "pki")
	if err != nil || info == nil || !info.Terminating {
		t.Errorf("want a terminating Secret, got %+v %v", info, err)
	}
	broken := newKubeCluster(fake.NewClientBuilder().WithScheme(coreScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("forbidden")
		},
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return errors.New("forbidden")
		},
	}).Build())
	if info, err := broken.GetSecret(ctx, "ns", "pki"); err == nil || info != nil {
		t.Errorf("an unreadable Secret is an error, never absent: %v %v", info, err)
	}
	if err := broken.LabelSecret(ctx, "ns", "pki", MoveLabel, ""); err == nil {
		t.Error("a refused patch must be an error")
	}
}

func TestInventoryNameUnknownType(t *testing.T) {
	if got := inventoryName("IPAMProvider", "in-cluster"); got != "IPAMProvider/in-cluster" {
		t.Errorf("got %q", got)
	}
	if got := inventoryName("AddonProvider", "helm"); got != "addon-helm" {
		t.Errorf("addon provider: got %q", got)
	}
}

// Every pinned provider lands in its own InitOptions list; CAAPH is an addon.
func TestInitOptionsPlacesEveryPinnedProvider(t *testing.T) {
	opts, err := initOptions("kc", Providers())
	if err != nil {
		t.Fatal(err)
	}
	if opts.CoreProvider != "cluster-api:"+CAPIVersion || !slices.Contains(opts.InfrastructureProviders, "hydra:"+HydraVersion) ||
		!slices.Equal(opts.AddonProviders, []string{"helm:" + CAAPHVersion}) {
		t.Errorf("got %+v", opts)
	}
}

func TestInitOptionsRejectsUnknownKind(t *testing.T) {
	if _, err := initOptions("kc", []Provider{{Name: "x", Ref: "x:v1", Kind: "ipam"}}); err == nil || !strings.Contains(err.Error(), `unknown provider kind "ipam"`) {
		t.Errorf("got %v", err)
	}
}

// Init must refuse a provider it cannot place before contacting the cluster,
// and leave neither the kubeconfig copy nor the overrides directory behind.
func TestInitRefusesUnknownKindAndCleansUp(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	k := &KubeCluster{kubeconfig: testKubeconfig}
	err := k.Init(context.Background(), []Provider{{Name: "x", Ref: "x:v1", Kind: "ipam"}})
	if err == nil || !strings.Contains(err.Error(), "unknown provider kind") {
		t.Fatalf("got %v", err)
	}
	assertEmptyDir(t, tmp)
}

func TestScratchFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	path, cleanup, err := scratchFile("secret kubeconfig")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "secret kubeconfig" {
		t.Fatalf("content %q %v", b, err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("a kubeconfig copy must be private: %v %v", fi.Mode(), err)
	}
	cleanup()
	assertEmptyDir(t, tmp)

	t.Setenv("TMPDIR", tmp+"/does-not-exist")
	if _, _, err := scratchFile("x"); err == nil {
		t.Error("an unwritable temp dir must be an error")
	}
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("temporary files left behind: %v", names)
	}
}
