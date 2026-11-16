package validate

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/Petatron/hydra/internal/bootstrap"
	"github.com/Petatron/hydra/internal/config"
)

var now = time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)

// fakeProber answers TCP probes from a table keyed "host:port"; anything
// absent gets NoAnswer.
type fakeProber struct {
	where string
	table map[string]Outcome
	asked []string
}

func (f *fakeProber) Where() string    { return f.where }
func (f *fakeProber) OnThisHost() bool { return f.where == thisMachine }
func (f *fakeProber) TCP(_ context.Context, host string, port int) (Outcome, error) {
	k := fmt.Sprintf("%s:%d", host, port)
	f.asked = append(f.asked, k)
	if o, ok := f.table[k]; ok {
		return o, nil
	}
	return NoAnswer, nil
}

type fakeHV struct {
	poolErr  error
	pools    map[string]PoolInfo
	vols     map[string]bool // "pool/name"
	ifaces   map[string]Iface
	networks map[string]bool
	closed   bool
}

func (h *fakeHV) Pool(_ context.Context, n string) (PoolInfo, error) {
	if h.poolErr != nil {
		return PoolInfo{}, h.poolErr
	}
	p, ok := h.pools[n]
	if !ok {
		return PoolInfo{}, ErrNotFound
	}
	return p, nil
}
func (h *fakeHV) Volume(_ context.Context, pool, n string) error {
	if !h.vols[pool+"/"+n] {
		return ErrNotFound
	}
	return nil
}
func (h *fakeHV) Interface(_ context.Context, n string) (Iface, error) {
	i, ok := h.ifaces[n]
	if !ok {
		return Iface{}, ErrNotFound
	}
	return i, nil
}
func (h *fakeHV) Network(_ context.Context, n string) (bool, error) {
	a, ok := h.networks[n]
	if !ok {
		return false, ErrNotFound
	}
	return a, nil
}
func (h *fakeHV) Close() error { h.closed = true; return nil }

// cert makes a self-signed client certificate.
func cert(t *testing.T, usage x509.KeyUsage, notBefore, notAfter time.Time) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "client"},
		NotBefore: notBefore, NotAfter: notAfter, KeyUsage: usage}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

const siteYAML = `apiVersion: hydra.io/v1alpha1
kind: ClusterConfig
metadata: {name: lab}
spec:
  hypervisor: {address: 192.168.15.13}
  storagePool: default
  image: {name: base.img}
  network:
    controlPlaneEndpoint: 192.168.15.40
    bridges: [br0]
  controlPlane: {replicas: 3, diskSize: 40Gi}
  nodePools:
  - {name: w, autoscaling: {min: 1, max: 2}, vcpus: 2, memory: 4Gi, diskSize: 20Gi}
`

