package netstack

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

// fakeEgress is a scriptable hook; every method records that it was asked.
type fakeEgress struct {
	mu       sync.Mutex
	query    func(string, uint16) bool
	answer   func(string, []netip.Addr) []netip.Addr
	conn     func(netip.AddrPort) ConnDecision
	inspect  func(netip.AddrPort, []byte) InspectResult
	udp      func(netip.AddrPort) bool
	reply    func(netip.AddrPort, []byte) ReplyResult
	retry    func(netip.AddrPort, []byte) InspectResult
	queries  []string
	answers  []string
	offered  [][]netip.Addr
	conns    []netip.AddrPort
	inspects [][]byte
	udps     []netip.AddrPort
}

func (f *fakeEgress) AllowQuery(name string, qtype uint16) bool {
	f.mu.Lock()
	f.queries = append(f.queries, name)
	f.mu.Unlock()
	return f.query != nil && f.query(name, qtype)
}

func (f *fakeEgress) Answer(name string, addrs []netip.Addr) []netip.Addr {
	f.mu.Lock()
	f.answers = append(f.answers, name)
	f.offered = append(f.offered, addrs)
	f.mu.Unlock()
	if f.answer != nil {
		return f.answer(name, addrs)
	}
	return addrs
}

func (f *fakeEgress) Conn(dst netip.AddrPort) ConnDecision {
	f.mu.Lock()
	f.conns = append(f.conns, dst)
	f.mu.Unlock()
	if f.conn != nil {
		return f.conn(dst)
	}
	return ConnDeny
}

func (f *fakeEgress) Inspect(dst netip.AddrPort, first []byte) InspectResult {
	f.mu.Lock()
	f.inspects = append(f.inspects, append([]byte(nil), first...))
	f.mu.Unlock()
	if f.inspect != nil {
		return f.inspect(dst, first)
	}
	return InspectDeny
}

func (f *fakeEgress) InspectReply(dst netip.AddrPort, reply []byte) ReplyResult {
	if f.reply != nil {
		return f.reply(dst, reply)
	}
	return ReplyDeny
}

func (f *fakeEgress) InspectRetry(dst netip.AddrPort, next []byte) InspectResult {
	if f.retry != nil {
		return f.retry(dst, next)
	}
	return InspectDeny
}

func (f *fakeEgress) UDP(dst netip.AddrPort) bool {
	f.mu.Lock()
	f.udps = append(f.udps, dst)
	f.mu.Unlock()
	return f.udp != nil && f.udp(dst)
}

func (f *fakeEgress) seen() (queries, conns, inspects, udps int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries), len(f.conns), len(f.inspects), len(f.udps)
}

// countingResolver answers everything and counts every lookup of any kind.
type countingResolver struct {
	calls atomic.Int64
	addrs []netip.Addr
	names sync.Map
}

func (r *countingResolver) count(host string) {
	r.calls.Add(1)
	r.names.Store(host, true)
}
func (r *countingResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.count(host)
	return r.addrs, nil
}
func (r *countingResolver) LookupCNAME(_ context.Context, host string) (string, error) {
	r.count(host)
	return "alias.example.", nil
}
func (r *countingResolver) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	r.count(name)
	return []*net.MX{{Host: "mx.example.", Pref: 1}}, nil
}
func (r *countingResolver) LookupNS(_ context.Context, name string) ([]*net.NS, error) {
	r.count(name)
	return []*net.NS{{Host: "ns.example."}}, nil
}
func (r *countingResolver) LookupSRV(_ context.Context, _, _, name string) (string, []*net.SRV, error) {
	r.count(name)
	return "", []*net.SRV{{Target: "srv.example.", Port: 1}}, nil
}
func (r *countingResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	r.count(name)
	return []string{"text"}, nil
}
func (r *countingResolver) LookupAddr(_ context.Context, addr string) ([]string, error) {
	r.count(addr)
	return []string{"ptr.example."}, nil
}

var allQueryTypes = []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeCNAME, dns.TypeMX, dns.TypeNS, dns.TypeSRV, dns.TypeTXT, dns.TypePTR, dns.TypeSOA, dns.TypeHTTPS, dns.TypeANY}

func wantRcode(q uint16) int {
	switch q {
	case dns.TypeA, dns.TypeAAAA, dns.TypeCNAME:
		return dns.RcodeNameError
	}
	return dns.RcodeRefused
}

func queryNameFor(q uint16) string {
	if q == dns.TypePTR {
		return "14.215.184.93.in-addr.arpa."
	}
	return "blocked.example."
}

