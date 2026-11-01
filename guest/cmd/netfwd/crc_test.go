//go:build linux

package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Higangssh/hyperbutler/internal/netstack/frame"
)

var crcLink = linkFormat{mtu: 1500, vnet: true, crc: true}

// vnetTestFrame is an offload-mode frame of n bytes (header included) with pattern i.
func vnetTestFrame(i, n int) []byte {
	f := relayFrame(i, n)
	clear(f[:frame.VnetHdrLen]) // a virtio_net_hdr without flags
	return f
}

// TestHostToTapCRC: a frame from the host corrupted after its CRC was computed never
// reaches the TAP; it is counted, and the frames around it go through. In strict mode
// it ends the link instead, after the frames before it.
func TestHostToTapCRC(t *testing.T) {
	fs := [][]byte{vnetTestFrame(0, 100), vnetTestFrame(1, 60000), vnetTestFrame(2, 1514), vnetTestFrame(3, 300)}
	var in []byte
	for i, f := range fs {
		at := len(in)
		in = frame.AppendCRC(in, f)
		if i == 1 {
			in[at+frame.HeaderLen+frame.CRCLen+30000] ^= 0x04
		}
	}
	for _, strict := range []bool{false, true} {
		lk := crcLink
		lk.strict, lk.st = strict, &linkStats{}
		var out bytes.Buffer
		w := &frameWriter{out: &out}
		err := hostToTap(bytes.NewReader(in), w, lk, func() {})
		if strict {
			if !errors.Is(err, frame.ErrCRC) || w.n != 1 || !bytes.Equal(out.Bytes(), fs[0]) {
				t.Fatalf("strict: %v after %d frames, want ErrCRC after 1", err, w.n)
			}
		} else {
			want := append(append(append([]byte{}, fs[0]...), fs[2]...), fs[3]...)
			if !errors.Is(err, io.EOF) || w.n != 3 || !bytes.Equal(out.Bytes(), want) {
				t.Fatalf("%v after %d frames; the TAP got %d bytes, want EOF after 3 and %d", err, w.n, out.Len(), len(want))
			}
		}
		if n := lk.st.crcDropped.Load(); n != 1 {
			t.Fatalf("strict %v: crcDropped = %d, want 1", strict, n)
		}
	}
}

// frameWriter is a TAP that counts its writes.
type frameWriter struct {
	out *bytes.Buffer
	n   int
}

func (w *frameWriter) Write(b []byte) (int, error) { w.n++; return w.out.Write(b) }

// TestTapToHostCRC: tapBatchReader puts a CRC on every frame it reads from the TAP
// (here a datagram socket, which also hands out one frame per read), and the host's
// check passes them all. A byte changed after that, as a buffer reused under the
// vsock write would, makes the host drop that frame.
func TestTapToHostCRC(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	tapSide := os.NewFile(uintptr(fds[0]), "tap")
	kernel := os.NewFile(uintptr(fds[1]), "kernel")
	defer tapSide.Close()
	defer kernel.Close()
	// Datagrams of up to 64 KiB need room in the socket buffers.
	_ = unix.SetsockoptInt(fds[0], unix.SOL_SOCKET, unix.SO_RCVBUF, 4<<20)
	_ = unix.SetsockoptInt(fds[1], unix.SOL_SOCKET, unix.SO_SNDBUF, 4<<20)
	var fs [][]byte
	for i, n := range []int{100, 9000, frame.MinVnetFrameLen, 1514, 20000} {
		f := vnetTestFrame(i, n)
		if _, err := kernel.Write(f); err != nil {
			t.Fatal(err)
		}
		fs = append(fs, f)
	}
	if _, err := kernel.Write(make([]byte, 5)); err != nil { // a runt, skipped
		t.Fatal(err)
	}
	rc, err := tapSide.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	_ = tapSide.SetReadDeadline(time.Now().Add(200 * time.Millisecond)) // ends the relay
	var out bytes.Buffer
	if err := tapToHost(tapBatchReader(rc, crcLink), &out, crcLink); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("tapToHost: %v", err)
	}
	stream := out.Bytes()
	decode := func(stream []byte) (got [][]byte, bad int) {
		cr := frame.NewChunkReader(bytes.NewReader(stream), crcLink.minFrame(), crcLink.maxFrame()).
			WithCRC(frame.CRCCheck{Bad: func(int) { bad++ }})
		_ = frame.Pump(cr, 2, 0, func(f []byte) error { got = append(got, append([]byte(nil), f...)); return nil }, func() {})
		return got, bad
	}
	got, bad := decode(stream)
	if bad != 0 || len(got) != len(fs) {
		t.Fatalf("the host got %d frames and %d bad, want %d and 0", len(got), bad, len(fs))
	}
	for i := range fs {
		if !bytes.Equal(got[i], fs[i]) {
			t.Fatalf("frame %d differs", i)
		}
	}
	// Corrupt the second frame's payload in the framed stream.
	second := frame.HeaderLen + frame.CRCLen + len(fs[0])
	stream[second+frame.HeaderLen+frame.CRCLen+5000] ^= 0x80
	got, bad = decode(stream)
	if bad != 1 || len(got) != len(fs)-1 || !bytes.Equal(got[1], fs[2]) {
		t.Fatalf("after corrupting frame 1: %d frames, %d bad", len(got), bad)
	}
}