func parse(t *testing.T, y string) *config.ClusterConfig {
	t.Helper()
	c, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// lab is a healthy site: everything validate looks at is in order.
type lab struct {
	hv    *fakeHV
	host  *fakeProber
	kind  *fakeProber // nil: no bootstrap cluster
	creds map[string][]byte
	rt    error
	conn  error
}

func newLab(t *testing.T) *lab {
	return &lab{
		hv: &fakeHV{
			pools: map[string]PoolInfo{"default": {Active: true, Available: 500 << 30}},
			vols:  map[string]bool{"default/base.img": true},
			ifaces: map[string]Iface{"br0": {Active: true, IsBridge: true, Addresses: []string{"192.168.15.13/24"},
				Members: []Member{{Name: "enp1s0"}}}},
			networks: map[string]bool{},
		},
		host: &fakeProber{where: thisMachine, table: map[string]Outcome{
			"192.168.15.13:16514": Open, "192.168.15.13:16509": Refused, "192.168.15.13:22": Open,
		}},
		creds: map[string][]byte{bootstrap.ClientCertFile: cert(t, x509.KeyUsageDigitalSignature|x509.KeyUsageKeyEncipherment, now.Add(-time.Hour), now.Add(365*24*time.Hour))},
	}
}

func (l *lab) run(t *testing.T, cfg *config.ClusterConfig) []Result {
	t.Helper()
	d := Deps{
		Runtime: func() (string, error) { return "docker", l.rt },
		Connect: func(context.Context, string, map[string][]byte) (Hypervisor, error) {
			if l.conn != nil {
				return nil, l.conn
			}
			return l.hv, nil
		},
		Host: l.host,
		Kind: func() (Prober, error) {
			if l.kind == nil {
				return nil, nil
			}
			return l.kind, nil
		},
		Now: func() time.Time { return now },
	}
	return Run(context.Background(), Input{Config: cfg, Credentials: l.creds}, d)
}

func find(t *testing.T, rs []Result, prefix string) Result {
	t.Helper()
	for _, r := range rs {
		if strings.HasPrefix(r.Check, prefix) {
			return r
		}
	}
	t.Fatalf("no %q check in %+v", prefix, rs)
	return Result{}
}

func TestHealthyLabPasses(t *testing.T) {
	l := newLab(t)
	rs := l.run(t, parse(t, siteYAML))
	for _, r := range rs {
		if r.Status != Pass {
			t.Errorf("%s: %s %s", r.Check, r.Status, r.Detail)
		}
	}
	if Failed(rs) {
		t.Error("Failed must be false")
	}
	if !l.hv.closed {
		t.Error("the libvirt connection must be closed")
	}
}

func TestEachFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, l *lab, y *string)
		check  string
		want   Status
		detail string
	}{
		{"no container runtime", func(_ *testing.T, l *lab, _ *string) { l.rt = errors.New("docker: not running") }, "container runtime", Fail, "not running"},
		{"hypervisor unreachable", func(_ *testing.T, l *lab, _ *string) { delete(l.host.table, "192.168.15.13:16514") }, "hypervisor reachable", Fail, "no answer"},
		{"hypervisor on loopback", func(_ *testing.T, _ *lab, y *string) {
			*y = strings.Replace(*y, "address: 192.168.15.13", "address: 127.0.0.1", 1)
		}, "hypervisor reachable", Fail, "loopback"},
		{"plaintext listener open", func(_ *testing.T, l *lab, _ *string) { l.host.table["192.168.15.13:16509"] = Open }, "plaintext libvirt closed", Warn, "unauthenticated"},
		{"plaintext listener not observed", func(_ *testing.T, l *lab, _ *string) { delete(l.host.table, "192.168.15.13:16509") }, "plaintext libvirt closed", Warn, "not observed"},
		{"bridge port could not be looked up", func(_ *testing.T, l *lab, _ *string) {
			i := l.hv.ifaces["br0"]
			i.Members = []Member{{Name: "enp1s0", Unchecked: true}}
			l.hv.ifaces["br0"] = i
		}, "bridge br0", Warn, "not checked: enp1s0"},
		{"no credentials", func(_ *testing.T, l *lab, _ *string) { l.creds = nil }, "hypervisor credentials", Warn, "not checked"},
		{"cert without keyEncipherment", func(t *testing.T, l *lab, _ *string) {
			l.creds[bootstrap.ClientCertFile] = cert(t, x509.KeyUsageDigitalSignature, now.Add(-time.Hour), now.Add(time.Hour*24*365))
		}, "client certificate", Fail, "keyEncipherment"},
		{"cert without a key-usage extension is accepted", func(t *testing.T, l *lab, _ *string) {
			l.creds[bootstrap.ClientCertFile] = cert(t, 0, now.Add(-time.Hour), now.Add(365*24*time.Hour))
		}, "client certificate", Pass, "valid until"},
		{"pool lookup error still says what to do", func(_ *testing.T, l *lab, _ *string) { l.hv.poolErr = errors.New("rpc broke") }, "storage pool", Fail, "rpc broke"},
		{"cert expired", func(t *testing.T, l *lab, _ *string) {
			l.creds[bootstrap.ClientCertFile] = cert(t, x509.KeyUsageKeyEncipherment, now.Add(-48*time.Hour), now.Add(-time.Hour))
		}, "client certificate", Fail, "expired"},
		{"cert expiring", func(t *testing.T, l *lab, _ *string) {
			l.creds[bootstrap.ClientCertFile] = cert(t, x509.KeyUsageKeyEncipherment, now.Add(-time.Hour), now.Add(7*24*time.Hour))
		}, "client certificate", Warn, "expires"},
		{"TLS login rejected", func(_ *testing.T, l *lab, _ *string) { l.conn = errors.New("rejected the client certificate") }, "libvirt TLS login", Fail, "rejected"},
		{"pool missing", func(_ *testing.T, l *lab, _ *string) { delete(l.hv.pools, "default") }, "storage pool", Fail, "no storage pool"},
		{"pool stopped", func(_ *testing.T, l *lab, _ *string) { l.hv.pools["default"] = PoolInfo{Active: false} }, "storage pool", Fail, "not running"},
		{"pool short", func(_ *testing.T, l *lab, _ *string) {
			l.hv.pools["default"] = PoolInfo{Active: true, Available: 100 << 30}
		}, "storage pool", Warn, "160.0 GiB"},
		{"image missing", func(_ *testing.T, l *lab, _ *string) { l.hv.vols = nil }, "base image", Fail, "not in pool"},
		{"bridge missing", func(_ *testing.T, l *lab, _ *string) { delete(l.hv.ifaces, "br0") }, "bridge br0", Fail, "no interface"},
		{"not a bridge", func(_ *testing.T, l *lab, _ *string) { l.hv.ifaces["br0"] = Iface{Active: true} }, "bridge br0", Fail, "not a bridge"},
		{"bridge down", func(_ *testing.T, l *lab, _ *string) {
			i := l.hv.ifaces["br0"]
			i.Active = false
			l.hv.ifaces["br0"] = i
		}, "bridge br0", Fail, "down"},
		{"bridge port holds an address", func(_ *testing.T, l *lab, _ *string) {
			i := l.hv.ifaces["br0"]
			i.Members = []Member{{Name: "enp1s0", Addresses: []string{"192.168.15.99"}}}
			l.hv.ifaces["br0"] = i
		}, "bridge br0", Warn, "enp1s0 (192.168.15.99)"},
		{"endpoint in use (listening)", func(_ *testing.T, l *lab, _ *string) { l.host.table["192.168.15.40:6443"] = Open }, "control-plane endpoint free", Fail, "already in use"},
		{"endpoint in use (refuses)", func(_ *testing.T, l *lab, _ *string) { l.host.table["192.168.15.40:22"] = Refused }, "control-plane endpoint free", Fail, "already in use"},
		{"site network unreachable", func(_ *testing.T, l *lab, _ *string) {
			delete(l.host.table, "192.168.15.13:22")
			delete(l.host.table, "192.168.15.13:16514")
		}, "control-plane network reachable", Fail, "could not reach the new API"},
		{"silence without reachability proves nothing", func(_ *testing.T, l *lab, _ *string) {
			delete(l.host.table, "192.168.15.13:22")
			delete(l.host.table, "192.168.15.13:16514")
		}, "control-plane endpoint free", Warn, "does not prove it free"},
		{"free endpoint reported by the network (on-link)", func(_ *testing.T, l *lab, _ *string) {
			l.host.table["192.168.15.40:6443"] = NoHost
			delete(l.host.table, "192.168.15.13:22") // the bridge-address proof is not needed
			delete(l.host.table, "192.168.15.13:16514")
		}, "control-plane endpoint free", Pass, "reports no host"},
		{"no route to the endpoint", func(_ *testing.T, l *lab, _ *string) { l.host.table["192.168.15.40:6443"] = NoRoute }, "control-plane network reachable", Fail, NoRoute.String()},
		{"libvirt reports no addresses (udev backend)", func(_ *testing.T, l *lab, _ *string) {
			i := l.hv.ifaces["br0"]
			i.Addresses = nil
			l.hv.ifaces["br0"] = i
		}, "bridge br0", Warn, "reports no addresses"},
		{"endpoint off every bridge subnet", func(_ *testing.T, _ *lab, y *string) {
			*y = strings.Replace(*y, "192.168.15.40", "10.9.9.9", 1)
		}, "control-plane network reachable", Warn, "not shown reachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLab(t)
			y := siteYAML
			tc.mutate(t, l, &y)
			rs := l.run(t, parse(t, y))
			r := find(t, rs, tc.check)
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) {
				t.Fatalf("want %s containing %q, got %s %q", tc.want, tc.detail, r.Status, r.Detail)
			}
			for _, r := range rs {
				if r.Status != Pass && r.Fix == "" {
					t.Errorf("%s is %s with no fix line", r.Check, r.Status)
				}
			}
			if Failed(rs) != (tc.want == Fail) && tc.want == Fail {
				t.Error("Failed must report the failure")
			}
		})
	}
}