// REQ-SBX-005: a refused name causes no query at the host's resolver, of any kind.
func TestEgressRefusedNameCausesNoLookup(t *testing.T) {
	res := &countingResolver{addrs: []netip.Addr{netip.MustParseAddr("93.184.215.14")}}
	hook := &fakeEgress{}
	h := &dnsHandler{res: res, timeout: time.Second, subnet: DefaultSubnet, egress: hook}
	for _, q := range allQueryTypes {
		r := query(t, h, queryNameFor(q), q, true, 0)
		if r.Rcode != wantRcode(q) || len(r.Answer) != 0 {
			t.Errorf("type %s: rcode %s, %d answers", dns.Type(q), dns.RcodeToString[r.Rcode], len(r.Answer))
		}
		r = query(t, h, queryNameFor(q), q, false, 0)
		if r.Rcode != wantRcode(q) {
			t.Errorf("type %s over TCP: rcode %s", dns.Type(q), dns.RcodeToString[r.Rcode])
		}
	}
	if n := res.calls.Load(); n != 0 {
		t.Fatalf("the resolver was called %d times for refused names", n)
	}
	if len(hook.answers) != 0 {
		t.Fatalf("Answer was called for refused names: %v", hook.answers)
	}
	if len(hook.queries) != 2*len(allQueryTypes) {
		t.Fatalf("AllowQuery was asked %d times", len(hook.queries))
	}
}

// The same through the stack's own UDP and TCP servers, as the guest asks.
func TestEgressRefusedNameOverTheStack(t *testing.T) {
	res := &countingResolver{}
	n := newTestStack(t, func(c *Config) { c.Resolver = res; c.Egress = &fakeEgress{} })
	g := newFakeGuest(t, n)
	gw := tcpip.FullAddress{NIC: 1, Addr: tcpipAddr(n.Config().Gateway()), Port: 53}
	ask := func(c net.Conn, framed bool, name string, qt uint16) int {
		t.Helper()
		q := new(dns.Msg)
		q.SetQuestion(name, qt)
		b, _ := q.Pack()
		if framed {
			b = append([]byte{byte(len(b) >> 8), byte(len(b))}, b...)
		}
		if _, err := c.Write(b); err != nil {
			t.Fatal(err)
		}
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 4096)
		if framed {
			if _, err := io.ReadFull(c, buf[:2]); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(c, buf[:int(buf[0])<<8|int(buf[1])]); err != nil {
				t.Fatal(err)
			}
		} else if _, err := c.Read(buf); err != nil {
			t.Fatal(err)
		}
		var r dns.Msg
		if err := r.Unpack(buf); err != nil && !framed {
			// A datagram read leaves zeros after the message, which Unpack ignores.
			t.Fatal(err)
		}
		return r.Rcode
	}
	uc, err := gonet.DialUDP(g.s, nil, &gw, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	tc, err := gonet.DialTCP(g.s, gw, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	for _, q := range allQueryTypes {
		if rc := ask(uc, false, queryNameFor(q), q); rc != wantRcode(q) {
			t.Errorf("UDP type %s: rcode %d", dns.Type(q), rc)
		}
		if rc := ask(tc, true, queryNameFor(q), q); rc != wantRcode(q) {
			t.Errorf("TCP type %s: rcode %d", dns.Type(q), rc)
		}
	}
	if c := res.calls.Load(); c != 0 {
		t.Fatalf("the resolver was called %d times", c)
	}
}

// A hook that refuses nothing and changes nothing leaves every answer as it was, byte for
// byte: all the hook adds is the possibility to say no.
func TestEgressAllowingHookChangesNoAnswer(t *testing.T) {
	addrs := []netip.Addr{netip.MustParseAddr("93.184.215.14"), netip.MustParseAddr("192.168.127.9")}
	plain := &dnsHandler{res: &countingResolver{addrs: addrs}, timeout: time.Second, subnet: DefaultSubnet}
	hooked := &dnsHandler{res: &countingResolver{addrs: addrs}, timeout: time.Second, subnet: DefaultSubnet,
		egress: &fakeEgress{query: func(string, uint16) bool { return true }}}
	for _, qt := range allQueryTypes {
		q := new(dns.Msg)
		q.SetQuestion(queryNameFor(qt), qt)
		b, _ := q.Pack()
		a, c := plain.answer(context.Background(), b, true), hooked.answer(context.Background(), b, true)
		if !bytes.Equal(a, c) {
			t.Errorf("type %s differs with an allowing hook", dns.Type(qt))
		}
	}
}

func TestEgressAnswerFilters(t *testing.T) {
	pub, drop, subnetAddr := netip.MustParseAddr("93.184.215.14"), netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("192.168.127.5")
	res := &countingResolver{addrs: []netip.Addr{pub, drop, subnetAddr, netip.MustParseAddr("::ffff:192.168.127.6")}}
	hook := &fakeEgress{
		query: func(string, uint16) bool { return true },
		answer: func(_ string, in []netip.Addr) []netip.Addr {
			var out []netip.Addr
			for _, a := range in {
				if a != drop {
					out = append(out, a)
				}
			}
			return out
		},
	}
	h := &dnsHandler{res: res, timeout: time.Second, subnet: DefaultSubnet, egress: hook}
	r := query(t, h, "Allowed.Example.", dns.TypeA, true, 0)
	if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != pub.String() {
		t.Fatalf("A: %v", r)
	}
	if r.Answer[0].Header().Name != "Allowed.Example." {
		t.Fatalf("the answer must keep the question's spelling: %v", r.Answer[0])
	}
	// The hook and the resolver see one spelling, and subnet addresses are never offered,
	// so they cannot be pinned.
	if len(hook.answers) != 1 || hook.answers[0] != "allowed.example" || hook.queries[0] != "allowed.example" {
		t.Fatalf("hook saw %v %v", hook.queries, hook.answers)
	}
	if _, ok := res.names.Load("allowed.example"); !ok {
		t.Fatal("the resolver did not get the lower-case spelling")
	}
	if _, ok := res.names.Load("Allowed.Example"); ok {
		t.Fatal("the guest's case reached the resolver")
	}
	if len(hook.offered[0]) != 2 {
		t.Fatalf("offered %v", hook.offered[0])
	}
	r = query(t, h, "allowed.example.", dns.TypeAAAA, true, 0)
	if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 || len(hook.answers) != 2 {
		t.Fatalf("AAAA must be NODATA, and shown to the hook: %v, %v", r, hook.answers)
	}
	// A hook that drops all of them leaves NODATA.
	hook.answer = func(string, []netip.Addr) []netip.Addr { return nil }
	r = query(t, h, "allowed.example.", dns.TypeA, true, 0)
	if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
		t.Fatalf("all dropped: %v", r)
	}
	// A hook cannot bring a subnet address in either.
	hook.answer = func(string, []netip.Addr) []netip.Addr { return []netip.Addr{subnetAddr} }
	if r = query(t, h, "allowed.example.", dns.TypeA, true, 0); len(r.Answer) != 0 {
		t.Fatalf("subnet address from the hook: %v", r)
	}
	// A failed lookup keeps its rcode and shows the hook nothing.
	calls := len(hook.answers)
	fr := &fakeResolver{}
	h.res = fr
	if r = query(t, h, "nope.example.", dns.TypeA, true, 0); r.Rcode != dns.RcodeNameError || len(hook.answers) != calls {
		t.Fatalf("failed lookup: %v, %v", r, hook.answers)
	}
}

