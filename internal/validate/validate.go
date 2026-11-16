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
	"math"
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
	// NoHost: the network reports nothing at the address (EHOSTUNREACH:
	// ARP got no reply on-link, or a router said so). The network itself
	// was reached.
	NoHost
	// NoRoute: there is no route to the address's network (ENETUNREACH).
	NoRoute
)

func (o Outcome) String() string {
	return [...]string{"open", "refused", "no answer", "no host there", "no route"}[o]
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
	// Unchecked is set when the port could not be looked up on its own, so
	// its addresses are unknown.
	Unchecked bool
}

// Hypervisor is the read-only part of libvirt validate uses.
// Every call is bounded: by ctx, and by the implementation's own timeout.
type Hypervisor interface {
	Pool(ctx context.Context, name string) (PoolInfo, error)
	Volume(ctx context.Context, pool, name string) error
	Interface(ctx context.Context, name string) (Iface, error)
	// Network reports whether a libvirt network exists and is active.
	Network(ctx context.Context, name string) (active bool, err error)
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
	vantage, trusted := d.Host, true
	if rt.Status == Pass {
		var vr *Result
		vantage, vr = vantagePoint(d)
		if vr != nil {
			add(*vr)
			trusted = false
		}
	}

	h := cfg.Spec.Hypervisor
	var hv Hypervisor
	hvPort := libvirtTLSPort
	if h == nil {
		// Bootstrap refuses this (bootstrap.ErrNoHypervisor): the temporary
		// management cluster has no manager-wide libvirt-config to fall back on.
		add(Result{Check: "hypervisor", Status: Fail,
			Detail: "hydra.yaml names no hypervisor; bootstrap needs one, since its temporary management cluster has no manager-wide libvirt-config to fall back on",
			Fix:    "set spec.hypervisor.address (and credentialsSecretName if not <cluster>-libvirt-pki)"})
	} else {
		host, port := splitHypervisor(h.Address)
		hvPort = port
		add(checkReach(ctx, vantage, checkHypervisorReach, host, port,
			"open the libvirt TLS listener (libvirtd --listen, port 16514) and make sure "+vantage.Where()+" has a route to it"))
		add(checkPlaintext(ctx, vantage, host))
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
		add(checkPool(ctx, hv, cfg))
		add(checkImage(ctx, hv, cfg))
		for _, b := range cfg.Spec.Network.Bridges {
			r, ifc := checkBridge(ctx, hv, b)
			add(r)
			if ifc != nil {
				bridges = append(bridges, namedIface{b, *ifc})
			}
		}
	}
	out = append(out, checkEndpoint(ctx, vantage, hv, hvPort, bridges, cfg)...)
	if !trusted {
		out = capReachability(out)
	}
	// Every WARN and FAIL carries something to do.
	for i := range out {
		if out[i].Status != Pass && out[i].Fix == "" {
			out[i].Fix = "resolve the error shown, then rerun hydra validate"
		}
	}
	return out
}

// reachabilityChecks are the checks whose PASS rests on the vantage point.
// Check names the reachability cap matches on.
const (
	checkHypervisorReach  = "hypervisor reachable"
	checkSiteReachName    = "control-plane network reachable"
	checkManagedPrefix    = "managed network "
	checkEndpointFreeName = "control-plane endpoint free"
)

var reachabilityChecks = []string{checkHypervisorReach, checkSiteReachName, checkManagedPrefix, checkEndpointFreeName}

