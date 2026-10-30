// Package netstack is hyperbutler's user-mode guest network (ADR 0006): a gVisor TCP/IP stack
// in the hyperbutler process, reached by the guest over one hvsock stream that carries
// Ethernet frames (see package frame and guest/cmd/netfwd).
//
// It is built from gvisor-tap-vsock's pieces rather than its VirtualNetwork, because
// hyperbutler needs things VirtualNetwork has no hooks for:
//   - Every guest connection is checked against a Policy before the host dials it, so the
//     guest cannot reach this host's own services (loopback, LAN and VPN addresses of the
//     host, link-local and metadata addresses) unless a HostForward allows it.
//   - DNS goes through the host resolver for every record type it can ask for, with
//     bounded concurrency and timeouts, so VPN split DNS keeps working.
//   - The guest link reader rejects runt and oversized frames instead of trusting them.
//   - Connection counts and UDP flows are capped.
//
// gvisor-tap-vsock's tap.LinkEndpoint (the gateway NIC, including its ARP anti-spoofing)
// is used as is, and the forwarders follow its design.
//
// There is no DHCP server: the guest's address, route and resolver are static and passed
// on the kernel command line.
package netstack

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/tap"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/Higangssh/hyperbutler/internal/netstack/frame"
)

const (
	nicID = 1
	// tcpBufferMax bounds each guest TCP endpoint's send and receive buffer. The link to
	// the guest has microsecond RTTs, so large windows buy nothing, and the bound keeps a
	// guest that opens many connections and never reads them from growing this process
	// without limit: MaxTCPConns x 2 x tcpBufferMax, 256 MiB with the defaults. At the
	// link's RTT, 256 KiB still covers well over 10 Gbit/s.
	tcpBufferMax       = 256 << 10
	maxPublishConns    = 256
	maxDNSTCPConns     = 16
	maxDNSTCPQueries   = 64
	dnsTCPIdle         = 10 * time.Second
	linkReadBufferSize = 256 << 10
)

// Stats are counters for diagnostics and tests.
type Stats struct {
	FramesIn, FramesOut, FramesDropped uint64
	TCPDenied, UDPDenied, TCPRefused   uint64
	TCPActive, UDPActive               int64
	DNSQueries, DNSDropped             uint64
	TCPSegmentsSent, TCPRetransmits    uint64
	TCPTimeouts                        uint64
}

// Stack is one guest network.
type Stack struct {
	cfg Config
	s   *stack.Stack
	ep  *tap.LinkEndpoint
	pol *Policy
	dns *dnsHandler
	log *slog.Logger

	gwMAC tcpip.LinkAddress

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// closeMu orders wg.Add (under RLock, only while !closing) before Close's wg.Wait.
	closeMu sync.RWMutex
	closing bool

	tcpSem, udpSem, dnsSem, dnsTCPSem chan struct{}

	linkMu sync.Mutex
	link   *guestLink

	st struct {
		framesIn, framesOut, framesDropped atomic.Uint64
		tcpDenied, udpDenied, tcpRefused   atomic.Uint64
		tcpActive, udpActive               atomic.Int64
		dnsQueries, dnsDropped             atomic.Uint64
	}
}

