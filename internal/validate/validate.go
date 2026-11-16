// Package validate checks, read-only, that a host and its hypervisor are
// ready for hydra.yaml before bootstrap (PET-79, ADR-001).
//
// Every check reports pass, warn or fail with a remediation line. Nothing is
// written anywhere: the hypervisor is only queried, and reachability is only
// probed with TCP connects.
//
// Reachability is judged from where Cluster API's controllers will run: the
// bootstrap kind cluster's node container. When that cluster exists, the
// probes run inside its node. Before it exists they run from this host, whose
// network kind's containers reach the outside through, and an address only
// this host can reach (its loopback) fails outright.
package validate

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/Petatron/hydra/internal/bootstrap"
	"github.com/Petatron/hydra/internal/config"
)

// Status is a check's verdict.
type Status int

const (
	// Pass means the check found what it needs.
	Pass Status = iota
	// Warn means something is off but need not stop a build.
	Warn
	// Fail means bootstrap or create would not work.
	Fail
)

func (s Status) String() string {
	switch s {
	case Pass:
		return "PASS"
	case Warn:
		return "WARN"
	default:
		return "FAIL"
	}
}

// Result is one check's outcome.
type Result struct {
	Check  string
	Status Status
	Detail string
	// Fix says what to do about a warning or failure.
	Fix string
}

// Outcome is what a TCP connect attempt found.
type Outcome int

const (
	// Open: something accepted the connection.
	Open Outcome = iota
	// Refused: a host answered and nothing listens on the port.
	Refused
	// NoAnswer: nothing answered before the timeout.
	NoAnswer
	// Unreachable: there is no route to the address.
	Unreachable
)

func (o Outcome) String() string {
	return [...]string{"open", "refused", "no answer", "unreachable"}[o]
}

// Prober attempts TCP connects from one vantage point.
type Prober interface {
	// Where says where the probes run from, for messages.
	Where() string
	// OnThisHost is true when the probes run from this machine rather than
	// from inside a container, so this machine's loopback looks reachable.
	OnThisHost() bool
	TCP(ctx context.Context, host string, port int) (Outcome, error)
}

// ErrNotFound is what a Hypervisor returns for a missing pool, volume,
// interface or network.
var ErrNotFound = errors.New("not found")

// PoolInfo is a storage pool's state.
type PoolInfo struct {
	Active              bool
	Capacity, Available uint64
}

// Iface is a host interface as libvirt reports it.
type Iface struct {
	Active   bool
	IsBridge bool
	// Addresses are the interface's own addresses, as CIDR (address/prefix).
	Addresses []string
	// Members are the bridge's ports and the addresses each holds itself.
	Members []Member
}

// Member is one port of a bridge.
type Member struct {
	Name      string
	Addresses []string
}

// Hypervisor is the read-only part of libvirt validate uses.
type Hypervisor interface {
	Pool(name string) (PoolInfo, error)
	Volume(pool, name string) error
	Interface(name string) (Iface, error)
	// Network reports whether a libvirt network exists and is active.
	Network(name string) (active bool, err error)
	Close() error
}

// Deps are validate's ways of looking at the world; tests replace them.
type Deps struct {
	// Runtime names the container runtime kind would use.
	Runtime func() (string, error)
	// Connect opens a TLS connection to libvirt at addr with the credentials.
	Connect func(ctx context.Context, addr string, creds map[string][]byte) (Hypervisor, error)
	// Host probes from this machine.
	Host Prober
	// Kind returns a prober inside the bootstrap cluster's node, or nil when
	// that cluster does not exist.
	Kind func() (Prober, error)
	Now  func() time.Time
}

// Input is what to validate.
type Input struct {
	Config *config.ClusterConfig
	// Credentials are the hypervisor credentials files, or nil when none
	// were given: the checks that need them then warn instead.
	Credentials map[string][]byte
}

const (
	libvirtTLSPort = 16514
	libvirtTCPPort = 16509
	apiServerPort  = 6443
	sshPort        = 22
	dnsPort        = 53
	certExpiryWarn = 30 * 24 * time.Hour
)

