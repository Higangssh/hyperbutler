package netstack

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/netstack/frame"
)

// offloadPipe serves an offload link of n over a pipe and returns the guest's end,
// after the Hello, and ServeOffload's result.
func offloadPipe(t *testing.T, n *Stack, wantVersion byte) (net.Conn, <-chan error) {
	t.Helper()
	host, guest := net.Pipe()
	t.Cleanup(func() { guest.Close() })
	done := make(chan error, 1)
	go func() { done <- n.ServeOffload(host) }()
	var hello [frame.HeaderLen]byte
	if _, err := io.ReadFull(guest, hello[:]); err != nil {
		t.Fatal(err)
	}
	if v, ok := frame.ParseHello(hello); !ok || v != wantVersion {
		t.Fatalf("hello %x, want version %d", hello, wantVersion)
	}
	return guest, done
}

// arpFrame is an offload-mode frame (virtio_net_hdr and Ethernet frame) that the
// stack accepts (FramesIn): an ARP frame for the gateway, with an empty body.
func arpFrame(n *Stack) []byte {
	b := make([]byte, frame.VnetHdrLen, frame.VnetHdrLen+frame.EthernetHeaderLen)
	b = append(b, n.Config().GatewayMAC...)
	return append(b, 2, 0, 0, 0, 0, 9, 0x08, 0x06)
}

// waitStats waits until ok(n.Stats()) holds.
func waitStats(t *testing.T, n *Stack, ok func(Stats) bool) Stats {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s := n.Stats()
		if ok(s) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("stats: %+v", s)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestOffloadCRCDrops: a guest frame whose CRC does not match is dropped and counted,
// and the link goes on.
func TestOffloadCRCDrops(t *testing.T) {
	n := newTestStack(t, nil)
	guest, done := offloadPipe(t, n, frame.VersionCRC)
	good := frame.AppendCRC(nil, arpFrame(n))
	bad := frame.AppendCRC(nil, arpFrame(n))
	bad[len(bad)-1] ^= 0x40 // one bit of the Ethernet frame, after the CRC was computed
	for _, f := range [][]byte{good, bad, bad, good} {
		if _, err := guest.Write(f); err != nil {
			t.Fatal(err)
		}
	}
	s := waitStats(t, n, func(s Stats) bool { return s.FramesIn == 2 })
	if s.FramesCRCDropped != 2 || s.FramesDropped != 0 {
		t.Fatalf("FramesCRCDropped = %d, FramesDropped = %d; want 2 and 0", s.FramesCRCDropped, s.FramesDropped)
	}
	// Corrupting the CRC field itself is the same.
	bad = frame.AppendCRC(nil, arpFrame(n))
	bad[frame.HeaderLen] ^= 1
	if _, err := guest.Write(append(bad, good...)); err != nil {
		t.Fatal(err)
	}
	waitStats(t, n, func(s Stats) bool { return s.FramesIn == 3 && s.FramesCRCDropped == 3 })
	guest.Close()
	if err := <-done; !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("ServeOffload: %v", err)
	}
}

