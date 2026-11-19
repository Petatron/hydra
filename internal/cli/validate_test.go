package cli

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

	"github.com/Petatron/hydra/internal/validate"
)

// silentProber reaches the hypervisor's libvirt TLS listener, which refuses
// plaintext, and hears nothing else.
type silentProber struct{}

func (silentProber) Where() string    { return "this machine" }
func (silentProber) OnThisHost() bool { return true }
func (silentProber) TCP(_ context.Context, _ string, port int) (validate.Outcome, error) {
	switch port {
	case 16514:
		return validate.Open, nil
	case 16509:
		return validate.Refused, nil
	}
	return validate.NoAnswer, nil
}

func TestValidateExitsNonZeroOnAFailure(t *testing.T) {
	orig := validateDeps
	t.Cleanup(func() { validateDeps = orig })
	for _, tc := range []struct {
		name    string
		runtime error
		wantErr bool
	}{
		{"a failing check", errors.New("docker is not running"), true},
		{"warnings only", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validateDeps = func(string) validate.Deps {
				return validate.Deps{
					Runtime: func() (string, error) { return lcDocker, tc.runtime },
					Host:    silentProber{},
					Now:     time.Now,
				}
			}
			dir := t.TempDir()
			f := filepath.Join(dir, "hydra.yaml")
			src, err := os.ReadFile("../../examples/workload-managed-network.yaml")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f, src, 0o600); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			root := NewRootCommand(&out, &errOut)
			root.SetArgs([]string{lcValidate, "-f", f})
			err = root.Execute()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err %v\n%s", err, out.String())
			}
			if !strings.Contains(out.String(), "container runtime") || !strings.Contains(out.String(), "fix:") {
				t.Errorf("output lacks a check or its fix line:\n%s", out.String())
			}
		})
	}
}

// cluWriteCredentials writes a self-signed client certificate, its key, and
// the same certificate as the CA into a new directory.
func cluWriteCredentials(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "hydra-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	dir := t.TempDir()
	for name, b := range map[string][]byte{
		"cacert.pem":     cert,
		"clientcert.pem": cert,
		"clientkey.pem":  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestCluValidateWithCredentialsLogsIn(t *testing.T) {
	orig := validateDeps
	t.Cleanup(func() { validateDeps = orig })
	var gotAddr string
	var gotCreds map[string][]byte
	validateDeps = func(string) validate.Deps {
		return validate.Deps{
			Runtime: func() (string, error) { return lcDocker, nil },
			Connect: func(_ context.Context, addr string, creds map[string][]byte) (validate.Hypervisor, error) {
				gotAddr, gotCreds = addr, creds
				return nil, errors.New("certificate signed by unknown authority")
			},
			Host: silentProber{},
			Now:  time.Now,
		}
	}
	out, _, err := run(t, lcValidate, "-f", "../../examples/workload-managed-network.yaml", "--hypervisor-credentials", cluWriteCredentials(t))
	if err == nil || err.Error() != "validation failed" {
		t.Fatalf("a failed login must fail validation: %v\n%s", err, out)
	}
	if gotAddr == "" {
		t.Fatal("the hypervisor was never logged in to")
	}
	for _, k := range []string{"cacert.pem", "clientcert.pem", "clientkey.pem"} {
		if len(gotCreds[k]) == 0 {
			t.Errorf("credential %s not passed to the login", k)
		}
	}
	if !strings.Contains(out, "libvirt TLS login") || !strings.Contains(out, "certificate signed by unknown authority") {
		t.Errorf("the login failure must be reported:\n%s", out)
	}
}

func TestCluValidateRefusesBadInput(t *testing.T) {
	orig := validateDeps
	t.Cleanup(func() { validateDeps = orig })
	called := false
	validateDeps = func(string) validate.Deps { called = true; return validate.Deps{} }
	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"missing file", []string{"-f", "/nonexistent/hydra.yaml"}, "no such file"},
		{"empty credentials directory", []string{"-f", "../../examples/workload-managed-network.yaml", "--hypervisor-credentials", t.TempDir()}, "hypervisor credentials"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := run(t, append([]string{lcValidate}, tc.args...)...)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
		})
	}
	if called {
		t.Error("no check may run on bad input")
	}
}

func TestCluPrintResults(t *testing.T) {
	var b bytes.Buffer
	err := printResults(&b, []validate.Result{
		{Check: "container runtime", Status: validate.Pass, Detail: lcDocker, Fix: "never shown"},
		{Check: "endpoint", Status: validate.Warn, Detail: "unchecked", Fix: "check by hand"},
		{Check: "pool", Status: validate.Fail, Detail: "missing"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := b.String()
	if strings.Contains(s, "never shown") {
		t.Errorf("a passing check's fix must not be printed:\n%s", s)
	}
	if !strings.Contains(s, "fix: check by hand") || !strings.HasSuffix(s, "1 pass, 1 warn, 1 fail\n") {
		t.Errorf("output:\n%s", s)
	}
	if strings.Count(s, "fix:") != 1 {
		t.Errorf("only the warning has a fix:\n%s", s)
	}
}

// cluFailWriter fails every write after the first n bytes.
type cluFailWriter struct{ n int }

func (w *cluFailWriter) Write(p []byte) (int, error) {
	if w.n <= 0 {
		return 0, errors.New("disk full")
	}
	w.n -= len(p)
	return len(p), nil
}

func TestCluPrintResultsWriteErrors(t *testing.T) {
	rs := []validate.Result{{Check: "a", Status: validate.Fail, Detail: "x", Fix: "y"}}
	var full bytes.Buffer
	if err := printResults(&full, rs); err != nil {
		t.Fatal(err)
	}
	table := full.Len() - len("0 pass, 0 warn, 1 fail\n")
	// No room at all fails the table; room for exactly the table fails the
	// summary line.
	for _, budget := range []int{0, table} {
		if err := printResults(&cluFailWriter{n: budget}, rs); err == nil || err.Error() != "disk full" {
			t.Errorf("budget %d: want the write error, got %v", budget, err)
		}
	}
}

func TestCluDefaultValidateDeps(t *testing.T) {
	d := validateDeps("c")
	if d.Runtime == nil || d.Connect == nil || d.Host == nil || d.Kind == nil || d.Now == nil {
		t.Errorf("every real dependency must be wired: %+v", d)
	}
}
