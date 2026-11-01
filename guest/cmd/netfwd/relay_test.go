//go:build linux

package main

import (
	"bytes"
	"fmt"
	"io"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/netstack/frame"
)

// chunkReader returns at most one queued slice per Read, like a TAP device.
type chunkReader struct{ chunks [][]byte }

func (c *chunkReader) Read(b []byte) (int, error) {
	if len(c.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(b, c.chunks[0])
	c.chunks = c.chunks[1:]
	return n, nil
}

func TestTapToHostFrames(t *testing.T) {
	f1 := bytes.Repeat([]byte{1}, 60)
	f2 := bytes.Repeat([]byte{2}, 1514)
	var out bytes.Buffer
	cr := &chunkReader{[][]byte{f1, {9, 9}, f2}}
	// One frame per batch, like a TAP with one frame queued at a time.
	read := func(buf []byte) ([]byte, error) {
		for {
			slot := make([]byte, frame.MaxFrameLen(1500))
			n, err := cr.Read(slot)
			if err != nil {
				return buf[:0], err
			}
			if n >= frame.EthernetHeaderLen {
				return frame.Append(buf[:0], slot[:n]), nil
			}
		}
	}
	err := tapToHost(read, &out, linkFormat{mtu: 1500})
	if err == nil {
		t.Fatal("want EOF error")
	}
	want := frame.Append(frame.Append(nil, f1), f2) // the runt is skipped
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatal("framed output differs")
	}
}

func TestHostToTapFrames(t *testing.T) {
	f1 := bytes.Repeat([]byte{1}, 60)
	in := frame.Append(frame.Append(nil, f1), make([]byte, 14))
	in = append(in, 0xFF, 0, 0, 0) // bad length ends the relay
	var out bytes.Buffer
	if err := hostToTap(bytes.NewReader(in), &out, linkFormat{mtu: 1500}, func() {}); err == nil {
		t.Fatal("bad length accepted")
	}
	if out.Len() != 60+14 {
		t.Fatalf("wrote %d bytes to tap", out.Len())
	}
}

func TestHostToTapVnet(t *testing.T) {
	big := make([]byte, frame.VnetHdrLen+frame.EthernetHeaderLen+60000) // a GSO segment
	in := frame.Append(frame.Append(nil, big), make([]byte, frame.MinVnetFrameLen))
	in = append(in, 0, 0, 0, 5) // shorter than a vnet header and an Ethernet header
	var out bytes.Buffer
	if err := hostToTap(bytes.NewReader(in), &out, linkFormat{mtu: 1500, vnet: true}, func() {}); err == nil {
		t.Fatal("runt accepted")
	}
	if out.Len() != len(big)+frame.MinVnetFrameLen {
		t.Fatalf("wrote %d bytes to tap", out.Len())
	}
}

func TestLinkFormat(t *testing.T) {
	plain, vnet := linkFormat{mtu: 9000}, linkFormat{mtu: 9000, vnet: true}
	if plain.maxFrame() != 9014 || plain.minFrame() != 14 {
		t.Fatalf("plain: %d %d", plain.maxFrame(), plain.minFrame())
	}
	if vnet.maxFrame() != frame.MaxVnetFrameLen || vnet.minFrame() != frame.MinVnetFrameLen {
		t.Fatalf("vnet: %d %d", vnet.maxFrame(), vnet.minFrame())
	}
}

// relayFrame is frame i of the stall tests: n bytes, every byte a function of i and its
// position, so a frame cut, shifted or overwritten by another one does not match.
func relayFrame(i, n int) []byte {
	f := make([]byte, n)
	for k := range f {
		f[k] = byte(i*131 + k*7 + k>>8)
	}
	return f
}

// relayFrameLens returns n frame lengths between minLen and maxLen from a seeded
// schedule.
func relayFrameLens(n, minLen, maxLen int, seed uint64) []int {
	rng := rand.New(rand.NewPCG(seed, 1))
	lens := make([]int, n)
	for i := range lens {
		lens[i] = minLen + rng.IntN(maxLen-minLen+1)
	}
	return lens
}

// stallWriter is the vsock seen from netfwd: its first Write blocks until release is
// closed, and every Write takes the caller's buffer in 16 KiB pieces, now and then
// sleeping between them, as a socket send in flight reads it over time. It keeps what
// it read.
type stallWriter struct {
	release chan struct{}
	started chan struct{} // closed when the first Write begins
	once    sync.Once
	rng     *rand.Rand
	mu      sync.Mutex
	out     bytes.Buffer
}

func (w *stallWriter) Write(b []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	for p := b; len(p) > 0; {
		n := min(len(p), 16<<10)
		w.mu.Lock()
		w.out.Write(p[:n])
		stall := w.rng.IntN(8) == 0
		w.mu.Unlock()
		if stall {
			time.Sleep(time.Duration(1+w.rng.IntN(3)) * time.Millisecond)
		} else {
			runtime.Gosched()
		}
		p = p[n:]
	}
	return len(b), nil
}

