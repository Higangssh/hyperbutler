package netstack

import (
	"errors"
	"net/netip"
	"testing"
	"time"
)

func TestPolicyCheck(t *testing.T) {
	host := []netip.Addr{netip.MustParseAddr("192.168.1.10"), netip.MustParseAddr("100.101.102.103"), netip.MustParseAddr("2001:db8::10")}
	c := Config{HostAddrs: func() ([]netip.Addr, error) { return host, nil }}.withDefaults()
	p := newPolicy(&c)
	deny := []string{
		"127.0.0.1", "127.255.0.1", "0.0.0.0", "0.1.2.3", "169.254.169.254", "169.254.1.1",
		"224.0.0.1", "239.255.255.250", "255.255.255.255", "240.0.0.1",
		"192.168.127.1", "192.168.127.2", "192.168.127.254",
		"192.168.1.10", "100.101.102.103", "2001:db8::10",
		"::", "::1", "::127.0.0.1", "::ffff:127.0.0.1", "::ffff:192.168.1.10", "fe80::1", "ff02::1",
		"2002:7f00:1::", "64:ff9b::7f00:1", "2001:0:7f00:1::",
	}
	for _, s := range deny {
		if err := p.Check(netip.MustParseAddr(s)); err == nil {
			t.Errorf("%s allowed", s)
		}
	}
	allow := []string{"1.1.1.1", "93.184.215.14", "192.168.1.1", "100.64.0.7", "10.0.0.1", "2606:4700::1111"}
	for _, s := range allow {
		if err := p.Check(netip.MustParseAddr(s)); err != nil {
			t.Errorf("%s denied: %v", s, err)
		}
	}
}

func TestPolicyFailsClosed(t *testing.T) {
	c := Config{HostAddrs: func() ([]netip.Addr, error) { return nil, errors.New("boom") }}.withDefaults()
	p := newPolicy(&c)
	if err := p.Check(netip.MustParseAddr("1.1.1.1")); !errors.Is(err, ErrHostAddress) {
		t.Fatalf("got %v, want ErrHostAddress", err)
	}
}

func TestPolicyRefreshesHostAddrs(t *testing.T) {
	addrs := []netip.Addr{}
	c := Config{HostAddrs: func() ([]netip.Addr, error) { return addrs, nil }}.withDefaults()
	p := newPolicy(&c)
	now := time.Unix(1000, 0)
	p.now = func() time.Time { return now }
	vpn := netip.MustParseAddr("100.70.0.1")
	if err := p.Check(vpn); err != nil {
		t.Fatal(err)
	}
	addrs = []netip.Addr{vpn} // a VPN connects and gives the host this address
	now = now.Add(hostAddrCacheTTL)
	if err := p.Check(vpn); !errors.Is(err, ErrHostAddress) {
		t.Fatalf("new host address allowed after refresh: %v", err)
	}
}

func TestPolicyRouteForwards(t *testing.T) {
	v := netip.MustParseAddrPort("192.168.127.254:5201")
	h := netip.MustParseAddrPort("127.0.0.1:5201")
	c := Config{HostForwards: []HostForward{{v, h}}, HostAddrs: func() ([]netip.Addr, error) { return nil, nil }}.withDefaults()
	p := newPolicy(&c)
	got, err := p.Route(v)
	if err != nil || got.Kind != TargetForward || got.Addr != h {
		t.Fatalf("Route(%s) = %+v %v", v, got, err)
	}
	if _, err := p.Route(netip.MustParseAddrPort("192.168.127.254:5202")); err == nil {
		t.Fatal("other port on the virtual address allowed")
	}
	if _, err := p.Route(netip.MustParseAddrPort("1.1.1.1:0")); err == nil {
		t.Fatal("port 0 allowed")
	}
}

