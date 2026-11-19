package validate

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Petatron/hydra/internal/bootstrap"
)

// This file drives ConnectLibvirt and libvirtHypervisor against an in-process
// libvirtd stand-in: a TLS listener on loopback that sends libvirtd's verdict
// byte and then answers the handful of remote-protocol procedures validate
// calls, XDR-encoded by hand.

// Remote-protocol procedure numbers (libvirt's remote_protocol.x).
const (
	procConnectOpen             = 1
	procConnectClose            = 2
	procNetworkLookupByName     = 46
	procAuthList                = 66
	procStoragePoolLookupByName = 84
	procStoragePoolGetInfo      = 87
	procStorageVolLookupByName  = 95
	procInterfaceLookupByName   = 128
	procInterfaceGetXMLDesc     = 130
	procNetworkIsActive         = 152
	procInterfaceIsActive       = 156

	// libvirt error codes the fake returns.
	virErrInternal      = 1
	virErrNoNetwork     = 43
	virErrNoStoragePool = 49
	virErrNoStorageVol  = 50
	virErrNoInterface   = 57
)

// --- PKI ---------------------------------------------------------------------

type keyPair struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
	kpem []byte
}

func issue(t *testing.T, tmpl *x509.Certificate, parent *keyPair) *keyPair {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, signerCert := k, tmpl
	if parent != nil {
		signer, signerCert = parent.key, parent.cert
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signerCert, &k.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return &keyPair{cert: c, key: k,
		pem:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		kpem: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})}
}

// pki is a throwaway CA with a server certificate for 127.0.0.1 and a client
// certificate, both signed by it.
type pki struct {
	ca, server, client *keyPair
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	nb, na := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	ca := issue(t, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: nb, NotAfter: na, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}, nil)
	server := issue(t, &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "libvirtd"},
		NotBefore: nb, NotAfter: na, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}, ca)
	client := issue(t, &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "hydra"},
		NotBefore: nb, NotAfter: na,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}, ca)
	return &pki{ca: ca, server: server, client: client}
}

func (p *pki) creds() map[string][]byte {
	return map[string][]byte{
		bootstrap.CACertFile:     p.ca.pem,
		bootstrap.ClientCertFile: p.client.pem,
		bootstrap.ClientKeyFile:  p.client.kpem,
	}
}

// --- XDR ---------------------------------------------------------------------

type xdrBuf struct{ bytes.Buffer }

func (b *xdrBuf) u32(v uint32) *xdrBuf {
	_ = binary.Write(&b.Buffer, binary.BigEndian, v)
	return b
}

func (b *xdrBuf) u64(v uint64) *xdrBuf {
	_ = binary.Write(&b.Buffer, binary.BigEndian, v)
	return b
}

func (b *xdrBuf) str(s string) *xdrBuf {
	b.u32(uint32(len(s)))
	b.WriteString(s)
	b.Write(make([]byte, (4-len(s)%4)%4))
	return b
}

func (b *xdrBuf) uuid() *xdrBuf {
	b.Write(make([]byte, 16))
	return b
}

// xdrString reads one string at the front of p, returning it and the rest.
func xdrString(p []byte) (string, []byte) {
	if len(p) < 4 {
		return "", nil
	}
	n := int(binary.BigEndian.Uint32(p))
	end := 4 + n
	if end > len(p) {
		return "", nil
	}
	s := string(p[4:end])
	end += (4 - n%4) % 4
	if end > len(p) {
		end = len(p)
	}
	return s, p[end:]
}

// --- the fake daemon ---------------------------------------------------------

type fakePool struct {
	state            uint32
	capacity, avail  uint64
	allocationUnused uint64
}

type fakeIface struct {
	active bool
	xml    string
}

