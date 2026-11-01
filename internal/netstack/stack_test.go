package netstack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/Higangssh/hyperbutler/internal/netstack/frame"
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
	msg := bytes.Repeat([]byte("hyperbutler"), 50000) // 300 kB, many segments
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
	// A wildcard bind makes Windows Firewall prompt for every freshly built netstack.test.exe,
	// so it runs only where asked for (CI sets this).
	if os.Getenv("HYPERBUTLER_TEST_WILDCARD") != "1" {
		t.Skip("binds the wildcard address; set HYPERBUTLER_TEST_WILDCARD=1 to run")
	}
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

// TestOffloadLink runs bulk TCP both ways over an offload-mode link and checks that
// segments larger than the MTU crossed it in both directions, intact.
func TestOffloadLink(t *testing.T) {
	echo := echoServer(t)
	virt := netip.MustParseAddrPort("192.168.127.254:7")
	n := newTestStack(t, func(c *Config) {
		c.HostForwards = []HostForward{{Virtual: virt, Host: echo}}
		c.MTU = 1500
	})
	g := newFakeGuestMode(t, n, true)
	c, err := guestDial(t, g, virt)
	if err != nil {
		t.Fatalf("dial host forward: %v", err)
	}
	defer c.Close()
	msg := make([]byte, 4<<20)
	for i := range msg {
		msg[i] = byte(i * 7)
	}
	go func() { c.Write(msg); c.(*gonet.TCPConn).CloseWrite() }()
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("echo returned %d bytes, want %d (or different bytes)", len(got), len(msg))
	}
	if g.csumOK.Load() == 0 {
		t.Fatal("no partial checksum from the host was checked")
	}
	if g.bigIn.Load() == 0 || g.bigOut.Load() == 0 {
		t.Fatalf("segments over the MTU: %d from the host, %d to it; want both", g.bigIn.Load(), g.bigOut.Load())
	}
	if on, known := n.Offloading(); !on || !known {
		t.Fatalf("Offloading() = %v, %v", on, known)
	}
}

// streamAt fills p with the bytes at off of the seeded test stream: 8-byte word k is
// splitmix64(seed+k), so any part of the stream can be computed on its own.
func streamAt(seed uint64, off int64, p []byte) {
	var w [8]byte
	for len(p) > 0 {
		z := seed + uint64(off)/8
		z = (z ^ z>>30) * 0xbf58476d1ce4e5b9
		z = (z ^ z>>27) * 0x94d049bb133111eb
		binary.LittleEndian.PutUint64(w[:], z^z>>31)
		n := copy(p, w[off%8:])
		p, off = p[n:], off+int64(n)
	}
}

// sendStream writes size bytes of stream seed to w and returns their SHA-256.
func sendStream(w io.Writer, seed uint64, size int64) ([32]byte, error) {
	h := sha256.New()
	buf := make([]byte, 64<<10)
	for off := int64(0); off < size; {
		b := buf[:min(int64(len(buf)), size-off)]
		streamAt(seed, off, b)
		h.Write(b)
		if _, err := w.Write(b); err != nil {
			return [32]byte{}, err
		}
		off += int64(len(b))
	}
	return [32]byte(h.Sum(nil)), nil
}

// recvStream reads r to the end and returns how many bytes it read and their SHA-256.
func recvStream(r io.Reader) (int64, [32]byte, error) {
	h := sha256.New()
	n, err := io.CopyBuffer(h, r, make([]byte, 64<<10))
	return n, [32]byte(h.Sum(nil)), err
}

// arrivalCheck checks, in the fake guest's link reader, the payload of every TCP
// segment from the host's port against the stream its connection carries: a segment
// whose bytes the host changed after queueing it is caught as it arrives, whatever the
// guest's TCP does with it afterwards.
type arrivalCheck struct {
	port uint16 // the host side's port
	mu   sync.Mutex
	// flows by the guest's port. The link reader adds a flow at the SYN; the test sets
	// its seed once the dial returns, before the host sends anything.
	flows    map[uint16]*arrivalFlow
	exp      []byte // only the link reader uses it
	segs     atomic.Int64
	bad      atomic.Int64
	firstBad atomic.Int64
	corrupt  chan struct{} // closed at the first bad segment, or when the link breaks
	once     sync.Once
}

func (a *arrivalCheck) fail() { a.once.Do(func() { close(a.corrupt) }) }