func TestProbesRunInsideKindWhenItExists(t *testing.T) {
	l := newLab(t)
	l.kind = &fakeProber{where: "the bootstrap cluster's node", table: map[string]Outcome{"192.168.15.13:16514": Open, "192.168.15.13:22": Open}}
	// The host itself cannot reach the hypervisor: only the kind node's view counts.
	delete(l.host.table, "192.168.15.13:16514")
	rs := l.run(t, parse(t, siteYAML))
	if r := find(t, rs, "hypervisor reachable"); r.Status != Pass || !strings.Contains(r.Detail, "node") {
		t.Fatalf("got %s %q", r.Status, r.Detail)
	}
	if len(l.kind.asked) == 0 {
		t.Error("the kind node must be probed")
	}
}

const managedYAML = `apiVersion: hydra.io/v1alpha1
kind: ClusterConfig
metadata: {name: lab}
spec:
  hypervisor: {address: 192.168.15.13}
  storagePool: default
  image: {name: base.img}
  network:
    bridges: [br0]
    vipInterface: enp2s0
    managedNetwork: {name: lab-net, subnet: 10.50.0.0/24, dhcpStart: 10.50.0.100, dhcpEnd: 10.50.0.200}
  controlPlane: {replicas: 1}
`

func TestManagedNetworkReachability(t *testing.T) {
	for _, tc := range []struct {
		name     string
		networks map[string]bool
		gateway  *Outcome
		want     Status
		detail   string
	}{
		{"network not created yet", map[string]bool{}, nil, Warn, "does not run yet"},
		{"gateway answers", map[string]bool{"lab-net": true}, new(Open), Pass, "10.50.0.1"},
		{"gateway refuses DNS (still a live host)", map[string]bool{"lab-net": true}, new(Refused), Pass, "10.50.0.1"},
		{"gateway unreachable: PET-75", map[string]bool{"lab-net": true}, new(NoRoute), Fail, NoRoute.String()},
		{"gateway silent: a drop or no route", map[string]bool{"lab-net": true}, nil, Warn, "drops port 53"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLab(t)
			l.hv.networks = tc.networks
			if tc.gateway != nil {
				l.host.table["10.50.0.1:53"] = *tc.gateway
			}
			r := find(t, l.run(t, parse(t, managedYAML)), "managed network lab-net")
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) {
				t.Fatalf("want %s %q, got %s %q", tc.want, tc.detail, r.Status, r.Detail)
			}
		})
	}
}