// New builds the stack. It does nothing on the network until ServeConn is given a guest
// link.
func New(cfg Config, log *slog.Logger) (*Stack, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Resolver == nil {
		cfg.Resolver = net.DefaultResolver
	}
	gw := cfg.Gateway()
	var virtual []string
	for _, h := range cfg.HostForwards {
		virtual = append(virtual, h.Virtual.Addr().String())
	}
	ep, err := tap.NewLinkEndpoint(false, uint32(cfg.MTU), cfg.GatewayMAC.String(), gw.String(), virtual)
	if err != nil {
		return nil, fmt.Errorf("netstack: link endpoint: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	n := &Stack{
		cfg:       cfg,
		ep:        ep,
		pol:       newPolicy(&cfg),
		dns:       &dnsHandler{res: cfg.Resolver, timeout: cfg.DNSTimeout},
		log:       log,
		gwMAC:     tcpip.LinkAddress(cfg.GatewayMAC),
		ctx:       ctx,
		cancel:    cancel,
		tcpSem:    make(chan struct{}, cfg.MaxTCPConns),
		udpSem:    make(chan struct{}, cfg.MaxUDPFlows),
		dnsSem:    make(chan struct{}, cfg.MaxDNSInFlight),
		dnsTCPSem: make(chan struct{}, maxDNSTCPConns),
	}
	ep.Connect(n)
	if err := n.buildStack(); err != nil {
		cancel()
		return nil, err
	}
	if err := n.startDNS(); err != nil {
		n.Close()
		return nil, err
	}
	return n, nil
}

func (n *Stack) buildStack() error {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	n.s = s
	if err := s.CreateNIC(nicID, n.ep); err != nil {
		return fmt.Errorf("netstack: create NIC: %s", err)
	}
	gw := tcpipAddr(n.cfg.Gateway())
	if err := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: gw.WithPrefix(),
	}, stack.AddressProperties{}); err != nil {
		return fmt.Errorf("netstack: add gateway address: %s", err)
	}
	// Promiscuous and spoofing: the stack accepts packets for any destination (that is
	// how it terminates the guest's connections to the internet) and answers from that
	// destination. IP forwarding stays off, so it never routes packets on.
	if err := s.SetPromiscuousMode(nicID, true); err != nil {
		return fmt.Errorf("netstack: promiscuous mode: %s", err)
	}
	if err := s.SetSpoofing(nicID, true); err != nil {
		return fmt.Errorf("netstack: spoofing: %s", err)
	}
	subnet, err := tcpip.NewSubnet(tcpipAddr(n.cfg.Subnet.Masked().Addr()), tcpip.MaskFromBytes(net.CIDRMask(n.cfg.Subnet.Bits(), 32)))
	if err != nil {
		return fmt.Errorf("netstack: subnet: %v", err)
	}
	// Replies to addresses outside the subnet (containers behind the guest's docker0
	// bridge, which the guest routes without NAT) go to the guest.
	s.SetRouteTable([]tcpip.Route{
		{Destination: subnet, NIC: nicID},
		{Destination: header.IPv4EmptySubnet, Gateway: tcpipAddr(n.cfg.GuestIP()), NIC: nicID},
	})
	sack := tcpip.TCPSACKEnabled(true)
	_ = s.SetTransportProtocolOption(tcp.ProtocolNumber, &sack)
	// RACK (and its tail loss probe) times loss from the measured RTT. On this link the
	// RTT is tens of microseconds until the guest-bound queue fills, so RACK declared
	// in-flight segments lost and the guest saw each one about six times
	// (TCPDSACKOldSent), capping host-to-guest TCP at ~0.2 Gbit/s. The link is lossless
	// and in order: classic duplicate-ACK recovery is enough.
	recovery := tcpip.TCPRecovery(0)
	_ = s.SetTransportProtocolOption(tcp.ProtocolNumber, &recovery)
	rcv := tcpip.TCPReceiveBufferSizeRangeOption{Min: 4 << 10, Default: 128 << 10, Max: tcpBufferMax}
	_ = s.SetTransportProtocolOption(tcp.ProtocolNumber, &rcv)
	snd := tcpip.TCPSendBufferSizeRangeOption{Min: 4 << 10, Default: 128 << 10, Max: tcpBufferMax}
	_ = s.SetTransportProtocolOption(tcp.ProtocolNumber, &snd)

	tf := tcp.NewForwarder(s, 0, n.cfg.MaxTCPInFlight, n.handleTCP)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tf.HandlePacket)
	uf := udp.NewForwarder(s, n.handleUDP)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, uf.HandlePacket)
	return nil
}

// Config returns the effective configuration (defaults applied).
func (n *Stack) Config() Config { return n.cfg }

// Stats returns a snapshot of the counters.
func (n *Stack) Stats() Stats {
	return Stats{
		FramesIn: n.st.framesIn.Load(), FramesOut: n.st.framesOut.Load(), FramesDropped: n.st.framesDropped.Load(),
		TCPDenied: n.st.tcpDenied.Load(), UDPDenied: n.st.udpDenied.Load(), TCPRefused: n.st.tcpRefused.Load(),
		TCPActive: n.st.tcpActive.Load(), UDPActive: n.st.udpActive.Load(),
		DNSQueries: n.st.dnsQueries.Load(), DNSDropped: n.st.dnsDropped.Load(),
		TCPSegmentsSent: n.s.Stats().TCP.SegmentsSent.Value(), TCPRetransmits: n.s.Stats().TCP.Retransmits.Value(),
		TCPTimeouts: n.s.Stats().TCP.Timeouts.Value(),
	}
}