// slowTap returns nothing for a while, then its error, so that tapToHost's writer
// gets to send a link report.
func slowTap(d time.Duration, err error) readBatchFunc {
	return func(buf []byte) ([]byte, error) {
		time.Sleep(d)
		return buf[:0], err
	}
}

// lockedBuffer is a vsock that tapToHost's writer goroutine writes and the test reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// TestLinkReportSent: netfwd tells the host its count of frames dropped for their
// CRC, once per change.
func TestLinkReportSent(t *testing.T) {
	old := reportEvery
	reportEvery = 10 * time.Millisecond
	defer func() { reportEvery = old }()
	lk := crcLink
	lk.st = &linkStats{}
	lk.st.crcDropped.Store(5)
	var out lockedBuffer
	if err := tapToHost(slowTap(200*time.Millisecond, io.EOF), &out, lk); err == nil {
		t.Fatal("want the TAP's EOF")
	}
	want := frame.AppendLinkReport(nil, frame.LinkReport{CRCDropped: 5})
	if !bytes.Equal(out.b.Bytes(), want) {
		t.Fatalf("the host got %x, want one report of 5 (%x)", out.b.Bytes(), want)
	}
	// Version 1 has no reports.
	lk.crc = false
	out.b.Reset()
	_ = tapToHost(slowTap(50*time.Millisecond, io.EOF), &out, lk)
	if out.b.Len() != 0 {
		t.Fatalf("a version 1 link got %d bytes", out.b.Len())
	}
}

func TestReadHello(t *testing.T) {
	for _, tc := range []struct {
		hello  [frame.HeaderLen]byte
		expect byte
		ok     bool
	}{
		{frame.Hello, frame.VersionCRC, true},
		{frame.HelloVersion(frame.VersionNoCRC), frame.VersionNoCRC, true},
		{frame.HelloVersion(frame.VersionNoCRC), frame.VersionCRC, false},
		{frame.Hello, frame.VersionNoCRC, false},
		{[frame.HeaderLen]byte{0, 0, 0, 60}, frame.VersionCRC, false},
	} {
		err := readHello(bytes.NewReader(tc.hello[:]), tc.expect)
		if (err == nil) != tc.ok {
			t.Errorf("hello %x, expecting version %d: %v", tc.hello, tc.expect, err)
		}
		if err != nil && tc.hello[0] == 0xFF && !strings.Contains(err.Error(), "link version") {
			t.Errorf("the refusal does not name the versions: %v", err)
		}
	}
}

func TestLinkFormatCRC(t *testing.T) {
	if crcLink.prefix() != 8 || crcLink.version() != frame.VersionCRC {
		t.Fatalf("version 2: prefix %d, version %d", crcLink.prefix(), crcLink.version())
	}
	v1 := linkFormat{mtu: 1500, vnet: true}
	if v1.prefix() != 4 || v1.version() != frame.VersionNoCRC {
		t.Fatalf("version 1: prefix %d, version %d", v1.prefix(), v1.version())
	}
}
