package bootstrap

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Petatron/hydra/internal/config"
)

type fakeKind struct {
	clusters map[string]bool
	created  []string
	deleted  []string
	err      error
}

func (f *fakeKind) Exists(name string) (bool, error) { return f.clusters[name], f.err }
func (f *fakeKind) Create(name, _ string) error {
	f.created = append(f.created, name)
	f.clusters[name] = true
	return nil
}
func (f *fakeKind) Delete(name string) error {
	f.deleted = append(f.deleted, name)
	delete(f.clusters, name)
	return nil
}
func (f *fakeKind) KubeConfig(name string) (string, error) { return "kubeconfig-for-" + name, nil }

type fakeCluster struct {
	installed map[string]string
	inited    [][]Provider
	initErr   error
	secrets   map[string]map[string][]byte
	labels    map[string]map[string]string
	clusters  []string
	waits     int
	waitErr   error
	// secretInfo overrides what GetSecret reports for a Secret.
	secretInfo map[string]*SecretInfo
	labelled   []string
}

func (f *fakeCluster) WaitProvidersAvailable(context.Context) error {
	f.waits++
	return f.waitErr
}

func (f *fakeCluster) InstalledProviders(context.Context) (map[string]string, error) {
	return f.installed, nil
}
func (f *fakeCluster) Init(_ context.Context, ps []Provider) error {
	if f.initErr != nil {
		return f.initErr
	}
	f.inited = append(f.inited, ps)
	for _, p := range ps {
		f.installed[p.Name] = p.Version
	}
	return nil
}
func (f *fakeCluster) GetSecret(_ context.Context, ns, name string) (*SecretInfo, error) {
	if info, ok := f.secretInfo[ns+"/"+name]; ok {
		return info, nil
	}
	data, ok := f.secrets[ns+"/"+name]
	if !ok {
		return nil, nil
	}
	info := &SecretInfo{Labels: f.labels[ns+"/"+name]}
	for k := range data {
		info.Keys = append(info.Keys, k)
	}
	return info, nil
}

func (f *fakeCluster) LabelSecret(_ context.Context, ns, name, key, value string) error {
	f.labelled = append(f.labelled, ns+"/"+name+":"+key)
	if info, ok := f.secretInfo[ns+"/"+name]; ok {
		if info.Labels == nil {
			info.Labels = map[string]string{}
		}
		info.Labels[key] = value
	}
	return nil
}
func (f *fakeCluster) ApplySecret(_ context.Context, ns, name string, data map[string][]byte, labels map[string]string) error {
	f.secrets[ns+"/"+name] = data
	f.labels[ns+"/"+name] = labels
	return nil
}
func (f *fakeCluster) Clusters(context.Context) ([]string, error) { return f.clusters, nil }

func newFakes() (*fakeKind, *fakeCluster, *Bootstrapper, *bytes.Buffer) {
	k := &fakeKind{clusters: map[string]bool{}}
	c := &fakeCluster{installed: map[string]string{}, secrets: map[string]map[string][]byte{}, labels: map[string]map[string]string{}}
	var out bytes.Buffer
	b := &Bootstrapper{Kind: k, Connect: func(string) (Cluster, error) { return c, nil }, Out: &out}
	return k, c, b, &out
}

const cfgYAML = `
apiVersion: hydra.io/v1alpha1
kind: ClusterConfig
metadata:
  name: lab
  namespace: clusters
spec:
  hypervisor:
    address: hv.example:16514
  storagePool: pool
  image:
    name: img.qcow2
  network:
    controlPlaneEndpoint: 10.0.0.10
    bridges: [br0]
`

