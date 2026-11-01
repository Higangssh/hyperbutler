package netstack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
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
	conn net.Conn
	vnet bool
	// crc: every frame carries a CRC-32C (offload link version 2).
	crc     bool
	mu      sync.Mutex
	cond    sync.Cond
	pending []byte
	spare   []byte
	closed  bool
	// inflight is the buffer writeLoop is writing, tracked only while linkCheck is set.
	inflight []byte
}

const maxPending = 1 << 20

// maxSpare bounds the buffer writeLoop keeps as its spare: the queue bound plus one
// frame of the largest size.
const maxSpare = maxPending + frame.HeaderLen + frame.CRCLen + frame.MaxVnetFrameLen

// linkCheck, when set, is called under guestLink.mu wherever the link's buffers change
// hands, with the queue, the buffer being written (nil between writes) and the spare.
// Tests set it, before any link runs, to check that no two of them share an array: a
// buffer that is written to the guest while enqueue appends to it is corrupted silently
// (ADR 0006). It is nil in production, which costs one load per call site.
var linkCheck func(pending, inflight, spare []byte)

// newGuestLink returns a link over conn; crc (offload mode only) puts a CRC-32C on
// every frame.
func newGuestLink(conn net.Conn, vnet, crc bool) *guestLink {
	l := &guestLink{conn: conn, vnet: vnet, crc: vnet && crc}
	l.cond.L = &l.mu
	return l
}

