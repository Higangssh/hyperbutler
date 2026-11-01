package frame

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"testing"
	"testing/iotest"
	"time"
)

func TestCRCEncoding(t *testing.T) {
	body := vnetFrame(VnetHdr{}, 60)
	b := AppendCRC(nil, body)
	if n := binary.BigEndian.Uint32(b); int(n) != CRCLen+len(body) {
		t.Fatalf("length %d, want %d", n, CRCLen+len(body))
	}
	// The CRC is CRC-32C of the virtio_net_hdr and the frame, big-endian.
	if got, want := binary.BigEndian.Uint32(b[HeaderLen:]), crc32.Checksum(body, crc32.MakeTable(crc32.Castagnoli)); got != want {
		t.Fatalf("CRC %08x, want %08x", got, want)
	}
	if !bytes.Equal(b[HeaderLen+CRCLen:], body) || !CheckCRC(b[HeaderLen:]) {
		t.Fatal("frame does not round-trip")
	}
	// Any one bit flipped after the CRC was computed fails it.
	for i := HeaderLen; i < len(b); i++ {
		for bit := 0; bit < 8; bit++ {
			b[i] ^= 1 << bit
			if CheckCRC(b[HeaderLen:]) {
				t.Fatalf("byte %d bit %d flipped and the CRC still matches", i, bit)
			}
			b[i] ^= 1 << bit
		}
	}
	if CheckCRC(nil) || CheckCRC([]byte{0, 0, 0}) {
		t.Fatal("a short frame passes")
	}
	// SealCRC is the same CRC, written in place.
	c := append([]byte(nil), b[HeaderLen:]...)
	PutCRC(c, 0)
	SealCRC(c)
	if !bytes.Equal(c, b[HeaderLen:]) {
		t.Fatal("SealCRC differs from AppendCRC")
	}
}

func TestHello(t *testing.T) {
	if v, ok := ParseHello(Hello); !ok || v != VersionCRC {
		t.Fatalf("Hello is version %d (%v), want %d", v, ok, VersionCRC)
	}
	if h := HelloVersion(VersionNoCRC); h != [HeaderLen]byte{0xFF, 'h', 'b', 1} {
		t.Fatalf("version 1 Hello %x", h)
	}
	// A plain stream's first frame length can never be a Hello.
	if _, ok := ParseHello([HeaderLen]byte{0, 0, 5, 0xEE}); ok {
		t.Fatal("a length prefix parsed as a Hello")
	}
}

func TestLinkReportRoundTrip(t *testing.T) {
	b := AppendLinkReport(nil, LinkReport{CRCDropped: 1<<40 + 5})
	if len(b) != HeaderLen+CRCLen+MinVnetFrameLen {
		t.Fatalf("report of %d bytes", len(b))
	}
	f := b[HeaderLen:]
	if !CheckCRC(f) || !IsLinkReport(f[CRCLen:]) {
		t.Fatal("not a valid report")
	}
	r, err := ParseLinkReport(f[CRCLen:])
	if err != nil || r.CRCDropped != 1<<40+5 {
		t.Fatalf("%+v, %v", r, err)
	}
	// A TAP's frames are never reports, and ParseVnet refuses the flag.
	if IsLinkReport(vnetFrame(VnetHdr{Flags: VnetFlagNeedsCsum | VnetFlagDataValid}, 60)) {
		t.Fatal("a TAP frame is taken for a report")
	}
	if _, _, err := ParseVnet(f[CRCLen:], 1500); !errors.Is(err, ErrVnetHdr) {
		t.Fatalf("ParseVnet took a report: %v", err)
	}
	for _, bad := range [][]byte{f[CRCLen : len(f)-1], append(append([]byte{}, f[CRCLen:]...), 0)} {
		if _, err := ParseLinkReport(bad); err == nil {
			t.Fatalf("report of %d bytes accepted", len(bad))
		}
	}
}

// crcStream frames fs as version 2, and corrupts the frames in bad after their CRC was
// computed.
func crcStream(fs [][]byte, bad map[int]bool) []byte {
	var s []byte
	for i, f := range fs {
		at := len(s)
		s = AppendCRC(s, f)
		if bad[i] {
			s[at+HeaderLen+CRCLen+len(f)/2] ^= 0x20
		}
	}
	return s
}