// Local answers stay local, whatever the hook says, and the hook is not asked.
func TestEgressLocalAnswersStayLocal(t *testing.T) {
	res := &countingResolver{}
	hook := &fakeEgress{}
	h := &dnsHandler{res: res, timeout: time.Second, subnet: DefaultSubnet, egress: hook,
		hostGateway: func() (netip.Addr, bool) { return hgAddr, true }}
	if r := query(t, h, "5.127.168.192.in-addr.arpa.", dns.TypePTR, true, 0); r.Rcode != dns.RcodeNameError {
		t.Fatalf("subnet reverse name: %v", r)
	}
	if r := query(t, h, "254.127.168.192.in-addr.arpa.", dns.TypePTR, true, 0); len(r.Answer) != 1 {
		t.Fatalf("host gateway reverse name: %v", r)
	}
	if r := query(t, h, "host.docker.internal.", dns.TypeA, true, 0); len(r.Answer) != 1 {
		t.Fatalf("host gateway name: %v", r)
	}
	if res.calls.Load() != 0 || len(hook.queries) != 0 {
		t.Fatalf("resolver %d, hook %v", res.calls.Load(), hook.queries)
	}
	// Names the forwarder will not pass on are refused before the hook.
	if r := query(t, h, "a\\000b.example.", dns.TypeA, true, 0); r.Rcode != dns.RcodeRefused || len(hook.queries) != 0 {
		t.Fatalf("odd name: %v, %v", r, hook.queries)
	}
}