// Close stops the stack and every connection it relays.
func (n *Stack) Close() {
	n.closeMu.Lock()
	n.closing = true
	n.closeMu.Unlock()
	n.cancel()
	n.linkMu.Lock()
	if n.link != nil {
		n.link.close()
	}
	n.linkMu.Unlock()
	if n.s != nil {
		n.s.Close()
	}
	n.wg.Wait()
	if n.s != nil {
		n.s.Wait()
	}
}

// track registers k goroutines with Close's wait group. It returns false once Close has
// begun, and the caller must then not start them.
func (n *Stack) track(k int) bool {
	n.closeMu.RLock()
	defer n.closeMu.RUnlock()
	if n.closing {
		return false
	}
	n.wg.Add(k)
	return true
}

// --- guest link ---------------------------------------------------------------------

// guestLink queues frames for the guest and writes them in batches: under load one
// hvsock write carries many frames, which is what makes the host-to-guest direction
// (image pulls) fast. Producers block while maxPending bytes are queued, so a guest that
// stops reading slows the stack down instead of growing the queue.
type guestLink struct {
	conn    net.Conn
	mu      sync.Mutex
	cond    sync.Cond
	pending []byte
	spare   []byte
	closed  bool
}

const maxPending = 1 << 20

func newGuestLink(conn net.Conn) *guestLink {
	l := &guestLink{conn: conn}
	l.cond.L = &l.mu
	return l
}

// enqueue appends one frame. It returns false once the link is closed.
func (l *guestLink) enqueue(pkt *stack.PacketBuffer, size int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for len(l.pending) >= maxPending && !l.closed {
		l.cond.Wait()
	}
	if l.closed {
		return false
	}
	var h [frame.HeaderLen]byte
	frame.PutHeader(h[:], size)
	l.pending = append(l.pending, h[:]...)
	for _, s := range pkt.AsSlices() {
		l.pending = append(l.pending, s...)
	}
	l.cond.Broadcast()
	return true
}

// writeLoop writes queued frames until the link closes or a write fails.
func (l *guestLink) writeLoop() {
	for {
		l.mu.Lock()
		for len(l.pending) == 0 && !l.closed {
			l.cond.Wait()
		}
		if l.closed {
			l.mu.Unlock()
			return
		}
		buf := l.pending
		l.pending = l.spare[:0]
		l.cond.Broadcast()
		l.mu.Unlock()

		_, err := l.conn.Write(buf)

		l.mu.Lock()
		if cap(buf) <= 4*maxPending {
			l.spare = buf[:0]
		}
		l.mu.Unlock()
		if err != nil {
			l.close()
			return
		}
	}
}

func (l *guestLink) close() {
	l.mu.Lock()
	l.closed = true
	l.cond.Broadcast()
	l.mu.Unlock()
	_ = l.conn.Close()
}

// ServeConn runs the guest link over conn until it fails or the stack is closed. Only
// one link is active: a new ServeConn replaces (and closes) the previous one, which is
// what a restarted guest forwarder needs.
func (n *Stack) ServeConn(conn net.Conn) error {
	l := newGuestLink(conn)
	n.linkMu.Lock()
	if n.ctx.Err() != nil {
		n.linkMu.Unlock()
		_ = conn.Close()
		return n.ctx.Err()
	}
	old := n.link
	n.link = l
	n.linkMu.Unlock()
	if old != nil {
		old.close()
	}
	writerDone := make(chan struct{})
	go func() { defer close(writerDone); l.writeLoop() }()
	defer func() {
		n.linkMu.Lock()
		if n.link == l {
			n.link = nil
		}
		n.linkMu.Unlock()
		l.close()
		<-writerDone
	}()
	stop := context.AfterFunc(n.ctx, l.close)
	defer stop()

	fr, err := frame.NewReader(newBufReader(conn), n.cfg.MTU)
	if err != nil {
		return err
	}
	for {
		b, err := fr.Next()
		if err != nil {
			if n.ctx.Err() != nil {
				return n.ctx.Err()
			}
			return err
		}
		n.deliver(b)
	}
}