func TestChunkReaderCRC(t *testing.T) {
	var fs [][]byte
	for i := 0; i < 60; i++ {
		fs = append(fs, pumpFrame(i))
	}
	bad := map[int]bool{0: true, 7: true, 8: true, 30: true, 59: true}
	stream := crcStream(fs, bad)
	for _, strict := range []bool{false, true} {
		var sizes []int
		cr := NewChunkReader(iotest.HalfReader(bytes.NewReader(stream)), EthernetHeaderLen, MaxFrameLen(9000)).
			WithCRC(CRCCheck{Strict: strict, Bad: func(n int) { sizes = append(sizes, n) }})
		var got []int
		next := 0
		err := Pump(cr, 2, 0, func(f []byte) error {
			for bad[next] {
				next++
			}
			if !bytes.Equal(f, fs[next]) {
				t.Fatalf("strict %v: frame %d differs (%d bytes)", strict, next, len(f))
			}
			got = append(got, next)
			next++
			return nil
		}, func() {})
		if strict {
			if !errors.Is(err, ErrCRC) || len(got) != 0 || len(sizes) != 1 || sizes[0] != CRCLen+len(fs[0]) {
				t.Fatalf("strict: %v after %v, bad sizes %v", err, got, sizes)
			}
			continue
		}
		if !errors.Is(err, io.EOF) || len(got) != len(fs)-len(bad) || len(sizes) != len(bad) {
			t.Fatalf("Pump: %v after %d frames, %d bad (want EOF, %d, %d)", err, len(got), len(sizes), len(fs)-len(bad), len(bad))
		}
	}
}

// A chunk whose frames all fail their CRC is not returned empty: the reader reads on.
func TestChunkReaderCRCAllBad(t *testing.T) {
	fs := [][]byte{pumpFrame(1), pumpFrame(2), pumpFrame(3)}
	stream := crcStream(fs, map[int]bool{0: true, 1: true})
	r := &steppedReader{b: stream, step: len(stream)}
	n := 0
	cr := NewChunkReader(r, EthernetHeaderLen, MaxFrameLen(1500)).WithCRC(CRCCheck{Bad: func(int) { n++ }})
	buf := make([]byte, cr.MinChunk())
	chunk, err := cr.ReadChunk(buf)
	if err != nil || n != 2 {
		t.Fatalf("ReadChunk: %v, %d bad", err, n)
	}
	var got [][]byte
	_ = cr.EachFrame(chunk, func(f []byte) error { got = append(got, f); return nil })
	if len(got) != 1 || !bytes.Equal(got[0], fs[2]) {
		t.Fatalf("got %d frames", len(got))
	}
	if _, err := cr.ReadChunk(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("then %v, want EOF", err)
	}
}