// TestOffloadCRCStrict: in strict mode a mismatch ends the link, after the frames
// before it.
func TestOffloadCRCStrict(t *testing.T) {
	n := newTestStack(t, func(c *Config) { c.LinkCRC = CRCStrict })
	guest, done := offloadPipe(t, n, frame.VersionCRC)
	good := frame.AppendCRC(nil, arpFrame(n))
	bad := frame.AppendCRC(nil, arpFrame(n))
	bad[len(bad)-3] ^= 0x01
	go func() { _, _ = guest.Write(append(append(append([]byte{}, good...), bad...), good...)) }()
	select {
	case err := <-done:
		if !errors.Is(err, frame.ErrCRC) {
			t.Fatalf("ServeOffload: %v, want ErrCRC", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a CRC mismatch did not end a strict link")
	}
	if s := n.Stats(); s.FramesIn != 1 || s.FramesCRCDropped != 1 {
		t.Fatalf("FramesIn = %d, FramesCRCDropped = %d; want 1 and 1", s.FramesIn, s.FramesCRCDropped)
	}
}

// TestOffloadVersion1: with the CRC off the host says version 1, and frames carry no
// CRC either way.
func TestOffloadVersion1(t *testing.T) {
	n := newTestStack(t, func(c *Config) { c.LinkCRC = CRCOff })
	guest, done := offloadPipe(t, n, frame.VersionNoCRC)
	if _, err := guest.Write(frame.Append(nil, arpFrame(n))); err != nil {
		t.Fatal(err)
	}
	waitStats(t, n, func(s Stats) bool { return s.FramesIn == 1 })
	guest.Close()
	<-done
}

// TestOffloadGuestRefusesVersion: a guest that closes the link after the Hello, as one
// that expects another version does, gets an error that says so.
func TestOffloadGuestRefusesVersion(t *testing.T) {
	n := newTestStack(t, nil)
	guest, done := offloadPipe(t, n, frame.VersionCRC)
	guest.Close()
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "version 2") || !strings.Contains(err.Error(), "hyperbutler.netcrc") {
		t.Fatalf("ServeOffload: %v, want the link version named", err)
	}
}

// TestLinkReport: netfwd's count of frames it dropped for their CRC shows in Stats.
func TestLinkReport(t *testing.T) {
	n := newTestStack(t, nil)
	guest, done := offloadPipe(t, n, frame.VersionCRC)
	for _, c := range []uint64{3, 7} {
		if _, err := guest.Write(frame.AppendLinkReport(nil, frame.LinkReport{CRCDropped: c})); err != nil {
			t.Fatal(err)
		}
		waitStats(t, n, func(s Stats) bool { return s.GuestFramesCRCDropped == c })
	}
	// A malformed report is dropped like a bad frame.
	r := frame.AppendLinkReport(nil, frame.LinkReport{CRCDropped: 9})
	r[frame.HeaderLen+frame.CRCLen+1] = 1
	frame.SealCRC(r[frame.HeaderLen:])
	if _, err := guest.Write(r); err != nil {
		t.Fatal(err)
	}
	s := waitStats(t, n, func(s Stats) bool { return s.FramesDropped == 1 })
	if s.GuestFramesCRCDropped != 7 || s.FramesIn != 0 {
		t.Fatalf("after a malformed report: %+v", s)
	}
	guest.Close()
	<-done
}

// corruptingConn is the engine's end of the link with a bad buffer behind it: it
// flips one byte in the middle of every 40th large frame it writes. Every write is
// whole frames (writeLoop's queue), after the Hello.
type corruptingConn struct {
	net.Conn
	mu      sync.Mutex
	hello   bool
	frames  int
	flipped atomic.Int64
}

func (c *corruptingConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	if !c.hello {
		c.hello = true
		c.mu.Unlock()
		return c.Conn.Write(b)
	}
	out := append([]byte(nil), b...)
	for p := out; len(p) >= frame.HeaderLen; {
		n := frame.HeaderLen + int(binary.BigEndian.Uint32(p))
		if n > 2000 {
			if c.frames++; c.frames%40 == 0 {
				p[n/2] ^= 0x10
				c.flipped.Add(1)
			}
		}
		p = p[n:]
	}
	c.mu.Unlock()
	if _, err := c.Conn.Write(out); err != nil {
		return 0, err
	}
	return len(b), nil
}

// TestCRCEndToEnd: frames corrupted between the sender's framing and the receiver,
// in both directions, are dropped and counted, and TCP delivers every stream whole by
// retransmitting them.
func TestCRCEndToEnd(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	const size = 8 << 20
	const upSeed, downSeed = 0xc0ffee, 0xbeef
	hostSide := make(chan [2]streamResult, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			hostSide <- [2]streamResult{{err: err}, {err: err}}
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(30 * time.Second))
		sent, got := duplex(c, downSeed, size)
		hostSide <- [2]streamResult{sent, got}
	}()
	virt := netip.MustParseAddrPort("192.168.127.254:7")
	n := newTestStack(t, func(c *Config) {
		c.HostForwards = []HostForward{{Virtual: virt, Host: netip.MustParseAddrPort(ln.Addr().String())}}
	})
	var cc *corruptingConn
	var sentFrames, upFlipped atomic.Int64
	g := newFakeGuestWith(t, n, fakeGuestOpts{
		offload: true,
		wrap:    func(c net.Conn) net.Conn { cc = &corruptingConn{Conn: c}; return cc },
		sent: func(b []byte) {
			if len(b) > 2000 && sentFrames.Add(1)%40 == 0 {
				b[len(b)/2] ^= 0x08
				upFlipped.Add(1)
			}
		},
	})
	c, err := guestDial(t, g, virt)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	gSent, gGot := duplex(c, upSeed, size)
	h := <-hostSide
	for _, r := range []streamResult{gSent, gGot, h[0], h[1]} {
		if r.err != nil {
			t.Fatal(r.err)
		}
	}
	if gGot.n != size || gGot.sum != h[0].sum || h[1].n != size || h[1].sum != gSent.sum {
		t.Fatal("a stream arrived different")
	}
	down, up := g.crcBad.Load(), n.Stats().FramesCRCDropped
	if down == 0 || down != cc.flipped.Load() || up == 0 || int64(up) != upFlipped.Load() {
		t.Fatalf("frames dropped for their CRC: %d of %d corrupted to the guest, %d of %d to the host",
			down, cc.flipped.Load(), up, upFlipped.Load())
	}
	if d := n.Stats().FramesDropped; d != 0 {
		t.Errorf("FramesDropped = %d", d)
	}
	t.Logf("%d MiB each way; dropped for their CRC: %d frames to the guest, %d to the host", size>>20, down, up)
}