// capReachability turns reachability PASSes into WARNs when the probes ran
// from this machine only because the bootstrap cluster's node, where the
// controllers run, could not be entered: this machine reaching an address
// does not show the node can.
func capReachability(rs []Result) []Result {
	for i, r := range rs {
		if r.Status != Pass {
			continue
		}
		for _, c := range reachabilityChecks {
			if strings.HasPrefix(r.Check, c) {
				rs[i].Status = Warn
				rs[i].Detail += "; but only from this machine, not from the bootstrap cluster's node where the controllers run"
				rs[i].Fix = "fix the probe vantage above and rerun"
			}
		}
	}
	return rs
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
			Detail: "cannot run probes inside the bootstrap cluster's node (" + err.Error() + "); probing from this machine instead, so reachability cannot pass",
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
	target := fmt.Sprintf("%s:%d", host, libvirtTCPPort)
	o, err := p.TCP(ctx, host, libvirtTCPPort)
	switch {
	case err != nil:
		return Result{Check: check, Status: Warn, Detail: fmt.Sprintf("%s was not observed from %s (%v)", target, p.Where(), err),
			Fix: "check by hand that libvirtd's TCP listener is off"}
	case o == Open:
		return Result{Check: check, Status: Warn,
			Detail: target + " accepts connections: libvirt's unauthenticated TCP listener is open",
			Fix:    "disable libvirtd's TCP listener (listen_tcp = 0, and mask libvirtd-tcp.socket); hydra only uses TLS on 16514"}
	case o == Refused:
		return Result{Check: check, Status: Pass, Detail: target + " refuses connections"}
	}
	// A timeout or no route: the port was not seen either way.
	return Result{Check: check, Status: Warn, Detail: fmt.Sprintf("%s was not observed from %s (%s), so it is not shown closed", target, p.Where(), o),
		Fix: "check by hand that libvirtd's TCP listener is off"}
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
	// No key-usage extension at all (KeyUsage == 0) restricts nothing (RFC
	// 5280 4.2.1.3), and GnuTLS, libvirt's TLS, accepts it. What fails is an
	// extension that is present and leaves keyEncipherment out.
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
	v, ok := total.AsInt64()
	if !ok || v < 0 {
		// Too large to count: no pool holds it.
		return math.MaxUint64, n
	}
	return uint64(v), n
}

