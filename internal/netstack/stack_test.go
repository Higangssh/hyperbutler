package netstack

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"

	"github.com/Higangssh/winbox/internal/netstack/frame"
)

// testHostAddr stands in for a LAN address of this host.
var testHostAddr = netip.MustParseAddr("10.99.0.5")

func newTestStack(t *testing.T, mod func(*Config)) *Stack {
	t.Helper()
	cfg := Config{
		Resolver:  &fakeResolver{a: map[string][]netip.Addr{"example.com": {netip.MustParseAddr("93.184.215.14")}}},
		HostAddrs: func() ([]netip.Addr, error) { return []netip.Addr{testHostAddr}, nil },
	}
	if mod != nil {
		mod(&cfg)
	}
	n, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	return n
}

func guestDial(t *testing.T, g *fakeGuest, ap netip.AddrPort) (net.Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return gonet.DialContextTCP(ctx, g.s, tcpip.FullAddress{NIC: 1, Addr: tcpipAddr(ap.Addr()), Port: ap.Port()}, ipv4.ProtocolNumber)
}

// echoServer listens on loopback and echoes one connection's bytes.
func echoServer(t *testing.T) netip.AddrPort {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return netip.MustParseAddrPort(ln.Addr().String())
}

func TestHostForwardRelaysTCP(t *testing.T) {
	echo := echoServer(t)
	virt := netip.MustParseAddrPort("192.168.127.254:7")
	n := newTestStack(t, func(c *Config) { c.HostForwards = []HostForward{{Virtual: virt, Host: echo}} })
	g := newFakeGuest(t, n)
	c, err := guestDial(t, g, virt)
	if err != nil {
		t.Fatalf("dial host forward: %v", err)
	}
	defer c.Close()
	msg := bytes.Repeat([]byte("winbox"), 50000) // 300 kB, many segments
	go func() { c.Write(msg); c.(*gonet.TCPConn).CloseWrite() }()
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("echo returned %d bytes, want %d", len(got), len(msg))
	}
}

func TestGuestCannotReachHost(t *testing.T) {
	echo := echoServer(t)
	n := newTestStack(t, nil)
	g := newFakeGuest(t, n)
	for _, dst := range []netip.AddrPort{
		netip.AddrPortFrom(testHostAddr, echo.Port()),                  // a host LAN address
		netip.AddrPortFrom(netip.MustParseAddr("169.254.169.254"), 80), // metadata
		netip.AddrPortFrom(netip.MustParseAddr("192.168.127.254"), 80), // unforwarded subnet address
	} {
		start := time.Now()
		c, err := guestDial(t, g, dst)
		if err == nil {
			c.Close()
			t.Fatalf("guest reached %s", dst)
		}
		if time.Since(start) > 3*time.Second {
			t.Errorf("%s: refusal took %v; want a fast RST", dst, time.Since(start))
		}
	}
	if n.Stats().TCPDenied < 3 {
		t.Fatalf("TCPDenied = %d", n.Stats().TCPDenied)
	}
}

// A guest that spoofs a packet to 127.0.0.1 must not reach the host's loopback. A guest
// kernel would not route it, so the fake guest is given an on-link route.
func TestSpoofedLoopbackDenied(t *testing.T) {
	echo := echoServer(t)
	n := newTestStack(t, nil)
	g := newFakeGuest(t, n)
	lo, _ := tcpip.NewSubnet(tcpip.AddrFrom4([4]byte{127, 0, 0, 0}), tcpip.MaskFromBytes([]byte{255, 0, 0, 0}))
	g.s.SetRouteTable(append([]tcpip.Route{{Destination: lo, Gateway: tcpipAddr(n.Config().Gateway()), NIC: 1}}, g.s.GetRouteTable()...))
	c, err := guestDial(t, g, echo)
	if err == nil {
		c.Close()
		t.Fatal("guest reached host loopback")
	}
}

