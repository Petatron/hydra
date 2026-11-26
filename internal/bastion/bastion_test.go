package bastion

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// TestMain makes the test binary a fake ssh when asked: it forwards the -L
// port for real (so a request can cross it), or fails, or hangs, as the test
// says. It records its arguments for the test to check.
func TestMain(m *testing.M) {
	if mode := os.Getenv("HYDRA_FAKE_SSH"); mode != "" {
		fakeSSH(mode)
		return
	}
	os.Exit(m.Run())
}

func fakeSSH(mode string) {
	args := os.Args[1:]
	if f := os.Getenv("HYDRA_FAKE_SSH_ARGS"); f != "" {
		_ = os.WriteFile(f, []byte(strings.Join(args, "\n")), 0o600)
	}
	switch mode {
	case "deny":
		fmt.Fprintln(os.Stderr, "user@bastion: Permission denied (publickey).")
		os.Exit(255)
	case "hang":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	if mode == "die" { // forward for a moment, then drop, as a network blip would
		go func() {
			time.Sleep(300 * time.Millisecond)
			fmt.Fprintln(os.Stderr, "Timeout, server bastion not responding.")
			os.Exit(255)
		}()
	}
	var spec string
	for i, a := range args {
		if a == "-L" && i+1 < len(args) {
			spec = args[i+1]
		}
	}
	// 127.0.0.1:LOCAL:HOST:PORT
	parts := strings.SplitN(spec, ":", 3)
	l, err := net.Listen("tcp", parts[0]+":"+parts[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "bind: Address already in use")
		os.Exit(255)
	}
	for {
		c, err := l.Accept()
		if err != nil {
			os.Exit(0)
		}
		go func(c net.Conn) {
			target := parts[2]
			// HYDRA_FAKE_SSH_RESOLVE=name=addr: what the bastion's DNS says.
			if r := os.Getenv("HYDRA_FAKE_SSH_RESOLVE"); r != "" {
				name, addr, _ := strings.Cut(r, "=")
				if h, p, err := net.SplitHostPort(target); err == nil && h == name {
					target = net.JoinHostPort(addr, p)
				}
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				_ = c.Close()
				return
			}
			go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
			_, _ = io.Copy(c, up)
			_ = c.Close()
		}(c)
	}
}

func useFakeSSH(t *testing.T, mode string) string {
	t.Helper()
	orig := sshBinary
	t.Cleanup(func() { sshBinary = orig })
	sshBinary = os.Args[0]
	t.Setenv("HYDRA_FAKE_SSH", mode)
	args := t.TempDir() + "/args"
	t.Setenv("HYDRA_FAKE_SSH_ARGS", args)
	return args
}

func kubeconfig(t *testing.T, server string, ca []byte, tlsServerName string) []byte {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["mgmt"] = &clientcmdapi.Cluster{Server: server, CertificateAuthorityData: ca, TLSServerName: tlsServerName}
	cfg.AuthInfos["me"] = &clientcmdapi.AuthInfo{Token: "t"}
	cfg.Contexts["mgmt"] = &clientcmdapi.Context{Cluster: "mgmt", AuthInfo: "me"}
	cfg.CurrentContext = "mgmt"
	b, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSplitDest(t *testing.T) {
	for _, tc := range []struct {
		in, host, port string
		bad            bool
	}{
		{"cp", "cp", "", false},
		{"yibofu@cp", "yibofu@cp", "", false},
		{"yibofu@cp:2222", "yibofu@cp", "2222", false},
		{"ci@bastion.example.com:22", "ci@bastion.example.com", "22", false},
		{"ci@[fd00::1]:2222", "ci@fd00::1", "2222", false},
		{"ci@[fd00::1]", "ci@fd00::1", "", false},
		{"", "", "", true},
		{"-oProxyCommand=evil", "", "", true}, // never an ssh option
		{"cp:notaport", "", "", true},
		{"cp:70000", "", "", true},
		{"two words", "", "", true},
		{"ci@fd00::22", "", "", true}, // unbracketed IPv6: the port is ambiguous
		{"cp:22:33", "", "", true},
		{"ci@", "", "", true},
		{"ci@:22", "", "", true},
		{"[]", "", "", true},
		{"ssh://cp", "", "", true},
	} {
		host, port, err := SplitDest(tc.in)
		if (err != nil) != tc.bad || host != tc.host || port != tc.port {
			t.Errorf("%q: got %q %q %v", tc.in, host, port, err)
		}
	}
}

// A request with TLS verified against the API server's own certificate
// crosses the tunnel, and the arguments are the ones documented.
func TestOpenForwardsWithTLSVerified(t *testing.T) {
	argsFile := useFakeSSH(t, "forward")
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer api.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw})
	// The real server URL names 127.0.0.1, which the test certificate covers.
	kc := kubeconfig(t, api.URL, ca, "")

	tun, rewritten, err := Open(context.Background(), "ci@bastion:2222", kc, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tun.Close() }()

	cfg, err := clientcmd.Load(rewritten)
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Clusters["mgmt"]
	if c.Server != "https://"+tun.Local || c.TLSServerName != "127.0.0.1" {
		t.Fatalf("server %q tls-server-name %q", c.Server, c.TLSServerName)
	}
	rc, err := clientcmd.RESTConfigFromKubeConfig(rewritten)
	if err != nil {
		t.Fatal(err)
	}
	hc, err := rest.HTTPClientFor(rc)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Get(rc.Host + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "ok" {
		t.Fatalf("body %q", body)
	}

	raw, _ := os.ReadFile(argsFile)
	args := strings.Split(string(raw), "\n")
	want := []string{"-N", "-o", "ExitOnForwardFailure=yes", "-o", "BatchMode=yes"}
	if strings.Join(args[:5], " ") != strings.Join(want, " ") {
		t.Errorf("args start %v", args[:5])
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-L "+tun.Local+":"+strings.TrimPrefix(api.URL, "https://")) ||
		!strings.Contains(joined, "-p 2222 -- ci@bastion") || args[len(args)-1] != "ci@bastion" {
		t.Errorf("args %v", args)
	}
}

// A wrong certificate is still refused through the tunnel: TLS is verified,
// not skipped.
func TestOpenDoesNotSkipTLS(t *testing.T) {
	useFakeSSH(t, "forward")
	api := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer api.Close()
	// No CA: the system roots, which do not trust the test certificate.
	tun, rewritten, err := Open(context.Background(), "cp", kubeconfig(t, api.URL, nil, ""), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tun.Close() }()
	rc, _ := clientcmd.RESTConfigFromKubeConfig(rewritten)
	hc, _ := rest.HTTPClientFor(rc)
	_, err = hc.Get(rc.Host + "/")
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("want a certificate error, got %v", err)
	}
}

func TestOpenKeepsAGivenTLSServerName(t *testing.T) {
	useFakeSSH(t, "forward")
	api := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer api.Close()
	tun, rewritten, err := Open(context.Background(), "cp", kubeconfig(t, api.URL, nil, "api.internal"), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tun.Close() }()
	cfg, _ := clientcmd.Load(rewritten)
	if got := cfg.Clusters["mgmt"].TLSServerName; got != "api.internal" {
		t.Fatalf("tls-server-name %q", got)
	}
}

func TestOpenReportsSSHFailures(t *testing.T) {
	kc := kubeconfig(t, "https://192.0.2.10:6443", nil, "")
	useFakeSSH(t, "deny")
	_, _, err := Open(context.Background(), "ci@bastion", kc, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "Permission denied (publickey)") {
		t.Fatalf("want ssh's reason, got %v", err)
	}
	useFakeSSH(t, "hang")
	_, _, err = Open(context.Background(), "ci@bastion", kc, 500*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not forward 192.0.2.10:6443 within") {
		t.Fatalf("want a timeout, got %v", err)
	}
}

func TestOpenRefusesBadInput(t *testing.T) {
	useFakeSSH(t, "forward")
	for _, tc := range []struct {
		name string
		kc   []byte
		dest string
		want string
	}{
		{"http server", kubeconfig(t, "http://192.0.2.10:6443", nil, ""), "cp", "not an https URL"},
		{"no context", []byte("apiVersion: v1\nkind: Config\n"), "cp", "no current context"},
		{"bad dest", kubeconfig(t, "https://192.0.2.10:6443", nil, ""), "-oX", "want user@host"},
	} {
		if _, _, err := Open(context.Background(), tc.dest, tc.kc, time.Second); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v", tc.name, err)
		}
	}
}