// Run runs every check and returns the results in order.
func Run(ctx context.Context, in Input, d Deps) []Result {
	var out []Result
	add := func(r Result) { out = append(out, r) }
	cfg := in.Config

	rt := checkRuntime(d)
	add(rt)

	// Without a working runtime there is no kind node to probe from, and the
	// runtime check already says so.
	vantage := d.Host
	if rt.Status == Pass {
		var vr *Result
		vantage, vr = vantagePoint(d)
		if vr != nil {
			add(*vr)
		}
	}

	h := cfg.Spec.Hypervisor
	var hv Hypervisor
	if h == nil {
		add(Result{Check: "hypervisor", Status: Warn,
			Detail: "hydra.yaml names no hypervisor, so the provider's manager-wide libvirt-config will be used; validate cannot check it",
			Fix:    "set spec.hypervisor.address to have it checked"})
	} else {
		host, port := splitHypervisor(h.Address)
		add(checkReach(ctx, vantage, "hypervisor reachable", host, port,
			"open the libvirt TLS listener (libvirtd --listen, port 16514) and make sure "+vantage.Where()+" has a route to it"))
		add(checkPlaintext(ctx, d.Host, host))
		if in.Credentials == nil {
			add(Result{Check: "hypervisor credentials", Status: Warn,
				Detail: "no credentials given, so the client certificate, the TLS login, the storage pool, the image and the bridges are not checked",
				Fix:    "pass --hypervisor-credentials <dir> holding cacert.pem, clientcert.pem and clientkey.pem"})
		} else {
			add(checkClientCert(in.Credentials[bootstrap.ClientCertFile], d.Now()))
			var r Result
			hv, r = connect(ctx, d, net.JoinHostPort(host, strconv.Itoa(port)), in.Credentials)
			add(r)
		}
	}
	var bridges []namedIface
	if hv != nil {
		defer func() { _ = hv.Close() }()
		add(checkPool(hv, cfg))
		add(checkImage(hv, cfg))
		for _, b := range cfg.Spec.Network.Bridges {
			r, ifc := checkBridge(hv, b)
			add(r)
			if ifc != nil {
				bridges = append(bridges, namedIface{b, *ifc})
			}
		}
	}
	out = append(out, checkEndpoint(ctx, vantage, hv, bridges, cfg)...)
	return out
}

// Failed reports whether any result failed.
func Failed(rs []Result) bool {
	for _, r := range rs {
		if r.Status == Fail {
			return true
		}
	}
	return false
}

func checkRuntime(d Deps) Result {
	const check = "container runtime"
	name, err := d.Runtime()
	if err != nil {
		return Result{Check: check, Status: Fail, Detail: err.Error(),
			Fix: "install Docker, Podman or nerdctl, and make sure this user can run containers (`docker ps`)"}
	}
	return Result{Check: check, Status: Pass, Detail: name + " (kind will use it)"}
}

// vantagePoint picks where reachability is judged from: inside the kind node
// when the bootstrap cluster exists, else this host.
func vantagePoint(d Deps) (Prober, *Result) {
	if d.Kind == nil {
		return d.Host, nil
	}
	p, err := d.Kind()
	if err != nil {
		return d.Host, &Result{Check: "probe vantage", Status: Warn,
			Detail: "cannot run probes inside the bootstrap cluster's node (" + err.Error() + "); probing from this host instead",
			Fix:    "check that the kind node container is running"}
	}
	if p == nil {
		return d.Host, nil
	}
	return p, nil
}

func splitHypervisor(addr string) (string, int) {
	if h, p, err := net.SplitHostPort(addr); err == nil {
		if n, err := strconv.Atoi(p); err == nil {
			return h, n
		}
	}
	return strings.Trim(addr, "[]"), libvirtTLSPort
}