// recordingServer accepts on loopback, keeps what each connection sends and sends it back.
type recordingServer struct {
	ln       net.Listener
	addr     netip.AddrPort
	accepted atomic.Int64
	mu       sync.Mutex
	got      bytes.Buffer
	wg       sync.WaitGroup
}

func newRecordingServer(t *testing.T) *recordingServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &recordingServer{ln: ln, addr: netip.MustParseAddrPort(ln.Addr().String())}
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
				buf := make([]byte, 4096)
				for {
					k, err := c.Read(buf)
					s.mu.Lock()
					s.got.Write(buf[:k])
					s.mu.Unlock()
					if k > 0 {
						_, _ = c.Write(buf[:k])
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { ln.Close(); s.wg.Wait() })
	return s
}

func (s *recordingServer) received() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.got.Bytes()...)
}

var egressVirt = netip.MustParseAddrPort("192.168.127.253:443")

// egressStack is a stack with one host forward to a recording server and hook as Egress:
// a forward is how a test reaches something real without leaving this machine.
func egressStack(t *testing.T, hook Egress) (*Stack, *fakeGuest, *recordingServer) {
	t.Helper()
	srv := newRecordingServer(t)
	n := newTestStack(t, func(c *Config) {
		c.HostForwards = []HostForward{{Virtual: egressVirt, Host: srv.addr}}
		c.Egress = hook
	})
	return n, newFakeGuest(t, n), srv
}

func flowRoutes(n *Stack, dst netip.AddrPort) map[string]uint64 {
	out := map[string]uint64{}
	for _, f := range n.Flows().Flows {
		if f.Dst == dst.String() {
			out[f.Route] += f.Count
		}
	}
	return out
}

func TestEgressConnDenyDialsNothing(t *testing.T) {
	for _, decision := range []ConnDecision{ConnDeny, ConnDecision(77)} {
		hook := &fakeEgress{conn: func(netip.AddrPort) ConnDecision { return decision }}
		n, g, srv := egressStack(t, hook)
		if c, err := guestDial(t, g, egressVirt); err == nil {
			c.Close()
			t.Fatal("connected")
		}
		if srv.accepted.Load() != 0 {
			t.Fatal("something was dialed")
		}
		if s := n.Stats(); s.TCPDenied != 1 || s.TCPRefused != 0 {
			t.Fatalf("%+v", s)
		}
		if r := flowRoutes(n, egressVirt); r["denied"] != 1 || len(r) != 1 {
			t.Fatalf("flows %v", r)
		}
	}
}

// Egress narrows, it never widens: what Policy refuses stays refused, and the hook is not
// even asked.
func TestEgressCannotWiden(t *testing.T) {
	hook := &fakeEgress{conn: func(netip.AddrPort) ConnDecision { return ConnAllow }, udp: func(netip.AddrPort) bool { return true }}
	echo := echoServer(t)
	n := newTestStack(t, func(c *Config) { c.Egress = hook })
	g := newFakeGuest(t, n)
	dsts := []netip.AddrPort{
		netip.AddrPortFrom(testHostAddr, echo.Port()),
		netip.AddrPortFrom(netip.MustParseAddr("169.254.169.254"), 80),
		netip.AddrPortFrom(netip.MustParseAddr("192.168.127.254"), 80),
	}
	for _, dst := range dsts {
		if c, err := guestDial(t, g, dst); err == nil {
			c.Close()
			t.Fatalf("guest reached %s", dst)
		}
	}
	if _, conns, _, _ := hook.seen(); conns != 0 {
		t.Fatalf("the hook was asked %d times about refused addresses", conns)
	}
	if n.Stats().TCPDenied != uint64(len(dsts)) {
		t.Fatalf("%+v", n.Stats())
	}
}

func TestEgressConnAllowRelaysAsToday(t *testing.T) {
	hook := &fakeEgress{conn: func(netip.AddrPort) ConnDecision { return ConnAllow }}
	n, g, srv := egressStack(t, hook)
	c, err := guestDial(t, g, egressVirt)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	msg := []byte("no inspection")
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || !bytes.Equal(buf, msg) {
		t.Fatalf("%q %v", buf, err)
	}
	if _, _, inspects, _ := hook.seen(); inspects != 0 {
		t.Fatal("inspected")
	}
	if srv.accepted.Load() != 1 || flowRoutes(n, egressVirt)["forward"] != 1 {
		t.Fatalf("accepted %d, flows %v", srv.accepted.Load(), flowRoutes(n, egressVirt))
	}
}

