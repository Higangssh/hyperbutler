package netstack

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/Higangssh/hyperbutler/internal/netstack/frame"
)

// checkLinkBuffers sets linkCheck for the rest of the test: every link checks, wherever
// its buffers change hands, that the queue, the buffer being written and the spare are
// three different arrays. Call it before starting any link, so that its cleanup runs
// after the test's links have stopped.
func checkLinkBuffers(t *testing.T) *linkBufferStats {
	t.Helper()
	st := &linkBufferStats{}
	linkCheck = func(pending, inflight, spare []byte) {
		if sameArray(pending, inflight) || sameArray(pending, spare) || sameArray(inflight, spare) {
			st.shared.Add(1)
		}
		if cap(inflight) > maxPending+frame.MaxVnetFrameLen+frame.HeaderLen {
			st.grown.Add(1)
		}
	}
	t.Cleanup(func() {
		linkCheck = nil
		if n := st.shared.Load(); n > 0 {
			t.Errorf("link buffers shared an array %d times (queue, in flight, spare)", n)
		}
	})
	return st
}

type linkBufferStats struct {
	shared atomic.Int64 // checks that found two of the buffers sharing an array
	grown  atomic.Int64 // checks during a write of a queue that outgrew the spare bound
}

// sameArray reports whether a and b start at the same element of one array.
func sameArray(a, b []byte) bool {
	return cap(a) > 0 && cap(b) > 0 && &a[:1][0] == &b[:1][0]
}

func TestSameArray(t *testing.T) {
	a := make([]byte, 4, 8)
	if !sameArray(a, a[:0]) || sameArray(a, make([]byte, 4)) || sameArray(a, nil) || sameArray(nil, nil) {
		t.Fatal("sameArray is wrong")
	}
}

// stallingConn is the engine's end of an hvsock link to a guest that is sometimes
// descheduled: on a seeded schedule a Write sleeps 1-20 ms, before it starts or between
// pieces, and it hands the caller's buffer to the connection in 16 KiB pieces, so the
// guest reads it over time as Windows does. The queue behind it then grows to the
// bound and past it.
type stallingConn struct {
	net.Conn
	mu     sync.Mutex
	rng    *rand.Rand
	stalls int
}

func newStallingConn(c net.Conn, seed uint64) *stallingConn {
	return &stallingConn{Conn: c, rng: rand.New(rand.NewPCG(seed, seed^0x5bd1e995))}
}

// stall returns how long to sleep at the next point of a write: one point in every
// oneIn sleeps.
func (c *stallingConn) stall(oneIn int) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rng.IntN(oneIn) != 0 {
		return 0
	}
	c.stalls++
	return time.Duration(1+c.rng.IntN(20)) * time.Millisecond
}

func (c *stallingConn) Write(b []byte) (int, error) {
	// Most writes are small and go through. A large one (a full queue) stalls at the
	// start every other time, and every 128 KiB one time in eight: a buffer that is
	// appended to while it is written is then sent partly before and partly after, so
	// frames arrive cut and mixed, not just replaced by whole later frames.
	large := len(b) > 256<<10
	if large {
		time.Sleep(c.stall(2))
	} else {
		time.Sleep(c.stall(16))
	}
	done := 0
	for done < len(b) {
		n, err := c.Conn.Write(b[done:min(len(b), done+16<<10)])
		if large && (done+n)%(128<<10) == 0 {
			time.Sleep(c.stall(8))
		}
		done += n
		if err != nil {
			return done, err
		}
	}
	return done, nil
}

func (c *stallingConn) stallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stalls
}

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
	checkLinkBuffers(t)
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
	checkLinkBuffers(t)
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