// REQ-NET-004: the host gateway address maps port P to 127.0.0.1:P and nothing else;
// every other host, LAN, link-local and subnet address stays refused.
func TestPolicyRouteHostGateway(t *testing.T) {
	host := []netip.Addr{netip.MustParseAddr("192.168.1.10")}
	fwd := netip.MustParseAddrPort("192.168.127.254:5201")
	c := Config{
		HostForwards: []HostForward{{fwd, netip.MustParseAddrPort("127.0.0.1:9999")}},
		HostGateway:  &HostGateway{Enabled: true},
		HostAddrs:    func() ([]netip.Addr, error) { return host, nil },
	}.withDefaults()
	p := newPolicy(&c)
	hg := c.HostGatewayAddr()
	if hg != netip.MustParseAddr("192.168.127.254") {
		t.Fatalf("host gateway address %s", hg)
	}
	for _, port := range []uint16{1, 80, 5432, 65535} {
		got, err := p.Route(netip.AddrPortFrom(hg, port))
		want := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port)
		if err != nil || got.Kind != TargetHostGateway || got.Addr != want {
			t.Fatalf("port %d: Route = %+v %v, want %s", port, got, err, want)
		}
	}
	// An IPv4-mapped spelling of the address is the same address.
	if got, err := p.Route(netip.AddrPortFrom(netip.MustParseAddr("::ffff:192.168.127.254"), 80)); err != nil || got.Kind != TargetHostGateway {
		t.Fatalf("mapped address: %+v %v", got, err)
	}
	// A HostForward on the same address and port wins.
	if got, err := p.Route(fwd); err != nil || got.Kind != TargetForward {
		t.Fatalf("forward: %+v %v", got, err)
	}
	if _, err := p.Route(netip.AddrPortFrom(hg, 0)); !errors.Is(err, ErrHostGatewayPort) {
		t.Fatalf("port 0: %v", err)
	}
	for _, s := range []string{"192.168.127.253:80", "192.168.127.1:80", "192.168.127.255:80", "192.168.1.10:80", "127.0.0.1:80", "169.254.169.254:80"} {
		if got, err := p.Route(netip.MustParseAddrPort(s)); err == nil {
			t.Errorf("%s routed to %+v", s, got)
		}
	}

	ports, err := ParsePortList("5432,8000-8001")
	if err != nil {
		t.Fatal(err)
	}
	p.hg.Store(&HostGateway{Enabled: true, Ports: ports})
	for port, ok := range map[uint16]bool{5432: true, 8000: true, 8001: true, 8002: false, 80: false, 5431: false} {
		_, err := p.Route(netip.AddrPortFrom(hg, port))
		if ok != (err == nil) {
			t.Errorf("ports %s, port %d: err %v", ports, port, err)
		}
		if !ok && !errors.Is(err, ErrHostGatewayPort) {
			t.Errorf("port %d: %v, want ErrHostGatewayPort", port, err)
		}
	}
	p.hg.Store(&HostGateway{Enabled: false})
	if _, err := p.Route(netip.AddrPortFrom(hg, 5432)); !errors.Is(err, ErrHostGatewayOff) {
		t.Fatalf("off: %v", err)
	}
}

// Without a host gateway, its address is an ordinary refused subnet address.
func TestPolicyNoHostGateway(t *testing.T) {
	c := Config{HostAddrs: func() ([]netip.Addr, error) { return nil, nil }}.withDefaults()
	p := newPolicy(&c)
	if _, err := p.Route(netip.MustParseAddrPort("192.168.127.254:80")); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("got %v", err)
	}
}

func TestParsePortList(t *testing.T) {
	for in, want := range map[string]string{
		"": "all", "all": "all", " ALL ": "all", "1-65535": "all", "65535,1-65534": "all",
		"80": "80", "443, 80": "80,443", "8000-8100,8050-8200": "8000-8200",
		"10,11,12": "10-12", "080, 00443": "80,443", "1": "1", "65535": "65535", "5-5": "5", " 7 - 9 ": "7-9",
	} {
		l, err := ParsePortList(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if l.String() != want {
			t.Errorf("%q: %s, want %s", in, l, want)
		}
		if err := l.validate(); err != nil {
			t.Errorf("%q: %v", in, err)
		}
	}
	for _, in := range []string{"0", "65536", "-1", "1-", "-5", "9-8", "a", "80,,81", "80;81", "+80", "0x50", "1-2-3", "080 81", "99999999999999999999"} {
		if l, err := ParsePortList(in); err == nil {
			t.Errorf("%q accepted as %s", in, l)
		}
	}
	l, _ := ParsePortList("22,80-90,443")
	for p, want := range map[uint16]bool{21: false, 22: true, 23: false, 79: false, 80: true, 85: true, 90: true, 91: false, 443: true, 444: false} {
		if l.Contains(p) != want {
			t.Errorf("Contains(%d) = %v", p, !want)
		}
	}
	if (HostGateway{Enabled: true}).Allows(0) {
		t.Error("port 0 allowed")
	}
}
