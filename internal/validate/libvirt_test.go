package validate

import (
	"context"
	"encoding/xml"
	"net"
	"reflect"
	"strconv"
	"testing"
)

// A bridge as libvirt's udev interface backend describes it.
const bridgeXML = `<interface type='bridge' name='br0'>
  <protocol family='ipv4'><ip address='192.168.15.13' prefix='24'/></protocol>
  <protocol family='ipv6'><ip address='fe80::1' prefix='64'/><ip address='2001:db8::13' prefix='64'/></protocol>
  <link state='up'/>
  <bridge>
    <interface type='ethernet' name='enp142s0'><link speed='1000' state='up'/></interface>
  </bridge>
</interface>`

const portXML = `<interface type='ethernet' name='enp142s0'>
  <protocol family='ipv4'><ip address='192.168.15.50' prefix='24'/></protocol>
  <protocol family='ipv6'><ip address='fe80::2' prefix='64'/></protocol>
</interface>`

func TestInterfaceXML(t *testing.T) {
	var b ifaceXML
	if err := xml.Unmarshal([]byte(bridgeXML), &b); err != nil {
		t.Fatal(err)
	}
	if b.Type != "bridge" || b.Bridge == nil || len(b.Bridge.Interfaces) != 1 || b.Bridge.Interfaces[0].Name != "enp142s0" {
		t.Fatalf("bridge parsed as %+v", b)
	}
	if got := cidrs(b); !reflect.DeepEqual(got, []string{"192.168.15.13/24", "2001:db8::13/64"}) {
		t.Errorf("bridge addresses %v (link-local must be left out)", got)
	}
	var p ifaceXML
	if err := xml.Unmarshal([]byte(portXML), &p); err != nil {
		t.Fatal(err)
	}
	if got := globalAddresses(p); !reflect.DeepEqual(got, []string{"192.168.15.50"}) {
		t.Errorf("port addresses %v", got)
	}
}

func TestHostProberAgainstRealSockets(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, ps, _ := net.SplitHostPort(l.Addr().String())
	port, _ := strconv.Atoi(ps)
	ctx := context.Background()
	if o, err := HostProber().TCP(ctx, "127.0.0.1", port); o != Open || err != nil {
		t.Errorf("listening: %v %v", o, err)
	}
	_ = l.Close()
	if o, err := HostProber().TCP(ctx, "127.0.0.1", port); o != Refused || err != nil {
		t.Errorf("closed: %v %v", o, err)
	}
}

func TestFirstHost(t *testing.T) {
	if gw, err := firstHost("10.50.0.0/24"); err != nil || gw != "10.50.0.1" {
		t.Fatalf("%q %v", gw, err)
	}
}
