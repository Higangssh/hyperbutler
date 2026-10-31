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
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	nicID = 1
	// tcpBufferMax bounds the send and receive buffer of a relayed or published TCP
	// connection, so that a guest that opens many connections and never reads them
	// cannot grow this process without limit. tcpBufferLarge is the bound for the
	// largeBufferConns connections that may have larger buffers at any one time: under
	// load the link's RTT is a millisecond or more (the queues in front of the guest),
	// and with 256 KiB the window, not the CPU, capped a bulk transfer at 3-6 Gbit/s;
	// with 1 MiB it ran at 8-11 (ADR 0006). The buffers are used only as data queues.
	// Relayed connections: 16 x 2 x 1 MiB + 496 x 2 x 256 KiB = 280 MiB at most.
	tcpBufferMax     = 256 << 10
	tcpBufferLarge   = 1 << 20
	largeBufferConns = 16
	// timeWait is the TIME-WAIT period of guest connections. The guest reuses a port
	// only after its whole ephemeral range (28k ports), so 1 s is safe up to 28k
	// connections per second, and it bounds the endpoints in TIME-WAIT to one second's
	// worth of connections.
	timeWait         = time.Second
	maxPublishConns  = 256
	maxDNSTCPConns   = 16
	maxDNSTCPQueries = 64
	dnsTCPIdle       = 10 * time.Second
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
	// hgSem bounds host gateway TCP connections (MaxHostGatewayConns).
	hgSem chan struct{}
	// largeSem holds a token for each connection whose buffers may grow past tcpBufferMax.
	largeSem chan struct{}

	linkMu sync.Mutex
	link   *guestLink
	// mode is the framing of the guest link (linkUnknown until the first link), fixed
	// for the stack's life: TCP endpoints created while the link offloads segmentation
	// keep sending segments larger than the MTU.
	mode atomic.Int32

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
	if cfg.HostGateway != nil {
		virtual = append(virtual, cfg.HostGatewayAddr().String())
	}
	ep, err := tap.NewLinkEndpoint(false, uint32(cfg.MTU), cfg.GatewayMAC.String(), gw.String(), virtual)
	if err != nil {
		return nil, fmt.Errorf("netstack: link endpoint: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	pol := newPolicy(&cfg)
	n := &Stack{
		cfg:       cfg,
		ep:        ep,
		pol:       pol,
		dns:       &dnsHandler{res: cfg.Resolver, timeout: cfg.DNSTimeout, subnet: cfg.Subnet, hostGateway: pol.hostGatewayDNS},
		hgSem:     make(chan struct{}, cfg.MaxHostGatewayConns),
		log:       log,
		gwMAC:     tcpip.LinkAddress(cfg.GatewayMAC),
		ctx:       ctx,
		cancel:    cancel,
		tcpSem:    make(chan struct{}, cfg.MaxTCPConns),
		udpSem:    make(chan struct{}, cfg.MaxUDPFlows),
		dnsSem:    make(chan struct{}, cfg.MaxDNSInFlight),
		dnsTCPSem: make(chan struct{}, maxDNSTCPConns),
		largeSem:  make(chan struct{}, largeBufferConns),
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
	if err := s.CreateNIC(nicID, &gsoLink{LinkEndpoint: n.ep, n: n}); err != nil {
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
	// Buffers start at 128 KiB and are tuned by gVisor (the receive buffer by how fast
	// the relay reads it, the send buffer by the congestion window) up to
	// tcpBufferLarge. Only largeBufferConns connections at a time may grow that far
	// (see sizeBuffers); the others are pinned at tcpBufferMax.
	rcv := tcpip.TCPReceiveBufferSizeRangeOption{Min: 4 << 10, Default: 128 << 10, Max: tcpBufferLarge}
	_ = s.SetTransportProtocolOption(tcp.ProtocolNumber, &rcv)
	mod := tcpip.TCPModerateReceiveBufferOption(true)
	_ = s.SetTransportProtocolOption(tcp.ProtocolNumber, &mod)
	snd := tcpip.TCPSendBufferSizeRangeOption{Min: 4 << 10, Default: 128 << 10, Max: tcpBufferLarge}
	_ = s.SetTransportProtocolOption(tcp.ProtocolNumber, &snd)
	// TIME-WAIT guards against delayed duplicates of an old connection; this link has
	// none, its delays are microseconds. With gVisor's 60 s, a guest that opened short
	// connections held one endpoint per connection for a minute (the engine grew to
	// 180 MB at 640 connections/s), and once the guest reused a port, gVisor answered
	// the new SYN from the TIME-WAIT endpoint instead of handing it to the forwarder
	// (it only does that for a listener), so every new connection on that port failed.
	tw := tcpip.TCPTimeWaitTimeoutOption(timeWait)
	_ = s.SetTransportProtocolOption(tcp.ProtocolNumber, &tw)

	tf := tcp.NewForwarder(s, 0, n.cfg.MaxTCPInFlight, n.handleTCP)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tf.HandlePacket)
	uf := udp.NewForwarder(s, n.handleUDP)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, uf.HandlePacket)
	return nil
}

// Config returns the effective configuration (defaults applied).
func (n *Stack) Config() Config { return n.cfg }

// GVisorStats returns gVisor's own counters (for diagnostics).
func (n *Stack) GVisorStats() tcpip.Stats { return n.s.Stats() }

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
	tgt, err := n.pol.Route(dst)
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
	if tgt.Kind == TargetHostGateway {
		select {
		case n.hgSem <- struct{}{}:
		default:
			n.st.tcpRefused.Add(1)
			n.log.Debug("guest tcp refused: host gateway connection limit", "dst", dst)
			r.Complete(true)
			return
		}
		defer func() { <-n.hgSem }()
	}

	ctx, cancel := context.WithTimeout(n.ctx, n.cfg.DialTimeout)
	var out net.Conn
	if tgt.Kind == TargetHostGateway {
		out, err = n.dialHostGateway(ctx, "tcp", tgt.Addr.Port())
	} else {
		out, err = n.dialer(tgt.Kind == TargetForward).DialContext(ctx, "tcp", tgt.Addr.String())
	}
	cancel()
	if err != nil {
		n.st.tcpRefused.Add(1)
		n.log.Debug("guest tcp dial failed", "dst", tgt.Addr, "err", err)
		r.Complete(true)
		return
	}
	explicit := tgt.Kind != TargetOut
	if !explicit && selfConnected(out) {
		// The destination became a host address after the policy's address list was
		// read (a VPN connecting, a new DHCP lease): Windows connected us to ourselves.
		_ = out.Close()
		n.st.tcpDenied.Add(1)
		n.log.Debug("guest tcp denied after dial: host address", "dst", tgt.Addr)
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
	defer n.sizeBuffers(ep)()
	if explicit {
		setLocalBuffers(out)
	}
	n.st.tcpActive.Add(1)
	defer n.st.tcpActive.Add(-1)
	stop := context.AfterFunc(n.ctx, func() { _ = in.Close(); _ = out.Close() })
	defer stop()
	proxy(in, ep, out)
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

// proxy copies between the guest side (a gonet connection on endpoint gep) and the
// host side both ways, and propagates half-close. It returns when both directions are
// done or one fails, and closes both connections.
//
// A reset is passed on as a reset. Closed normally, the side that closes first keeps
// the connection in TIME_WAIT: a Windows socket for minutes, which a guest that opens
// and resets many connections to one destination turns into ephemeral port
// exhaustion, and a gVisor endpoint for timeWait, with its memory.
func proxy(guest *gonet.TCPConn, gep tcpip.Endpoint, host net.Conn) {
	done := make(chan error, 2)
	cp := func(dst, src net.Conn) {
		bp := copyBufs.Get().(*[]byte)
		_, err := io.CopyBuffer(writerOnly{dst}, readerOnly{src}, *bp)
		copyBufs.Put(bp)
		switch {
		case err == nil:
			if cw, ok := dst.(closeWriter); ok {
				err = cw.CloseWrite()
			}
		// The error may come from reading src or writing dst; the two sides' errors
		// are told apart by their kind.
		case guestReset(err):
			if tc, ok := host.(*net.TCPConn); ok {
				_ = tc.SetLinger(0)
			}
		case hostReset(err):
			gep.SocketOptions().SetLinger(tcpip.LingerOption{Enabled: true})
		}
		done <- err
	}
	a, b := net.Conn(guest), host
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

// copyBufs are the relay's copy buffers, reused across connections. io.Copy between a
// Windows socket and gonet allocated two fresh 32 KiB buffers per connection; at 5000
// published connections per second that was 320 MB/s of garbage, and the engine's heap
// stood at 240 MB.
var copyBufs = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}

// writerOnly and readerOnly hide ReadFrom and WriteTo, so that io.CopyBuffer uses the
// pooled buffer instead of the net package's own.
type writerOnly struct{ io.Writer }
type readerOnly struct{ io.Reader }

// resetText is how gonet reports a connection reset (it keeps only the text of
// gVisor's error).
var resetText = (&tcpip.ErrConnectionReset{}).String()

// guestReset reports whether err, from reading the guest side, is a reset: the
// innermost error is gonet's, with exactly gVisor's text.
func guestReset(err error) bool {
	for ; err != nil; err = errors.Unwrap(err) {
		if errors.Unwrap(err) == nil {
			return err.Error() == resetText
		}
	}
	return false
}

// --- UDP ------------------------------------------------------------------------------

func (n *Stack) handleUDP(r *udp.ForwarderRequest) bool {
	id := r.ID()
	dst := netip.AddrPortFrom(netipAddr(id.LocalAddress), id.LocalPort)
	// HostForwards are TCP only; the host gateway forwards UDP too (to 127.0.0.1 only:
	// a UDP "connect" cannot tell whether anything listens, so there is no [::1]
	// fallback).
	tgt, err := n.pol.Route(dst)
	if err != nil || tgt.Kind == TargetForward || tgt.Addr.Port() == 0 {
		n.st.udpDenied.Add(1)
		return false // ICMP port unreachable
	}
	select {
	case n.udpSem <- struct{}{}:
	default:
		n.st.udpDenied.Add(1)
		return true // drop silently: the flow table is full
	}
	var out net.Conn
	if tgt.Kind == TargetHostGateway {
		out, err = n.dialHostGateway(n.ctx, "udp", tgt.Addr.Port())
	} else {
		out, err = n.dialer(false).DialContext(n.ctx, "udp", tgt.Addr.String())
	}
	if err != nil {
		<-n.udpSem
		return false
	}
	if tgt.Kind == TargetOut && selfConnected(out) {
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
				g, gep, release, err := n.dialGuest(ctx, guest)
				cancel()
				if err != nil {
					_ = c.Close()
					return
				}
				defer release()
				stop := context.AfterFunc(n.ctx, func() { _ = c.Close(); _ = g.Close() })
				defer stop()
				setLocalBuffers(c)
				proxy(g, gep, c)
			}()
		}
	}()
	return ln, nil
}