// fakeLibvirtd answers validate's procedures from its tables. Anything named
// in failName fails with that libvirt error code on its lookup; any procedure
// in failProc fails outright; any procedure in hang is never answered.
type fakeLibvirtd struct {
	verdict      byte // sent after the handshake
	noVerdict    bool // close instead of sending one
	pools        map[string]fakePool
	vols         map[string]bool // "pool/name"
	networks     map[string]bool
	ifaces       map[string]fakeIface
	failName     map[string]uint32
	failProc     map[uint32]uint32
	hang         map[uint32]bool
	serverCert   *keyPair // defaults to the PKI's server certificate
	sawClientCN  string
	sawProcedure []uint32
	mu           sync.Mutex
}

func (f *fakeLibvirtd) reply(proc uint32, args []byte) (payload []byte, errCode uint32) {
	name, rest := xdrString(args)
	if code, ok := f.failProc[proc]; ok {
		return nil, code
	}
	switch proc {
	case procAuthList:
		return new(xdrBuf).u32(0).Bytes(), 0 // no authentication types
	case procConnectOpen, procConnectClose:
		return nil, 0
	}
	if code, ok := f.failName[name]; ok {
		return nil, code
	}
	switch proc {
	case procStoragePoolLookupByName:
		if _, ok := f.pools[name]; !ok {
			return nil, virErrNoStoragePool
		}
		return new(xdrBuf).str(name).uuid().Bytes(), 0
	case procStoragePoolGetInfo:
		p := f.pools[name]
		return new(xdrBuf).u32(p.state).u64(p.capacity).u64(p.allocationUnused).u64(p.avail).Bytes(), 0
	case procStorageVolLookupByName:
		vol, _ := xdrString(rest[16:]) // pool name, pool UUID, volume name
		if !f.vols[name+"/"+vol] {
			return nil, virErrNoStorageVol
		}
		return new(xdrBuf).str(name).str(vol).str("/pool/" + vol).Bytes(), 0
	case procNetworkLookupByName:
		if _, ok := f.networks[name]; !ok {
			return nil, virErrNoNetwork
		}
		return new(xdrBuf).str(name).uuid().Bytes(), 0
	case procNetworkIsActive:
		return new(xdrBuf).u32(b2u(f.networks[name])).Bytes(), 0
	case procInterfaceLookupByName:
		if _, ok := f.ifaces[name]; !ok {
			return nil, virErrNoInterface
		}
		return new(xdrBuf).str(name).str("52:54:00:00:00:01").Bytes(), 0
	case procInterfaceIsActive:
		return new(xdrBuf).u32(b2u(f.ifaces[name].active)).Bytes(), 0
	case procInterfaceGetXMLDesc:
		return new(xdrBuf).str(f.ifaces[name].xml).Bytes(), 0
	}
	return nil, virErrInternal
}

func b2u(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

// start serves f on a loopback TLS listener and returns its address.
func (f *fakeLibvirtd) start(t *testing.T, p *pki) string {
	t.Helper()
	sc := f.serverCert
	if sc == nil {
		sc = p.server
	}
	cert, err := tls.X509KeyPair(sc.pem, sc.kpem)
	if err != nil {
		t.Fatal(err)
	}
	cas := x509.NewCertPool()
	cas.AddCert(p.ca.cert)
	conf := &tls.Config{Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: cas}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { _ = l.Close(); wg.Wait() })
	wg.Go(func() {
		for {
			raw, err := l.Accept()
			if err != nil {
				return
			}
			wg.Go(func() { f.serve(tls.Server(raw, conf)) })
		}
	})
	return l.Addr().String()
}

