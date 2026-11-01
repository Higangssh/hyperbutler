package netstack

import (
	"bytes"
	"encoding/binary"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/Higangssh/hyperbutler/internal/netstack/frame"
)

// heldConn is the guest side of a link whose writes the test lets through one at a
// time: each Write reports its length on started, waits for release, and only then
// reads the caller's buffer, as a socket send that is still in flight does.
type heldConn struct {
	net.Conn // nil; only Write and Close are used
	started  chan int
	release  chan struct{}
	out      bytes.Buffer
}

func (c *heldConn) Write(b []byte) (int, error) {
	c.started <- len(b)
	if _, ok := <-c.release; !ok {
		return 0, net.ErrClosed
	}
	c.out.Write(b)
	return len(b), nil
}

func (c *heldConn) Close() error { return nil }

// stallConn reads the caller's buffer in pieces, over time, and stalls on every third
// write, so the queue behind it sometimes fills to the bound and sometimes does not.
type stallConn struct {
	net.Conn // nil; only Write and Close are used
	mu       sync.Mutex
	writes   int
	out      bytes.Buffer
}

func (c *stallConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	c.writes++
	stall := c.writes%3 == 0
	c.mu.Unlock()
	if stall {
		time.Sleep(time.Millisecond)
	}
	for p := b; len(p) > 0; {
		n := min(len(p), 16<<10)
		c.mu.Lock()
		c.out.Write(p[:n])
		c.mu.Unlock()
		p = p[n:]
		runtime.Gosched()
	}
	return len(b), nil
}

func (c *stallConn) Close() error { return nil }

func (c *stallConn) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.Len()
}

func (c *stallConn) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.out.Bytes())
}

const testFrameLen = 65000 // about one GSO segment

// testFrame is frame i: its index in every 4-byte group.
func testFrame(i int) []byte {
	p := make([]byte, testFrameLen)
	for k := 0; k+4 <= len(p); k += 4 {
		binary.BigEndian.PutUint32(p[k:], uint32(i))
	}
	return p
}

func enqueueFrame(t *testing.T, l *guestLink, i int) {
	t.Helper()
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(testFrame(i))})
	defer pkt.DecRef()
	if !l.enqueue(pkt, testFrameLen, &frame.VnetHdr{}) {
		t.Fatal("link closed")
	}
}

// TestGuestLinkSpareNotLive: frames queued while a write is in flight must reach the
// guest as they were queued, also after a write of a queue that grew past the spare
// bound. Such a write used to leave the spare pointing at the live queue, so the next
// write sent a buffer that enqueue was overwriting: the guest got TCP segments whose
// payload belonged to other segments, with a checksum it does not verify (NEEDS_CSUM),
// and image layers arrived with the right size and the wrong digest.
func TestGuestLinkSpareNotLive(t *testing.T) {
	c := &heldConn{started: make(chan int, 8), release: make(chan struct{})}
	l := newGuestLink(c, false)
	done := make(chan struct{})
	go func() { defer close(done); l.writeLoop() }()
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			close(c.release) // a write still waiting fails, which ends writeLoop
			l.close()
			<-done
		}
	}
	defer stop()
	next := 0
	queue := func(n int) {
		for range n {
			enqueueFrame(t, l, next)
			next++
		}
	}
	step := func() { c.release <- struct{}{} }

	// Write 1: a small queue. Its buffer becomes the spare.
	queue(1)
	<-c.started
	// Behind it the queue grows to the bound (the guest is slow), past the spare bound.
	queue(17)
	l.mu.Lock()
	grown := cap(l.pending)
	l.mu.Unlock()
	if bound := maxPending + frame.MaxVnetFrameLen + frame.HeaderLen; grown <= bound {
		t.Fatalf("test setup: queue capacity %d is within the spare bound %d", grown, bound)
	}
	step()
	// Write 2 sends the grown queue; the queue is now the spare's array.
	<-c.started
	queue(1)
	step()
	// Write 3 sends that queue; whatever is queued while it is in flight must not
	// land in the buffer being sent.
	<-c.started
	queue(1)
	step()
	<-c.started
	step()
	stop() // after this, nothing writes c.out

	got := c.out.Bytes()
	rec := frame.HeaderLen + testFrameLen
	if len(got) != next*rec {
		t.Fatalf("guest got %d bytes, want %d frames of %d", len(got), next, rec)
	}
	for i := range next {
		f := got[i*rec:][:rec]
		if n := binary.BigEndian.Uint32(f); n != testFrameLen {
			t.Fatalf("frame %d: length %d, want %d", i, n, testFrameLen)
		}
		if p := f[frame.HeaderLen:]; !bytes.Equal(p, testFrame(i)) {
			t.Errorf("frame %d arrived with the payload of frame %d", i, binary.BigEndian.Uint32(p))
		}
	}
}

// TestGuestLinkBackpressureStress: the same property with free-running producers and a
// guest that stalls now and then.
func TestGuestLinkBackpressureStress(t *testing.T) {
	c := &stallConn{}
	l := newGuestLink(c, false)
	done := make(chan struct{})
	go func() { defer close(done); l.writeLoop() }()
	const frames = 3000
	for i := range frames {
		enqueueFrame(t, l, i)
		if i%5 == 0 {
			time.Sleep(20 * time.Microsecond)
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for c.len() < frames*(frame.HeaderLen+testFrameLen) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	l.close()
	<-done
	got := c.bytes()
	rec := frame.HeaderLen + testFrameLen
	if len(got) != frames*rec {
		t.Fatalf("guest got %d bytes, want %d", len(got), frames*rec)
	}
	bad := 0
	for i := range frames {
		if !bytes.Equal(got[i*rec+frame.HeaderLen:][:testFrameLen], testFrame(i)) {
			bad++
		}
	}
	if bad > 0 {
		t.Fatalf("%d of %d frames arrived with another frame's payload", bad, frames)
	}
}