// The guest's handshake completes before anything is dialed, nothing is dialed before the
// hook allows, and the guest's first bytes arrive unchanged, whatever pieces they came in.
func TestEgressInspectSendsFirstBytesUnchanged(t *testing.T) {
	first := append([]byte("\x16\x03\x01first bytes \x00\xff\r\n"), bytes.Repeat([]byte{0xAB}, 3000)...)
	release := make(chan struct{})
	hook := &fakeEgress{
		conn: func(netip.AddrPort) ConnDecision { return ConnInspect },
		inspect: func(_ netip.AddrPort, b []byte) InspectResult {
			if len(b) < len(first) {
				return InspectMore
			}
			<-release
			return InspectAllow
		},
	}
	n, g, srv := egressStack(t, hook)
	c, err := guestDial(t, g, egressVirt)
	if err != nil {
		t.Fatalf("the guest's handshake must complete first: %v", err)
	}
	defer c.Close()
	if srv.accepted.Load() != 0 {
		t.Fatal("dialed before any byte")
	}
	for _, piece := range [][]byte{first[:1], first[1:10], first[10:]} {
		if _, err := c.Write(piece); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if srv.accepted.Load() != 0 {
		t.Fatal("dialed before the hook allowed")
	}
	if r := flowRoutes(n, egressVirt); len(r) != 0 {
		t.Fatalf("a flow was logged before the decision: %v", r)
	}
	close(release)

	buf := make([]byte, len(first))
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, first) {
		t.Fatal("the echo differs")
	}
	if !bytes.Equal(srv.received(), first) {
		t.Fatalf("the server got %d bytes that differ from the guest's %d", len(srv.received()), len(first))
	}
	// The relay goes on as usual, both ways.
	if _, err := c.Write([]byte("more")); err != nil {
		t.Fatal(err)
	}
	more := make([]byte, 4)
	if _, err := io.ReadFull(c, more); err != nil || string(more) != "more" {
		t.Fatalf("%q %v", more, err)
	}
	if r := flowRoutes(n, egressVirt); r["forward"] != 1 || len(r) != 1 {
		t.Fatalf("flows %v", r)
	}
	if n.Stats().TCPDenied != 0 {
		t.Fatalf("%+v", n.Stats())
	}
	_, _, inspects, _ := hook.seen()
	if inspects == 0 {
		t.Fatal("not inspected")
	}
	// Half-close still crosses, with the buffered bytes before it.
	c.(*gonet.TCPConn).CloseWrite()
	if _, err := io.ReadAll(c); err != nil {
		t.Fatalf("after half-close: %v", err)
	}
}

// Everything the guest sent in one go, and its half-close, is relayed.
func TestEgressInspectFirstBytesWithHalfClose(t *testing.T) {
	hook := &fakeEgress{
		conn:    func(netip.AddrPort) ConnDecision { return ConnInspect },
		inspect: func(netip.AddrPort, []byte) InspectResult { return InspectAllow },
	}
	_, g, _ := egressStack(t, hook)
	c, err := guestDial(t, g, egressVirt)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	c.(*gonet.TCPConn).CloseWrite()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "ping" {
		t.Fatalf("%q %v", got, err)
	}
}

// expectClosed checks that the stack ended the connection and sent nothing on it.
func expectClosed(t *testing.T, c net.Conn) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, err := c.Read(make([]byte, 64))
	if err == nil {
		t.Fatal("the stack sent data on a connection it should refuse")
	}
	if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		t.Fatal("the connection stayed open")
	}
}

func TestEgressInspectDeny(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result InspectResult
	}{{"deny", InspectDeny}, {"unknown result", InspectResult(55)}} {
		t.Run(tc.name, func(t *testing.T) {
			hook := &fakeEgress{
				conn:    func(netip.AddrPort) ConnDecision { return ConnInspect },
				inspect: func(netip.AddrPort, []byte) InspectResult { return tc.result },
			}
			n, g, srv := egressStack(t, hook)
			c, err := guestDial(t, g, egressVirt)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if _, err := c.Write([]byte("bad hello")); err != nil {
				t.Fatal(err)
			}
			expectClosed(t, c)
			if srv.accepted.Load() != 0 {
				t.Fatal("dialed after a denial")
			}
			if s := n.Stats(); s.TCPDenied != 1 || s.TCPActive != 0 {
				t.Fatalf("%+v", s)
			}
			if r := flowRoutes(n, egressVirt); r["denied"] != 1 || len(r) != 1 {
				t.Fatalf("flows %v", r)
			}
		})
	}
}