// deliver hands one guest frame to the stack. Only frames for the gateway (or
// broadcast) carrying IPv4 or ARP are accepted.
func (n *Stack) deliver(b []byte) {
	if !n.acceptFrame(b) {
		n.st.framesDropped.Add(1)
		return
	}
	n.st.framesIn.Add(1)
	eth := header.Ethernet(b)
	data := buffer.MakeWithData(b[header.EthernetMinimumSize:])
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: data})
	n.ep.DeliverNetworkPacket(eth.Type(), pkt)
	pkt.DecRef()
}

func (n *Stack) acceptFrame(b []byte) bool {
	if len(b) < header.EthernetMinimumSize || len(b) > frame.MaxFrameLen(n.cfg.MTU) {
		return false
	}
	eth := header.Ethernet(b)
	dst := eth.DestinationAddress()
	if dst != n.gwMAC && dst != header.EthernetBroadcastAddress {
		return false
	}
	if src := eth.SourceAddress(); src == n.gwMAC || header.IsMulticastEthernetAddress(src) {
		return false
	}
	switch eth.Type() {
	case header.IPv4ProtocolNumber, header.ARPProtocolNumber:
		return true
	}
	return false
}

// DeliverNetworkPacket implements tap.NetworkSwitch: the stack sends a frame to the
// guest.
func (n *Stack) DeliverNetworkPacket(_ tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) {
	n.linkMu.Lock()
	l := n.link
	n.linkMu.Unlock()
	if l == nil {
		return
	}
	size := pkt.Size()
	if size > frame.MaxFrameLen(n.cfg.MTU) {
		n.st.framesDropped.Add(1)
		return
	}
	if l.enqueue(pkt, size) {
		n.st.framesOut.Add(1)
	}
}

// --- TCP ------------------------------------------------------------------------------

func (n *Stack) handleTCP(r *tcp.ForwarderRequest) {
	// gVisor runs this on its own goroutine; register it before doing anything.
	if !n.track(1) {
		r.Complete(true)
		return
	}
	defer n.wg.Done()
	id := r.ID()
	dst := netip.AddrPortFrom(netipAddr(id.LocalAddress), id.LocalPort)
	target, explicit, err := n.pol.Route(dst)
	if err != nil {
		n.st.tcpDenied.Add(1)
		n.log.Debug("guest tcp denied", "dst", dst, "err", err)
		r.Complete(true)
		return
	}
	select {
	case n.tcpSem <- struct{}{}:
	default:
		n.st.tcpRefused.Add(1)
		r.Complete(true)
		return
	}
	defer func() { <-n.tcpSem }()

	ctx, cancel := context.WithTimeout(n.ctx, n.cfg.DialTimeout)
	out, err := n.dialer(explicit).DialContext(ctx, "tcp", target.String())
	cancel()
	if err != nil {
		n.st.tcpRefused.Add(1)
		n.log.Debug("guest tcp dial failed", "dst", target, "err", err)
		r.Complete(true)
		return
	}
	if !explicit && selfConnected(out) {
		// The destination became a host address after the policy's address list was
		// read (a VPN connecting, a new DHCP lease): Windows connected us to ourselves.
		_ = out.Close()
		n.st.tcpDenied.Add(1)
		n.log.Debug("guest tcp denied after dial: host address", "dst", target)
		r.Complete(true)
		return
	}
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	r.Complete(false)
	if terr != nil {
		_ = out.Close()
		return
	}
	in := gonet.NewTCPConn(&wq, ep)
	n.st.tcpActive.Add(1)
	defer n.st.tcpActive.Add(-1)
	stop := context.AfterFunc(n.ctx, func() { _ = in.Close(); _ = out.Close() })
	defer stop()
	proxy(in, out)
}

// dialer returns a dialer that re-checks the policy on the address actually connected
// to, so nothing between Route and connect can widen it. Explicit host forwards are
// exempt: their targets are this host by design.
func (n *Stack) dialer(explicit bool) *net.Dialer {
	d := &net.Dialer{KeepAlive: 30 * time.Second}
	if !explicit {
		d.Control = func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return err
			}
			return n.pol.Check(ap.Addr())
		}
	}
	return d
}

// selfConnected reports whether a dialed connection ended up between two addresses of
// this host, which is how a connection to one of the host's own non-loopback addresses
// looks on Windows. It closes the window in which Policy's cached address list is stale.
func selfConnected(c net.Conn) bool {
	l, lok := addrOf(c.LocalAddr())
	r, rok := addrOf(c.RemoteAddr())
	return lok && rok && l == r
}

