package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// fuzzFrame builds a raw frame with a correct length prefix.
func fuzzFrame(version, typ uint8, fields ...byte) []byte {
	b := []byte{0, 0, version, typ}
	b = append(b, fields...)
	binary.BigEndian.PutUint16(b, uint16(len(b)-2))
	return b
}

func fuzzU32(tag uint8, v uint32) []byte {
	return append([]byte{tag, 4}, binary.LittleEndian.AppendUint32(nil, v)...)
}

// fuzzRequestSeeds is one valid frame of every request shape plus malformed variants.
func fuzzRequestSeeds() [][]byte {
	var out [][]byte
	for _, r := range []Request{
		{Op: OpVersion},
		{Op: OpStart},
		{Op: OpStop},
		{Op: OpStop, Force: true},
		{Op: OpStatus},
		{Op: OpResize, CPUs: 4},
		{Op: OpResize, MemoryMB: 2048},
		{Op: OpResize, CPUs: MaxCPUs, MemoryMB: MaxMemoryMB},
		{Op: OpSetDataLocation, Handle: 0x1a4},
		{Op: OpSetDataLocation, Handle: MaxHandle},
	} {
		b, err := EncodeRequest(r)
		if err != nil {
			panic(err)
		}
		out = append(out, b)
	}
	start := fuzzFrame(Version, uint8(OpStart))
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	out = append(out,
		nil,
		[]byte{0, 2, 1},
		append(append([]byte{}, start...), 0), // trailing byte
		[]byte{0, 3, 1, 2},                    // length too long
		[]byte{0, 1, 1, 2},                    // length too short
		append(fuzzFrame(Version, 2), make([]byte, 600)...),    // oversize
		fuzzFrame(Version, 2, make([]byte, MaxFrameSize-4)...), // exactly MaxFrameSize, junk fields
		fuzzFrame(0, uint8(OpStart)),
		fuzzFrame(2, uint8(OpStart)),
		fuzzFrame(Version, 7),
		fuzzFrame(Version, 0),
		fuzzFrame(Version, ReplyFlag|uint8(OpStop)),
		fuzzFrame(Version, uint8(OpStop), 200, 0),
		fuzzFrame(Version, uint8(OpResize), cat(fuzzU32(2, 1), fuzzU32(2, 2))...),
		fuzzFrame(Version, uint8(OpResize), cat(fuzzU32(3, 1024), fuzzU32(2, 2))...),
		fuzzFrame(Version, uint8(OpStop), 1),
		fuzzFrame(Version, uint8(OpStop), 1, 2, 1),
		fuzzFrame(Version, uint8(OpStop), 1, 1, 0), // bool false encoded
		fuzzFrame(Version, uint8(OpStop), 1, 1, 2),
		fuzzFrame(Version, uint8(OpResize), cat(fuzzU32(2, 0), fuzzU32(3, 1024))...),
		fuzzFrame(Version, uint8(OpResize), fuzzU32(3, 0)...),
		fuzzFrame(Version, uint8(OpResize), fuzzU32(2, MaxCPUs+1)...),
		fuzzFrame(Version, uint8(OpResize), fuzzU32(3, MinMemoryMB-1)...),
		fuzzFrame(Version, uint8(OpSetDataLocation)),
		fuzzFrame(Version, uint8(OpSetDataLocation), append([]byte{4, 8}, bytes.Repeat([]byte{0xff}, 8)...)...), // pseudo-handle -1
		fuzzFrame(Version, uint8(OpSetDataLocation), append([]byte{4, 8}, make([]byte, 8)...)...),               // handle 0
		fuzzFrame(Version, uint8(OpSetDataLocation), fuzzU32(4, 0x10)...),
		fuzzFrame(Version, uint8(OpStart), 1, 1, 1), // tag not allowed for start
	)
	return out
}