func TestNoHypervisorInConfig(t *testing.T) {
	l := newLab(t)
	y := strings.Replace(siteYAML, "  hypervisor: {address: 192.168.15.13}\n", "", 1)
	rs := l.run(t, parse(t, y))
	// Bootstrap refuses this config (ErrNoHypervisor), so validate must fail.
	if r := find(t, rs, "hypervisor"); r.Status != Fail {
		t.Fatalf("got %+v", r)
	}
	if !Failed(rs) {
		t.Error("validate must exit non-zero for a config bootstrap refuses")
	}
	for _, r := range rs {
		if strings.HasPrefix(r.Check, "storage pool") || strings.HasPrefix(r.Check, "libvirt TLS") {
			t.Errorf("%s must not run without a hypervisor", r.Check)
		}
	}
}

func TestDiskNeededCountsAutoscalingMax(t *testing.T) {
	need, n := diskNeeded(parse(t, siteYAML))
	if n != 5 || need != (3*40+2*20)<<30 {
		t.Fatalf("got %d bytes for %d machines", need, n)
	}
}

func TestKindVantageClassifies(t *testing.T) {
	for _, tc := range []struct {
		stderr string
		exit   int
		err    error
		want   Outcome
		bad    bool
	}{
		{"", 0, nil, Open, false},
		{"bash: connect: Connection refused\n", 1, errors.New("exit 1"), Refused, false},
		{"bash: connect: No route to host\n", 1, errors.New("exit 1"), NoHost, false},
		{"bash: connect: Network is unreachable\n", 1, errors.New("exit 1"), NoRoute, false},
		{"", timeoutExit, errors.New("exit 124"), NoAnswer, false},
		// An exec that failed without a word is not a silent address.
		{"", -1, errors.New("docker exec failed"), 0, true},
		{"", 1, errors.New("exit 1"), 0, true},
		{"bash: x: Name or service not known\n", 1, errors.New("exit 1"), 0, true},
	} {
		var args []string
		k := &kindVantage{cluster: "k", run: func(_ context.Context, a ...string) (string, int, error) {
			args = a
			return tc.stderr, tc.exit, tc.err
		}}
		o, err := k.TCP(context.Background(), "10.0.0.1", 22)
		if (err != nil) != tc.bad || (!tc.bad && o != tc.want) {
			t.Errorf("%q exit %d: got %v %v", tc.stderr, tc.exit, o, err)
		}
		// The host travels as an argument, never inside the script.
		if strings.Contains(args[4], "10.0.0.1") || args[5] != "10.0.0.1" {
			t.Errorf("host spliced into the script: %q", args)
		}
	}
}