func checkPool(ctx context.Context, hv Hypervisor, cfg *config.ClusterConfig) Result {
	name := cfg.Spec.StoragePool
	check := "storage pool " + name
	info, err := hv.Pool(ctx, name)
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

func checkImage(ctx context.Context, hv Hypervisor, cfg *config.ClusterConfig) Result {
	img, pool := cfg.Spec.Image.Name, cfg.Spec.StoragePool
	check := "base image"
	err := hv.Volume(ctx, pool, img)
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
func checkBridge(ctx context.Context, hv Hypervisor, name string) (Result, *Iface) {
	check := "bridge " + name
	ifc, err := hv.Interface(ctx, name)
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
	var dirty, unchecked []string
	for _, m := range ifc.Members {
		switch {
		case m.Unchecked:
			unchecked = append(unchecked, m.Name)
		case len(m.Addresses) > 0:
			dirty = append(dirty, m.Name+" ("+strings.Join(m.Addresses, ", ")+")")
		}
	}
	reported := len(ifc.Addresses) > 0
	for _, m := range ifc.Members {
		reported = reported || len(m.Addresses) > 0
	}
	if !reported && len(ifc.Members) > len(unchecked) {
		names := make([]string, 0, len(ifc.Members))
		for _, m := range ifc.Members {
			names = append(names, m.Name)
		}
		return Result{Check: check, Status: Warn,
			Detail: "up, but libvirt reports no addresses on " + name + " or its ports (its udev interface backend never does), so the ports were not checked for addresses of their own",
			Fix:    "check on the hypervisor that " + strings.Join(names, ", ") + " carries no address of its own (ip -br addr show), PET-34"}, &ifc
	}
	if len(unchecked) > 0 && len(dirty) == 0 {
		return Result{Check: check, Status: Warn,
			Detail: "ports named on the bridge could not be looked up, so their addresses were not checked: " + strings.Join(unchecked, ", "),
			Fix:    "check on the hypervisor that " + strings.Join(unchecked, ", ") + " carries no address (ip addr show)"}, &ifc
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

// endpointProbe is what probing the control-plane endpoint observed.
type endpointProbe struct {
	host    string
	outcome Outcome
	port    int
}

// probeEndpoint probes the endpoint's API port, then SSH, stopping at the
// first decisive outcome: anything but silence.
func probeEndpoint(ctx context.Context, p Prober, host string) (endpointProbe, error) {
	ep := endpointProbe{host: host, outcome: NoAnswer}
	for _, port := range []int{apiServerPort, sshPort} {
		o, err := p.TCP(ctx, host, port)
		if err != nil {
			return ep, fmt.Errorf("probing %s:%d from %s: %w", host, port, p.Where(), err)
		}
		if o != NoAnswer {
			ep.outcome, ep.port = o, port
			return ep, nil
		}
	}
	return ep, nil
}

// checkEndpoint checks the control-plane endpoint is free and that the
// controllers can reach the network it lives on: the managed network's, or,
// for an endpoint outside it (or with no managed network), the site's.
func checkEndpoint(ctx context.Context, p Prober, hv Hypervisor, hvPort int, bridges []namedIface, cfg *config.ClusterConfig) []Result {
	n := cfg.Spec.Network
	var managed Result
	if mn := n.ManagedNetwork; mn != nil {
		managed = checkManagedReach(ctx, p, hv, mn)
		if n.ControlPlaneEndpoint == "" {
			return []Result{managed}
		}
	}
	host, _, err := config.SplitEndpoint(n.ControlPlaneEndpoint)
	if err != nil {
		return append(nonEmpty(managed), Result{Check: checkEndpointFreeName, Status: Fail, Detail: err.Error(),
			Fix: "fix spec.network.controlPlaneEndpoint"})
	}
	if p.OnThisHost() && hostOnly(host) {
		return append(nonEmpty(managed), Result{Check: checkEndpointFreeName, Status: Fail,
			Detail: host + " is this machine's loopback", Fix: "the endpoint must be an address on the cluster's network"})
	}
	ep, err := probeEndpoint(ctx, p, host)
	if err != nil {
		if net.ParseIP(host) == nil {
			// A name that does not resolve yet, say: not checkable, which is
			// what a name endpoint is.
			return append(nonEmpty(managed), Result{Check: checkEndpointFreeName, Status: Warn,
				Detail: "the endpoint " + host + " is a name, and probing it failed (" + err.Error() + "), so it is not checked",
				Fix:    "make sure " + host + " resolves to a free address on the cluster's network before create"})
		}
		return append(nonEmpty(managed), Result{Check: checkEndpointFreeName, Status: Fail, Detail: err.Error(),
			Fix: "check that this machine can open TCP connections, then rerun"})
	}
	rs := nonEmpty(managed)
	reachable := managed.Status == Pass
	if n.ManagedNetwork == nil || !inSubnet(host, n.ManagedNetwork.Subnet) {
		// The endpoint is on the site network: its reachability is the
		// site's, whatever the managed network's is.
		site := checkSiteReach(ctx, p, hv, hvPort, bridges, ep)
		rs = append(rs, site)
		reachable = site.Status == Pass
	}
	return append(rs, checkEndpointFree(p, ep, reachable))
}

// inSubnet reports whether host is an IP inside cidr.
func inSubnet(host, cidr string) bool {
	ip := net.ParseIP(host)
	_, n, err := net.ParseCIDR(cidr)
	return ip != nil && err == nil && n.Contains(ip)
}

func nonEmpty(r Result) []Result {
	if r.Check == "" {
		return nil
	}
	return []Result{r}
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
// controllers run. The endpoint's own probe shows it when the network
// answered for the address (a host there, or the network saying none is);
// failing that, the hypervisor's own address on the bridge whose subnet
// holds the endpoint, when libvirt reports bridge addresses. Without either,
// silence could mean a free address or an unreachable network (PET-75).
// hvPort is the hypervisor's libvirt TLS port, as hydra.yaml states it.
func checkSiteReach(ctx context.Context, p Prober, hv Hypervisor, hvPort int, bridges []namedIface, ep endpointProbe) Result {
	const check = checkSiteReachName
	fixRoute := "run hydra from a machine on, or routed to, the endpoint's network (PET-75)"
	switch ep.outcome {
	case Open, Refused, NoHost:
		return Result{Check: check, Status: Pass, Detail: fmt.Sprintf("the network answered for %s from %s (%s)", ep.host, p.Where(), ep.outcome)}
	case NoRoute:
		return Result{Check: check, Status: Fail,
			Detail: fmt.Sprintf("no route from %s to %s: the bootstrap cluster's controllers could not reach the new API, and create would stall", p.Where(), ep.host),
			Fix:    fixRoute}
	}
	ip := net.ParseIP(ep.host)
	if hv == nil || ip == nil {
		return Result{Check: check, Status: Warn,
			Detail: fmt.Sprintf("nothing answered for %s from %s, and there is no other way here to show its network reachable", ep.host, p.Where()),
			Fix:    "run validate from a machine on the endpoint's network, where a free address is reported as such; or set spec.hypervisor and pass --hypervisor-credentials"}
	}
	for _, b := range bridges {
		for _, a := range b.Addresses {
			bip, subnet, err := net.ParseCIDR(a)
			if err != nil || !subnet.Contains(ip) {
				continue
			}
			ok, seen, err := answers(ctx, p, bip.String(), sshPort, hvPort)
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
		Detail: fmt.Sprintf("nothing answered for %s from %s, and libvirt reports no hypervisor address on a bridge subnet holding it, so its network is not shown reachable", ep.host, p.Where()),
		Fix:    "run validate from a machine on the endpoint's network, where a free address is reported as such"}
}

func outcomes(os []Outcome) string {
	s := make([]string, len(os))
	for i, o := range os {
		s[i] = o.String()
	}
	return strings.Join(s, ", ")
}

// checkEndpointFree judges the endpoint's probe. Any host answering means
// the address is taken; the network reporting no host there means it is
// free. Silence means free only once its network is known to be reachable.
func checkEndpointFree(p Prober, ep endpointProbe, reachable bool) Result {
	const check = checkEndpointFreeName
	switch {
	case ep.outcome == Open || ep.outcome == Refused:
		return Result{Check: check, Status: Fail,
			Detail: fmt.Sprintf("%s answers on port %d from %s (%s): the address is already in use", ep.host, ep.port, p.Where(), ep.outcome),
			Fix:    "choose a free address outside every DHCP range for spec.network.controlPlaneEndpoint; if this cluster already exists, validate is for before bootstrap"}
	case ep.outcome == NoHost:
		return Result{Check: check, Status: Pass, Detail: fmt.Sprintf("the network reports no host at %s (seen from %s)", ep.host, p.Where())}
	case ep.outcome == NoAnswer && reachable:
		return Result{Check: check, Status: Pass, Detail: fmt.Sprintf("nothing answers at %s from %s, on a network that is reachable", ep.host, p.Where())}
	}
	return Result{Check: check, Status: Warn,
		Detail: fmt.Sprintf("nothing answers at %s from %s (%s), but its network is not shown reachable, so that does not prove it free", ep.host, p.Where(), ep.outcome),
		Fix:    "resolve the reachability check above, or confirm by hand that nothing uses " + ep.host}
}

// checkManagedReach checks the controllers can reach a managed network, which
// is routed behind the hypervisor (forward mode open), not bridged to the
// site: PET-75 found a site that could not. libvirt's DNS listens on the
// network's gateway, the subnet's first address, so a TCP connect to it
// shows a route.
func checkManagedReach(ctx context.Context, p Prober, hv Hypervisor, mn *config.ManagedNetworkSpec) Result {
	check := checkManagedPrefix + mn.Name + " reachable"
	gw, err := firstHost(mn.Subnet)
	if err != nil {
		return Result{Check: check, Status: Fail, Detail: err.Error()}
	}
	fix := "the machine running the bootstrap cluster needs a route to " + mn.Subnet + " via the hypervisor (PET-75), or run hydra on a machine that has one"
	if hv == nil {
		return Result{Check: check, Status: Warn, Detail: "not checked: the hypervisor was not queried", Fix: "set spec.hypervisor and pass --hypervisor-credentials so validate can find the network"}
	}
	active, err := hv.Network(ctx, mn.Name)
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
	switch o {
	case Open, Refused:
		return Result{Check: check, Status: Pass, Detail: fmt.Sprintf("gateway %s answers from %s", gw, p.Where())}
	case NoRoute, NoHost:
		return Result{Check: check, Status: Fail,
			Detail: fmt.Sprintf("gateway %s from %s: %s; the controllers could not reach the new cluster's API", gw, p.Where(), o),
			Fix:    fix}
	}
	// A routed network installs no libvirt filter, so the hypervisor's own
	// firewall decides; it can drop port 53 to the gateway and still forward
	// to the machines.
	return Result{Check: check, Status: Warn,
		Detail: fmt.Sprintf("nothing answered on gateway %s:%d from %s: either there is no route to %s, or the hypervisor drops port 53 to its own address", gw, dnsPort, p.Where(), mn.Subnet),
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