func fuzzReplySeeds() [][]byte {
	id := [16]byte{1, 2, 3}
	var out [][]byte
	for _, r := range []Reply{
		{Type: ReplyFlag | uint8(OpVersion), ProtoVersion: Version, BrokerVersion: "0.1.0-dev"},
		{Type: ReplyFlag | uint8(OpVersion), ProtoVersion: 255, BrokerVersion: strings.Repeat("~", MaxVersionLen)},
		{Type: ReplyFlag | uint8(OpStart), State: StateRunning, CPUs: 2, MemoryMB: 2048, VMID: id, DataLocationSet: true},
		{Type: ReplyFlag | uint8(OpStatus), State: StateStopped, CPUs: 2, MemoryMB: 2048},
		{Type: ReplyFlag | uint8(OpResize), State: StateStopped, CPUs: 1, MemoryMB: MinMemoryMB},
		{Type: ReplyFlag | uint8(OpStop)},
		{Type: ReplyFlag | uint8(OpSetDataLocation)},
		{Type: TypeError, Code: CodeBadRequest},
		{Type: TypeError, Code: CodeInternal, Message: strings.Repeat("x", MaxMessageLen)},
	} {
		b, err := EncodeReply(r)
		if err != nil {
			panic(err)
		}
		out = append(out, b)
	}
	out = append(out,
		fuzzFrame(Version, TypeError),
		fuzzFrame(Version, TypeError, 10, 2, 0, 0),
		fuzzFrame(Version, TypeError, 10, 2, 99, 0),
		fuzzFrame(Version, TypeError, 10, 2, 1, 0, 11, 0),
		fuzzFrame(Version, TypeError, 10, 2, 1, 0, 11, 1, 0x80),
		fuzzFrame(Version, TypeError, 11, 1, 'x', 10, 2, 1, 0), // out of order
		fuzzFrame(Version, ReplyFlag|uint8(OpStatus), 7, 1, 2),
		fuzzFrame(Version, uint8(OpStatus)),
		fuzzFrame(Version, ReplyFlag|uint8(OpStop), 8, 16),
		fuzzFrame(Version, ReplyFlag|7),
		fuzzFrame(Version, ReplyFlag|uint8(OpVersion), 5, 1, 1),
		fuzzFrame(Version, ReplyFlag|uint8(OpVersion), 5, 1, 0, 6, 1, 'v'), // proto version 0 encoded
		fuzzFrame(Version, ReplyFlag|uint8(OpStatus), append(append(fuzzU32(2, 2), fuzzU32(3, 2048)...), 7, 1, 1, 8, 16, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)...), // zero VMID
		fuzzFrame(Version, ReplyFlag|uint8(OpStatus), append(append(fuzzU32(2, 2), fuzzU32(3, 2048)...), 7, 1, 1, 9, 1, 0)...),                                               // bool 0
	)
	return out
}

// checkDecodeAllocs fails if decode allocates more than max times on b. The decoder holds
// its fields in fixed arrays and copies at most one string of at most MaxMessageLen bytes,
// so nothing should scale with an attacker-chosen length.
func checkDecodeAllocs(t *testing.T, name string, b []byte, max float64, decode func([]byte)) {
	t.Helper()
	if n := testing.AllocsPerRun(2, func() { decode(b) }); n > max {
		t.Fatalf("%s(%x) allocates %v times, want <= %v", name, b, n, max)
	}
}