func (f *fakeLibvirtd) serve(conn *tls.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := conn.Handshake(); err != nil {
		return
	}
	if pc := conn.ConnectionState().PeerCertificates; len(pc) > 0 {
		f.mu.Lock()
		f.sawClientCN = pc[0].Subject.CommonName
		f.mu.Unlock()
	}
	if f.noVerdict {
		return
	}
	if _, err := conn.Write([]byte{f.verdict}); err != nil {
		return
	}
	for {
		// length, then program, version, procedure, type, serial, status.
		hdr := make([]byte, 28)
		if _, err := io.ReadFull(conn, hdr); err != nil {
			return
		}
		args := make([]byte, int(binary.BigEndian.Uint32(hdr[0:4]))-28)
		if _, err := io.ReadFull(conn, args); err != nil {
			return
		}
		proc := binary.BigEndian.Uint32(hdr[12:16])
		f.mu.Lock()
		f.sawProcedure = append(f.sawProcedure, proc)
		f.mu.Unlock()
		if f.hang[proc] {
			continue
		}
		payload, code := f.reply(proc, args)
		status := uint32(0)
		if code != 0 {
			status = 1
			payload = new(xdrBuf).u32(code).u32(0).u32(1).str("fake libvirtd error").u32(2).Bytes()
		}
		out := new(xdrBuf).u32(uint32(28 + len(payload)))
		out.Write(hdr[4:12])  // program, version
		out.u32(proc).u32(1)  // procedure, type: reply
		out.Write(hdr[20:24]) // serial
		out.u32(status)
		out.Write(payload)
		if _, err := conn.Write(out.Bytes()); err != nil {
			return
		}
	}
}

func (f *fakeLibvirtd) procedures() []uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uint32(nil), f.sawProcedure...)
}

// dialFake brings up a daemon and connects to it.
func dialFake(t *testing.T, f *fakeLibvirtd) Hypervisor {
	t.Helper()
	p := newPKI(t)
	f.verdict = tlsVerificationOK
	addr := f.start(t, p)
	hv, err := ConnectLibvirt(context.Background(), addr, p.creds())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hv.Close() })
	return hv
}

// --- tests -------------------------------------------------------------------

func TestConnectLibvirtOverTLS(t *testing.T) {
	p := newPKI(t)
	f := &fakeLibvirtd{verdict: tlsVerificationOK}
	addr := f.start(t, p)
	hv, err := ConnectLibvirt(context.Background(), addr, p.creds())
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	cn := f.sawClientCN
	f.mu.Unlock()
	if cn != "hydra" {
		t.Errorf("the daemon saw client certificate %q, want the one from the credentials", cn)
	}
	if err := hv.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	got := f.procedures()
	want := []uint32{procAuthList, procConnectOpen, procConnectClose}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("procedures %v, want %v (auth-list, connect-open, connect-close)", got, want)
	}
}

func TestConnectLibvirtRejectsBadCredentials(t *testing.T) {
	p := newPKI(t)
	other := newPKI(t)
	for name, tc := range map[string]struct {
		addr  string
		creds func() map[string][]byte
		want  string
	}{
		"key does not match certificate": {"127.0.0.1:1", func() map[string][]byte {
			c := p.creds()
			c[bootstrap.ClientKeyFile] = other.client.kpem
			return c
		}, "client certificate or key"},
		"no client certificate": {"127.0.0.1:1", func() map[string][]byte {
			c := p.creds()
			delete(c, bootstrap.ClientCertFile)
			return c
		}, "client certificate or key"},
		"CA file is not PEM": {"127.0.0.1:1", func() map[string][]byte {
			c := p.creds()
			c[bootstrap.CACertFile] = []byte("not a certificate")
			return c
		}, "cacert.pem holds no PEM certificate"},
		"address without a port": {"127.0.0.1", p.creds, "missing port"},
	} {
		t.Run(name, func(t *testing.T) {
			hv, err := ConnectLibvirt(context.Background(), tc.addr, tc.creds())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, %v; want an error containing %q", hv, err, tc.want)
			}
		})
	}
}