func addrOf(a net.Addr) (netip.Addr, bool) {
	var ip net.IP
	switch v := a.(type) {
	case *net.TCPAddr:
		ip = v.IP
	case *net.UDPAddr:
		ip = v.IP
	default:
		return netip.Addr{}, false
	}
	na, ok := netip.AddrFromSlice(ip)
	return na.Unmap(), ok
}

type closeWriter interface{ CloseWrite() error }

// proxy copies both ways and propagates half-close. It returns when both directions
// are done or one fails, and closes both connections.
func proxy(a, b net.Conn) {
	done := make(chan error, 2)
	cp := func(dst, src net.Conn) {
		_, err := io.Copy(dst, src)
		if err == nil {
			if cw, ok := dst.(closeWriter); ok {
				err = cw.CloseWrite()
			}
		}
		done <- err
	}
	go cp(a, b)
	go cp(b, a)
	if err := <-done; err != nil {
		_ = a.Close()
		_ = b.Close()
	}
	<-done
	_ = a.Close()
	_ = b.Close()
}

// --- UDP ------------------------------------------------------------------------------

func (n *Stack) handleUDP(r *udp.ForwarderRequest) bool {
	id := r.ID()
	dst := netip.AddrPortFrom(netipAddr(id.LocalAddress), id.LocalPort)
	if err := n.pol.Check(dst.Addr()); err != nil || dst.Port() == 0 {
		n.st.udpDenied.Add(1)
		return false // ICMP port unreachable
	}
	select {
	case n.udpSem <- struct{}{}:
	default:
		n.st.udpDenied.Add(1)
		return true // drop silently: the flow table is full
	}
	out, err := n.dialer(false).DialContext(n.ctx, "udp", dst.String())
	if err != nil {
		<-n.udpSem
		return false
	}
	if selfConnected(out) {
		_ = out.Close()
		<-n.udpSem
		n.st.udpDenied.Add(1)
		return false
	}
	if !n.track(1) {
		_ = out.Close()
		<-n.udpSem
		return true
	}
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		_ = out.Close()
		<-n.udpSem
		n.wg.Done()
		return true
	}
	in := gonet.NewUDPConn(&wq, ep)
	n.st.udpActive.Add(1)
	go func() {
		defer n.wg.Done()
		defer func() { <-n.udpSem }()
		defer n.st.udpActive.Add(-1)
		n.udpFlow(in, out)
	}()
	return true
}

// udpFlow relays datagrams both ways until neither side has sent anything for
// UDPIdleTimeout.
func (n *Stack) udpFlow(in, out net.Conn) {
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	idle := n.cfg.UDPIdleTimeout
	stop := context.AfterFunc(n.ctx, func() { _ = in.Close(); _ = out.Close() })
	defer stop()
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		buf := make([]byte, 64<<10)
		for {
			_ = src.SetReadDeadline(time.Now().Add(idle))
			k, err := src.Read(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() &&
					time.Since(time.Unix(0, last.Load())) < idle {
					continue
				}
				_ = in.Close()
				_ = out.Close()
				return
			}
			last.Store(time.Now().UnixNano())
			_, _ = dst.Write(buf[:k])
		}
	}
	wg.Add(2)
	go cp(out, in)
	cp(in, out)
	wg.Wait()
}

// --- DNS ------------------------------------------------------------------------------

func (n *Stack) startDNS() error {
	gw := tcpip.FullAddress{NIC: nicID, Addr: tcpipAddr(n.cfg.Gateway()), Port: 53}
	uc, err := gonet.DialUDP(n.s, &gw, nil, ipv4.ProtocolNumber)
	if err != nil {
		return fmt.Errorf("netstack: dns udp: %w", err)
	}
	tl, err := gonet.ListenTCP(n.s, gw, ipv4.ProtocolNumber)
	if err != nil {
		_ = uc.Close()
		return fmt.Errorf("netstack: dns tcp: %w", err)
	}
	context.AfterFunc(n.ctx, func() { _ = uc.Close(); _ = tl.Close() })
	n.wg.Add(2)
	go func() { defer n.wg.Done(); n.serveDNSUDP(uc) }()
	go func() { defer n.wg.Done(); n.serveDNSTCP(tl) }()
	return nil
}

