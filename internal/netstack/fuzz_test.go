package netstack

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/Higangssh/hyperbutler/internal/netstack/frame"
)

// FuzzParsePublish: accepted specs are loopback unless allowLAN, have non-zero ports,
// and round-trip through Publish.String to the same host address.
func FuzzParsePublish(f *testing.F) {
	for _, s := range []string{"8080:80", "127.0.0.1:8080:80/tcp", "[::1]:1:2", "0.0.0.0:80:80", "::ffff:127.0.0.1:1:1", "[::ffff:10.0.0.1]:80:80", "1:2:3:4"} {
		f.Add(s, false)
		f.Add(s, true)
	}
	f.Fuzz(func(t *testing.T, s string, lan bool) {
		p, err := ParsePublish(s, lan)
		if err != nil {
			return
		}
		a := p.Host.Addr()
		if !lan && !a.IsLoopback() {
			t.Fatalf("%q: non-loopback %s accepted without allowLAN", s, a)
		}
		if p.Host.Port() == 0 || p.GuestPort == 0 || a.Zone() != "" || a.Is4In6() || a.IsMulticast() {
			t.Fatalf("%q: bad publish %v", s, p)
		}
		if len(s) > maxSpecLen {
			t.Fatalf("over-long spec accepted")
		}
	})
}

// FuzzParseHostForward: accepted forwards are IPv4 on both sides with non-zero ports;
// whether a forward is allowed is then decided by Config.Validate, which must not panic.
func FuzzParseHostForward(f *testing.F) {
	for _, s := range []string{"192.168.127.254:5201=127.0.0.1:5201", "1.2.3.4:1=5.6.7.8:9", "=", "a:b=c:d", "192.168.127.254:65535=[::1]:1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		h, err := ParseHostForward(s)
		if err != nil {
			return
		}
		if !h.Virtual.Addr().Is4() || !h.Host.Addr().Is4() || h.Virtual.Port() == 0 || h.Host.Port() == 0 {
			t.Fatalf("%q: bad forward %v", s, h)
		}
		c := Config{HostForwards: []HostForward{h}}
		if err := c.Validate(); err == nil {
			d := c.withDefaults()
			v := h.Virtual.Addr()
			if !d.Subnet.Contains(v) || v == d.Gateway() || v == d.GuestIP() {
				t.Fatalf("%q: forward on a reserved address accepted", s)
			}
		}
	})
}

// FuzzPolicyCheck: whatever the address, loopback, link-local, multicast, unspecified,
// the guest subnet and the host's own addresses are never allowed, in any IPv6 spelling.
func FuzzPolicyCheck(f *testing.F) {
	for _, s := range []string{"127.0.0.1", "::ffff:127.0.0.1", "169.254.169.254", "10.99.0.5", "::ffff:10.99.0.5", "1.1.1.1", "2002:7f00:1::", "::7f00:1", "fe80::1%eth0"} {
		a := netip.MustParseAddr(s)
		b := a.As16()
		f.Add(b[:])
	}
	host := netip.MustParseAddr("10.99.0.5")
	c := Config{HostAddrs: func() ([]netip.Addr, error) { return []netip.Addr{host}, nil }}.withDefaults()
	p := newPolicy(&c)
	f.Fuzz(func(t *testing.T, b []byte) {
		a, ok := netip.AddrFromSlice(b)
		if !ok {
			return
		}
		if p.Check(a) != nil {
			return
		}
		u := a.Unmap()
		if u.IsLoopback() || u.IsLinkLocalUnicast() || u.IsMulticast() || u.IsUnspecified() ||
			c.Subnet.Contains(u) || u == host {
			t.Fatalf("%s allowed", a)
		}
		if u.Is4() && (u.As4()[0] == 0 || u.As4()[0] >= 240) {
			t.Fatalf("%s allowed", a)
		}
	})
}

