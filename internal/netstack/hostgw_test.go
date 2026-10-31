package netstack

import (
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

// REQ-NET-004 tests: the host gateway reaches this host's loopback on the port the
// guest asked for, and nothing else.

var hgAddr = netip.MustParseAddr("192.168.127.254")

func newHostGatewayStack(t *testing.T, mod func(*Config)) *Stack {
	t.Helper()
	return newTestStack(t, func(c *Config) {
		c.HostGateway = &HostGateway{Enabled: true}
		if mod != nil {
			mod(c)
		}
	})
}

// serveOnce listens on network/addr (loopback only) and answers every connection
// with msg.
func serveMsg(t *testing.T, network, addr, msg string) netip.AddrPort {
	t.Helper()
	ln, err := net.Listen(network, addr)
	if err != nil {
		t.Skipf("listen %s %s: %v", network, addr, err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte(msg))
			c.Close()
		}
	}()
	return netip.MustParseAddrPort(ln.Addr().String())
}

func readFrom(t *testing.T, g *fakeGuest, ap netip.AddrPort) (string, error) {
	t.Helper()
	c, err := guestDial(t, g, ap)
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	b, err := io.ReadAll(c)
	return string(b), err
}

func TestHostGatewayReachesLoopback(t *testing.T) {
	srv := serveMsg(t, "tcp4", "127.0.0.1:0", "hello from 127.0.0.1")
	n := newHostGatewayStack(t, nil)
	if a, h := n.HostGateway(); a != hgAddr || !h.Enabled {
		t.Fatalf("HostGateway() = %s %+v", a, h)
	}
	g := newFakeGuest(t, n)
	got, err := readFrom(t, g, netip.AddrPortFrom(hgAddr, srv.Port()))
	if err != nil || got != "hello from 127.0.0.1" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// A service that listens on [::1] only (a "localhost" dev server) is reached when
// 127.0.0.1 refuses.
func TestHostGatewayFallsBackToIPv6Loopback(t *testing.T) {
	srv := serveMsg(t, "tcp6", "[::1]:0", "hello from ::1")
	if c, err := net.DialTimeout("tcp4", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), srv.Port()).String(), time.Second); err == nil {
		c.Close()
		t.Skip("127.0.0.1 has a listener on the same port")
	}
	n := newHostGatewayStack(t, nil)
	g := newFakeGuest(t, n)
	got, err := readFrom(t, g, netip.AddrPortFrom(hgAddr, srv.Port()))
	if err != nil || got != "hello from ::1" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// Prefer 127.0.0.1: with both listening, [::1] is never connected to.