func cfg(t *testing.T, yaml string) *config.ClusterConfig {
	t.Helper()
	c, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFreshCreate(t *testing.T) {
	k, c, b, _ := newFakes()
	creds := testCredentials(t)
	if err := b.Create(context.Background(), cfg(t, cfgYAML), Options{Credentials: creds}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(k.created, ",") != "hydra-bootstrap-lab" {
		t.Errorf("created %v", k.created)
	}
	if len(c.inited) != 1 || len(c.inited[0]) != 4 {
		t.Fatalf("want one init of all four providers, got %v", c.inited)
	}
	data := c.secrets["clusters/lab-libvirt-pki"]
	if data == nil || len(data) != 3 {
		t.Fatalf("credentials Secret not written with three keys: %v", c.secrets)
	}
	if _, ok := c.labels["clusters/lab-libvirt-pki"][MoveLabel]; !ok {
		t.Error("credentials Secret must carry the clusterctl move label")
	}
}

func TestRerunIsANoOp(t *testing.T) {
	k, c, b, out := newFakes()
	ctx := context.Background()
	if err := b.Create(ctx, cfg(t, cfgYAML), Options{Credentials: testCredentials(t)}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := b.Create(ctx, cfg(t, cfgYAML), Options{}); err != nil {
		t.Fatal(err)
	}
	if len(k.created) != 1 || len(c.inited) != 1 {
		t.Errorf("a rerun must create and install nothing: created %v, inits %d", k.created, len(c.inited))
	}
	if !strings.Contains(out.String(), "reusing") {
		t.Errorf("rerun should say it reuses the cluster:\n%s", out)
	}
}

func TestResumesPartialInstall(t *testing.T) {
	k, c, b, _ := newFakes()
	k.clusters["hydra-bootstrap-lab"] = true
	c.installed["cluster-api"] = CAPIVersion
	c.installed["bootstrap-kubeadm"] = CAPIVersion
	if err := b.Create(context.Background(), cfg(t, cfgYAML), Options{Credentials: testCredentials(t)}); err != nil {
		t.Fatal(err)
	}
	if len(k.created) != 0 {
		t.Error("an existing kind cluster must be reused")
	}
	if len(c.inited) != 1 {
		t.Fatalf("inits: %v", c.inited)
	}
	got := make([]string, 0, len(c.inited[0]))
	for _, p := range c.inited[0] {
		got = append(got, p.Name)
	}
	if strings.Join(got, ",") != "control-plane-kubeadm,infrastructure-hydra" {
		t.Errorf("only the missing providers should be installed, got %v", got)
	}
}

func TestRefusesWrongVersion(t *testing.T) {
	k, c, b, _ := newFakes()
	k.clusters["hydra-bootstrap-lab"] = true
	c.installed["cluster-api"] = "v1.13.2"
	err := b.Create(context.Background(), cfg(t, cfgYAML), Options{Credentials: testCredentials(t)})
	if err == nil || !strings.Contains(err.Error(), "cluster-api is v1.13.2, this CLI pins "+CAPIVersion) {
		t.Fatalf("want a version error, got %v", err)
	}
	if len(c.inited) != 0 || len(c.secrets) != 0 {
		t.Error("nothing may be installed or written after a version mismatch")
	}
}

func TestInitFailureIsResumable(t *testing.T) {
	_, c, b, _ := newFakes()
	c.initErr = errors.New("boom")
	err := b.Create(context.Background(), cfg(t, cfgYAML), Options{Credentials: testCredentials(t)})
	if err == nil || !strings.Contains(err.Error(), "run the same command again") {
		t.Fatalf("got %v", err)
	}
}

func TestNeedsHypervisor(t *testing.T) {
	_, _, b, _ := newFakes()
	noHV := strings.Replace(cfgYAML, "  hypervisor:\n    address: hv.example:16514\n", "", 1)
	if err := b.Create(context.Background(), cfg(t, noHV), Options{}); !errors.Is(err, ErrNoHypervisor) {
		t.Fatalf("got %v", err)
	}
}

func TestNeedsCredentialsTheFirstTime(t *testing.T) {
	_, _, b, _ := newFakes()
	err := b.Create(context.Background(), cfg(t, cfgYAML), Options{})
	if err == nil || !strings.Contains(err.Error(), "--hypervisor-credentials") {
		t.Fatalf("got %v", err)
	}
}

func TestDeleteRefusesWhileManagingClusters(t *testing.T) {
	k, c, b, _ := newFakes()
	k.clusters["hydra-bootstrap-lab"] = true
	c.clusters = []string{"Cluster clusters/lab"}
	err := b.Delete(context.Background(), "lab", false)
	if err == nil || !strings.Contains(err.Error(), "clusters/lab") {
		t.Fatalf("want a refusal naming the cluster, got %v", err)
	}
	if len(k.deleted) != 0 {
		t.Error("must not delete")
	}
	if err := b.Delete(context.Background(), "lab", true); err != nil {
		t.Fatal(err)
	}
	if len(k.deleted) != 1 {
		t.Error("--force must delete")
	}
}

func TestDeleteAbsentIsANoOp(t *testing.T) {
	k, _, b, out := newFakes()
	if err := b.Delete(context.Background(), "lab", false); err != nil {
		t.Fatal(err)
	}
	if len(k.deleted) != 0 || !strings.Contains(out.String(), "nothing to delete") {
		t.Errorf("deleted %v, out %q", k.deleted, out)
	}
}

func TestKubeConfigNeedsCluster(t *testing.T) {
	k, _, b, _ := newFakes()
	if _, err := b.KubeConfig("lab"); err == nil {
		t.Error("no cluster: want an error")
	}
	k.clusters["hydra-bootstrap-lab"] = true
	if kc, err := b.KubeConfig("lab"); err != nil || kc != "kubeconfig-for-hydra-bootstrap-lab" {
		t.Errorf("%q %v", kc, err)
	}
}

func TestInventoryNames(t *testing.T) {
	for typ, want := range map[string]string{
		"CoreProvider": "cluster-api", "BootstrapProvider": "bootstrap-cluster-api",
		"ControlPlaneProvider": "control-plane-cluster-api", "InfrastructureProvider": "infrastructure-cluster-api",
	} {
		if got := inventoryName(typ, "cluster-api"); got != want {
			t.Errorf("%s: %s", typ, got)
		}
	}
	// The names Providers() pins must be the ones InstalledProviders produces.
	have := map[string]bool{inventoryName("CoreProvider", "cluster-api"): true, inventoryName("BootstrapProvider", "kubeadm"): true,
		inventoryName("ControlPlaneProvider", "kubeadm"): true, inventoryName("InfrastructureProvider", "hydra"): true}
	for _, p := range Providers() {
		if !have[p.Name] {
			t.Errorf("pinned provider %s does not match any inventory name", p.Name)
		}
	}
}

// --- credentials ---

func writePEM(t *testing.T, dir, name, typ string, der []byte) {
	t.Helper()
	b := pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func selfSigned(t *testing.T) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der, key
}

func credentialsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	ca, _ := selfSigned(t)
	cert, key := selfSigned(t)
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, dir, CACertFile, "CERTIFICATE", ca)
	writePEM(t, dir, ClientCertFile, "CERTIFICATE", cert)
	writePEM(t, dir, ClientKeyFile, "EC PRIVATE KEY", kder)
	return dir
}

func testCredentials(t *testing.T) *Credentials {
	t.Helper()
	c, err := LoadCredentials(credentialsDir(t))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLoadCredentialsRejects(t *testing.T) {
	dir := credentialsDir(t)
	if err := os.Remove(filepath.Join(dir, CACertFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredentials(dir); err == nil {
		t.Error("missing cacert.pem accepted")
	}

	dir = credentialsDir(t)
	_, otherKey := selfSigned(t)
	kder, _ := x509.MarshalECPrivateKey(otherKey)
	writePEM(t, dir, ClientKeyFile, "EC PRIVATE KEY", kder)
	_, err := LoadCredentials(dir)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("mismatched key accepted: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Error("error text must not carry key material")
	}

	dir = credentialsDir(t)
	if err := os.WriteFile(filepath.Join(dir, CACertFile), []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredentials(dir); err == nil {
		t.Error("non-PEM cacert accepted")
	}
}

func TestWaitsForProvidersEvenWhenInstalled(t *testing.T) {
	k, c, b, _ := newFakes()
	k.clusters["hydra-bootstrap-lab"] = true
	for _, p := range Providers() {
		c.installed[p.Name] = p.Version
	}
	c.waitErr = errors.New("still not Available: cluster-api-provider-hydra-system/cluster-api-provider-hydra-controller-manager")
	err := b.Create(context.Background(), cfg(t, cfgYAML), Options{Credentials: testCredentials(t)})
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("an installed but unavailable provider must not pass: %v", err)
	}
	if c.waits != 1 || len(c.inited) != 0 || len(c.secrets) != 0 {
		t.Errorf("waits %d inits %d secrets %d", c.waits, len(c.inited), len(c.secrets))
	}
}

func TestExistingSecretIsChecked(t *testing.T) {
	all := []string{CACertFile, ClientCertFile, ClientKeyFile}
	for _, tc := range []struct {
		name      string
		info      *SecretInfo
		wantErr   string
		wantLabel bool
	}{
		{"labelled and complete", &SecretInfo{Keys: all, Labels: map[string]string{MoveLabel: ""}}, "", false},
		{"made by hand without the move label", &SecretInfo{Keys: all}, "", true},
		{"missing a key", &SecretInfo{Keys: []string{CACertFile, ClientCertFile}, Labels: map[string]string{MoveLabel: ""}}, "lacks clientkey.pem", false},
		{"being deleted", &SecretInfo{Keys: all, Terminating: true}, "being deleted", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, c, b, _ := newFakes()
			k.clusters["hydra-bootstrap-lab"] = true
			for _, p := range Providers() {
				c.installed[p.Name] = p.Version
			}
			c.secretInfo = map[string]*SecretInfo{"clusters/lab-libvirt-pki": tc.info}
			err := b.Create(context.Background(), cfg(t, cfgYAML), Options{})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "--hypervisor-credentials") {
					t.Fatalf("want error %q asking for credentials, got %v", tc.wantErr, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if got := len(c.labelled) == 1; got != tc.wantLabel {
				t.Errorf("labelled: %v, want %v", c.labelled, tc.wantLabel)
			}
		})
	}
}