func (n *Stack) serveDNSUDP(pc net.PacketConn) {
	buf := make([]byte, 64<<10)
	for {
		k, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if n.ctx.Err() != nil {
				return
			}
			continue
		}
		n.st.dnsQueries.Add(1)
		select {
		case n.dnsSem <- struct{}{}:
		default:
			n.st.dnsDropped.Add(1)
			continue
		}
		q := append([]byte(nil), buf[:k]...)
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			defer func() { <-n.dnsSem }()
			if resp := n.dns.answer(n.ctx, q, true); resp != nil {
				_, _ = pc.WriteTo(resp, addr)
			}
		}()
	}
}

func (n *Stack) serveDNSTCP(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if n.ctx.Err() != nil {
				return
			}
			continue
		}
		select {
		case n.dnsTCPSem <- struct{}{}:
		default:
			_ = c.Close()
			continue
		}
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			defer func() { <-n.dnsTCPSem }()
			n.serveDNSTCPConn(c)
		}()
	}
}

func (n *Stack) serveDNSTCPConn(c net.Conn) {
	defer c.Close()
	stop := context.AfterFunc(n.ctx, func() { _ = c.Close() })
	defer stop()
	var hdr [2]byte
	buf := make([]byte, 64<<10)
	for i := 0; i < maxDNSTCPQueries; i++ {
		_ = c.SetDeadline(time.Now().Add(dnsTCPIdle))
		if _, err := io.ReadFull(c, hdr[:]); err != nil {
			return
		}
		l := int(hdr[0])<<8 | int(hdr[1])
		if l < 12 {
			return
		}
		if _, err := io.ReadFull(c, buf[:l]); err != nil {
			return
		}
		n.st.dnsQueries.Add(1)
		select {
		case n.dnsSem <- struct{}{}:
		default:
			n.st.dnsDropped.Add(1)
			return
		}
		resp := n.dns.answer(n.ctx, buf[:l], false)
		<-n.dnsSem
		if resp == nil {
			return
		}
		out := make([]byte, 2+len(resp))
		out[0], out[1] = byte(len(resp)>>8), byte(len(resp))
		copy(out[2:], resp)
		if _, err := c.Write(out); err != nil {
			return
		}
	}
}

// --- published ports --------------------------------------------------------------------

// Publish listens on p.Host and relays each connection to the guest's p.GuestPort. Close
// the result to stop listening; connections already relayed keep running until either
// side closes or the stack does.
func (n *Stack) Publish(p Publish) (io.Closer, error) {
	ln, err := net.Listen("tcp", p.Host.String())
	if err != nil {
		return nil, fmt.Errorf("netstack: publish %s: %w", p, err)
	}
	sem := make(chan struct{}, maxPublishConns)
	guest := tcpip.FullAddress{NIC: nicID, Addr: tcpipAddr(n.cfg.GuestIP()), Port: p.GuestPort}
	if !n.track(1) {
		_ = ln.Close()
		return nil, errors.New("netstack: stack is closed")
	}
	stop := context.AfterFunc(n.ctx, func() { _ = ln.Close() })
	go func() {
		defer n.wg.Done()
		defer stop()
		for {
			c, err := ln.Accept()
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					continue
				}
				return
			}
			select {
			case sem <- struct{}{}:
			default:
				_ = c.Close()
				continue
			}
			n.wg.Add(1)
			go func() {
				defer n.wg.Done()
				defer func() { <-sem }()
				ctx, cancel := context.WithTimeout(n.ctx, 5*time.Second)
				g, err := gonet.DialContextTCP(ctx, n.s, guest, ipv4.ProtocolNumber)
				cancel()
				if err != nil {
					_ = c.Close()
					return
				}
				stop := context.AfterFunc(n.ctx, func() { _ = c.Close(); _ = g.Close() })
				defer stop()
				proxy(c, g)
			}()
		}
	}()
	return ln, nil
}

// --- helpers --------------------------------------------------------------------------

func newBufReader(r io.Reader) *bufio.Reader { return bufio.NewReaderSize(r, linkReadBufferSize) }

func tcpipAddr(a netip.Addr) tcpip.Address {
	b := a.Unmap().As4()
	return tcpip.AddrFrom4(b)
}

func netipAddr(a tcpip.Address) netip.Addr {
	if a.Len() == 4 {
		return netip.AddrFrom4(a.As4())
	}
	if a.Len() == 16 {
		return netip.AddrFrom16(a.As16())
	}
	return netip.Addr{}
}