func TestHostGatewayPrefersIPv4(t *testing.T) {
	v4, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer v4.Close()
	port := v4.Addr().(*net.TCPAddr).Port
	v6, err := net.Listen("tcp6", netip.AddrPortFrom(netip.IPv6Loopback(), uint16(port)).String())
	if err != nil {
		t.Skipf("[::1]:%d: %v", port, err)
	}
	defer v6.Close()
	v6hits := make(chan struct{}, 1)
	go func() {
		if c, err := v6.Accept(); err == nil {
			v6hits <- struct{}{}
			c.Close()
		}
	}()
	go func() {
		for {
			c, err := v4.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("v4"))
			c.Close()
		}
	}()
	n := newHostGatewayStack(t, nil)
	g := newFakeGuest(t, n)
	got, err := readFrom(t, g, netip.AddrPortFrom(hgAddr, uint16(port)))
	if err != nil || got != "v4" {
		t.Fatalf("got %q, %v", got, err)
	}
	select {
	case <-v6hits:
		t.Fatal("[::1] got a connection too")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestHostGatewaySettingIsLive(t *testing.T) {
	a := serveMsg(t, "tcp4", "127.0.0.1:0", "a")
	b := serveMsg(t, "tcp4", "127.0.0.1:0", "b")
	n := newHostGatewayStack(t, nil)
	g := newFakeGuest(t, n)
	try := func(p netip.AddrPort) error {
		_, err := readFrom(t, g, netip.AddrPortFrom(hgAddr, p.Port()))
		return err
	}
	ports, err := ParsePortList(strconv.Itoa(int(a.Port())))
	if err != nil {
		t.Fatal(err)
	}
	if err := n.SetHostGateway(HostGateway{Enabled: true, Ports: ports}); err != nil {
		t.Fatal(err)
	}
	if err := try(a); err != nil {
		t.Fatalf("listed port refused: %v", err)
	}
	denied := n.Stats().TCPDenied
	if err := try(b); err == nil {
		t.Fatal("unlisted port reached")
	}
	if n.Stats().TCPDenied != denied+1 {
		t.Fatal("unlisted port not counted as denied")
	}
	if err := n.SetHostGateway(HostGateway{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if err := try(a); err == nil {
		t.Fatal("reached with the host gateway off")
	}
	if err := n.SetHostGateway(HostGateway{Enabled: true, Ports: PortList{{5, 4}}}); err == nil {
		t.Fatal("bad port list accepted")
	}
	if err := n.SetHostGateway(HostGateway{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := try(b); err != nil {
		t.Fatalf("all ports: %v", err)
	}
}

func TestSetHostGatewayWithoutOne(t *testing.T) {
	n := newTestStack(t, nil)
	if err := n.SetHostGateway(HostGateway{Enabled: true}); err == nil {
		t.Fatal("accepted on a stack without a host gateway")
	}
	if a, _ := n.HostGateway(); a.IsValid() {
		t.Fatal("address without a host gateway")
	}
}

// The forward is no proxy: whatever the guest sends, the host side connects to
// loopback only, and other subnet and host addresses stay refused.
func TestHostGatewayIsNotAProxy(t *testing.T) {
	srv := serveMsg(t, "tcp4", "127.0.0.1:0", "x")
	n := newHostGatewayStack(t, nil)
	g := newFakeGuest(t, n)
	for _, dst := range []netip.AddrPort{
		netip.AddrPortFrom(testHostAddr, srv.Port()),
		netip.AddrPortFrom(netip.MustParseAddr("192.168.127.253"), srv.Port()),
		netip.AddrPortFrom(netip.MustParseAddr("192.168.127.1"), srv.Port()),
		netip.AddrPortFrom(netip.MustParseAddr("169.254.169.254"), 80),
	} {
		if _, err := readFrom(t, g, dst); err == nil {
			t.Errorf("guest reached %s", dst)
		}
	}
	if err := loopbackOnly("tcp4", "10.99.0.5:80", nil); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("loopbackOnly allowed a LAN address: %v", err)
	}
	for _, ok := range []string{"127.0.0.1:80", "[::1]:80"} {
		if err := loopbackOnly("tcp", ok, nil); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"127.0.0.2:80", "[::ffff:10.0.0.1]:80", "0.0.0.0:80", "[fe80::1]:80"} {
		if err := loopbackOnly("tcp", bad, nil); err == nil {
			t.Errorf("%s allowed", bad)
		}
	}
}

// A guest that opens many host gateway connections is bounded by
// MaxHostGatewayConns; connections end, and the slots come back.
func TestHostGatewayConnLimit(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 64)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				close(accepted)
				return
			}
			accepted <- c
		}
	}()
	defer func() {
		ln.Close()
		for c := range accepted {
			c.Close()
		}
	}()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	n := newHostGatewayStack(t, func(c *Config) { c.MaxHostGatewayConns = 2 })
	g := newFakeGuest(t, n)
	dst := netip.AddrPortFrom(hgAddr, port)
	var open []net.Conn
	for i := 0; i < 2; i++ {
		c, err := guestDial(t, g, dst)
		if err != nil {
			t.Fatalf("conn %d: %v", i, err)
		}
		open = append(open, c)
		(<-accepted).Close() // the relay is up; the guest side stays open
	}
	if c, err := guestDial(t, g, dst); err == nil {
		c.Close()
		t.Fatal("a third connection got through a limit of 2")
	}
	for _, c := range open {
		c.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := guestDial(t, g, dst)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the slots did not come back")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestHostGatewayUDP(t *testing.T) {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 1500)
		for {
			k, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(append([]byte("echo "), buf[:k]...), from)
		}
	}()
	port := uint16(pc.LocalAddr().(*net.UDPAddr).Port)
	n := newHostGatewayStack(t, nil)
	g := newFakeGuest(t, n)
	ask := func() (string, error) {
		uc, err := gonet.DialUDP(g.s, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpipAddr(hgAddr), Port: port}, ipv4.ProtocolNumber)
		if err != nil {
			return "", err
		}
		defer uc.Close()
		if _, err := uc.Write([]byte("ping")); err != nil {
			return "", err
		}
		uc.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 64)
		k, err := uc.Read(buf)
		return string(buf[:k]), err
	}
	if got, err := ask(); err != nil || got != "echo ping" {
		t.Fatalf("got %q, %v", got, err)
	}
	if err := n.SetHostGateway(HostGateway{Enabled: true, Ports: PortList{{1, 1}}}); err != nil {
		t.Fatal(err)
	}
	if got, err := ask(); err == nil {
		t.Fatalf("unlisted UDP port answered %q", got)
	}
}