func TestSelfConnected(t *testing.T) {
	echo := echoServer(t)
	c, err := net.Dial("tcp", echo.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !selfConnected(c) {
		t.Fatal("a connection from this host to itself was not detected")
	}
}

// Review finding 2: the policy's host address list is cached, so a host address that
// appeared after the last read (a VPN connecting) must still be refused, by the
// post-dial self-connection check.
func TestStaleHostAddrsStillDenied(t *testing.T) {
	var own netip.Addr
	addrs, _ := interfaceAddrs()
	for _, a := range addrs {
		if a.Is4() && !a.IsLoopback() && !a.IsLinkLocalUnicast() {
			own = a
			break
		}
	}
	if !own.IsValid() {
		t.Skip("no non-loopback IPv4 address on this host")
	}
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("host service"))
			c.Close()
		}
	}()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	n := newTestStack(t, func(c *Config) {
		c.HostAddrs = func() ([]netip.Addr, error) { return nil, nil } // stale: knows nothing
	})
	g := newFakeGuest(t, n)
	c, err := guestDial(t, g, netip.AddrPortFrom(own, port))
	if err == nil {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		b, _ := io.ReadAll(c)
		c.Close()
		if len(b) > 0 {
			t.Fatalf("guest reached a host service on %s: %q", own, b)
		}
	}
	if n.Stats().TCPDenied == 0 {
		t.Fatal("self-connection was not counted as denied")
	}
}

func TestDNSOverStack(t *testing.T) {
	n := newTestStack(t, nil)
	g := newFakeGuest(t, n)
	gw := n.Config().Gateway()
	uc, err := gonet.DialUDP(g.s, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpipAddr(gw), Port: 53}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	b, _ := q.Pack()
	if _, err := uc.Write(b); err != nil {
		t.Fatal(err)
	}
	uc.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1500)
	k, err := uc.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	var r dns.Msg
	if err := r.Unpack(buf[:k]); err != nil {
		t.Fatal(err)
	}
	if r.Id != q.Id || len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != "93.184.215.14" {
		t.Fatalf("bad answer: %v", &r)
	}
}

