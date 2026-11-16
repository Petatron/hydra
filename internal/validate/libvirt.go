package validate

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/Petatron/hydra/internal/bootstrap"
)

const (
	dialTimeout = 10 * time.Second
	// rpcTimeout bounds every libvirt call after the handshake, the connect
	// (auth and connect-open) included: go-libvirt waits on replies with no
	// deadline of its own.
	rpcTimeout = 15 * time.Second
	// closeTimeout bounds Disconnect, which blocks on a daemon that stopped
	// answering.
	closeTimeout = 5 * time.Second
	// tlsVerificationOK is the byte libvirtd sends after the TLS handshake
	// when it accepts the client certificate.
	tlsVerificationOK = 1
	// poolRunning is VIR_STORAGE_POOL_RUNNING.
	poolRunning = 2
)

// tlsDialer dials libvirtd's TLS listener the way the provider does: one
// deadline over connect, handshake and libvirtd's verdict byte.
type tlsDialer struct {
	addr string
	conf *tls.Config

	mu   sync.Mutex
	conn net.Conn // the last connection dialled, closed to abandon a hung call
}

// abandon closes the connection, which unblocks any call waiting on it.
func (d *tlsDialer) abandon() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn != nil {
		_ = d.conn.Close()
	}
}

func (d *tlsDialer) Dial() (net.Conn, error) {
	deadline := time.Now().Add(dialTimeout)
	raw, err := (&net.Dialer{Deadline: deadline}).Dial("tcp", d.addr)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", d.addr, err)
	}
	conn := tls.Client(raw, d.conf)
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := conn.Handshake(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("TLS handshake with %s: %w", d.addr, err)
	}
	verdict := make([]byte, 1)
	if _, err := io.ReadFull(conn, verdict); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("waiting for %s to accept the client certificate: %w", d.addr, err)
	}
	if verdict[0] != tlsVerificationOK {
		_ = conn.Close()
		return nil, fmt.Errorf("%s rejected the client certificate (tls_allowed_dn_list?)", d.addr)
	}
	// From here every call is bounded by call(), which closes the connection
	// when its deadline passes.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	d.mu.Lock()
	d.conn = conn
	d.mu.Unlock()
	return conn, nil
}

// libvirtHypervisor is a read-only libvirt connection.
type libvirtHypervisor struct {
	l *golibvirt.Libvirt
	d *tlsDialer
}