func (a *arrivalCheck) failed() bool {
	select {
	case <-a.corrupt:
		return true
	default:
		return false
	}
}

type arrivalFlow struct {
	isn  uint32
	seed uint64
	set  bool
}

func newArrivalCheck(port uint16) *arrivalCheck {
	return &arrivalCheck{port: port, flows: map[uint16]*arrivalFlow{}, corrupt: make(chan struct{})}
}

func (a *arrivalCheck) flow(guestPort uint16) *arrivalFlow {
	f := a.flows[guestPort]
	if f == nil {
		f = &arrivalFlow{}
		a.flows[guestPort] = f
	}
	return f
}

// expect sets the seed of the stream the host sends to the guest's port.
func (a *arrivalCheck) expect(guestPort uint16, seed uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.flow(guestPort)
	f.seed, f.set = seed, true
}

func (a *arrivalCheck) inspect(eth []byte) {
	if len(eth) < header.EthernetMinimumSize || header.Ethernet(eth).Type() != header.IPv4ProtocolNumber {
		return
	}
	ip := header.IPv4(eth[header.EthernetMinimumSize:])
	if !ip.IsValid(len(ip)) || ip.TransportProtocol() != tcp.ProtocolNumber {
		return
	}
	seg := header.TCP(ip.Payload())
	if len(seg) < header.TCPMinimumSize || int(seg.DataOffset()) < header.TCPMinimumSize ||
		int(seg.DataOffset()) > len(seg) || seg.SourcePort() != a.port {
		return
	}
	syn := seg.Flags()&header.TCPFlagSyn != 0
	a.mu.Lock()
	f := a.flow(seg.DestinationPort())
	if syn {
		f.isn = seg.SequenceNumber()
	}
	fl := *f
	a.mu.Unlock()
	p := seg.Payload()
	if len(p) == 0 || !fl.set || syn {
		return
	}
	off := int64(seg.SequenceNumber() - fl.isn - 1)
	if cap(a.exp) < len(p) {
		a.exp = make([]byte, len(p))
	}
	exp := a.exp[:len(p)]
	streamAt(fl.seed, off, exp)
	a.segs.Add(1)
	if !bytes.Equal(p, exp) {
		if a.bad.Add(1) == 1 {
			a.firstBad.Store(off)
		}
		a.fail()
	}
}

// streamResult is one stream as one end saw it.
type streamResult struct {
	n   int64
	sum [32]byte
	err error
}

// duplex sends size bytes of stream seed on c and reads c to the end at the same time,
// then closes c. It returns what it sent and what it read.
func duplex(c net.Conn, seed uint64, size int64) (sent, got streamResult) {
	done := make(chan streamResult, 1)
	go func() {
		sum, err := sendStream(c, seed, size)
		if err == nil {
			err = c.(interface{ CloseWrite() error }).CloseWrite()
		}
		done <- streamResult{size, sum, err}
	}()
	got.n, got.sum, got.err = recvStream(c)
	if got.err != nil {
		c.Close() // unblock the sender
	}
	sent = <-done
	c.Close()
	return sent, got
}

// dribble writes stream seed to c in small writes of random sizes, a fraction of a
// millisecond apart, until the peer half-closes c, then closes c.
func dribble(c net.Conn, seed uint64) {
	defer c.Close()
	stop := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, c); close(stop) }()
	rng := rand.New(rand.NewPCG(seed, 1))
	msg := make([]byte, 3000)
	for off := int64(0); ; {
		select {
		case <-stop:
			return
		default:
		}
		m := msg[:1+rng.IntN(len(msg))]
		streamAt(seed, off, m)
		if _, err := c.Write(m); err != nil {
			<-stop
			return
		}
		off += int64(len(m))
		time.Sleep(100 * time.Microsecond)
	}
}

