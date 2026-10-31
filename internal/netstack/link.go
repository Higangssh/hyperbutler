package netstack

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/tap"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/Higangssh/hyperbutler/internal/netstack/frame"
)

// Link modes (Stack.mode).
const (
	linkUnknown = iota
	linkPlain   // ServeConn: length-prefixed Ethernet frames of at most the MTU
	linkVnet    // ServeOffload: frames with a virtio_net_hdr (segmentation offload)
)

// ErrLinkMode is returned for a guest link whose framing differs from the stack's
// first link.
var ErrLinkMode = errors.New("netstack: guest link framing differs from the first link")

// gsoLink is the stack's NIC: gvisor-tap-vsock's link endpoint, plus segmentation
// offload once the guest link is in offload mode. TCP endpoints then hand the link
// segments of up to frame.GSOMaxSize with only the pseudo-header checksum (gVisor's
// "host GSO"), and the guest's TAP device takes them as they are, the way a virtio NIC
// passes a large receive to the kernel: one frame and no checksum pass per 64 KiB
// instead of one per MSS. Whether an endpoint uses it is decided when the endpoint is
// created, which is why the mode never changes once set.
type gsoLink struct {
	*tap.LinkEndpoint
	n *Stack
}

// GSOMaxSize implements stack.GSOEndpoint.
func (l *gsoLink) GSOMaxSize() uint32 { return frame.GSOMaxSize }

// SupportedGSO implements stack.GSOEndpoint.
func (l *gsoLink) SupportedGSO() stack.SupportedGSO {
	if l.n.mode.Load() == linkVnet {
		return stack.HostGSOSupported
	}
	return stack.GSONotSupported
}

// guestLink queues frames for the guest and writes them in batches: under load one
// hvsock write carries many frames, which is what makes the host-to-guest direction
// (image pulls) fast. Producers block while maxPending bytes are queued, so a guest that
// stops reading slows the stack down instead of growing the queue.
type guestLink struct {
	conn    net.Conn
	vnet    bool
	mu      sync.Mutex
	cond    sync.Cond
	pending []byte
	spare   []byte
	closed  bool
}

const maxPending = 1 << 20

func newGuestLink(conn net.Conn, vnet bool) *guestLink {
	l := &guestLink{conn: conn, vnet: vnet}
	l.cond.L = &l.mu
	return l
}