// FuzzDecodeRequest: no panic; oversize input is always rejected; accepted input passes
// Validate, re-encodes to exactly the input (canonical), is not also a reply, and decodes
// with a bounded number of allocations.
func FuzzDecodeRequest(f *testing.F) {
	for _, b := range fuzzRequestSeeds() {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := DecodeRequest(b)
		if err != nil {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("DecodeRequest(%x): error %v does not wrap ErrInvalid", b, err)
			}
			if r != (Request{}) {
				t.Fatalf("DecodeRequest(%x): non-zero %+v with error", b, r)
			}
			if len(b) > MaxFrameSize && !errors.Is(err, ErrFrameTooLarge) {
				t.Fatalf("DecodeRequest: %d-byte input rejected with %v, want ErrFrameTooLarge", len(b), err)
			}
			if len(b) >= headerSize && len(b) <= MaxFrameSize && int(binary.BigEndian.Uint16(b)) == len(b)-2 && b[2] != Version &&
				!errors.Is(err, ErrUnsupportedVersion) {
				t.Fatalf("DecodeRequest(%x): version %d rejected with %v, want ErrUnsupportedVersion", b, b[2], err)
			}
			checkDecodeAllocs(t, "DecodeRequest", b, 8, func(b []byte) { _, _ = DecodeRequest(b) })
			return
		}
		if len(b) > MaxFrameSize {
			t.Fatalf("DecodeRequest accepted %d-byte input", len(b))
		}
		if err := r.Validate(); err != nil {
			t.Fatalf("DecodeRequest(%x) = %+v, which fails Validate: %v", b, r, err)
		}
		again, err := EncodeRequest(r)
		if err != nil {
			t.Fatalf("EncodeRequest(%+v) after decoding %x: %v", r, b, err)
		}
		if !bytes.Equal(again, b) {
			t.Fatalf("not canonical: %x decodes to %+v, which encodes to %x", b, r, again)
		}
		if rep, err := DecodeReply(b); err == nil {
			t.Fatalf("%x decodes as both request %+v and reply %+v", b, r, rep)
		}
		var buf [MaxFrameSize]byte
		got, err := ReadFrame(bytes.NewReader(b), &buf)
		if err != nil || !bytes.Equal(got, b) {
			t.Fatalf("ReadFrame of accepted request %x = %x, %v", b, got, err)
		}
		checkDecodeAllocs(t, "DecodeRequest", b, 1, func(b []byte) { _, _ = DecodeRequest(b) })
	})
}

// FuzzDecodeReply: the same properties as FuzzDecodeRequest, for replies.
func FuzzDecodeReply(f *testing.F) {
	for _, b := range fuzzReplySeeds() {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := DecodeReply(b)
		if err != nil {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("DecodeReply(%x): error %v does not wrap ErrInvalid", b, err)
			}
			if r != (Reply{}) {
				t.Fatalf("DecodeReply(%x): non-zero %+v with error", b, r)
			}
			if len(b) > MaxFrameSize && !errors.Is(err, ErrFrameTooLarge) {
				t.Fatalf("DecodeReply: %d-byte input rejected with %v, want ErrFrameTooLarge", len(b), err)
			}
			checkDecodeAllocs(t, "DecodeReply", b, 8, func(b []byte) { _, _ = DecodeReply(b) })
			return
		}
		if len(b) > MaxFrameSize {
			t.Fatalf("DecodeReply accepted %d-byte input", len(b))
		}
		if err := r.Validate(); err != nil {
			t.Fatalf("DecodeReply(%x) = %+v, which fails Validate: %v", b, r, err)
		}
		if r.IsError() != (b[3] == TypeError) || r.Type != b[3] {
			t.Fatalf("DecodeReply(%x): type %#x", b, r.Type)
		}
		if len(r.Message) > MaxMessageLen || len(r.BrokerVersion) > MaxVersionLen {
			t.Fatalf("DecodeReply(%x): oversize string in %+v", b, r)
		}
		again, err := EncodeReply(r)
		if err != nil {
			t.Fatalf("EncodeReply(%+v) after decoding %x: %v", r, b, err)
		}
		if !bytes.Equal(again, b) {
			t.Fatalf("not canonical: %x decodes to %+v, which encodes to %x", b, r, again)
		}
		if req, err := DecodeRequest(b); err == nil {
			t.Fatalf("%x decodes as both reply %+v and request %+v", b, r, req)
		}
		// One fields struct plus at most one string copy (no type has two strings).
		checkDecodeAllocs(t, "DecodeReply", b, 2, func(b []byte) { _, _ = DecodeReply(b) })
	})
}