func TestPublishReachesGuest(t *testing.T) {
	n := newTestStack(t, nil)
	g := newFakeGuest(t, n)
	gl, err := gonet.ListenTCP(g.s, tcpip.FullAddress{NIC: 1, Port: 8080}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer gl.Close()
	go func() {
		c, err := gl.Accept()
		if err != nil {
			return
		}
		c.Write([]byte("hello from guest"))
		c.Close()
	}()
	cl, err := n.Publish(Publish{Host: netip.MustParseAddrPort("127.0.0.1:0"), GuestPort: 8080})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	addr := cl.(net.Listener).Addr().String()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "hello from guest" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestBadFramesAreDropped(t *testing.T) {
	n := newTestStack(t, nil)
	host, guest := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- n.ServeConn(host) }()
	gwMAC := []byte(n.Config().GatewayMAC)
	frames := [][]byte{
		make([]byte, 14), // zero MACs, EtherType 0
		append(append(append([]byte{}, gwMAC...), gwMAC...), 0x08, 0x00),                        // source = gateway
		append(append(append([]byte{}, gwMAC...), 2, 0, 0, 0, 0, 9), 0x86, 0xdd, 0x60),          // IPv6
		append(append(append([]byte{}, gwMAC...), 2, 0, 0, 0, 0, 9), 0x08, 0x00, 0x45, 0, 0, 1), // truncated IPv4
		append(append(append([]byte{}, gwMAC...), 2, 0, 0, 0, 0, 9), 0x08, 0x06),                // empty ARP
	}
	for _, f := range frames {
		if _, err := guest.Write(frame.Append(nil, f)); err != nil {
			t.Fatal(err)
		}
	}
	// An oversized length ends the link.
	guest.Write([]byte{0, 1, 0, 0})
	select {
	case err := <-done:
		if !errors.Is(err, frame.ErrFrameSize) {
			t.Fatalf("ServeConn: %v, want ErrFrameSize", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeConn did not return")
	}
	if s := n.Stats(); s.FramesDropped < 3 {
		t.Fatalf("FramesDropped = %d", s.FramesDropped)
	}
}

func TestNewReplacesLink(t *testing.T) {
	n := newTestStack(t, nil)
	a1, _ := net.Pipe()
	b1, _ := net.Pipe()
	e1 := make(chan error, 1)
	go func() { e1 <- n.ServeConn(a1) }()
	time.Sleep(50 * time.Millisecond)
	go n.ServeConn(b1)
	select {
	case <-e1:
	case <-time.After(5 * time.Second):
		t.Fatal("old link was not closed")
	}
}

func TestConfigValidate(t *testing.T) {
	bad := []Config{
		{Subnet: netip.MustParsePrefix("8.8.8.0/24")},
		{Subnet: netip.MustParsePrefix("10.0.0.0/30")},
		{Subnet: netip.MustParsePrefix("fd00::/64")},
		{MTU: 100},
		{GatewayMAC: net.HardwareAddr{1, 0, 0, 0, 0, 1}},
		{HostForwards: []HostForward{{Virtual: netip.MustParseAddrPort("192.168.127.1:80"), Host: netip.MustParseAddrPort("127.0.0.1:80")}}},
		{HostForwards: []HostForward{{Virtual: netip.MustParseAddrPort("10.1.1.1:80"), Host: netip.MustParseAddrPort("127.0.0.1:80")}}},
		{HostForwards: []HostForward{{Virtual: netip.MustParseAddrPort("192.168.127.255:80"), Host: netip.MustParseAddrPort("127.0.0.1:80")}}},
		{MaxTCPConns: -1},
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("config %d accepted", i)
		}
	}
	if err := (Config{}).Validate(); err != nil {
		t.Fatalf("zero config: %v", err)
	}
	c := Config{}.withDefaults()
	if c.Gateway().String() != "192.168.127.1" || c.GuestIP().String() != "192.168.127.2" {
		t.Fatalf("gateway %s guest %s", c.Gateway(), c.GuestIP())
	}
}

func TestParsePublish(t *testing.T) {
	good := map[string]string{
		"8080:80":               "127.0.0.1:8080->guest:80",
		"127.0.0.1:8080:80/tcp": "127.0.0.1:8080->guest:80",
		"[::1]:8080:80":         "[::1]:8080->guest:80",
		"127.5.5.5:1:65535":     "127.5.5.5:1->guest:65535",
	}
	for in, want := range good {
		p, err := ParsePublish(in, false)
		if err != nil || p.String() != want {
			t.Errorf("%q: %v, %v; want %s", in, p, err, want)
		}
	}
	for _, in := range []string{"", "80", "0:80", "8080:0", "8080:80/udp", "0.0.0.0:8080:80",
		"192.168.1.5:80:80", "::1:80:80", "x:80", "8080:+80", "8080:0080000", "[fe80::1%eth0]:80:80",
		"224.0.0.1:80:80", strings.Repeat("1", 200)} {
		if p, err := ParsePublish(in, false); err == nil {
			t.Errorf("%q accepted as %v", in, p)
		}
	}
	if _, err := ParsePublish("0.0.0.0:8080:80", true); err != nil {
		t.Errorf("LAN publish refused with allowLAN: %v", err)
	}
}

func TestParseHostForward(t *testing.T) {
	h, err := ParseHostForward("192.168.127.254:5201=127.0.0.1:5201")
	if err != nil || h.String() != "192.168.127.254:5201=127.0.0.1:5201" {
		t.Fatalf("%v %v", h, err)
	}
	for _, in := range []string{"", "a=b", "192.168.127.254:0=127.0.0.1:1", "[::1]:1=127.0.0.1:1",
		"192.168.127.254:1", "192.168.127.254:1=127.0.0.1"} {
		if _, err := ParseHostForward(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}