// More than the bound never reaches a dial, and the hook sees no more than the bound.
func TestEgressInspectBound(t *testing.T) {
	var maxSeen atomic.Int64
	hook := &fakeEgress{
		conn: func(netip.AddrPort) ConnDecision { return ConnInspect },
		inspect: func(_ netip.AddrPort, b []byte) InspectResult {
			maxSeen.Store(max(maxSeen.Load(), int64(len(b))))
			return InspectMore
		},
	}
	n, g, srv := egressStack(t, hook)
	c, err := guestDial(t, g, egressVirt)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	go func() { _, _ = c.Write(make([]byte, 64<<10)) }()
	expectClosed(t, c)
	if got := maxSeen.Load(); got != MaxInspectBytes {
		t.Fatalf("the hook saw %d bytes at most, want %d", got, MaxInspectBytes)
	}
	if srv.accepted.Load() != 0 || n.Stats().TCPDenied != 1 {
		t.Fatalf("accepted %d, %+v", srv.accepted.Load(), n.Stats())
	}
}

// The deadline is overall: a guest that keeps sending a byte now and then gets no more
// time than one that sends nothing.
func TestEgressInspectDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("takes the whole deadline")
	}
	hook := &fakeEgress{
		conn:    func(netip.AddrPort) ConnDecision { return ConnInspect },
		inspect: func(netip.AddrPort, []byte) InspectResult { return InspectMore },
	}
	n, g, srv := egressStack(t, hook)
	c, err := guestDial(t, g, egressVirt)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	silent, err := guestDial(t, g, egressVirt)
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	start := time.Now()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Millisecond):
				_, _ = c.Write([]byte{1})
			}
		}
	}()
	expectClosed(t, c)
	elapsed := time.Since(start)
	close(stop)
	wg.Wait()
	expectClosed(t, silent)
	if elapsed < inspectTimeout-500*time.Millisecond || elapsed > inspectTimeout+3*time.Second {
		t.Fatalf("closed after %v, want about %v", elapsed, inspectTimeout)
	}
	if srv.accepted.Load() != 0 || n.Stats().TCPDenied != 2 {
		t.Fatalf("accepted %d, %+v", srv.accepted.Load(), n.Stats())
	}
}

// A guest that closes before a decision is denied, with nothing dialed.
func TestEgressInspectGuestCloses(t *testing.T) {
	hook := &fakeEgress{
		conn:    func(netip.AddrPort) ConnDecision { return ConnInspect },
		inspect: func(netip.AddrPort, []byte) InspectResult { return InspectMore },
	}
	n, g, srv := egressStack(t, hook)
	c, err := guestDial(t, g, egressVirt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	c.Close()
	deadline := time.Now().Add(5 * time.Second)
	for n.Stats().TCPDenied == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n.Stats().TCPDenied != 1 || srv.accepted.Load() != 0 {
		t.Fatalf("accepted %d, %+v", srv.accepted.Load(), n.Stats())
	}
}

// An allowed connection whose dial fails is a refusal to the guest, not a hang.
func TestEgressInspectDialFails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := netip.MustParseAddrPort(ln.Addr().String())
	ln.Close()
	hook := &fakeEgress{
		conn:    func(netip.AddrPort) ConnDecision { return ConnInspect },
		inspect: func(netip.AddrPort, []byte) InspectResult { return InspectAllow },
	}
	n := newTestStack(t, func(c *Config) {
		c.HostForwards = []HostForward{{Virtual: egressVirt, Host: closed}}
		c.Egress = hook
		c.DialTimeout = 3 * time.Second
	})
	g := newFakeGuest(t, n)
	c, err := guestDial(t, g, egressVirt)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, c)
	if s := n.Stats(); s.TCPRefused != 1 || s.TCPDenied != 0 {
		t.Fatalf("%+v", s)
	}
}

// The connection limit holds for connections waiting to be inspected, and they give their
// slot back.
func TestEgressInspectHoldsTheSemaphore(t *testing.T) {
	release := make(chan struct{})
	hook := &fakeEgress{
		conn: func(netip.AddrPort) ConnDecision { return ConnInspect },
		inspect: func(netip.AddrPort, []byte) InspectResult {
			<-release
			return InspectDeny
		},
	}
	srv := newRecordingServer(t)
	n := newTestStack(t, func(c *Config) {
		c.HostForwards = []HostForward{{Virtual: egressVirt, Host: srv.addr}}
		c.Egress = hook
		c.MaxTCPConns = 1
	})
	defer close(release)
	g := newFakeGuest(t, n)
	c, err := guestDial(t, g, egressVirt)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if c2, err := guestDial(t, g, egressVirt); err == nil {
		c2.Close()
		t.Fatal("a second connection past the limit")
	}
	if s := n.Stats(); s.TCPRefused != 1 {
		t.Fatalf("%+v", s)
	}
	if r := flowRoutes(n, egressVirt); r["denied"] != 1 {
		t.Fatalf("flows %v", r)
	}
}