func TestConnectLibvirtDialFailures(t *testing.T) {
	p := newPKI(t)
	other := newPKI(t)

	// A port nothing listens on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := l.Addr().String()
	_ = l.Close()

	for name, tc := range map[string]struct {
		addr func(t *testing.T) string
		want string
	}{
		"nothing listening": {func(*testing.T) string { return closed }, "connecting to " + closed},
		"server certificate from another CA": {func(t *testing.T) string {
			return (&fakeLibvirtd{verdict: tlsVerificationOK, serverCert: other.server}).start(t, p)
		}, "TLS handshake with"},
		"daemon hangs up instead of a verdict": {func(t *testing.T) string {
			return (&fakeLibvirtd{noVerdict: true}).start(t, p)
		}, "to accept the client certificate"},
		"daemon refuses the client certificate": {func(t *testing.T) string {
			return (&fakeLibvirtd{verdict: 0}).start(t, p)
		}, "rejected the client certificate"},
		"connect-open fails": {func(t *testing.T) string {
			return (&fakeLibvirtd{verdict: tlsVerificationOK, failProc: map[uint32]uint32{procConnectOpen: virErrInternal}}).start(t, p)
		}, "fake libvirtd error"},
	} {
		t.Run(name, func(t *testing.T) {
			addr := tc.addr(t)
			hv, err := ConnectLibvirt(context.Background(), addr, p.creds())
			if err == nil {
				_ = hv.Close()
				t.Fatal("connected; want an error")
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "connecting to qemu:///system at "+addr) {
				t.Errorf("got %q; want it to name %s and contain %q", err, addr, tc.want)
			}
		})
	}
}

func TestLibvirtPool(t *testing.T) {
	hv := dialFake(t, &fakeLibvirtd{
		pools: map[string]fakePool{
			"default": {state: poolRunning, capacity: 1000, avail: 400},
			"stopped": {state: 0, capacity: 10, avail: 10},
		},
		failProc: map[uint32]uint32{},
	})
	ctx := context.Background()
	if got, err := hv.Pool(ctx, "default"); err != nil || got != (PoolInfo{Active: true, Capacity: 1000, Available: 400}) {
		t.Errorf("default: %+v %v", got, err)
	}
	if got, err := hv.Pool(ctx, "stopped"); err != nil || got.Active {
		t.Errorf("an inactive pool must report Active=false: %+v %v", got, err)
	}
	if _, err := hv.Pool(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing pool: %v, want ErrNotFound", err)
	}
}

func TestLibvirtPoolErrors(t *testing.T) {
	ctx := context.Background()
	hv := dialFake(t, &fakeLibvirtd{
		pools:    map[string]fakePool{"default": {}},
		failName: map[string]uint32{"denied": virErrInternal},
		failProc: map[uint32]uint32{},
	})
	if _, err := hv.Pool(ctx, "denied"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("a lookup failing for another reason must not read as not found: %v", err)
	}

	hv2 := dialFake(t, &fakeLibvirtd{
		pools:    map[string]fakePool{"default": {}},
		failProc: map[uint32]uint32{procStoragePoolGetInfo: virErrInternal},
	})
	if _, err := hv2.Pool(ctx, "default"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("get-info failure: %v", err)
	}
}

func TestLibvirtVolume(t *testing.T) {
	hv := dialFake(t, &fakeLibvirtd{
		pools: map[string]fakePool{"default": {state: poolRunning}},
		vols:  map[string]bool{"default/base.img": true},
	})
	ctx := context.Background()
	if err := hv.Volume(ctx, "default", "base.img"); err != nil {
		t.Errorf("present volume: %v", err)
	}
	if err := hv.Volume(ctx, "default", "other.img"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing volume: %v, want ErrNotFound", err)
	}
	if err := hv.Volume(ctx, "nopool", "base.img"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing pool: %v, want ErrNotFound", err)
	}
}

func TestLibvirtNetwork(t *testing.T) {
	hv := dialFake(t, &fakeLibvirtd{
		networks: map[string]bool{"up": true, "down": false},
		failName: map[string]uint32{"denied": virErrInternal},
	})
	ctx := context.Background()
	if a, err := hv.Network(ctx, "up"); err != nil || !a {
		t.Errorf("up: %v %v", a, err)
	}
	if a, err := hv.Network(ctx, "down"); err != nil || a {
		t.Errorf("down: %v %v", a, err)
	}
	if _, err := hv.Network(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v, want ErrNotFound", err)
	}
	if _, err := hv.Network(ctx, "denied"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("denied: %v", err)
	}
}