// hostOnly reports whether addr is reachable from this machine only, never
// from a container: loopback.
func hostOnly(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func checkReach(ctx context.Context, p Prober, check, host string, port int, fix string) Result {
	target := net.JoinHostPort(host, strconv.Itoa(port))
	if p.OnThisHost() && hostOnly(host) {
		return Result{Check: check, Status: Fail,
			Detail: target + " is this machine's loopback; the bootstrap cluster's controllers run in a container and cannot reach it",
			Fix:    "use an address of the hypervisor that a container can route to"}
	}
	o, err := p.TCP(ctx, host, port)
	if err != nil {
		return Result{Check: check, Status: Fail, Detail: fmt.Sprintf("probing %s from %s: %v", target, p.Where(), err), Fix: fix}
	}
	if o == Open {
		return Result{Check: check, Status: Pass, Detail: target + " accepts connections from " + p.Where()}
	}
	return Result{Check: check, Status: Fail, Detail: fmt.Sprintf("%s from %s: %s", target, p.Where(), o), Fix: fix}
}

func checkPlaintext(ctx context.Context, p Prober, host string) Result {
	const check = "plaintext libvirt closed"
	o, err := p.TCP(ctx, host, libvirtTCPPort)
	if err == nil && o == Open {
		return Result{Check: check, Status: Warn,
			Detail: fmt.Sprintf("%s:%d accepts connections: libvirt's unauthenticated TCP listener is open", host, libvirtTCPPort),
			Fix:    "disable libvirtd's TCP listener (listen_tcp = 0, and mask libvirtd-tcp.socket); hydra only uses TLS on 16514"}
	}
	return Result{Check: check, Status: Pass, Detail: fmt.Sprintf("%s:%d does not accept connections", host, libvirtTCPPort)}
}

// checkClientCert checks what the hypervisor will need of the client
// certificate. libvirt's TLS needs keyEncipherment on an RSA client key.
func checkClientCert(certPEM []byte, now time.Time) Result {
	const check = "client certificate"
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return Result{Check: check, Status: Fail, Detail: "clientcert.pem holds no PEM certificate", Fix: "reissue the client certificate"}
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return Result{Check: check, Status: Fail, Detail: "clientcert.pem: " + err.Error(), Fix: "reissue the client certificate"}
	}
	switch {
	case c.KeyUsage != 0 && c.KeyUsage&x509.KeyUsageKeyEncipherment == 0:
		return Result{Check: check, Status: Fail, Detail: "the client certificate lacks the keyEncipherment key usage, which libvirt's TLS needs",
			Fix: "reissue it with keyUsage = digitalSignature, keyEncipherment"}
	case now.Before(c.NotBefore):
		return Result{Check: check, Status: Fail, Detail: "the client certificate is not valid until " + c.NotBefore.UTC().Format(time.RFC3339),
			Fix: "check this machine's clock, or reissue the certificate"}
	case now.After(c.NotAfter):
		return Result{Check: check, Status: Fail, Detail: "the client certificate expired " + c.NotAfter.UTC().Format(time.RFC3339),
			Fix: "reissue it, and update the credentials Secret of every cluster using it"}
	case c.NotAfter.Sub(now) < certExpiryWarn:
		return Result{Check: check, Status: Warn, Detail: "the client certificate expires " + c.NotAfter.UTC().Format(time.RFC3339),
			Fix: "reissue it soon"}
	}
	return Result{Check: check, Status: Pass, Detail: "valid until " + c.NotAfter.UTC().Format("2006-01-02")}
}

func connect(ctx context.Context, d Deps, addr string, creds map[string][]byte) (Hypervisor, Result) {
	const check = "libvirt TLS login"
	hv, err := d.Connect(ctx, addr, creds)
	if err != nil {
		return nil, Result{Check: check, Status: Fail, Detail: err.Error(),
			Fix: "check that the server certificate names this address, that cacert.pem signed it, and that libvirtd accepts the client certificate (tls_allowed_dn_list)"}
	}
	return hv, Result{Check: check, Status: Pass, Detail: "connected to qemu:///system at " + addr}
}

