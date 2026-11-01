package frame

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"testing/iotest"
	"time"
)

func vnetFrame(h VnetHdr, ethLen int) []byte {
	b := make([]byte, VnetHdrLen+ethLen)
	h.Put(b)
	return b
}

func TestParseVnet(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    VnetHdr
		eth  int
		ok   bool
	}{
		{"plain", VnetHdr{}, 60, true},
		{"plain at MTU", VnetHdr{}, MaxFrameLen(1500), true},
		{"plain over MTU", VnetHdr{}, MaxFrameLen(1500) + 1, false},
		{"partial checksum", VnetHdr{Flags: VnetFlagNeedsCsum, CsumStart: 34, CsumOffset: 16}, 60, true},
		{"data valid", VnetHdr{Flags: VnetFlagDataValid}, 60, true},
		{"checksum outside", VnetHdr{Flags: VnetFlagNeedsCsum, CsumStart: 34, CsumOffset: 16}, 51, false},
		{"unknown flag", VnetHdr{Flags: 4}, 60, false},
		{"gso", VnetHdr{Flags: VnetFlagNeedsCsum, GSOType: VnetGSOTCPv4, GSOSize: 1448, HdrLen: 66, CsumStart: 34, CsumOffset: 16}, 60000, true},
		{"gso ecn", VnetHdr{GSOType: VnetGSOTCPv4 | VnetGSOECN, GSOSize: 1448}, 60000, true},
		{"gso max", VnetHdr{GSOType: VnetGSOTCPv4, GSOSize: 1448}, EthernetHeaderLen + MaxIPv4Packet, true},
		{"gso over max", VnetHdr{GSOType: VnetGSOTCPv4, GSOSize: 1448}, EthernetHeaderLen + MaxIPv4Packet + 1, false},
		{"gso no size", VnetHdr{GSOType: VnetGSOTCPv4}, 6000, false},
		{"gso header past frame", VnetHdr{GSOType: VnetGSOTCPv4, GSOSize: 1448, HdrLen: 7000}, 6000, false},
		{"ecn alone", VnetHdr{GSOType: VnetGSOECN}, 60, false},
		{"tcpv6", VnetHdr{GSOType: 4, GSOSize: 1440}, 6000, false},
		{"udp", VnetHdr{GSOType: 3, GSOSize: 1440}, 6000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, eth, err := ParseVnet(vnetFrame(tc.h, tc.eth), 1500)
			if tc.ok != (err == nil) {
				t.Fatalf("err = %v, want ok %v", err, tc.ok)
			}
			if err != nil {
				if !errors.Is(err, ErrVnetHdr) {
					t.Fatalf("error %v is not ErrVnetHdr", err)
				}
				return
			}
			if h != tc.h || len(eth) != tc.eth {
				t.Fatalf("got %+v and %d bytes, want %+v and %d", h, len(eth), tc.h, tc.eth)
			}
		})
	}
	if _, _, err := ParseVnet(make([]byte, MinVnetFrameLen-1), 1500); err == nil {
		t.Fatal("runt accepted")
	}
}