// enqueue appends one frame (with vh in offload mode). It returns false once the link
// is closed.
func (l *guestLink) enqueue(pkt *stack.PacketBuffer, size int, vh *frame.VnetHdr) bool {
	// The frame: length | CRC (version 2) | virtio_net_hdr (offload mode) | packet.
	var h [frame.HeaderLen + frame.CRCLen + frame.VnetHdrLen]byte
	hdr := h[:frame.HeaderLen]
	switch {
	case l.crc:
		frame.PutHeader(h[:], frame.CRCLen+frame.VnetHdrLen+size)
		vh.Put(h[frame.HeaderLen+frame.CRCLen:])
		hdr = h[:]
	case l.vnet:
		frame.PutHeader(h[:], frame.VnetHdrLen+size)
		vh.Put(h[frame.HeaderLen:])
		hdr = h[:frame.HeaderLen+frame.VnetHdrLen]
	default:
		frame.PutHeader(h[:], size)
	}
	slices := pkt.AsSlices()
	if l.crc {
		// From the bytes that are about to be queued, before the lock: whatever
		// changes them from here until the guest's check (the copy into the queue, a
		// buffer reused while the hvsock write still reads it) drops the frame instead
		// of delivering it (ADR 0006, "Integrity").
		crc := frame.UpdateCRC(0, h[frame.HeaderLen+frame.CRCLen:])
		for _, s := range slices {
			crc = frame.UpdateCRC(crc, s)
		}
		frame.PutCRC(h[frame.HeaderLen:], crc)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for len(l.pending) >= maxPending && !l.closed {
		l.cond.Wait()
	}
	if l.closed {
		return false
	}
	l.pending = append(l.pending, hdr...)
	for _, s := range slices {
		l.pending = append(l.pending, s...)
	}
	if linkCheck != nil {
		linkCheck(l.pending, l.inflight, l.spare)
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
		// The spare becomes the queue, so it is no longer a spare: left in place, a
		// write of buf that does not hand buf back (one a burst grew) would make the
		// live queue the next spare, and the next write would send a buffer that
		// enqueue is overwriting. The guest does not verify the TCP checksums of these
		// frames (NEEDS_CSUM): without the CRC that is silent payload corruption, with
		// it a dropped frame per overwritten one.
		buf := l.pending
		l.pending, l.spare = l.spare[:0], nil
		if linkCheck != nil {
			l.inflight = buf
			linkCheck(l.pending, l.inflight, l.spare)
		}
		l.cond.Broadcast()
		l.mu.Unlock()

		_, err := l.conn.Write(buf)

		l.mu.Lock()
		// buf is written out: keep it as the spare if it is about the queue bound, not
		// a larger one a burst grew.
		if cap(buf) <= maxSpare {
			l.spare = buf[:0]
		}
		if linkCheck != nil {
			l.inflight = nil
			linkCheck(l.pending, l.inflight, l.spare)
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
// dialed to the guest forwarder (netfwd -vnet): it sends the Hello of the link version
// of Config.LinkCRC, then frames with a virtio_net_hdr, and in version 2 a CRC-32C, in
// both directions. Otherwise it behaves as ServeConn.
//
// There is no fallback between versions: a guest that expects the other one closes
// the link, and the link stays down, with an error that says so, until engine and
// guest agree. The engine ships its own guest image, so a mismatch is a broken
// install, and quietly running without the CRC is what this version exists to stop.
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
	version := n.cfg.LinkCRC.linkVersion()
	if mode == linkVnet {
		// Before the link replaces the previous one: a guest that does not read
		// leaves the old link in place, and the write is bounded.
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		hello := frame.HelloVersion(version)
		_, err := conn.Write(hello[:])
		_ = conn.SetWriteDeadline(time.Time{})
		if err != nil {
			_ = conn.Close()
			return err
		}
	}
	l := newGuestLink(conn, mode == linkVnet, version == frame.VersionCRC)
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
	var got atomic.Bool // a frame arrived on this link
	handle := func(b []byte) error { got.Store(true); n.deliver(b, frame.MaxFrameLen(n.cfg.MTU)); return nil }
	if l.vnet {
		cr = frame.NewChunkReader(conn, frame.MinVnetFrameLen, frame.MaxVnetFrameLen)
		handle = func(b []byte) error { got.Store(true); n.deliverVnet(b); return nil }
		if l.crc {
			// Checked on Pump's reader goroutine, while the stack handles the chunk
			// before.
			cr.WithCRC(frame.CRCCheck{Strict: n.cfg.LinkCRC == CRCStrict, Bad: func(size int) {
				got.Store(true)
				n.noteCRCMismatch("guest to host", size, n.st.crcDropped.Add(1))
			}})
		}
	} else {
		cr = frame.NewChunkReader(conn, frame.EthernetHeaderLen, frame.MaxFrameLen(n.cfg.MTU))
	}
	err := frame.Pump(cr, linkChunks, linkChunk, handle, l.close)
	if n.ctx.Err() != nil {
		return n.ctx.Err()
	}
	if l.vnet && !got.Load() && (errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed)) {
		// What a guest does with a Hello it does not take (netfwd logs why on the
		// guest console).
		return fmt.Errorf("netstack: the guest closed the offload link (version %d) before sending a frame; a guest image that expects another link version does that (link CRC %s, hyperbutler.netcrc): %w",
			version, n.cfg.LinkCRC, err)
	}
	return err
}

// noteCRCMismatch logs a frame that failed its CRC, rate-limited (frame.MismatchLog):
// dir is the direction it was sent in, size its length and total the count so far.
func (n *Stack) noteCRCMismatch(dir string, size int, total uint64) {
	if ok, skipped := n.crcLog.Note(time.Now()); ok {
		n.log.Warn("guest link: a frame failed its CRC-32C and was dropped (TCP retransmits it)",
			"direction", dir, "bytes", size, "total", total, "notLogged", skipped)
	}
}

// The guest link is read in chunks of up to linkChunk bytes, linkChunks ahead.
const (
	linkChunk  = 256 << 10
	linkChunks = 4
)

// deliverVnet hands one offload-mode frame to the stack.
func (n *Stack) deliverVnet(b []byte) {
	if frame.IsLinkReport(b) {
		n.linkReport(b)
		return
	}
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
	// so gVisor does not verify it. In link version 2 the frame's CRC-32C was checked
	// before it got here; a buffer reused while it was still being written, on either
	// end, makes the frame fail it instead of corrupting a payload silently (ADR 0006,
	// "Integrity"). Version 1 (CRCOff) trusts the frame. The stack terminates every
	// packet rather than forwarding it.
	n.deliver(eth, limit)
}

// linkReport takes a link report from netfwd: the guest's count of frames from this
// host that failed their CRC.
func (n *Stack) linkReport(b []byte) {
	r, err := frame.ParseLinkReport(b)
	if err != nil {
		n.st.framesDropped.Add(1)
		return
	}
	if old := n.st.guestCRCDropped.Swap(r.CRCDropped); r.CRCDropped > old {
		if ok, skipped := n.crcLog.Note(time.Now()); ok {
			// The guest logs each frame's size on its console.
			n.log.Warn("guest link: the guest dropped frames from this host that failed their CRC-32C (TCP retransmits them)",
				"direction", "host to guest", "total", r.CRCDropped, "notLogged", skipped)
		}
	}
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
		vh.HdrLen = uint16(pkt.HeaderSize()) //nolint:gosec // G115: link, network and transport headers total well under 64 KiB
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