func TestReaderCRC(t *testing.T) {
	fs := [][]byte{vnetFrame(VnetHdr{}, 60), vnetFrame(VnetHdr{}, 1514), vnetFrame(VnetHdr{}, 100)}
	fr := NewVnetReader(bytes.NewReader(crcStream(fs, map[int]bool{1: true}))).WithCRC()
	for i, f := range fs {
		b, err := fr.Next()
		if i == 1 {
			if !errors.Is(err, ErrCRC) {
				t.Fatalf("frame 1: %v, want ErrCRC", err)
			}
			continue
		}
		if err != nil || !bytes.Equal(b, f) {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	if _, err := fr.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("at the end: %v", err)
	}
	// The bounds count the CRC: a frame too short to have one is refused.
	fr = NewVnetReader(bytes.NewReader(Append(nil, make([]byte, MinVnetFrameLen)))).WithCRC()
	if _, err := fr.Next(); !errors.Is(err, ErrFrameSize) {
		t.Fatalf("a frame without room for a CRC: %v", err)
	}
}

func TestMismatchLog(t *testing.T) {
	var l MismatchLog
	now := time.Unix(1000, 0)
	logged := 0
	for i := 0; i < 100; i++ {
		if ok, _ := l.Note(now); ok {
			logged++
		}
	}
	if logged != MismatchLogFirst {
		t.Fatalf("%d of 100 logged at once, want %d", logged, MismatchLogFirst)
	}
	ok, skipped := l.Note(now.Add(MismatchLogEvery))
	if !ok || skipped != 100-MismatchLogFirst {
		t.Fatalf("a minute later: %v, %d not logged; want true, %d", ok, skipped, 100-MismatchLogFirst)
	}
	if ok, _ := l.Note(now.Add(MismatchLogEvery + time.Second)); ok {
		t.Fatal("logged again within the interval")
	}
}

// BenchmarkCRC is the cost of the CRC of one 64 KiB segment, on each side of the link.
func BenchmarkCRC(b *testing.B) {
	p := make([]byte, 64<<10)
	b.SetBytes(int64(len(p)))
	for b.Loop() {
		_ = UpdateCRC(0, p)
	}
}

// FuzzChunkReaderCRC: arbitrary version 2 streams, read in arbitrary pieces. The frames
// returned are exactly those of a frame-by-frame parse whose CRC matches, in order and
// without their CRC; every other whole frame is reported as bad; nothing panics. The
// fuzzer may also flip one byte of a well-formed stream.
func FuzzChunkReaderCRC(f *testing.F) {
	good := crcStream([][]byte{pumpFrame(1), vnetFrame(VnetHdr{}, 1514), pumpFrame(0)}, nil)
	f.Add(good, uint8(7), uint16(0), false)
	f.Add(good, uint8(1), uint16(30), true)
	f.Add(AppendLinkReport(nil, LinkReport{CRCDropped: 3}), uint8(3), uint16(0), false)
	f.Add([]byte{0, 0, 0, 18, 0xFF, 0xFF, 0xFF, 0xFF}, uint8(2), uint16(0), false)
	f.Fuzz(func(t *testing.T, in []byte, step uint8, flipAt uint16, flip bool) {
		if flip && len(in) > 0 {
			in = append([]byte(nil), in...)
			in[int(flipAt)%len(in)] ^= 0x01
		}
		const min, max = EthernetHeaderLen, MinVnetFrameLen + 2000
		// The reference: whole frames of in with valid lengths, split by their CRC.
		var want [][]byte
		wantBad := 0
		for rest := in; len(rest) >= HeaderLen; {
			n := int(binary.BigEndian.Uint32(rest))
			if n < min+CRCLen || n > max+CRCLen || len(rest) < HeaderLen+n {
				break
			}
			if fr := rest[HeaderLen : HeaderLen+n]; CheckCRC(fr) {
				want = append(want, fr[CRCLen:])
			} else {
				wantBad++
			}
			rest = rest[HeaderLen+n:]
		}
		for _, strict := range []bool{false, true} {
			bad := 0
			cr := NewChunkReader(&steppedReader{b: in, step: int(step) + 1}, min, max).
				WithCRC(CRCCheck{Strict: strict, Bad: func(n int) {
					if n < min+CRCLen || n > max+CRCLen {
						t.Fatalf("bad frame of %d bytes", n)
					}
					bad++
				}})
			buf := make([]byte, cr.MinChunk())
			var got [][]byte
			var err error
			for i := 0; i < len(in)+2; i++ {
				var chunk []byte
				if chunk, err = cr.ReadChunk(buf); err != nil {
					break
				}
				if err := cr.EachFrame(chunk, func(fr []byte) error {
					if len(fr) < min || len(fr) > max {
						t.Fatalf("frame of %d bytes", len(fr))
					}
					got = append(got, append([]byte(nil), fr...))
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if strict && wantBad > 0 {
				// Only the frames before the first bad one, then ErrCRC.
				if !errors.Is(err, ErrCRC) || bad != 1 || len(got) > len(want) {
					t.Fatalf("strict: %v, %d bad, %d frames", err, bad, len(got))
				}
				for i := range got {
					if !bytes.Equal(got[i], want[i]) {
						t.Fatalf("strict: frame %d differs", i)
					}
				}
				continue
			}
			if len(got) != len(want) || bad != wantBad {
				t.Fatalf("strict %v: %d frames and %d bad, want %d and %d", strict, len(got), bad, len(want), wantBad)
			}
			for i := range want {
				if !bytes.Equal(got[i], want[i]) {
					t.Fatalf("frame %d differs", i)
				}
			}
		}
	})
}