// TestTapToHostStalledWriter: while the vsock write of the first batch is stalled, the
// TAP reader fills every other batch buffer; the host must still get every frame as it
// was read, in order. A buffer handed back to the reader before its write finished
// would be refilled under the write.
func TestTapToHostStalledWriter(t *testing.T) {
	lk := linkFormat{mtu: 1500, vnet: true, crc: true}
	const frames = 400
	lens := relayFrameLens(frames, lk.minFrame(), lk.maxFrame(), 7)
	var want []byte
	for i, n := range lens {
		want = frame.AppendCRC(want, relayFrame(i, n))
	}
	w := &stallWriter{release: make(chan struct{}), started: make(chan struct{}), rng: rand.New(rand.NewPCG(8, 1))}
	var reads atomic.Int32
	allTaken := make(chan struct{})
	next := 0
	rng := rand.New(rand.NewPCG(9, 1))
	// Like tapBatchReader: frames are written into buf in place, a few per batch.
	read := func(buf []byte) ([]byte, error) {
		if reads.Add(1) == tapBatches {
			close(allTaken)
		}
		if next == frames {
			return buf[:0], io.EOF
		}
		out := buf[:0]
		for k := 1 + rng.IntN(6); k > 0 && next < frames && cap(out)-len(out) >= lk.prefix()+lk.maxFrame(); k-- {
			out = frame.AppendCRC(out, relayFrame(next, lens[next]))
			next++
		}
		return out, nil
	}
	errc := make(chan error, 1)
	go func() { errc <- tapToHost(read, w, lk) }()
	<-w.started
	select {
	case <-allTaken:
	case <-time.After(10 * time.Second):
		t.Fatal("the reader did not take every batch buffer while the write was stalled")
	}
	time.Sleep(20 * time.Millisecond) // the reader is now waiting for a free buffer
	close(w.release)
	if err := <-errc; err == nil {
		t.Fatal("want the TAP's EOF")
	}
	w.mu.Lock()
	got := w.out.Bytes()
	w.mu.Unlock()
	if !bytes.Equal(got, want) {
		t.Fatalf("host got %d bytes, want %d: %s", len(got), len(want), firstBadFrame(got, lens))
	}
}

// firstBadFrame names the first frame of got that differs from its pattern.
func firstBadFrame(got []byte, lens []int) string {
	for i, n := range lens {
		f := frame.AppendCRC(nil, relayFrame(i, n))
		if len(got) < len(f) {
			return fmt.Sprintf("the stream ends in frame %d", i)
		}
		if !bytes.Equal(got[:len(f)], f) {
			return fmt.Sprintf("frame %d differs", i)
		}
		got = got[len(f):]
	}
	return "extra bytes after the last frame"
}

// pieceReader returns its data in reads of random sizes, as a vsock does, and counts
// what it has returned.
type pieceReader struct {
	data []byte
	rng  *rand.Rand
	read atomic.Int64
}

func (r *pieceReader) Read(b []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	if len(b) == 0 {
		return 0, nil
	}
	n := min(len(b), len(r.data), 1+r.rng.IntN(len(b)))
	copy(b, r.data[:n])
	r.data = r.data[n:]
	r.read.Add(int64(n))
	return n, nil
}

// stallTap is the TAP seen from netfwd: the first Write blocks until release is closed,
// later ones sometimes sleep, and each one checks the frame against its pattern only
// after that, as a kernel that copies the frame late would see it.
type stallTap struct {
	release chan struct{}
	started chan struct{}
	once    sync.Once
	rng     *rand.Rand
	lens    []int
	n       int
	bad     []int
}

func (w *stallTap) Write(b []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	if w.rng.IntN(16) == 0 {
		time.Sleep(time.Duration(1+w.rng.IntN(3)) * time.Millisecond)
	}
	if w.n >= len(w.lens) || !bytes.Equal(b, relayFrame(w.n, w.lens[w.n])) {
		w.bad = append(w.bad, w.n)
	}
	w.n++
	return len(b), nil
}

// TestHostToTapStalledWriter: while the TAP write of the first frame is stalled, the
// vsock is read ahead into every chunk buffer; every frame must still reach the TAP
// whole and in order. A chunk handed back to the reader while its frames were being
// written would be refilled under them.
func TestHostToTapStalledWriter(t *testing.T) {
	lk := linkFormat{mtu: 1500, vnet: true, crc: true}
	const frames = 300 // about 9 MiB: more than hostChunks chunks of hostChunk
	lens := relayFrameLens(frames, lk.minFrame(), lk.maxFrame(), 11)
	var in []byte
	for i, n := range lens {
		in = frame.AppendCRC(in, relayFrame(i, n))
	}
	r := &pieceReader{data: in, rng: rand.New(rand.NewPCG(12, 1))}
	tap := &stallTap{release: make(chan struct{}), started: make(chan struct{}), rng: rand.New(rand.NewPCG(13, 1)), lens: lens}
	errc := make(chan error, 1)
	go func() { errc <- hostToTap(r, tap, lk, func() {}) }()
	<-tap.started
	// The vsock never blocks, so once the reader stops reading it holds every chunk
	// buffer and waits for the stalled TAP writer to hand one back.
	for last := int64(-1); ; {
		time.Sleep(50 * time.Millisecond)
		n := r.read.Load()
		if n == last {
			break
		}
		last = n
	}
	if n := r.read.Load(); n == int64(len(in)) || n < hostChunk {
		t.Fatalf("test setup: the reader read %d of %d bytes ahead of the stalled TAP", n, len(in))
	}
	t.Logf("%d bytes read ahead of the stalled TAP", r.read.Load())
	close(tap.release)
	if err := <-errc; err == nil {
		t.Fatal("want the vsock's EOF")
	}
	if tap.n != frames || len(tap.bad) > 0 {
		t.Fatalf("TAP got %d frames, want %d; %d differ from their pattern (the first: %v)", tap.n, frames, len(tap.bad), tap.bad[:min(1, len(tap.bad))])
	}
}