func TestHostGatewayDNS(t *testing.T) {
	n := newHostGatewayStack(t, func(c *Config) {
		c.Resolver = &fakeResolver{a: map[string][]netip.Addr{
			"host.docker.internal": {netip.MustParseAddr("10.99.0.5")}, // as Docker Desktop's hosts file says
			"rebind.example":       {netip.MustParseAddr("192.168.127.254"), netip.MustParseAddr("192.168.127.7")},
			"mixed.example":        {netip.MustParseAddr("192.168.127.254"), netip.MustParseAddr("93.184.215.14")},
		}}
	})
	h := n.dns
	for _, name := range []string{"host.docker.internal.", "HOST.Docker.Internal.", "gateway.docker.internal."} {
		r := query(t, h, name, dns.TypeA, true, 0)
		if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != "192.168.127.254" {
			t.Fatalf("%s: %v", name, r)
		}
		if r := query(t, h, name, dns.TypeAAAA, true, 0); r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
			t.Fatalf("%s AAAA: %v", name, r)
		}
	}
	if r := query(t, h, "254.127.168.192.in-addr.arpa.", dns.TypePTR, true, 0); len(r.Answer) != 1 || r.Answer[0].(*dns.PTR).Ptr != "host.docker.internal." {
		t.Fatalf("PTR: %v", r)
	}
	if r := query(t, h, "7.127.168.192.in-addr.arpa.", dns.TypePTR, true, 0); r.Rcode != dns.RcodeNameError {
		t.Fatalf("PTR in the subnet: %v", r)
	}
	// Rebinding: an outside name that points into the guest subnet loses those records.
	if r := query(t, h, "rebind.example.", dns.TypeA, true, 0); r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
		t.Fatalf("rebind: %v", r)
	}
	if r := query(t, h, "mixed.example.", dns.TypeA, true, 0); len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != "93.184.215.14" {
		t.Fatalf("mixed: %v", r)
	}
	if err := n.SetHostGateway(HostGateway{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	for _, qt := range []uint16{dns.TypeA, dns.TypeAAAA} {
		if r := query(t, h, "host.docker.internal.", qt, true, 0); r.Rcode != dns.RcodeNameError {
			t.Fatalf("off, type %d: %v", qt, r)
		}
	}
	if r := query(t, h, "254.127.168.192.in-addr.arpa.", dns.TypePTR, true, 0); r.Rcode != dns.RcodeNameError {
		t.Fatalf("off, PTR: %v", r)
	}
}

// Without a host gateway the names are ordinary names for the host resolver.
func TestNoHostGatewayDNS(t *testing.T) {
	n := newTestStack(t, func(c *Config) {
		c.Resolver = &fakeResolver{a: map[string][]netip.Addr{"host.docker.internal": {netip.MustParseAddr("10.99.0.5")}}}
	})
	r := query(t, n.dns, "host.docker.internal.", dns.TypeA, true, 0)
	if len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != "10.99.0.5" {
		t.Fatalf("got %v", r)
	}
}

// Nothing listens on either loopback: the guest gets a prompt reset, and a denied port
// is a prompt reset too, never a dial timeout.
func TestHostGatewayRefusalsArePrompt(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	ln.Close()
	n := newHostGatewayStack(t, nil)
	g := newFakeGuest(t, n)
	refused := n.Stats().TCPRefused
	start := time.Now()
	if _, err := readFrom(t, g, netip.AddrPortFrom(hgAddr, port)); err == nil {
		t.Fatal("connected to a closed port")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("refusal took %v", d)
	}
	if n.Stats().TCPRefused != refused+1 {
		t.Fatal("not counted as refused")
	}
	if err := n.SetHostGateway(HostGateway{Enabled: true, Ports: PortList{{1, 1}}}); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	if _, err := readFrom(t, g, netip.AddrPortFrom(hgAddr, port)); err == nil {
		t.Fatal("connected to an unlisted port")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("denial took %v", d)
	}
}

// Host gateway UDP flows have their own bound inside the flow table.
func TestHostGatewayUDPFlowLimit(t *testing.T) {
	n := newHostGatewayStack(t, func(c *Config) { c.MaxUDPFlows = 8 })
	if cap(n.hgUDPSem) != 8 {
		t.Fatalf("gateway UDP bound %d with 8 flows", cap(n.hgUDPSem))
	}
	n2 := newHostGatewayStack(t, nil)
	if cap(n2.hgUDPSem) != maxHostGatewayUDPFlows || cap(n2.hgUDPSem) >= cap(n2.udpSem) {
		t.Fatalf("gateway UDP bound %d of %d", cap(n2.hgUDPSem), cap(n2.udpSem))
	}
}