func TestNoRuntimeMeansNoKindProbe(t *testing.T) {
	l := newLab(t)
	l.rt = errors.New("no runtime")
	l.kind = &fakeProber{where: "node"}
	rs := l.run(t, parse(t, siteYAML))
	for _, r := range rs {
		if r.Check == "probe vantage" {
			t.Errorf("no vantage warning when the runtime check already failed: %+v", r)
		}
	}
	if len(l.kind.asked) != 0 {
		t.Error("the kind node must not be probed without a runtime")
	}
}

// When the bootstrap cluster exists but its node cannot be entered, this
// machine's view must not let reachability or a free endpoint pass.
func TestUnusableKindNodeCapsReachability(t *testing.T) {
	l := newLab(t)
	d := Deps{
		Runtime: func() (string, error) { return "docker", nil },
		Connect: func(context.Context, string, map[string][]byte) (Hypervisor, error) { return l.hv, nil },
		Host:    l.host,
		Kind:    func() (Prober, error) { return nil, errors.New("exec into node failed") },
		Now:     func() time.Time { return now },
	}
	rs := Run(context.Background(), Input{Config: parse(t, siteYAML), Credentials: l.creds}, d)
	if r := find(t, rs, "probe vantage"); r.Status != Warn {
		t.Fatalf("vantage: %+v", r)
	}
	for _, c := range []string{"hypervisor reachable", "control-plane network reachable", "control-plane endpoint free"} {
		if r := find(t, rs, c); r.Status != Warn || !strings.Contains(r.Detail, "only from this machine") {
			t.Errorf("%s must be capped at WARN: %+v", c, r)
		}
	}
	if r := find(t, rs, "storage pool"); r.Status != Pass {
		t.Errorf("checks that do not rest on the vantage stay as they are: %+v", r)
	}
}