func TestChunkReader(t *testing.T) {
	var stream []byte
	var want [][]byte
	for i, n := range []int{14, 100, 9014, 14, 5000, 9014} {
		f := bytes.Repeat([]byte{byte(i + 1)}, n)
		want = append(want, f)
		stream = Append(stream, f)
	}
	for _, name := range []string{"whole", "one byte at a time", "half"} {
		t.Run(name, func(t *testing.T) {
			var r io.Reader = bytes.NewReader(stream)
			switch name {
			case "one byte at a time":
				r = iotest.OneByteReader(r)
			case "half":
				r = iotest.HalfReader(r)
			}
			cr := NewChunkReader(r, EthernetHeaderLen, MaxFrameLen(9000))
			buf := make([]byte, cr.MinChunk())
			var got [][]byte
			for {
				chunk, err := cr.ReadChunk(buf)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(chunk) == 0 {
					t.Fatal("empty chunk without an error")
				}
				if err := EachFrame(chunk, func(f []byte) error { got = append(got, append([]byte(nil), f...)); return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if len(got) != len(want) {
				t.Fatalf("%d frames, want %d", len(got), len(want))
			}
			for i := range want {
				if !bytes.Equal(got[i], want[i]) {
					t.Fatalf("frame %d differs", i)
				}
			}
		})
	}
}

func TestChunkReaderErrors(t *testing.T) {
	good := Append(nil, make([]byte, 20))
	for _, tc := range []struct {
		name   string
		in     []byte
		frames int
		want   error
	}{
		{"bad length after a frame", append(append([]byte{}, good...), 0, 0, 0, 1), 1, ErrFrameSize},
		{"huge", []byte{0xFF, 0xFF, 0xFF, 0xFF}, 0, ErrFrameSize},
		{"cut off", append(append([]byte{}, good...), 0, 0, 0, 20, 1, 2), 1, io.ErrUnexpectedEOF},
		{"half header", []byte{0, 0}, 0, io.ErrUnexpectedEOF},
		{"empty", nil, 0, io.EOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cr := NewChunkReader(bytes.NewReader(tc.in), EthernetHeaderLen, MaxFrameLen(1500))
			buf := make([]byte, cr.MinChunk())
			frames := 0
			var err error
			for i := 0; i < 10; i++ {
				var chunk []byte
				chunk, err = cr.ReadChunk(buf)
				if err != nil {
					break
				}
				_ = EachFrame(chunk, func([]byte) error { frames++; return nil })
			}
			if frames != tc.frames || !errors.Is(err, tc.want) {
				t.Fatalf("%d frames, %v; want %d, %v", frames, err, tc.frames, tc.want)
			}
			if _, err2 := cr.ReadChunk(buf); !errors.Is(err2, tc.want) {
				t.Fatalf("error is not sticky: %v", err2)
			}
		})
	}
	cr := NewChunkReader(bytes.NewReader(good), EthernetHeaderLen, MaxFrameLen(1500))
	if _, err := cr.ReadChunk(make([]byte, 10)); err == nil {
		t.Fatal("small buffer accepted")
	}
}

// pumpFrame is frame i of TestPump: 14+i*50 bytes, every byte a function of i and its
// position, so a frame cut, shifted or overwritten by another one does not match.
func pumpFrame(i int) []byte {
	f := make([]byte, 14+i*50)
	for k := range f {
		f[k] = byte(i*131 + k*7 + k>>8)
	}
	return f
}

func TestPump(t *testing.T) {
	var stream []byte
	for i := 0; i < 100; i++ {
		stream = Append(stream, pumpFrame(i))
	}
	for _, slow := range []bool{false, true} {
		cr := NewChunkReader(iotest.HalfReader(bytes.NewReader(stream)), EthernetHeaderLen, MaxFrameLen(9000))
		n := 0
		// A slow handler lets the reader fill every buffer ahead of it, so a buffer
		// handed back too early would be refilled under a frame still being handled.
		err := Pump(cr, 2, 0, func(f []byte) error {
			want := pumpFrame(n)
			if slow && n%10 == 0 {
				time.Sleep(5 * time.Millisecond)
			}
			if !bytes.Equal(f, want) {
				t.Errorf("slow %v, frame %d: %d bytes, want %d with its pattern", slow, n, len(f), len(want))
			}
			n++
			return nil
		}, func() {})
		if !errors.Is(err, io.EOF) || n != 100 {
			t.Fatalf("Pump (slow %v): %v after %d frames", slow, err, n)
		}
	}
	// A failing handler stops the reader through stop.
	pr, pw := io.Pipe()
	go func() {
		for {
			if _, err := pw.Write(Append(nil, make([]byte, 20))); err != nil {
				return
			}
		}
	}()
	boom := errors.New("boom")
	err := Pump(NewChunkReader(pr, EthernetHeaderLen, MaxFrameLen(1500)), 2, 0, func([]byte) error { return boom },
		func() { pr.CloseWithError(io.ErrClosedPipe); pw.CloseWithError(io.ErrClosedPipe) })
	if !errors.Is(err, boom) {
		t.Fatalf("Pump: %v, want the handler's error", err)
	}
}

// FuzzParseVnet: whatever the bytes, an accepted frame respects the bounds the stack
// relies on: a non-GSO frame fits the MTU, a GSO frame fits an IPv4 packet, and the
// checksum offsets lie inside the frame.
func FuzzParseVnet(f *testing.F) {
	f.Add(vnetFrame(VnetHdr{}, 60), uint16(1500))
	f.Add(vnetFrame(VnetHdr{Flags: 1, GSOType: 1, GSOSize: 1448, HdrLen: 66, CsumStart: 34, CsumOffset: 16}, 20000), uint16(9000))
	f.Add(vnetFrame(VnetHdr{GSOType: 0x81, GSOSize: 1}, 100), uint16(576))
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, uint16(1500))
	f.Fuzz(func(t *testing.T, b []byte, mtu uint16) {
		if !ValidMTU(int(mtu)) {
			return
		}
		h, eth, err := ParseVnet(b, int(mtu))
		if err != nil {
			return
		}
		if len(eth) != len(b)-VnetHdrLen || len(eth) < EthernetHeaderLen {
			t.Fatalf("frame of %d bytes from %d", len(eth), len(b))
		}
		if !h.IsGSO() && len(eth) > MaxFrameLen(int(mtu)) {
			t.Fatalf("non-GSO frame of %d bytes at MTU %d", len(eth), mtu)
		}
		if len(eth) > EthernetHeaderLen+MaxIPv4Packet {
			t.Fatalf("frame of %d bytes", len(eth))
		}
		if h.Flags&VnetFlagNeedsCsum != 0 && int(h.CsumStart)+int(h.CsumOffset)+2 > len(eth) {
			t.Fatal("checksum offset outside the frame")
		}
		var back [VnetHdrLen]byte
		h.Put(back[:])
		if !bytes.Equal(back[:], b[:VnetHdrLen]) {
			t.Fatal("header does not round-trip")
		}
	})
}

// FuzzChunkReader: arbitrary streams, read in arbitrary pieces. Every frame returned is
// within bounds and the frames are exactly a prefix of what a strict frame-by-frame
// parse of the stream gives; nothing panics.
func FuzzChunkReader(f *testing.F) {
	f.Add(Append(Append(nil, make([]byte, 60)), make([]byte, 1514)), uint16(1500), uint8(7))
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0}, uint16(1500), uint8(1))
	f.Add([]byte{0, 0, 0, 14}, uint16(576), uint8(3))
	f.Fuzz(func(t *testing.T, in []byte, mtu uint16, step uint8) {
		if !ValidMTU(int(mtu)) {
			return
		}
		max := MaxFrameLen(int(mtu))
		// The reference: frames of in, parsed one by one.
		var ref [][]byte
		for rest := in; len(rest) >= HeaderLen; {
			n := int(rest[0])<<24 | int(rest[1])<<16 | int(rest[2])<<8 | int(rest[3])
			if n < EthernetHeaderLen || n > max || len(rest) < HeaderLen+n {
				break
			}
			ref = append(ref, rest[HeaderLen:HeaderLen+n])
			rest = rest[HeaderLen+n:]
		}
		r := &steppedReader{b: in, step: int(step) + 1}
		cr := NewChunkReader(r, EthernetHeaderLen, max)
		buf := make([]byte, cr.MinChunk())
		var got [][]byte
		for i := 0; i < len(in)+2; i++ {
			chunk, err := cr.ReadChunk(buf)
			if err != nil {
				break
			}
			if err := EachFrame(chunk, func(fr []byte) error {
				if len(fr) < EthernetHeaderLen || len(fr) > max {
					t.Fatalf("frame of %d bytes", len(fr))
				}
				got = append(got, append([]byte(nil), fr...))
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		if len(got) != len(ref) {
			t.Fatalf("%d frames, want %d", len(got), len(ref))
		}
		for i := range ref {
			if !bytes.Equal(got[i], ref[i]) {
				t.Fatalf("frame %d differs", i)
			}
		}
	})
}

type zeroReader struct{}

func (zeroReader) Read([]byte) (int, error) { return 0, nil }

// A reader that returns nothing and no error must not make ReadChunk spin forever.
func TestChunkReaderNoProgress(t *testing.T) {
	cr := NewChunkReader(zeroReader{}, EthernetHeaderLen, MaxFrameLen(1500))
	if _, err := cr.ReadChunk(make([]byte, cr.MinChunk())); !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("got %v, want io.ErrNoProgress", err)
	}
}

type steppedReader struct {
	b    []byte
	step int
}

func (s *steppedReader) Read(p []byte) (int, error) {
	if len(s.b) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), s.step, len(s.b))
	copy(p, s.b[:n])
	s.b = s.b[n:]
	return n, nil
}