// FuzzDNSAnswer: arbitrary bytes from the guest. The handler never panics, never answers
// a response (no reflection), answers with the query's ID, and stays within the
// transport's size limit.
func FuzzDNSAnswer(f *testing.F) {
	for _, q := range []struct {
		name string
		t    uint16
		edns uint16
	}{{"example.com.", dns.TypeA, 0}, {"many.example.com.", dns.TypeA, 4096}, {"4.3.2.1.in-addr.arpa.", dns.TypePTR, 0}, {"example.com.", dns.TypeTXT, 512}, {".", dns.TypeNS, 0}} {
		m := new(dns.Msg)
		m.SetQuestion(q.name, q.t)
		if q.edns != 0 {
			m.SetEdns0(q.edns, true)
		}
		b, _ := m.Pack()
		f.Add(b, true)
		f.Add(b, false)
	}
	for _, name := range []string{"host.docker.internal.", "GATEWAY.docker.internal.", "rebind.example.", "254.127.168.192.in-addr.arpa."} {
		m := new(dns.Msg)
		m.SetQuestion(name, dns.TypeA)
		b, _ := m.Pack()
		f.Add(b, true)
	}
	f.Add([]byte{0, 1, 0x80, 0, 0, 1, 0, 0, 0, 0, 0, 0}, true)
	h, r := newTestDNS()
	h.timeout = 50 * time.Millisecond
	// With a host gateway, and an outside name that tries to rebind into the subnet.
	h.subnet = DefaultSubnet
	h.hostGateway = func() (netip.Addr, bool) { return hgAddr, true }
	r.a["rebind.example"] = []netip.Addr{hgAddr, netip.MustParseAddr("192.168.127.2")}
	f.Fuzz(func(t *testing.T, wire []byte, udp bool) {
		resp := h.answer(context.Background(), wire, udp)
		if resp == nil {
			return
		}
		limit := dns.MaxMsgSize
		if udp {
			limit = maxUDPAnswer
		}
		if len(resp) > limit {
			t.Fatalf("answer of %d bytes over %d", len(resp), limit)
		}
		if len(wire) < 2 || resp[0] != wire[0] || resp[1] != wire[1] {
			t.Fatal("answer ID differs from the query")
		}
		var in dns.Msg
		if in.Unpack(wire) == nil && in.Response {
			t.Fatal("answered a response")
		}
		var out dns.Msg
		if err := out.Unpack(resp); err != nil {
			t.Fatalf("unparseable answer: %v", err)
		}
		for _, rr := range out.Answer {
			if !strings.EqualFold(rr.Header().Name, out.Question[0].Name) {
				t.Fatalf("answer for another name: %v", rr)
			}
			// Only the host gateway's own names point into the guest subnet.
			if a, ok := rr.(*dns.A); ok {
				ip, _ := netip.AddrFromSlice(a.A)
				if DefaultSubnet.Contains(ip.Unmap()) && (!isHostGatewayName(rr.Header().Name) || ip.Unmap() != hgAddr) {
					t.Fatalf("subnet address in an answer: %v", rr)
				}
			}
			if c, ok := rr.(*dns.CNAME); ok && isHostGatewayName(c.Target) {
				t.Fatalf("alias of the host gateway: %v", rr)
			}
		}
	})
}

// FuzzParsePortList: an accepted list is valid, round-trips through String, and
// Contains agrees with the ranges.
func FuzzParsePortList(f *testing.F) {
	for _, s := range []string{"", "all", "80", "80,443", "8000-8100, 8050-8200", "1-65535", "0", "65536", "9-8", " 7 - 9 ,10", "1,3,5-7,6"} {
		f.Add(s, uint16(80))
	}
	f.Fuzz(func(t *testing.T, s string, p uint16) {
		l, err := ParsePortList(s)
		if err != nil {
			return
		}
		if err := l.validate(); err != nil {
			t.Fatalf("%q: invalid result %v: %v", s, l, err)
		}
		back, err := ParsePortList(l.String())
		if err != nil || back.String() != l.String() || len(back) != len(l) {
			t.Fatalf("%q: %s does not round-trip: %v %v", s, l, back, err)
		}
		in := false
		for _, r := range l {
			if r.Lo <= p && p <= r.Hi {
				in = true
			}
		}
		if l.Contains(p) != in {
			t.Fatalf("%q: Contains(%d) = %v", s, p, !in)
		}
		hg := HostGateway{Enabled: true, Ports: l}
		if hg.Allows(p) != (p != 0 && (len(l) == 0 || in)) {
			t.Fatalf("%q: Allows(%d)", s, p)
		}
	})
}