func TestCloseStopsTheTunnel(t *testing.T) {
	useFakeSSH(t, "forward")
	api := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer api.Close()
	tun, _, err := Open(context.Background(), "cp", kubeconfig(t, api.URL, nil, ""), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := tun.Close(); err != nil {
		t.Fatal(err)
	}
	_ = tun.Close() // twice is fine
	if c, err := (&net.Dialer{Timeout: time.Second}).Dial("tcp", tun.Local); err == nil {
		_ = c.Close()
		t.Fatal("still listening after Close")
	}
	var none *Tunnel
	_ = none.Close()
}

// nameOnlyServer is an API server whose certificate is valid only for
// api.internal, not for any IP, so a request verifies only by that name.
func nameOnlyServer(t *testing.T) (*httptest.Server, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "api.internal"},
		DNSNames: []string{"api.internal"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.URL.Path)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func get(t *testing.T, kc []byte, path string) (string, error) {
	t.Helper()
	rc, err := clientcmd.RESTConfigFromKubeConfig(kc)
	if err != nil {
		t.Fatal(err)
	}
	hc, err := rest.HTTPClientFor(rc)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Get(rc.Host + path)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

// The certificate is verified for the API server's own name through the
// tunnel, the server URL's path is kept, and a wrong name is refused.
func TestOpenVerifiesTheOriginalName(t *testing.T) {
	useFakeSSH(t, "forward")
	srv, ca := nameOnlyServer(t)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	t.Setenv("HYDRA_FAKE_SSH_RESOLVE", "api.internal=127.0.0.1")

	tun, kc, err := Open(context.Background(), "cp", kubeconfig(t, "https://api.internal:"+port+"/k8s/clusters/c-1", ca, ""), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tun.Close() }()
	got, err := get(t, kc, "/version")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/k8s/clusters/c-1/version" {
		t.Errorf("path %q: the server URL's path was lost", got)
	}

	tun2, kc2, err := Open(context.Background(), "cp", kubeconfig(t, "https://api.internal:"+port, ca, "other.internal"), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tun2.Close() }()
	if _, err := get(t, kc2, "/"); err == nil || !strings.Contains(err.Error(), "certificate is valid for api.internal") {
		t.Fatalf("a wrong tls-server-name must be refused, got %v", err)
	}
}

func TestOpenRefusesUnverifiedOrProxiedKubeconfigs(t *testing.T) {
	useFakeSSH(t, "forward")
	for name, mut := range map[string]func(*clientcmdapi.Cluster){
		"insecure-skip-tls-verify": func(c *clientcmdapi.Cluster) { c.InsecureSkipTLSVerify = true },
		"proxy-url":                func(c *clientcmdapi.Cluster) { c.ProxyURL = "http://proxy:3128" },
	} {
		cfg, _ := clientcmd.Load(kubeconfig(t, "https://192.0.2.10:6443", nil, ""))
		mut(cfg.Clusters["mgmt"])
		kc, _ := clientcmd.Write(*cfg)
		if _, _, err := Open(context.Background(), "cp", kc, time.Second); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

// A tunnel that drops says why; one that hydra closed says nothing.
func TestTunnelReportsAnUnexpectedExit(t *testing.T) {
	useFakeSSH(t, "die")
	api := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer api.Close()
	tun, _, err := Open(context.Background(), "ci@bastion", kubeconfig(t, api.URL, nil, ""), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tun.Close() }()
	select {
	case <-tun.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("ssh did not exit")
	}
	if d := tun.Diagnose(); !strings.Contains(d, "exited") || !strings.Contains(d, "server bastion not responding") {
		t.Errorf("diagnosis %q", d)
	}

	useFakeSSH(t, "forward")
	tun2, _, err := Open(context.Background(), "cp", kubeconfig(t, api.URL, nil, ""), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = tun2.Close()
	if u := tun2.Unexpected(); u != "" {
		t.Errorf("a closed tunnel reported %q", u)
	}
}

// ssh's arguments never let a ControlMaster own the forward.
func TestOpenDisablesConnectionSharing(t *testing.T) {
	args := useFakeSSH(t, "forward")
	api := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer api.Close()
	tun, _, err := Open(context.Background(), "cp", kubeconfig(t, api.URL, nil, ""), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = tun.Close()
	raw, _ := os.ReadFile(args)
	j := strings.ReplaceAll(string(raw), "\n", " ")
	if !strings.Contains(j, "-o ControlMaster=no") || !strings.Contains(j, "-o ControlPath=none") {
		t.Errorf("args %s", j)
	}
}