// enqueue appends one frame (with vh in offload mode). It returns false once the link
// is closed.
func (l *guestLink) enqueue(pkt *stack.PacketBuffer, size int, vh *frame.VnetHdr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for len(l.pending) >= maxPending && !l.closed {
		l.cond.Wait()
	}
	if l.closed {
		return false
	}
	var h [frame.HeaderLen + frame.VnetHdrLen]byte
	if l.vnet {
		frame.PutHeader(h[:], frame.VnetHdrLen+size)
		vh.Put(h[frame.HeaderLen:])
		l.pending = append(l.pending, h[:]...)
	} else {
		frame.PutHeader(h[:], size)
		l.pending = append(l.pending, h[:frame.HeaderLen]...)
	}
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
		// Keep one spare buffer of about the queue bound, not a larger one a burst grew.
		if cap(buf) <= maxPending+frame.MaxVnetFrameLen+frame.HeaderLen {
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

// ServeConn runs a plain guest link over conn, one the guest forwarder dialed (netfwd
// without -vnet), until it fails or the stack is closed. Only one link is active: a new
// link replaces (and closes) the previous one, which is what a restarted guest
// forwarder needs. Every link of a stack must use the same framing.
func (n *Stack) ServeConn(conn net.Conn) error { return n.serve(conn, linkPlain) }

// ServeOffload runs an offload-mode guest link over conn, a connection this host
// dialed to the guest forwarder (netfwd -vnet): it sends frame.Hello, then frames with
// a virtio_net_hdr in both directions. Otherwise it behaves as ServeConn.
//
// The host dials because Windows sends slowly on an accepted hvsock connection: on the
// same boot, host-to-guest TCP through a link the guest dialed ran at 2.0-2.6 Gbit/s
// with the host's writes taking 1 ms per 600 KB, and at 7-11 Gbit/s on one the host
// dialed (ADR 0006, "Throughput").
func (n *Stack) ServeOffload(conn net.Conn) error { return n.serve(conn, linkVnet) }

// Offloading reports whether the stack's guest link is in offload mode, and whether
// it has had a link at all.
func (n *Stack) Offloading() (offload, known bool) {
	m := n.mode.Load()
	return m == linkVnet, m != linkUnknown
}

func (n *Stack) serve(conn net.Conn, mode int32) error {
	stopConn := context.AfterFunc(n.ctx, func() { _ = conn.Close() })
	defer stopConn()
	if !n.mode.CompareAndSwap(linkUnknown, mode) && n.mode.Load() != mode {
		_ = conn.Close()
		return ErrLinkMode
	}
	if mode == linkVnet {
		// Before the link replaces the previous one: a guest that does not read
		// leaves the old link in place, and the write is bounded.
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, err := conn.Write(frame.Hello[:])
		_ = conn.SetWriteDeadline(time.Time{})
		if err != nil {
			_ = conn.Close()
			return err
		}
	}
	l := newGuestLink(conn, mode == linkVnet)
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

	// The link is read ahead on its own goroutine (frame.Pump), so the guest's writes
	// keep flowing while the stack handles the frames already read.
	var cr *frame.ChunkReader
	handle := func(b []byte) error { n.deliver(b, frame.MaxFrameLen(n.cfg.MTU)); return nil }
	if l.vnet {
		cr = frame.NewChunkReader(conn, frame.MinVnetFrameLen, frame.MaxVnetFrameLen)
		handle = func(b []byte) error { n.deliverVnet(b); return nil }
	} else {
		cr = frame.NewChunkReader(conn, frame.EthernetHeaderLen, frame.MaxFrameLen(n.cfg.MTU))
	}
	err := frame.Pump(cr, linkChunks, linkChunk, handle, l.close)
	if n.ctx.Err() != nil {
		return n.ctx.Err()
	}
	return err
}

// The guest link is read in chunks of up to linkChunk bytes, linkChunks ahead.
const (
	linkChunk  = 256 << 10
	linkChunks = 4
)

// deliverVnet hands one offload-mode frame to the stack.
func (n *Stack) deliverVnet(b []byte) {
	h, eth, err := frame.ParseVnet(b, n.cfg.MTU)
	if err != nil {
		n.st.framesDropped.Add(1)
		return
	}
	limit := frame.MaxFrameLen(n.cfg.MTU)
	if h.IsGSO() {
		limit = frame.EthernetHeaderLen + frame.MaxIPv4Packet
	}
	// The checksum may be partial (NEEDS_CSUM): the NIC has CapabilityRXChecksumOffload,
	// so gVisor does not verify it. Nothing on this link can corrupt a frame, and the
	// stack terminates every packet rather than forwarding it.
	n.deliver(eth, limit)
}

// deliver hands one guest Ethernet frame of at most limit bytes to the stack. Only
// frames for the gateway (or broadcast) carrying IPv4 or ARP are accepted.
func (n *Stack) deliver(b []byte, limit int) {
	if !n.acceptFrame(b, limit) {
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

func (n *Stack) acceptFrame(b []byte, limit int) bool {
	if len(b) < header.EthernetMinimumSize || len(b) > limit {
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
	limit := frame.MaxFrameLen(n.cfg.MTU)
	var vh frame.VnetHdr
	if g := pkt.GSOOptions; l.vnet && g.Type != stack.GSONone {
		// As gVisor's fdbased endpoint does for a TAP with IFF_VNET_HDR: a TCP packet
		// of a GSO endpoint carries only the pseudo-header checksum, and one larger
		// than the MSS is a GSO segment.
		vh.HdrLen = uint16(pkt.HeaderSize())
		if g.NeedsCsum {
			vh.Flags = frame.VnetFlagNeedsCsum
			vh.CsumStart = header.EthernetMinimumSize + g.L3HdrLen
			vh.CsumOffset = g.CsumOffset
		}
		if g.Type == stack.GSOTCPv4 && pkt.Data().Size() > int(g.MSS) {
			vh.GSOType = frame.VnetGSOTCPv4
			vh.GSOSize = g.MSS
			limit = frame.EthernetHeaderLen + frame.MaxIPv4Packet
		}
	}
	if size > limit {
		n.st.framesDropped.Add(1)
		return
	}
	if l.enqueue(pkt, size, &vh) {
		n.st.framesOut.Add(1)
	}
}