// diskNeeded is the most disk the cluster's machines can take: every disk
// at its full size, every autoscaled pool at its maximum.
func diskNeeded(cfg *config.ClusterConfig) (uint64, int32) {
	cp := cfg.Spec.ControlPlane
	total := resource.NewQuantity(0, resource.BinarySI)
	n := cp.Replicas
	for range cp.Replicas {
		total.Add(cp.DiskSize)
	}
	for _, p := range cfg.Spec.NodePools {
		r := p.Replicas
		if p.Autoscaling != nil {
			r = p.Autoscaling.Max
		}
		n += r
		for range r {
			total.Add(p.DiskSize)
		}
	}
	v, _ := total.AsInt64()
	return uint64(v), n
}

func checkPool(hv Hypervisor, cfg *config.ClusterConfig) Result {
	name := cfg.Spec.StoragePool
	check := "storage pool " + name
	info, err := hv.Pool(name)
	switch {
	case errors.Is(err, ErrNotFound):
		return Result{Check: check, Status: Fail, Detail: "no storage pool " + name + " on the hypervisor",
			Fix: "create it (virsh pool-define-as, pool-build, pool-start, pool-autostart) or set spec.storagePool"}
	case err != nil:
		return Result{Check: check, Status: Fail, Detail: err.Error()}
	case !info.Active:
		return Result{Check: check, Status: Fail, Detail: "the pool exists but is not running", Fix: "virsh pool-start " + name + " && virsh pool-autostart " + name}
	}
	need, machines := diskNeeded(cfg)
	if info.Available < need {
		return Result{Check: check, Status: Warn,
			Detail: fmt.Sprintf("%s free; the %d machines' disks can grow to %s (they start thin, as overlays on the image)", gib(info.Available), machines, gib(need)),
			Fix:    "free space in the pool, or plan for the disks not filling up"}
	}
	return Result{Check: check, Status: Pass, Detail: fmt.Sprintf("running, %s free, enough for %d machines' disks at full size (%s)", gib(info.Available), machines, gib(need))}
}

func gib(b uint64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }

func checkImage(hv Hypervisor, cfg *config.ClusterConfig) Result {
	img, pool := cfg.Spec.Image.Name, cfg.Spec.StoragePool
	check := "base image"
	err := hv.Volume(pool, img)
	switch {
	case errors.Is(err, ErrNotFound):
		return Result{Check: check, Status: Fail, Detail: img + " is not in pool " + pool,
			Fix: "upload it into the pool (virsh vol-upload) or set spec.image.name"}
	case err != nil:
		return Result{Check: check, Status: Fail, Detail: err.Error()}
	}
	return Result{Check: check, Status: Pass, Detail: img + " in pool " + pool}
}