// With a managed network, an endpoint outside its subnet lives on the site
// network: the site's reachability decides, not the managed gateway's.
func TestManagedNetworkWithASiteEndpoint(t *testing.T) {
	l := newLab(t)
	l.hv.networks = map[string]bool{"lab-net": true}
	l.host.table["10.50.0.1:53"] = Open // the managed network is reachable
	// The site is not: the hypervisor's br0 address is silent.
	delete(l.host.table, "192.168.15.13:22")
	delete(l.host.table, "192.168.15.13:16514")
	l.host.table["192.168.15.13:16514"] = Open // keep the hypervisor itself reachable via its address check
	y := strings.Replace(managedYAML, "    bridges: [br0]\n", "    controlPlaneEndpoint: 192.168.15.40\n    bridges: [br0]\n", 1)
	y = strings.Replace(y, "    vipInterface: enp2s0\n", "", 1)
	cfg := parse(t, y)
	rs := l.run(t, cfg)
	find(t, rs, "managed network lab-net")
	site := find(t, rs, "control-plane network reachable")
	if site.Status != Pass {
		t.Fatalf("the site reach must be checked for a site endpoint: %+v", site)
	}
	// Now the site goes silent: the endpoint must not pass as free on the
	// managed gateway's word.
	l2 := newLab(t)
	l2.hv.networks = map[string]bool{"lab-net": true}
	l2.host.table["10.50.0.1:53"] = Open
	delete(l2.host.table, "192.168.15.13:22")
	delete(l2.host.table, "192.168.15.13:16514")
	rs = l2.run(t, cfg)
	if r := find(t, rs, "control-plane endpoint free"); r.Status == Pass {
		t.Fatalf("a silent site endpoint must not pass on the managed gateway's reachability: %+v", r)
	}
}

func TestUnresolvableNameEndpointWarns(t *testing.T) {
	l := newLab(t)
	p := &errProber{fakeProber: l.host, failHost: "api.lab.invalid"}
	d := Deps{
		Runtime: func() (string, error) { return "docker", nil },
		Connect: func(context.Context, string, map[string][]byte) (Hypervisor, error) { return l.hv, nil },
		Host:    p,
		Now:     func() time.Time { return now },
	}
	y := strings.Replace(siteYAML, "controlPlaneEndpoint: 192.168.15.40", "controlPlaneEndpoint: api.lab.invalid", 1)
	rs := Run(context.Background(), Input{Config: parse(t, y), Credentials: l.creds}, d)
	if r := find(t, rs, "control-plane endpoint free"); r.Status != Warn || !strings.Contains(r.Detail, "is a name") {
		t.Fatalf("got %+v", r)
	}
}

// errProber fails every probe of one host, as a name that does not resolve.
type errProber struct {
	*fakeProber
	failHost string
}

func (e *errProber) TCP(ctx context.Context, host string, port int) (Outcome, error) {
	if host == e.failHost {
		return 0, errors.New("lookup " + host + ": no such host")
	}
	return e.fakeProber.TCP(ctx, host, port)
}

func TestDiskNeededSaturates(t *testing.T) {
	cfg := parse(t, siteYAML)
	cfg.Spec.ControlPlane.DiskSize = resource.MustParse("8Ei") // three of these overflow int64
	if need, _ := diskNeeded(cfg); need != math.MaxUint64 {
		t.Fatalf("an uncountable total must saturate, got %d", need)
	}
}

// The site-network proof probes the hypervisor's stated TLS port, not 16514.
func TestSiteReachUsesTheHypervisorsPort(t *testing.T) {
	l := newLab(t)
	l.host.table = map[string]Outcome{"192.168.15.13:17414": Open, "192.168.15.13:16509": Refused}
	y := strings.Replace(siteYAML, "address: 192.168.15.13}", "address: \"192.168.15.13:17414\"}", 1)
	rs := l.run(t, parse(t, y))
	if r := find(t, rs, "control-plane network reachable"); r.Status != Pass {
		t.Fatalf("got %+v", r)
	}
}
