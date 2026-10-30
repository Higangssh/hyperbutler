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
	got, explicit, err := p.Route(v)
	if err != nil || !explicit || got != h {
		t.Fatalf("Route(%s) = %s %v %v", v, got, explicit, err)
	}
	if _, _, err := p.Route(netip.MustParseAddrPort("192.168.127.254:5202")); err == nil {
		t.Fatal("other port on the virtual address allowed")
	}
	if _, _, err := p.Route(netip.MustParseAddrPort("1.1.1.1:0")); err == nil {
		t.Fatal("port 0 allowed")
	}
}
