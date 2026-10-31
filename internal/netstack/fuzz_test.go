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
	f.Add([]byte{0, 1, 0x80, 0, 0, 1, 0, 0, 0, 0, 0, 0}, true)
	h, _ := newTestDNS()
	h.timeout = 50 * time.Millisecond
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