func TestEgressUDP(t *testing.T) {
	quic := netip.MustParseAddrPort("93.184.215.14:443")
	hook := &fakeEgress{}
	res := &countingResolver{}
	n := newTestStack(t, func(c *Config) { c.Egress = hook; c.Resolver = res })
	g := newFakeGuest(t, n)
	uc, err := gonet.DialUDP(g.s, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpipAddr(quic.Addr()), Port: quic.Port()}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	if _, err := uc.Write([]byte("quic initial")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for n.Stats().UDPDenied == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s := n.Stats(); s.UDPDenied != 1 || s.UDPActive != 0 {
		t.Fatalf("%+v", s)
	}
	if _, _, _, udps := hook.seen(); udps != 1 {
		t.Fatalf("UDP asked %d times", udps)
	}
	var routes []string
	for _, f := range n.Flows().Flows {
		routes = append(routes, f.Proto+" "+f.Dst+" "+f.Route)
	}
	if want := "udp " + quic.String() + " denied"; !strings.Contains(strings.Join(routes, ","), want) {
		t.Fatalf("flows %v", routes)
	}

	// DNS to the stack's own server is no flow and does not ask.
	gw := tcpip.FullAddress{NIC: 1, Addr: tcpipAddr(n.Config().Gateway()), Port: 53}
	dc, err := gonet.DialUDP(g.s, nil, &gw, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Close()
	q := new(dns.Msg)
	q.SetQuestion("blocked.example.", dns.TypeA)
	b, _ := q.Pack()
	if _, err := dc.Write(b); err != nil {
		t.Fatal(err)
	}
	dc.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := dc.Read(make([]byte, 512)); err != nil {
		t.Fatal(err)
	}
	if _, _, _, udps := hook.seen(); udps != 1 {
		t.Fatalf("DNS to the stack asked the UDP hook: %d", udps)
	}
}

// tlsHook allows every first hello as TLS. The recording server echoes, so its "reply" is
// the guest's bytes.
func tlsHook() *fakeEgress {
	return &fakeEgress{
		conn:    func(netip.AddrPort) ConnDecision { return ConnInspect },
		inspect: func(netip.AddrPort, []byte) InspectResult { return InspectAllowTLS },
	}
}

func writeAndEcho(t *testing.T, c net.Conn, msg string) {
	t.Helper()
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != msg {
		t.Fatalf("%q %v", buf, err)
	}
}

func TestEgressReplyPassRelays(t *testing.T) {
	hook := tlsHook()
	hook.reply = func(netip.AddrPort, []byte) ReplyResult { return ReplyPass }
	hook.retry = func(netip.AddrPort, []byte) InspectResult {
		t.Error("InspectRetry asked without a retry")
		return InspectDeny
	}
	_, g, srv := egressStack(t, hook)
	c, err := guestDial(t, g, egressVirt)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	writeAndEcho(t, c, "hello")
	writeAndEcho(t, c, "relayed")
	if got := string(srv.received()); got != "hellorelayed" {
		t.Fatalf("%q", got)
	}
}

// After a retry, the guest's next bytes wait for the hook, then go out, then the relay runs.
func TestEgressReplyRetryInspectsNextBytes(t *testing.T) {
	hook := tlsHook()
	hook.reply = func(netip.AddrPort, []byte) ReplyResult { return ReplyRetry }
	var seen atomic.Int64
	hook.retry = func(_ netip.AddrPort, b []byte) InspectResult {
		seen.Add(1)
		if len(b) < len("second") {
			return InspectMore
		}
		return InspectAllow
	}
	_, g, srv := egressStack(t, hook)
	c, err := guestDial(t, g, egressVirt)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	writeAndEcho(t, c, "first")
	if _, err := c.Write([]byte("sec")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := string(srv.received()); got != "first" {
		t.Fatalf("part of the second hello went out before the hook allowed it: %q", got)
	}
	if _, err := c.Write([]byte("ond")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 6)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "second" {
		t.Fatalf("%q %v", buf, err)
	}
	writeAndEcho(t, c, "relayed")
	if seen.Load() == 0 {
		t.Fatal("not inspected")
	}
}

func TestEgressReplyRetryDenied(t *testing.T) {
	hook := tlsHook()
	hook.reply = func(netip.AddrPort, []byte) ReplyResult { return ReplyRetry }
	hook.retry = func(netip.AddrPort, []byte) InspectResult { return InspectDeny }
	n, g, srv := egressStack(t, hook)
	c, err := guestDial(t, g, egressVirt)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	writeAndEcho(t, c, "first")
	if _, err := c.Write([]byte("second")); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, c)
	if got := string(srv.received()); got != "first" {
		t.Fatalf("the server got %q", got)
	}
	if s := n.Stats(); s.TCPDenied != 1 || s.TCPActive != 0 {
		t.Fatalf("%+v", s)
	}
	if r := flowRoutes(n, egressVirt); r["denied"] != 1 {
		t.Fatalf("flows %v", r)
	}
}

func TestEgressReplyDeniedOrUnknown(t *testing.T) {
	for _, result := range []ReplyResult{ReplyDeny, ReplyResult(99)} {
		hook := tlsHook()
		hook.reply = func(netip.AddrPort, []byte) ReplyResult { return result }
		n, g, _ := egressStack(t, hook)
		c, err := guestDial(t, g, egressVirt)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := c.Write([]byte("first")); err != nil {
			t.Fatal(err)
		}
		expectClosed(t, c)
		if s := n.Stats(); s.TCPDenied != 1 {
			t.Fatalf("%+v", s)
		}
	}
}

// A server that keeps saying too little ends the connection at the deadline: it is not
// relayed uninspected, and later bytes from the guest do not reach it.
func TestEgressReplyDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s, err := ln.Accept()
		if err != nil {
			return
		}
		defer s.Close()
		_, _ = s.Write([]byte{22}) // the start of a record, and never the rest
		buf := make([]byte, 256)
		for {
			k, err := s.Read(buf)
			mu.Lock()
			got.Write(buf[:k])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	defer func() { ln.Close(); wg.Wait() }()
	hook := tlsHook()
	hook.reply = func(netip.AddrPort, []byte) ReplyResult { return ReplyMore }
	n := newTestStack(t, func(c *Config) {
		c.HostForwards = []HostForward{{Virtual: egressVirt, Host: netip.MustParseAddrPort(ln.Addr().String())}}
		c.Egress = hook
		c.DialTimeout = 500 * time.Millisecond
	})
	g := newFakeGuest(t, n)
	c, err := guestDial(t, g, egressVirt)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	if _, err := c.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	// In a later read than the first bytes: those go out in one piece, whatever they hold.
	for wait := time.Now().Add(5 * time.Second); time.Now().Before(wait); time.Sleep(2 * time.Millisecond) {
		mu.Lock()
		arrived := got.Len() > 0
		mu.Unlock()
		if arrived {
			break
		}
	}
	_, _ = c.Write([]byte("later"))
	expectClosed(t, c)
	if e := time.Since(start); e < 400*time.Millisecond || e > 5*time.Second {
		t.Fatalf("closed after %v", e)
	}
	if s := n.Stats(); s.TCPRefused != 1 || s.TCPActive != 0 {
		t.Fatalf("%+v", s)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.HasPrefix(got.String(), "first") || strings.Contains(got.String(), "later") {
		t.Fatalf("the server got %q", got.String())
	}
}

// Plain InspectAllow never asks about the reply: a silent server still gets the relay.
func TestEgressPlainAllowIsNotWatched(t *testing.T) {
	hook := tlsHook()
	hook.inspect = func(netip.AddrPort, []byte) InspectResult { return InspectAllow }
	hook.reply = func(netip.AddrPort, []byte) ReplyResult {
		t.Error("InspectReply asked on a plain allow")
		return ReplyDeny
	}
	_, g, _ := egressStack(t, hook)
	c, err := guestDial(t, g, egressVirt)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	writeAndEcho(t, c, "plain")
}

// Without a hook, the stack's denial errors and routes are what they were.
func TestEgressNilHookLeavesFlowsAlone(t *testing.T) {
	n, g, srv := egressStack(t, nil)
	c, err := guestDial(t, g, egressVirt)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if r := flowRoutes(n, egressVirt); r["forward"] != 1 {
		t.Fatalf("flows %v", r)
	}
	// Dialed before the guest sent anything, as always.
	deadline := time.Now().Add(5 * time.Second)
	for srv.accepted.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if srv.accepted.Load() != 1 {
		t.Fatal("not dialed")
	}
}