// countingReader counts the bytes read from the underlying reader.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// FuzzReadFrame reads frames from arbitrary bytes until an error, optionally through a
// reader that returns one byte at a time. Each successful frame is exactly the bytes
// consumed for it and at most MaxFrameSize long; a rejected length prefix consumes only
// the prefix; nothing is read past a frame; it never panics or loops.
func FuzzReadFrame(f *testing.F) {
	var stream []byte
	for _, b := range fuzzRequestSeeds()[:10] {
		stream = append(stream, b...)
	}
	f.Add(stream, false)
	f.Add(stream, true)
	for _, b := range fuzzRequestSeeds()[10:] {
		f.Add(b, false)
	}
	for _, b := range fuzzReplySeeds() {
		f.Add(b, true)
	}
	f.Add([]byte{0xff, 0xff, 1, 2, 3}, false)
	f.Add([]byte{0x01, 0xfe}, false)                                          // MaxFrameSize-2, no body
	f.Add(append([]byte{0x01, 0xfe}, make([]byte, MaxFrameSize-2)...), false) // largest frame
	f.Add(append([]byte{0x01, 0xff}, make([]byte, MaxFrameSize)...), false)   // one too large
	f.Add([]byte{0, 0}, false)
	f.Add([]byte{0}, true)
	f.Fuzz(func(t *testing.T, in []byte, oneByte bool) {
		var buf [MaxFrameSize]byte
		br := bytes.NewReader(in)
		cr := &countingReader{r: br}
		var r io.Reader = cr
		if oneByte {
			r = iotest.OneByteReader(cr)
		}
		off := 0
		for i := 0; ; i++ {
			if i > len(in)/4+1 {
				t.Fatalf("read %d frames from %d bytes", i, len(in))
			}
			before := cr.n
			got, err := ReadFrame(r, &buf)
			used := cr.n - before
			if used > MaxFrameSize {
				t.Fatalf("frame %d: consumed %d bytes", i, used)
			}
			if err != nil {
				if got != nil {
					t.Fatalf("frame %d: %x returned with error %v", i, got, err)
				}
				rest := len(in) - off
				switch {
				case err == io.EOF:
					if rest != 0 || used != 0 {
						t.Fatalf("frame %d: io.EOF with %d bytes left, %d consumed", i, rest, used)
					}
				case err == io.ErrUnexpectedEOF:
					if used != rest || rest == 0 {
						t.Fatalf("frame %d: unexpected EOF after %d of %d remaining bytes", i, used, rest)
					}
				case errors.Is(err, ErrFrameTooLarge), errors.Is(err, ErrInvalid):
					if used != 2 {
						t.Fatalf("frame %d: bad length %v consumed %d bytes, want 2", i, err, used)
					}
					n := int(binary.BigEndian.Uint16(in[off:]))
					if errors.Is(err, ErrFrameTooLarge) != (n > MaxFrameSize-2) {
						t.Fatalf("frame %d: length %d gave %v", i, n, err)
					}
				default:
					t.Fatalf("frame %d: unexpected error %v", i, err)
				}
				return
			}
			if len(got) > MaxFrameSize || len(got) < headerSize {
				t.Fatalf("frame %d: %d bytes", i, len(got))
			}
			if len(got) != used {
				t.Fatalf("frame %d: returned %d bytes but consumed %d", i, len(got), used)
			}
			if &got[0] != &buf[0] {
				t.Fatalf("frame %d: not a sub-slice of buf", i)
			}
			if int(binary.BigEndian.Uint16(got)) != len(got)-2 {
				t.Fatalf("frame %d: length prefix %x for %d bytes", i, got[:2], len(got))
			}
			if !bytes.Equal(got, in[off:off+used]) {
				t.Fatalf("frame %d: %x is not the input at offset %d", i, got, off)
			}
			off += used
			if br.Len() != len(in)-off {
				t.Fatalf("frame %d: reader has %d bytes left, want %d", i, br.Len(), len(in)-off)
			}
		}
	})
}