// growQueue makes the link's next queue outgrow the spare bound, until a write of such
// a queue has been seen (bufs.grown), at most growTries times. It is called at the start
// of a large write, after writeLoop took the queue: it gives the new queue an array of
// its own with no room (as a fresh link has, or one whose last queue a burst grew), so
// enqueue's appends grow it geometrically, and waits up to growWait for the queue to
// fill to the bound while the write stalls.
//
// Without it, whether a run wrote such a queue at all was up to scheduling: the two
// buffers writeLoop alternates soon settle at a capacity of 1088 KiB, within the bound,
// and only a queue that fills to the bound while still in a smaller array grows past
// it. TestLinkIntegrityUnderStalls failed its setup check ("0 checks while a queue past
// the spare bound was written") in about 1 run in 10 with GOMAXPROCS=1, and once in a
// loaded `go test ./...`.
func growQueue(n *Stack, bufs *linkBufferStats, tries *atomic.Int64) {
	const growTries, growWait = 40, 250 * time.Millisecond
	if bufs.grown.Load() > 0 || tries.Add(1) > growTries {
		return
	}
	n.linkMu.Lock()
	l := n.link
	n.linkMu.Unlock()
	if l == nil {
		return
	}
	l.mu.Lock()
	l.pending = slices.Clip(l.pending)
	l.mu.Unlock()
	for deadline := time.Now().Add(growWait); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		l.mu.Lock()
		full := len(l.pending) >= maxPending || cap(l.pending) > maxSpare || l.closed
		l.mu.Unlock()
		if full {
			return
		}
	}
}

