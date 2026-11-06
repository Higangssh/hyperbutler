package egress

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"

	"github.com/Higangssh/hyperbutler/internal/netstack"
	"github.com/Higangssh/hyperbutler/internal/netstack/frame"
)

// The policy on the real stack: a second gVisor stack plays the guest kernel over a framed
// stream, as package netstack's own tests do, and a mock resolver counts every lookup.
// Nothing leaves this machine: what gets through goes to a host forward on loopback.

type mockResolver struct {
	calls atomic.Int64
	addrs map[string][]netip.Addr
}

func (r *mockResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.calls.Add(1)
	if a, ok := r.addrs[host]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}
func (r *mockResolver) LookupCNAME(context.Context, string) (string, error) {
	r.calls.Add(1)
	return "", &net.DNSError{IsNotFound: true}
}
func (r *mockResolver) LookupMX(context.Context, string) ([]*net.MX, error) {
	r.calls.Add(1)
	return nil, nil
}
func (r *mockResolver) LookupNS(context.Context, string) ([]*net.NS, error) {
	r.calls.Add(1)
	return nil, nil
}
func (r *mockResolver) LookupSRV(context.Context, string, string, string) (string, []*net.SRV, error) {
	r.calls.Add(1)
	return "", nil, nil
}
func (r *mockResolver) LookupTXT(context.Context, string) ([]string, error) {
	r.calls.Add(1)
	return nil, nil
}
func (r *mockResolver) LookupAddr(context.Context, string) ([]string, error) {
	r.calls.Add(1)
	return nil, nil
}

var guestMAC = tcpip.LinkAddress("\x02\x00\x00\x00\x00\x02")

type guest struct {
	s  *stack.Stack
	gw netip.Addr
}

// newGuest connects a guest network stack to n.
func newGuest(t *testing.T, n *netstack.Stack) *guest {
	t.Helper()
	cfg := n.Config()
	hostEnd, guestEnd := net.Pipe()
	ch := channel.New(512, uint32(cfg.MTU+header.EthernetMinimumSize), guestMAC)
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	if err := s.CreateNIC(1, ethernet.New(ch)); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4(cfg.GuestIP().As4()), PrefixLen: cfg.Subnet.Bits()},
	}, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, Gateway: tcpip.AddrFrom4(cfg.Gateway().As4()), NIC: 1}})

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); _ = n.ServeConn(hostEnd) }()
	go func() { // guest to host
		defer wg.Done()
		for {
			pkt := ch.ReadContext(ctx)
			if pkt == nil {
				return
			}
			var b []byte
			for _, v := range pkt.AsSlices() {
				b = append(b, v...)
			}
			pkt.DecRef()
			if _, err := guestEnd.Write(frame.Append(nil, b)); err != nil {
				return
			}
		}
	}()
	go func() { // host to guest
		defer wg.Done()
		fr, err := frame.NewReader(bufio.NewReader(guestEnd), cfg.MTU)
		if err != nil {
			t.Error(err)
			return
		}
		for {
			b, err := fr.Next()
			if err != nil {
				return
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(b)})
			ch.InjectInbound(header.Ethernet(b).Type(), pkt)
			pkt.DecRef()
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = guestEnd.Close()
		ch.Close()
		s.Close()
		wg.Wait()
	})
	return &guest{s: s, gw: cfg.Gateway()}
}

func addrOf(a netip.AddrPort) tcpip.FullAddress {
	return tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(a.Addr().As4()), Port: a.Port()}
}

func (g *guest) dial(t *testing.T, dst netip.AddrPort) (*gonet.TCPConn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return gonet.DialContextTCP(ctx, g.s, addrOf(dst), ipv4.ProtocolNumber)
}

func (g *guest) queryUDP(t *testing.T, name string, qtype uint16) *dns.Msg {
	t.Helper()
	gw := addrOf(netip.AddrPortFrom(g.gw, 53))
	c, err := gonet.DialUDP(g.s, nil, &gw, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	q := new(dns.Msg)
	q.SetQuestion(name, qtype)
	b, _ := q.Pack()
	if _, err := c.Write(b); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4096)
	k, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	r := new(dns.Msg)
	if err := r.Unpack(buf[:k]); err != nil {
		t.Fatal(err)
	}
	return r
}