// A bridge's ports are looked up one by one; a port named on the bridge but
// unknown on its own is marked unchecked rather than address-free.
func TestLibvirtInterfaceBridge(t *testing.T) {
	const br = `<interface type='bridge' name='br0'>
  <protocol family='ipv4'><ip address='192.168.15.13' prefix='24'/></protocol>
  <bridge>
    <interface type='ethernet' name='enp142s0'/>
    <interface type='ethernet' name='ghost0'/>
  </bridge>
</interface>`
	hv := dialFake(t, &fakeLibvirtd{ifaces: map[string]fakeIface{
		"br0":      {active: true, xml: br},
		"enp142s0": {active: true, xml: portXML},
		"eth9":     {active: false, xml: `<interface type='ethernet' name='eth9'/>`},
	}})
	ctx := context.Background()
	got, err := hv.Interface(ctx, "br0")
	if err != nil {
		t.Fatal(err)
	}
	want := Iface{Active: true, IsBridge: true, Addresses: []string{"192.168.15.13/24"}, Members: []Member{
		{Name: "enp142s0", Addresses: []string{"192.168.15.50"}},
		{Name: "ghost0", Unchecked: true},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}

	plain, err := hv.Interface(ctx, "eth9")
	if err != nil || plain.Active || plain.IsBridge || plain.Members != nil {
		t.Errorf("plain inactive interface: %+v %v", plain, err)
	}
	if _, err := hv.Interface(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v, want ErrNotFound", err)
	}
}

func TestLibvirtInterfaceErrors(t *testing.T) {
	const br = `<interface type='bridge' name='br0'><bridge><interface type='ethernet' name='denied0'/></bridge></interface>`
	ctx := context.Background()
	for name, tc := range map[string]struct {
		f    *fakeLibvirtd
		want string
	}{
		"a port fails to look up": {&fakeLibvirtd{
			ifaces:   map[string]fakeIface{"br0": {active: true, xml: br}, "denied0": {}},
			failName: map[string]uint32{"denied0": virErrInternal},
		}, "fake libvirtd error"},
		"is-active fails": {&fakeLibvirtd{
			ifaces:   map[string]fakeIface{"br0": {xml: br}},
			failProc: map[uint32]uint32{procInterfaceIsActive: virErrInternal},
		}, "fake libvirtd error"},
		"XML description fails": {&fakeLibvirtd{
			ifaces:   map[string]fakeIface{"br0": {xml: br}},
			failProc: map[uint32]uint32{procInterfaceGetXMLDesc: virErrInternal},
		}, "fake libvirtd error"},
		"XML is malformed": {&fakeLibvirtd{
			ifaces: map[string]fakeIface{"br0": {xml: "<interface"}},
		}, "reading interface br0"},
	} {
		t.Run(name, func(t *testing.T) {
			hv := dialFake(t, tc.f)
			_, err := hv.Interface(ctx, "br0")
			if err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v; want an error containing %q", err, tc.want)
			}
		})
	}
}

// A daemon that stops answering mid-session: the call returns at the caller's
// deadline instead of blocking, and the connection is abandoned.
func TestLibvirtHungDaemon(t *testing.T) {
	hv := dialFake(t, &fakeLibvirtd{
		networks: map[string]bool{"up": true},
		hang:     map[uint32]bool{procNetworkLookupByName: true},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := hv.Network(ctx, "up")
	if err == nil || !strings.Contains(err.Error(), "reading network up: libvirt did not answer") {
		t.Fatalf("got %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the error must carry the deadline: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("a hung call must return at its deadline")
	}
	// The connection is gone: a later call fails rather than hanging.
	if _, err := hv.Network(context.Background(), "up"); err == nil {
		t.Error("a call after an abandoned one must fail")
	}
}

func TestNotFoundMapsOnlyTheGivenCode(t *testing.T) {
	plain := errors.New("boom")
	if got := notFound(plain, 49); got != plain {
		t.Errorf("a non-libvirt error must pass through: %v", got)
	}
	if got := notFound(nil, 49); got != nil {
		t.Errorf("nil must stay nil: %v", got)
	}
}