// TestLinkIntegrityUnderStalls streams seeded data both ways through TCP connections
// over an offload link whose hvsock writes stall, through the real guestLink and
// frame.Pump: the queue to the guest fills to its bound and past it, the guest's frames
// are read ahead of the stack, and only the link's CRC-32C checks the frames: neither
// side verifies a TCP checksum (ADR 0006, "Integrity"). Every stream must arrive with
// its SHA-256, every segment from the host must carry its own bytes when the guest
// reads it, no frame may fail its CRC, and the link's buffers must never share an
// array. Before 036c062 a queue that outgrew the spare bound was sent while enqueue
// overwrote it.
//
// Four connections share the link, as parallel layer downloads do: one connection's
// send buffer (tcpBufferLarge) is about the queue bound, so alone it rarely fills the
// queue. A fifth one carries small writes (dribble). Without the fix the link check
// fails on every run. In 20 such runs no segment with other bytes reached the guest:
// the guest dropped 1-4 frames mixed from several for their CRC in 8, and 15 ended
// with the frame stream out of sync (a length read from a payload, which ends the
// link; netfwd reconnects). With link version 1, which has no CRC, 2 of 20 runs
// delivered a segment with other bytes. HYPERBUTLER_TEST_LONG=1 streams 256 MiB each
// way instead of 128 (16 with -short or -race).
func TestLinkIntegrityUnderStalls(t *testing.T) {
	size := int64(128 << 20)
	switch {
	case os.Getenv("HYPERBUTLER_TEST_LONG") == "1":
		size = 256 << 20
	case testing.Short() || raceEnabled:
		size = 16 << 20
	}
	const conns = 4
	per := size / conns
	const upSeed, downSeed, chatSeed, stallSeed = 0x0123456789abcdef, 0xfedcba9876543210, 0x5eed, 20261101
	bufs := checkLinkBuffers(t)

	// The host service: a connection names its index in its first 8 bytes; the service
	// then sends that index's download while it hashes the upload.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	type hostFlow struct {
		i         int
		sent, got streamResult
	}
	hostSide := make(chan hostFlow, conns+1)
	go func() {
		for range conns + 1 {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = c.SetDeadline(time.Now().Add(30 * time.Second))
				var idx [8]byte
				if _, err := io.ReadFull(c, idx[:]); err != nil {
					c.Close()
					hostSide <- hostFlow{i: -1, got: streamResult{err: err}}
					return
				}
				i := binary.BigEndian.Uint64(idx[:])
				if i == conns {
					dribble(c, chatSeed)
					return
				}
				sent, got := duplex(c, downSeed+i, per)
				hostSide <- hostFlow{int(i % conns), sent, got}
			}()
		}
	}()

	virt := netip.MustParseAddrPort("192.168.127.254:7")
	n := newTestStack(t, func(c *Config) {
		c.HostForwards = []HostForward{{Virtual: virt, Host: netip.MustParseAddrPort(ln.Addr().String())}}
	})
	check := newArrivalCheck(virt.Port())
	var sc *stallingConn
	var growTries atomic.Int64
	g := newFakeGuestWith(t, n, fakeGuestOpts{
		offload: true,
		wrap: func(c net.Conn) net.Conn {
			sc = newStallingConn(c, stallSeed)
			sc.grow = func() { growQueue(n, bufs, &growTries) }
			return sc
		},
		inspect: check.inspect,
		linkErr: func(error) { check.fail() },
	})
	defer func() {
		t.Logf("frames dropped for a CRC mismatch: %d from the host (in the guest), %d from the guest (in the host)", g.crcBad.Load(), n.Stats().FramesCRCDropped)
	}()
	var cs []net.Conn
	defer func() {
		for _, c := range cs {
			c.Close()
		}
	}()
	for range conns + 1 { // the last one is the chatter
		c, err := guestDial(t, g, virt)
		if err != nil {
			t.Fatalf("dial host forward: %v", err)
		}
		_ = c.SetDeadline(time.Now().Add(30 * time.Second))
		cs = append(cs, c)
	}
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		// Corrupted headers can stall a connection: end them at the first bad segment.
		select {
		case <-check.corrupt:
			for _, c := range cs {
				c.Close()
			}
		case <-finished:
		}
	}()
	type guestFlow struct{ sent, got streamResult }
	guestSide := make([]guestFlow, conns)
	var wg sync.WaitGroup
	for i, c := range cs {
		seed := downSeed + uint64(i)
		if i == conns {
			seed = chatSeed // echoed
		}
		check.expect(uint16(c.LocalAddr().(*net.TCPAddr).Port), seed)
		var idx [8]byte
		binary.BigEndian.PutUint64(idx[:], uint64(i))
		if _, err := c.Write(idx[:]); err != nil {
			t.Fatal(err)
		}
		if i == conns {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			guestSide[i].sent, guestSide[i].got = duplex(c, upSeed+uint64(i), per)
		}()
	}
	// The host dribbles small writes on the last connection until the streams are done
	// (dribble). Its small segments land between the streams' segments, which are all
	// the same size, so the frames in one queue do not line up with those in the next:
	// a queue overwritten by later frames then reaches the guest as cut and mixed
	// frames, not as whole ones that TCP would take for retransmissions.
	chatErr := make(chan error, 1)
	go func() {
		got, sum, err := recvStream(cs[conns])
		if err == nil {
			exp := make([]byte, got)
			streamAt(chatSeed, 0, exp)
			if sha256.Sum256(exp) != sum {
				err = fmt.Errorf("the dribbled stream arrived different (%d bytes)", got)
			}
		}
		chatErr <- err
	}()
	wg.Wait()
	_ = cs[conns].(*gonet.TCPConn).CloseWrite() // stops the dribbler
	chatE := <-chatErr
	if b := check.bad.Load(); b > 0 {
		t.Fatalf("%d of %d segments from the host arrived with other bytes (the first at stream offset %d)",
			b, check.segs.Load(), check.firstBad.Load())
	}
	if check.failed() {
		t.Fatalf("the guest stopped reading the link after %d good segments", check.segs.Load())
	}
	if chatE != nil {
		t.Fatal(chatE)
	}
	hosts := make([]hostFlow, conns)
	for range conns {
		h := <-hostSide
		if h.i < 0 {
			t.Fatalf("host side: %v", h.got.err)
		}
		hosts[h.i] = h
	}
	for i, gf := range guestSide {
		hf := hosts[i]
		for _, r := range []streamResult{gf.sent, gf.got, hf.sent, hf.got} {
			if r.err != nil {
				t.Fatalf("connection %d: %v", i, r.err)
			}
		}
		if gf.got.n != per || gf.got.sum != hf.sent.sum {
			t.Errorf("connection %d, download: the guest got %d bytes with SHA-256 %x, the host sent %d with %x",
				i, gf.got.n, gf.got.sum[:8], per, hf.sent.sum[:8])
		}
		if hf.got.n != per || hf.got.sum != gf.sent.sum {
			t.Errorf("connection %d, upload: the host got %d bytes with SHA-256 %x, the guest sent %d with %x",
				i, hf.got.n, hf.got.sum[:8], per, gf.sent.sum[:8])
		}
	}
	if check.segs.Load() == 0 || g.bigIn.Load() == 0 || g.bigOut.Load() == 0 {
		t.Errorf("test setup: %d segments checked, %d and %d over the MTU in and out",
			check.segs.Load(), g.bigIn.Load(), g.bigOut.Load())
	}
	if d := n.Stats().FramesDropped; d != 0 {
		t.Errorf("FramesDropped = %d", d)
	}
	if c, h := g.crcBad.Load(), n.Stats().FramesCRCDropped; c != 0 || h != 0 {
		t.Errorf("frames dropped for a CRC mismatch: %d from the host (in the guest), %d from the guest (in the host)", c, h)
	}
	// Under the race detector the stacks may be too slow to fill the queue past the
	// bound in a short run; the other checks still apply.
	if sc.stallCount() == 0 || (bufs.grown.Load() == 0 && !raceEnabled) {
		t.Errorf("test setup: %d link stalls, %d checks while a queue past the spare bound was written; want both",
			sc.stallCount(), bufs.grown.Load())
	}
	t.Logf("%d MiB each way over %d connections, %d link stalls, %d checks while a queue past the spare bound was written, %d segments checked on arrival",
		size>>20, conns, sc.stallCount(), bufs.grown.Load(), check.segs.Load())
}