// dialGuest connects to a guest TCP port, as gonet.DialContextTCP does, but sizes the
// endpoint's buffers (sizeBuffers) before the handshake, so that the window scale it
// offers covers them. Call release when the connection is done.
func (n *Stack) dialGuest(ctx context.Context, addr tcpip.FullAddress) (*gonet.TCPConn, tcpip.Endpoint, func(), error) {
	var wq waiter.Queue
	ep, terr := n.s.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if terr != nil {
		return nil, nil, nil, errors.New(terr.String())
	}
	release := n.sizeBuffers(ep)
	fail := func(err error) (*gonet.TCPConn, tcpip.Endpoint, func(), error) {
		ep.Close()
		release()
		return nil, nil, nil, err
	}
	entry, ready := waiter.NewChannelEntry(waiter.WritableEvents)
	wq.EventRegister(&entry)
	defer wq.EventUnregister(&entry)
	terr = ep.Connect(addr)
	if _, ok := terr.(*tcpip.ErrConnectStarted); ok {
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-ready:
		}
		terr = ep.LastError()
	}
	if terr != nil {
		return fail(fmt.Errorf("netstack: connect to the guest: %s", terr))
	}
	return gonet.NewTCPConn(&wq, ep), ep, release, nil
}

// sizeBuffers lets ep's buffers be tuned up to tcpBufferLarge if one of the
// largeBufferConns tokens is free, and pins them at tcpBufferMax otherwise (setting a
// size turns gVisor's tuning off for that endpoint). The returned function gives the
// token back.
func (n *Stack) sizeBuffers(ep tcpip.Endpoint) (release func()) {
	select {
	case n.largeSem <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-n.largeSem }) }
	default:
	}
	ops := ep.SocketOptions()
	ops.SetReceiveBufferSize(tcpBufferMax, true)
	ops.SetSendBufferSize(tcpBufferMax, true)
	return func() {}
}

// --- helpers --------------------------------------------------------------------------

// localSocketBuffer is the Windows socket send and receive buffer for connections
// between this process and another program on this host: published ports and explicit
// host forwards. With the default (autotuned) buffers a loopback connection was the
// slowest part of the path: bulk transfers through the stack ran at 2-4 Gbit/s while
// the relay goroutine waited on the socket, and at 9-11 Gbit/s with 1 MiB. Connections
// to other machines keep Windows' autotuning, which a fixed size would cap on long
// paths. The memory is the kernel's and is used only as data queues.
const localSocketBuffer = 1 << 20

// setLocalBuffers sets the buffers of c if its peer is on loopback; a `-publish-lan`
// client or a host forward to another machine keeps autotuning.
func setLocalBuffers(c net.Conn) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return
	}
	if a, ok := addrOf(c.RemoteAddr()); !ok || !a.IsLoopback() {
		return
	}
	_ = tc.SetReadBuffer(localSocketBuffer)
	_ = tc.SetWriteBuffer(localSocketBuffer)
}

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
