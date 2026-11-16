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
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/Petatron/hydra/internal/bootstrap"
)

const (
	dialTimeout = 10 * time.Second
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
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// libvirtHypervisor is a read-only libvirt connection.
type libvirtHypervisor struct {
	l *golibvirt.Libvirt
}

// ConnectLibvirt opens a TLS connection to libvirt at addr (host:port) with
// the credentials files' contents.
func ConnectLibvirt(_ context.Context, addr string, creds map[string][]byte) (Hypervisor, error) {
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
	if err := l.ConnectToURI(golibvirt.QEMUSystem); err != nil {
		return nil, fmt.Errorf("connecting to qemu:///system at %s: %w", addr, err)
	}
	return &libvirtHypervisor{l: l}, nil
}

func (h *libvirtHypervisor) Close() error { return h.l.Disconnect() }

// notFound maps libvirt's "no such object" errors to ErrNotFound.
func notFound(err error, code golibvirt.ErrorNumber) error {
	var e golibvirt.Error
	if errors.As(err, &e) && e.Code == uint32(code) {
		return ErrNotFound
	}
	return err
}

func (h *libvirtHypervisor) Pool(name string) (PoolInfo, error) {
	p, err := h.l.StoragePoolLookupByName(name)
	if err != nil {
		return PoolInfo{}, notFound(err, golibvirt.ErrNoStoragePool)
	}
	state, capacity, _, available, err := h.l.StoragePoolGetInfo(p)
	if err != nil {
		return PoolInfo{}, err
	}
	return PoolInfo{Active: state == poolRunning, Capacity: capacity, Available: available}, nil
}

func (h *libvirtHypervisor) Volume(pool, name string) error {
	p, err := h.l.StoragePoolLookupByName(pool)
	if err != nil {
		return notFound(err, golibvirt.ErrNoStoragePool)
	}
	_, err = h.l.StorageVolLookupByName(p, name)
	return notFound(err, golibvirt.ErrNoStorageVol)
}

func (h *libvirtHypervisor) Network(name string) (bool, error) {
	n, err := h.l.NetworkLookupByName(name)
	if err != nil {
		return false, notFound(err, golibvirt.ErrNoNetwork)
	}
	active, err := h.l.NetworkIsActive(n)
	return active == 1, err
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

func (h *libvirtHypervisor) describe(name string) (ifaceXML, bool, error) {
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
func (h *libvirtHypervisor) Interface(name string) (Iface, error) {
	x, active, err := h.describe(name)
	if err != nil {
		return Iface{}, err
	}
	out := Iface{Active: active, IsBridge: x.Type == "bridge", Addresses: cidrs(x)}
	if x.Bridge == nil {
		return out, nil
	}
	for _, port := range x.Bridge.Interfaces {
		m := Member{Name: port.Name}
		px, _, err := h.describe(port.Name)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return Iface{}, err
		}
		m.Addresses = globalAddresses(px)
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