// TestGuestResetPropagates: a guest that resets a relayed connection makes the relay
// reset the Windows socket too (no TIME_WAIT on this host), while a normal close stays
// a normal close.
func TestGuestResetPropagates(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	results := make(chan error, 2)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b := make([]byte, 1)
				_, err := io.ReadFull(c, b)
				if err == nil {
					_, err = c.Read(b)
				}
				results <- err
			}()
		}
	}()
	virt := netip.MustParseAddrPort("192.168.127.254:9")
	host := netip.MustParseAddrPort(ln.Addr().String())
	n := newTestStack(t, func(c *Config) { c.HostForwards = []HostForward{{Virtual: virt, Host: host}} })
	g := newFakeGuest(t, n)
	for _, reset := range []bool{true, false} {
		var wq waiter.Queue
		ep, terr := g.s.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
		if terr != nil {
			t.Fatal(terr)
		}
		c := gonet.NewTCPConn(&wq, ep)
		entry, ready := waiter.NewChannelEntry(waiter.WritableEvents)
		wq.EventRegister(&entry)
		if terr := ep.Connect(tcpip.FullAddress{NIC: 1, Addr: tcpipAddr(virt.Addr()), Port: virt.Port()}); terr != nil {
			if _, ok := terr.(*tcpip.ErrConnectStarted); !ok {
				t.Fatal(terr)
			}
			select {
			case <-ready:
			case <-time.After(5 * time.Second):
				t.Fatal("connect timed out")
			}
		}
		wq.EventUnregister(&entry)
		if _, err := c.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		if reset {
			ep.SocketOptions().SetLinger(tcpip.LingerOption{Enabled: true, Timeout: 0})
		}
		c.Close()
		select {
		case err := <-results:
			if reset && (err == nil || errors.Is(err, io.EOF)) {
				t.Fatalf("guest reset arrived as %v, want a reset", err)
			}
			if !reset && !errors.Is(err, io.EOF) {
				t.Fatalf("guest close arrived as %v, want EOF", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("server saw nothing")
		}
	}
}

// TestHostResetPropagates: a host client that resets a published connection makes the
// relay reset the guest's connection too, instead of closing it (which would leave a
// gVisor endpoint in TIME_WAIT).
func TestHostResetPropagates(t *testing.T) {
	n := newTestStack(t, nil)
	g := newFakeGuest(t, n)
	gl, err := gonet.ListenTCP(g.s, tcpip.FullAddress{NIC: 1, Port: 8081}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer gl.Close()
	results := make(chan error, 2)
	go func() {
		for {
			c, err := gl.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b := make([]byte, 1)
				_, err := io.ReadFull(c, b)
				if err == nil {
					_, err = c.Read(b)
				}
				results <- err
			}()
		}
	}()
	cl, err := n.Publish(Publish{Host: netip.MustParseAddrPort("127.0.0.1:0"), GuestPort: 8081})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	for _, reset := range []bool{true, false} {
		c, err := net.DialTimeout("tcp", cl.(net.Listener).Addr().String(), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		if reset {
			_ = c.(*net.TCPConn).SetLinger(0)
		}
		c.Close()
		select {
		case err := <-results:
			if reset && (err == nil || errors.Is(err, io.EOF)) {
				t.Fatalf("host reset arrived as %v, want a reset", err)
			}
			if !reset && !errors.Is(err, io.EOF) {
				t.Fatalf("host close arrived as %v, want EOF", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("guest saw nothing")
		}
	}
}

// TestResetKinds: a host socket's reset is never taken for the guest's. On Linux both
// read "connection reset by peer", and TestHostResetPropagates failed there.
func TestResetKinds(t *testing.T) {
	host := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}
	guest := &net.OpError{Op: "read", Net: "tcp", Err: errors.New(resetText)}
	if guestReset(host) {
		t.Error("a host socket's ECONNRESET is taken for a guest reset")
	}
	if !guestReset(guest) || hostReset(guest) {
		t.Error("gonet's reset is not a guest reset")
	}
}

// TestSizeBuffers: only largeBufferConns endpoints at a time keep tunable buffers; the
// rest are pinned at tcpBufferMax, and a released token can be taken again.
func TestSizeBuffers(t *testing.T) {
	n := newTestStack(t, nil)
	var releases []func()
	pinned := 0
	for i := 0; i < largeBufferConns+3; i++ {
		var wq waiter.Queue
		ep, err := n.s.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
		if err != nil {
			t.Fatal(err)
		}
		defer ep.Close()
		releases = append(releases, n.sizeBuffers(ep))
		if ep.SocketOptions().GetReceiveBufferSize() == tcpBufferMax && ep.SocketOptions().GetSendBufferSize() == tcpBufferMax {
			pinned++
		}
	}
	if pinned != 3 {
		t.Fatalf("%d endpoints pinned, want 3", pinned)
	}
	releases[0]()
	releases[0]() // idempotent
	var wq waiter.Queue
	ep, _ := n.s.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	defer ep.Close()
	n.sizeBuffers(ep)
	if ep.SocketOptions().GetReceiveBufferSize() == tcpBufferMax {
		t.Fatal("released token not reused")
	}
	if len(n.largeSem) != largeBufferConns {
		t.Fatalf("%d tokens taken", len(n.largeSem))
	}
}

// TestLinkModeIsFixed: once a stack has had an offload link, a plain one is refused
// (its TCP endpoints already send segments only an offload link can carry).
func TestLinkModeIsFixed(t *testing.T) {
	n := newTestStack(t, nil)
	a, a2 := net.Pipe()
	defer a2.Close()
	done := make(chan error, 1)
	go func() { done <- n.ServeOffload(a) }()
	var hello [frame.HeaderLen]byte
	if _, err := io.ReadFull(a2, hello[:]); err != nil || hello != frame.Hello {
		t.Fatalf("hello %x, %v", hello, err)
	}
	b, _ := net.Pipe()
	if err := n.ServeConn(b); !errors.Is(err, ErrLinkMode) {
		t.Fatalf("plain link after an offload one: %v, want ErrLinkMode", err)
	}
	a2.Close()
	<-done
}

// TestOffloadBadFrames: frames whose virtio_net_hdr the link does not accept are
// dropped, and a bad length ends the link.
func TestOffloadBadFrames(t *testing.T) {
	n := newTestStack(t, nil)
	host, guest := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- n.ServeOffload(host) }()
	var hello [frame.HeaderLen]byte
	if _, err := io.ReadFull(guest, hello[:]); err != nil {
		t.Fatal(err)
	}
	gwMAC := []byte(n.Config().GatewayMAC)
	eth := append(append(append([]byte{}, gwMAC...), 2, 0, 0, 0, 0, 9), 0x08, 0x00)
	for _, vh := range []frame.VnetHdr{
		{Flags: 0x40},                 // unknown flag
		{GSOType: 4, GSOSize: 1400},   // TCPv6
		{GSOType: frame.VnetGSOECN},   // ECN without GSO
		{GSOType: frame.VnetGSOTCPv4}, // GSO without a size
		{Flags: 1, CsumStart: 100},    // checksum outside the frame
		{GSOType: 3, GSOSize: 1400},   // UDP
	} {
		b := make([]byte, frame.VnetHdrLen)
		vh.Put(b)
		if _, err := guest.Write(frame.AppendCRC(nil, append(b, eth...))); err != nil {
			t.Fatal(err)
		}
	}
	guest.Write([]byte{0, 0, 0, 5})
	select {
	case err := <-done:
		if !errors.Is(err, frame.ErrFrameSize) {
			t.Fatalf("ServeOffload: %v, want ErrFrameSize", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeOffload did not return")
	}
	if s := n.Stats(); s.FramesDropped != 6 {
		t.Fatalf("FramesDropped = %d, want 6", s.FramesDropped)
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