// checkBridge checks a site bridge exists and is up, and that no port of it
// holds an address of its own (PET-34: a port that also takes a DHCP lease).
func checkBridge(hv Hypervisor, name string) (Result, *Iface) {
	check := "bridge " + name
	ifc, err := hv.Interface(name)
	switch {
	case errors.Is(err, ErrNotFound):
		return Result{Check: check, Status: Fail, Detail: "no interface " + name + " on the hypervisor",
			Fix: "create the bridge on the hypervisor (netplan) or fix spec.network.bridges"}, nil
	case err != nil:
		return Result{Check: check, Status: Fail, Detail: err.Error()}, nil
	case !ifc.IsBridge:
		return Result{Check: check, Status: Fail, Detail: name + " is not a bridge", Fix: "machines attach to a Linux bridge; name one in spec.network.bridges"}, nil
	case !ifc.Active:
		return Result{Check: check, Status: Fail, Detail: name + " is down", Fix: "bring it up on the hypervisor"}, nil
	}
	var dirty []string
	for _, m := range ifc.Members {
		if len(m.Addresses) > 0 {
			dirty = append(dirty, m.Name+" ("+strings.Join(m.Addresses, ", ")+")")
		}
	}
	if len(dirty) > 0 {
		return Result{Check: check, Status: Warn,
			Detail: "bridge ports hold addresses of their own: " + strings.Join(dirty, "; "),
			Fix:    "a bridge port should carry no address; remove its dhcp4/addresses from netplan (PET-34)"}, &ifc
	}
	ports := make([]string, 0, len(ifc.Members))
	for _, m := range ifc.Members {
		ports = append(ports, m.Name)
	}
	return Result{Check: check, Status: Pass, Detail: "up; ports " + orNone(strings.Join(ports, ", "))}, &ifc
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

type namedIface struct {
	name string
	Iface
}

// checkEndpoint checks the control-plane endpoint is free and that the
// controllers can reach the network it lives on.
func checkEndpoint(ctx context.Context, p Prober, hv Hypervisor, bridges []namedIface, cfg *config.ClusterConfig) []Result {
	n := cfg.Spec.Network
	if mn := n.ManagedNetwork; mn != nil {
		reach := checkManagedReach(ctx, p, hv, mn)
		rs := []Result{reach}
		if n.ControlPlaneEndpoint != "" {
			rs = append(rs, checkEndpointFree(ctx, p, n.ControlPlaneEndpoint, reach.Status == Pass))
		}
		return rs
	}
	reach := checkSiteReach(ctx, p, hv, bridges, n.ControlPlaneEndpoint)
	return []Result{reach, checkEndpointFree(ctx, p, n.ControlPlaneEndpoint, reach.Status == Pass)}
}

// answers reports whether host answered on any of ports: a connection, or a
// refusal, which only a live host sends.
func answers(ctx context.Context, p Prober, host string, ports ...int) (bool, []Outcome, error) {
	var seen []Outcome
	for _, port := range ports {
		o, err := p.TCP(ctx, host, port)
		if err != nil {
			return false, seen, fmt.Errorf("probing %s:%d from %s: %w", host, port, p.Where(), err)
		}
		seen = append(seen, o)
		if o == Open || o == Refused {
			return true, seen, nil
		}
	}
	return false, seen, nil
}

// checkSiteReach shows the endpoint's network is reachable from where the
// controllers run, by probing the hypervisor's own address on the bridge
// whose subnet holds the endpoint. Without that, silence from the endpoint
// could mean a free address or an unreachable network (PET-75).
func checkSiteReach(ctx context.Context, p Prober, hv Hypervisor, bridges []namedIface, endpoint string) Result {
	const check = "control-plane network reachable"
	host, _, err := config.SplitEndpoint(endpoint)
	if err != nil {
		return Result{Check: check, Status: Fail, Detail: err.Error()}
	}
	ip := net.ParseIP(host)
	switch {
	case hv == nil:
		return Result{Check: check, Status: Warn, Detail: "not checked: the hypervisor was not queried, so its address on the endpoint's network is unknown",
			Fix: "set spec.hypervisor and pass --hypervisor-credentials"}
	case ip == nil:
		return Result{Check: check, Status: Warn, Detail: "the endpoint " + host + " is a name, not an address; its network is not checked"}
	}
	var seenNets []string
	for _, b := range bridges {
		for _, a := range b.Addresses {
			bip, subnet, err := net.ParseCIDR(a)
			if err != nil {
				continue
			}
			seenNets = append(seenNets, b.name+" "+subnet.String())
			if !subnet.Contains(ip) {
				continue
			}
			ok, seen, err := answers(ctx, p, bip.String(), sshPort, libvirtTLSPort)
			if err != nil {
				return Result{Check: check, Status: Fail, Detail: err.Error()}
			}
			if ok {
				return Result{Check: check, Status: Pass, Detail: fmt.Sprintf("the hypervisor's %s address %s answers from %s", b.name, bip, p.Where())}
			}
			return Result{Check: check, Status: Fail,
				Detail: fmt.Sprintf("the hypervisor's %s address %s does not answer from %s (%s): the bootstrap cluster's controllers could not reach the new API, and create would stall", b.name, bip, p.Where(), outcomes(seen)),
				Fix:    "run hydra from a machine on, or routed to, " + subnet.String() + " (PET-75)"}
		}
	}
	return Result{Check: check, Status: Warn,
		Detail: fmt.Sprintf("%s is on no bridge's subnet the hypervisor has an address in (%s), so its network is not checked", host, orNone(strings.Join(seenNets, ", "))),
		Fix:    "check the endpoint is on the cluster's site network"}
}

func outcomes(os []Outcome) string {
	s := make([]string, len(os))
	for i, o := range os {
		s[i] = o.String()
	}
	return strings.Join(s, ", ")
}

// checkEndpointFree probes the endpoint's API and SSH ports. Any answer means
// the address is taken. Silence means free only once its network is known to
// be reachable (reachable); otherwise it is reported as unknown.
func checkEndpointFree(ctx context.Context, p Prober, endpoint string, reachable bool) Result {
	const check = "control-plane endpoint free"
	host, _, err := config.SplitEndpoint(endpoint)
	if err != nil {
		return Result{Check: check, Status: Fail, Detail: err.Error()}
	}
	if p.OnThisHost() && hostOnly(host) {
		return Result{Check: check, Status: Fail, Detail: host + " is this machine's loopback", Fix: "the endpoint must be an address on the cluster's network"}
	}
	taken, seen, err := answers(ctx, p, host, apiServerPort, sshPort)
	switch {
	case err != nil:
		return Result{Check: check, Status: Fail, Detail: err.Error()}
	case taken:
		return Result{Check: check, Status: Fail,
			Detail: fmt.Sprintf("%s answers from %s (%s): the address is already in use", host, p.Where(), outcomes(seen)),
			Fix:    "choose a free address outside every DHCP range for spec.network.controlPlaneEndpoint; if this cluster already exists, validate is for before bootstrap"}
	case !reachable:
		return Result{Check: check, Status: Warn,
			Detail: fmt.Sprintf("nothing answers at %s from %s (%s), but its network is not shown reachable, so that does not prove it free", host, p.Where(), outcomes(seen)),
			Fix:    "resolve the reachability check above, or confirm by hand that nothing uses " + host}
	}
	return Result{Check: check, Status: Pass, Detail: fmt.Sprintf("nothing answers at %s from %s, on a network that is reachable", host, p.Where())}
}

// checkManagedReach checks the controllers can reach a managed network, which
// is routed behind the hypervisor (forward mode open), not bridged to the
// site: PET-75 found a site that could not. libvirt's DNS listens on the
// network's gateway, the subnet's first address, so a TCP connect to it
// shows a route.
func checkManagedReach(ctx context.Context, p Prober, hv Hypervisor, mn *config.ManagedNetworkSpec) Result {
	check := "managed network " + mn.Name + " reachable"
	gw, err := firstHost(mn.Subnet)
	if err != nil {
		return Result{Check: check, Status: Fail, Detail: err.Error()}
	}
	fix := "the machine running the bootstrap cluster needs a route to " + mn.Subnet + " via the hypervisor (PET-75), or run hydra on a machine that has one"
	if hv == nil {
		return Result{Check: check, Status: Warn, Detail: "not checked: the hypervisor was not queried", Fix: "set spec.hypervisor and pass --hypervisor-credentials so validate can find the network"}
	}
	active, err := hv.Network(mn.Name)
	switch {
	case errors.Is(err, ErrNotFound) || (err == nil && !active):
		return Result{Check: check, Status: Warn,
			Detail: "the network does not run yet (create makes it), so a route to " + mn.Subnet + " cannot be tested",
			Fix:    fix}
	case err != nil:
		return Result{Check: check, Status: Fail, Detail: err.Error()}
	}
	o, err := p.TCP(ctx, gw, dnsPort)
	if err != nil {
		return Result{Check: check, Status: Fail, Detail: fmt.Sprintf("probing %s:%d from %s: %v", gw, dnsPort, p.Where(), err), Fix: fix}
	}
	if o == Open || o == Refused {
		return Result{Check: check, Status: Pass, Detail: fmt.Sprintf("gateway %s answers from %s", gw, p.Where())}
	}
	return Result{Check: check, Status: Fail,
		Detail: fmt.Sprintf("gateway %s from %s: %s; the controllers could not reach the new cluster's API", gw, p.Where(), o),
		Fix:    fix}
}

func firstHost(cidr string) (string, error) {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", err
	}
	ip := n.IP.To4()
	if ip == nil {
		return "", fmt.Errorf("managed network subnet %s is not IPv4", cidr)
	}
	gw := make(net.IP, 4)
	copy(gw, ip)
	gw[3]++
	return gw.String(), nil
}