// FuzzPolicyRouteHostGateway: whatever the destination, a host gateway route goes to
// 127.0.0.1 on the asked port and only for the host gateway address, and every other
// route passes Check.
func FuzzPolicyRouteHostGateway(f *testing.F) {
	for _, s := range []string{"192.168.127.254:80", "[::ffff:192.168.127.254]:0", "127.0.0.1:80", "10.99.0.5:22", "1.1.1.1:443", "192.168.127.253:80"} {
		ap := netip.MustParseAddrPort(s)
		b := ap.Addr().As16()
		f.Add(b[:], ap.Port(), true)
	}
	host := netip.MustParseAddr("10.99.0.5")
	c := Config{HostGateway: &HostGateway{Enabled: true}, HostAddrs: func() ([]netip.Addr, error) { return []netip.Addr{host}, nil }}.withDefaults()
	p := newPolicy(&c)
	f.Fuzz(func(t *testing.T, b []byte, port uint16, on bool) {
		a, ok := netip.AddrFromSlice(b)
		if !ok {
			return
		}
		p.hg.Store(&HostGateway{Enabled: on})
		tgt, err := p.Route(netip.AddrPortFrom(a, port))
		if err != nil {
			return
		}
		switch tgt.Kind {
		case TargetHostGateway:
			if !on || a.Unmap() != hgAddr || tgt.Addr != netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port) || port == 0 {
				t.Fatalf("%s:%d -> %s", a, port, tgt.Addr)
			}
		case TargetOut:
			if p.Check(tgt.Addr.Addr()) != nil || tgt.Addr.Port() == 0 || tgt.Addr.Addr() == hgAddr {
				t.Fatalf("%s:%d -> %s passes no check", a, port, tgt.Addr)
			}
		default:
			t.Fatalf("unexpected kind %d", tgt.Kind)
		}
	})
}

// FuzzDeliver feeds arbitrary guest frames into the stack (ARP and IPv4 parsing in
// gVisor, our frame filter). It must not panic, and the stack must keep working.
func FuzzDeliver(f *testing.F) {
	gw := []byte(DefaultGatewayMAC)
	src := []byte{2, 0, 0, 0, 0, 2}
	eth := func(t uint16, payload ...byte) []byte {
		b := append(append(append([]byte{}, gw...), src...), byte(t>>8), byte(t))
		return append(b, payload...)
	}
	f.Add(eth(0x0800, 0x45, 0, 0, 20, 0, 0, 0, 0, 64, 6, 0, 0, 192, 168, 127, 2, 1, 1, 1, 1))
	f.Add(eth(0x0806, 0, 1, 8, 0, 6, 4, 0, 1, 2, 0, 0, 0, 0, 2, 192, 168, 127, 2, 0, 0, 0, 0, 0, 0, 192, 168, 127, 1))
	f.Add(eth(0x0800, 0x46, 0, 0, 28, 0, 1, 0x20, 0, 64, 17))
	f.Add(make([]byte, 14))
	n, err := New(Config{
		Resolver:  &fakeResolver{},
		HostAddrs: func() ([]netip.Addr, error) { return nil, nil },
	}, nil)
	if err != nil {
		f.Fatal(err)
	}
	defer n.Close()
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > frame.MaxFrameLen(n.cfg.MTU) {
			return
		}
		n.deliver(b, frame.MaxFrameLen(n.cfg.MTU))
	})
}

// FuzzDeliverVnet feeds arbitrary offload-mode frames (virtio_net_hdr plus Ethernet
// frame, as netfwd -vnet sends them) into a stack whose link is in offload mode,
// including GSO segments larger than the MTU. It must not panic, and the stack must
// keep working.
func FuzzDeliverVnet(f *testing.F) {
	gw := []byte(DefaultGatewayMAC)
	src := []byte{2, 0, 0, 0, 0, 2}
	vnet := func(h frame.VnetHdr, t uint16, payload ...byte) []byte {
		b := make([]byte, frame.VnetHdrLen)
		h.Put(b)
		b = append(append(append(b, gw...), src...), byte(t>>8), byte(t))
		return append(b, payload...)
	}
	ip := []byte{0x45, 0, 0xFF, 0xFF, 0, 0, 0x40, 0, 64, 6, 0, 0, 192, 168, 127, 2, 1, 1, 1, 1}
	f.Add(vnet(frame.VnetHdr{}, 0x0800, ip...))
	f.Add(vnet(frame.VnetHdr{Flags: 1, GSOType: 1, GSOSize: 1448, HdrLen: 54, CsumStart: 34, CsumOffset: 16}, 0x0800, append(ip, make([]byte, 40000)...)...))
	f.Add(vnet(frame.VnetHdr{GSOType: 0x81, GSOSize: 1}, 0x0806, 0, 1, 8, 0, 6, 4, 0, 1))
	f.Add(make([]byte, frame.MinVnetFrameLen))
	n, err := New(Config{
		Resolver:  &fakeResolver{},
		HostAddrs: func() ([]netip.Addr, error) { return nil, nil },
	}, nil)
	if err != nil {
		f.Fatal(err)
	}
	defer n.Close()
	n.mode.Store(linkVnet)
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < frame.MinVnetFrameLen || len(b) > frame.MaxVnetFrameLen {
			return
		}
		n.deliverVnet(b)
	})
}