// queryTCP is DNS over TCP to the stack's own server.
func (g *guest) queryTCP(t *testing.T, name string, qtype uint16) *dns.Msg {
	t.Helper()
	c, err := g.dial(t, netip.AddrPortFrom(g.gw, 53))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	q := new(dns.Msg)
	q.SetQuestion(name, qtype)
	b, _ := q.Pack()
	if _, err := c.Write(append([]byte{byte(len(b) >> 8), byte(len(b))}, b...)); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var hdr [2]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, int(hdr[0])<<8|int(hdr[1]))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	r := new(dns.Msg)
	if err := r.Unpack(buf); err != nil {
		t.Fatal(err)
	}
	return r
}

type sink struct {
	ln       net.Listener
	accepted atomic.Int64
	mu       sync.Mutex
	got      bytes.Buffer
	wg       sync.WaitGroup
}

// newSink keeps what it is sent, on loopback, and answers nothing.
func newSink(t *testing.T) (*sink, netip.AddrPort) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &sink{ln: ln}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.accepted.Add(1)
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer c.Close()
				b := make([]byte, 4096)
				for {
					k, err := c.Read(b)
					s.mu.Lock()
					s.got.Write(b[:k])
					s.mu.Unlock()
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { ln.Close(); s.wg.Wait() })
	return s, netip.MustParseAddrPort(ln.Addr().String())
}

func (s *sink) received() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.got.Bytes()...)
}

const stackName = "registry.npmjs.org"

// sessionNet is a dev or locked session's network: Stack, Policy, mock resolver, and a host
// forward on port 443 and on port 80 of an address the test pins by hand, because the
// stack never answers with an address in the guest subnet.
type sessionNet struct {
	n        *netstack.Stack
	g        *guest
	p        *Policy
	res      *mockResolver
	sink     *sink
	tls, web netip.AddrPort
}

func newSessionNet(t *testing.T, mode Mode, specs ...string) *sessionNet {
	t.Helper()
	h, p, err := Hook(mode, entries(t, specs...), Options{})
	if err != nil {
		t.Fatal(err)
	}
	res := &mockResolver{addrs: map[string][]netip.Addr{
		stackName:        {addrNPM},
		"rebind.example": {netip.MustParseAddr("192.168.1.1")},
	}}
	sk, target := newSink(t)
	tlsVirt := netip.MustParseAddrPort("192.168.127.253:443")
	webVirt := netip.MustParseAddrPort("192.168.127.253:80")
	n, err := netstack.New(netstack.Config{
		Resolver:     res,
		HostAddrs:    func() ([]netip.Addr, error) { return nil, nil },
		HostForwards: []netstack.HostForward{{Virtual: tlsVirt, Host: target}, {Virtual: webVirt, Host: target}},
		Egress:       h,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	return &sessionNet{n: n, g: newGuest(t, n), p: p, res: res, sink: sk, tls: tlsVirt, web: webVirt}
}

// pinForward pins stackName to the forwards' address. It is a private one, so the entry
// has to say lan: for the policy to keep it.
func (s *sessionNet) pinForward(t *testing.T) {
	t.Helper()
	if err := s.p.Allow(Entry{Name: stackName, LAN: true}); err != nil {
		t.Fatal(err)
	}
	if got := s.p.Answer(stackName, []netip.Addr{s.tls.Addr()}); len(got) != 1 {
		t.Fatalf("not pinned: %v", got)
	}
}

func (s *sessionNet) expectRefused(t *testing.T, c net.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, err := c.Read(make([]byte, 1))
	var ne net.Error
	if err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("the connection was not refused: %v", err)
	}
	if s.sink.accepted.Load() != 0 {
		t.Fatal("something was dialed")
	}
}

func TestStackBlockedNameNeverReachesTheResolver(t *testing.T) {
	s := newSessionNet(t, Dev, stackName)
	for _, ask := range []func(*testing.T, string, uint16) *dns.Msg{s.g.queryUDP, s.g.queryTCP} {
		if r := ask(t, "tunnel-data.evil.example.", dns.TypeA); r.Rcode != dns.RcodeNameError {
			t.Errorf("A: %v", r)
		}
		if r := ask(t, "tunnel-data.evil.example.", dns.TypeAAAA); r.Rcode != dns.RcodeNameError {
			t.Errorf("AAAA: %v", r)
		}
		if r := ask(t, stackName+".", dns.TypeTXT); r.Rcode != dns.RcodeRefused {
			t.Errorf("TXT of an allowed name: %v", r)
		}
		if r := ask(t, stackName+".", dns.TypeHTTPS); r.Rcode != dns.RcodeRefused {
			t.Errorf("HTTPS of an allowed name: %v", r)
		}
		if r := ask(t, "14.215.184.93.in-addr.arpa.", dns.TypePTR); r.Rcode != dns.RcodeRefused {
			t.Errorf("PTR: %v", r)
		}
	}
	if c := s.res.calls.Load(); c != 0 {
		t.Fatalf("the resolver was called %d times", c)
	}
	r := s.g.queryUDP(t, "ReGiStRy.NpMjS.oRg.", dns.TypeA)
	if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != addrNPM.String() {
		t.Fatalf("allowed name: %v", r)
	}
	if c := s.res.calls.Load(); c != 1 {
		t.Fatalf("the resolver was called %d times", c)
	}
}

func TestStackPrivateAnswerDropped(t *testing.T) {
	s := newSessionNet(t, Dev, "rebind.example")
	r := s.g.queryUDP(t, "rebind.example.", dns.TypeA)
	if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
		t.Fatalf("%v", r)
	}
	if s.p.Conn(netip.MustParseAddrPort("192.168.1.1:443")) != netstack.ConnDeny {
		t.Fatal("pinned")
	}
}