// call runs fn with a deadline: the earlier of ctx's and rpcTimeout. When it
// passes, the connection is closed so fn's blocked read returns, and the
// connection is not used again.
func call[T any](ctx context.Context, d *tlsDialer, what string, fn func() (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	type res struct {
		v   T
		err error
	}
	done := make(chan res, 1)
	go func() {
		v, err := fn()
		done <- res{v, err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-ctx.Done():
		d.abandon()
		var zero T
		return zero, fmt.Errorf("%s: libvirt did not answer: %w", what, ctx.Err())
	}
}

// ConnectLibvirt opens a TLS connection to libvirt at addr (host:port) with
// the credentials files' contents.
func ConnectLibvirt(ctx context.Context, addr string, creds map[string][]byte) (Hypervisor, error) {
	cert, err := tls.X509KeyPair(creds[bootstrap.ClientCertFile], creds[bootstrap.ClientKeyFile])
	if err != nil {
		return nil, fmt.Errorf("client certificate or key: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(creds[bootstrap.CACertFile]) {
		return nil, errors.New("cacert.pem holds no PEM certificate")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	d := &tlsDialer{addr: addr, conf: &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      roots,
		ServerName:   host,
		MinVersion:   tls.VersionTLS12,
	}}
	l := golibvirt.NewWithDialer(d)
	if _, err := call(ctx, d, "connecting to qemu:///system", func() (struct{}, error) {
		return struct{}{}, l.ConnectToURI(golibvirt.QEMUSystem)
	}); err != nil {
		d.abandon()
		return nil, fmt.Errorf("connecting to qemu:///system at %s: %w", addr, err)
	}
	return &libvirtHypervisor{l: l, d: d}, nil
}

// Close disconnects, giving up on a daemon that does not answer.
func (h *libvirtHypervisor) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	_, err := call(ctx, h.d, "disconnecting", func() (struct{}, error) { return struct{}{}, h.l.Disconnect() })
	h.d.abandon()
	return err
}

// notFound maps libvirt's "no such object" errors to ErrNotFound.
func notFound(err error, code golibvirt.ErrorNumber) error {
	var e golibvirt.Error
	if errors.As(err, &e) && e.Code == uint32(code) {
		return ErrNotFound
	}
	return err
}

func (h *libvirtHypervisor) Pool(ctx context.Context, name string) (PoolInfo, error) {
	return call(ctx, h.d, "reading pool "+name, func() (PoolInfo, error) {
		p, err := h.l.StoragePoolLookupByName(name)
		if err != nil {
			return PoolInfo{}, notFound(err, golibvirt.ErrNoStoragePool)
		}
		state, capacity, _, available, err := h.l.StoragePoolGetInfo(p)
		if err != nil {
			return PoolInfo{}, err
		}
		return PoolInfo{Active: state == poolRunning, Capacity: capacity, Available: available}, nil
	})
}

func (h *libvirtHypervisor) Volume(ctx context.Context, pool, name string) error {
	_, err := call(ctx, h.d, "looking up "+name, func() (struct{}, error) {
		p, err := h.l.StoragePoolLookupByName(pool)
		if err != nil {
			return struct{}{}, notFound(err, golibvirt.ErrNoStoragePool)
		}
		_, err = h.l.StorageVolLookupByName(p, name)
		return struct{}{}, notFound(err, golibvirt.ErrNoStorageVol)
	})
	return err
}

func (h *libvirtHypervisor) Network(ctx context.Context, name string) (bool, error) {
	return call(ctx, h.d, "reading network "+name, func() (bool, error) {
		n, err := h.l.NetworkLookupByName(name)
		if err != nil {
			return false, notFound(err, golibvirt.ErrNoNetwork)
		}
		active, err := h.l.NetworkIsActive(n)
		return active == 1, err
	})
}

// ifaceXML is the part of libvirt's interface XML validate reads.
type ifaceXML struct {
	Type      string        `xml:"type,attr"`
	Name      string        `xml:"name,attr"`
	Protocols []protocolXML `xml:"protocol"`
	Bridge    *struct {
		Interfaces []ifaceXML `xml:"interface"`
	} `xml:"bridge"`
}

type protocolXML struct {
	IPs []struct {
		Address string `xml:"address,attr"`
		Prefix  string `xml:"prefix,attr"`
	} `xml:"ip"`
}

type described struct {
	x      ifaceXML
	active bool
}

func (h *libvirtHypervisor) describe(ctx context.Context, name string) (ifaceXML, bool, error) {
	r, err := call(ctx, h.d, "reading interface "+name, func() (described, error) {
		x, active, err := h.describeNow(name)
		return described{x, active}, err
	})
	return r.x, r.active, err
}

func (h *libvirtHypervisor) describeNow(name string) (ifaceXML, bool, error) {
	i, err := h.l.InterfaceLookupByName(name)
	if err != nil {
		return ifaceXML{}, false, notFound(err, golibvirt.ErrNoInterface)
	}
	active, err := h.l.InterfaceIsActive(i)
	if err != nil {
		return ifaceXML{}, false, err
	}
	doc, err := h.l.InterfaceGetXMLDesc(i, 0)
	if err != nil {
		return ifaceXML{}, false, err
	}
	var x ifaceXML
	if err := xml.Unmarshal([]byte(doc), &x); err != nil {
		return ifaceXML{}, false, fmt.Errorf("reading interface %s: %w", name, err)
	}
	return x, active == 1, nil
}

// Interface describes a host interface and, for a bridge, the addresses each
// port holds itself (looked up port by port: the bridge's own description
// does not carry them).
func (h *libvirtHypervisor) Interface(ctx context.Context, name string) (Iface, error) {
	x, active, err := h.describe(ctx, name)
	if err != nil {
		return Iface{}, err
	}
	out := Iface{Active: active, IsBridge: x.Type == "bridge", Addresses: cidrs(x)}
	if x.Bridge == nil {
		return out, nil
	}
	for _, port := range x.Bridge.Interfaces {
		m := Member{Name: port.Name}
		px, _, err := h.describe(ctx, port.Name)
		switch {
		case errors.Is(err, ErrNotFound):
			// Named on the bridge but not found on its own: its addresses
			// were not seen, which is not the same as having none.
			m.Unchecked = true
		case err != nil:
			return Iface{}, err
		default:
			m.Addresses = globalAddresses(px)
		}
		out.Members = append(out.Members, m)
	}
	return out, nil
}

// globalAddresses are an interface's addresses other than link-local ones,
// which the kernel gives any interface with IPv6 on.
func globalAddresses(x ifaceXML) []string {
	var out []string
	for _, p := range x.Protocols {
		for _, ip := range p.IPs {
			if a := net.ParseIP(ip.Address); a != nil && !a.IsLinkLocalUnicast() {
				out = append(out, ip.Address)
			}
		}
	}
	return out
}

// cidrs are an interface's global addresses with their prefixes.
func cidrs(x ifaceXML) []string {
	var out []string
	for _, p := range x.Protocols {
		for _, ip := range p.IPs {
			if a := net.ParseIP(ip.Address); a != nil && !a.IsLinkLocalUnicast() && ip.Prefix != "" {
				out = append(out, ip.Address+"/"+ip.Prefix)
			}
		}
	}
	return out
}