func TestStackLiteralAddressRefused(t *testing.T) {
	s := newSessionNet(t, Locked, stackName)
	if c, err := s.g.dial(t, s.tls); err == nil {
		c.Close()
		t.Fatal("an unpinned address connected")
	}
	if s.sink.accepted.Load() != 0 || s.n.Stats().TCPDenied != 1 {
		t.Fatalf("accepted %d, %+v", s.sink.accepted.Load(), s.n.Stats())
	}
	if blockedKinds(s.p)[KindAddress] != s.tls.String() {
		t.Fatalf("%+v", s.p.Blocked())
	}
}

func TestStackRealClientHelloGoesThroughUnchanged(t *testing.T) {
	s := newSessionNet(t, Dev, stackName)
	s.pinForward(t)
	want := realHello(t, &tls.Config{ServerName: stackName})
	c, err := s.g.dial(t, s.tls)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// One byte at a time, as a hostile guest may; nothing goes out before the last one.
	for i, b := range want {
		if _, err := c.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
		if i%64 == 0 && s.sink.accepted.Load() != 0 {
			t.Fatalf("dialed after %d of %d bytes", i, len(want))
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(s.sink.received()) < len(want) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !bytes.Equal(s.sink.received(), want) {
		t.Fatalf("got %d bytes that differ from the hello's %d", len(s.sink.received()), len(want))
	}
}

func TestStackWrongServerNameRefused(t *testing.T) {
	s := newSessionNet(t, Dev, stackName)
	s.pinForward(t)
	for _, tc := range []struct {
		name string
		b    []byte
	}{
		{"shared CDN", realHello(t, &tls.Config{ServerName: "evil.example"})},
		{"no server name", realHello(t, &tls.Config{InsecureSkipVerify: true})},
		{"ECH", hello(sniExt(stackName), ext(tlsExtECH, []byte{1}))},
		{"not TLS", []byte("hello?\r\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := s.g.dial(t, s.tls)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if _, err := c.Write(tc.b); err != nil {
				t.Fatal(err)
			}
			s.expectRefused(t, c)
		})
	}
	if s.n.Stats().TCPDenied != 4 {
		t.Fatalf("%+v", s.n.Stats())
	}
}

func TestStackPort80(t *testing.T) {
	head := []byte("GET / HTTP/1.1\r\nHost: " + stackName + "\r\n\r\n")
	t.Run("dev", func(t *testing.T) {
		s := newSessionNet(t, Dev, stackName)
		s.pinForward(t)
		c, err := s.g.dial(t, s.web)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := c.Write(head); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for len(s.sink.received()) < len(head) && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if !bytes.Equal(s.sink.received(), head) {
			t.Fatalf("%q", s.sink.received())
		}
	})
	t.Run("locked", func(t *testing.T) {
		s := newSessionNet(t, Locked, stackName)
		s.pinForward(t)
		if c, err := s.g.dial(t, s.web); err == nil {
			c.Close()
			t.Fatal("connected")
		}
		if s.sink.accepted.Load() != 0 {
			t.Fatal("dialed")
		}
	})
}

func TestStackQUICRefused(t *testing.T) {
	s := newSessionNet(t, Dev, stackName)
	s.g.queryUDP(t, stackName+".", dns.TypeA)
	dst := addrOf(netip.AddrPortFrom(addrNPM, 443))
	c, err := gonet.DialUDP(s.g.s, nil, &dst, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("quic initial")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.n.Stats().UDPDenied == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if st := s.n.Stats(); st.UDPDenied != 1 || st.UDPActive != 0 {
		t.Fatalf("%+v", st)
	}
	if blockedKinds(s.p)[KindUDP] == "" {
		t.Fatalf("%+v", s.p.Blocked())
	}
}
