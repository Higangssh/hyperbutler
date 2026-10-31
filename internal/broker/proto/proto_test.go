package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

// frame builds a raw frame from a type and raw field bytes, with a correct length.
func frame(version, typ uint8, fields ...byte) []byte {
	b := []byte{0, 0, version, typ}
	b = append(b, fields...)
	binary.BigEndian.PutUint16(b, uint16(len(b)-2))
	return b
}

func TestRequestRoundTrip(t *testing.T) {
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
			t.Fatalf("encode %+v: %v", r, err)
		}
		got, err := DecodeRequest(b)
		if err != nil {
			t.Fatalf("decode %+v (%x): %v", r, b, err)
		}
		if got != r {
			t.Fatalf("round trip: %+v != %+v", got, r)
		}
	}
}

func TestReplyRoundTrip(t *testing.T) {
	id := [16]byte{1, 2, 3}
	for _, r := range []Reply{
		{Type: ReplyFlag | uint8(OpVersion), ProtoVersion: Version, BrokerVersion: "0.1.0-dev"},
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
			t.Fatalf("encode %+v: %v", r, err)
		}
		got, err := DecodeReply(b)
		if err != nil {
			t.Fatalf("decode %+v (%x): %v", r, b, err)
		}
		if got != r {
			t.Fatalf("round trip: %+v != %+v", got, r)
		}
	}
}

func TestEncodeRejectsInvalid(t *testing.T) {
	for _, r := range []Request{
		{},
		{Op: 7},
		{Op: OpStart, Force: true},
		{Op: OpStatus, Handle: 4},
		{Op: OpResize},
		{Op: OpResize, CPUs: MaxCPUs + 1},
		{Op: OpResize, MemoryMB: MinMemoryMB - 1},
		{Op: OpResize, MemoryMB: MaxMemoryMB + 1},
		{Op: OpSetDataLocation},
		{Op: OpSetDataLocation, Handle: MaxHandle + 1},
		{Op: OpSetDataLocation, Handle: ^uint64(0)}, // INVALID_HANDLE_VALUE / current process
	} {
		if _, err := EncodeRequest(r); !errors.Is(err, ErrInvalid) {
			t.Errorf("EncodeRequest(%+v) = %v, want ErrInvalid", r, err)
		}
	}
	for _, r := range []Reply{
		{},
		{Type: uint8(OpStart)},
		{Type: TypeError},
		{Type: TypeError, Code: maxCode + 1},
		{Type: TypeError, Code: CodeBusy, CPUs: 1},
		{Type: TypeError, Code: CodeBusy, Message: "bad\nline"},
		{Type: ReplyFlag | uint8(OpStatus), State: StateRunning, CPUs: 1},
		{Type: ReplyFlag | uint8(OpStatus), State: 3, CPUs: 1, MemoryMB: 1024},
		{Type: ReplyFlag | uint8(OpStop), State: StateRunning},
		{Type: ReplyFlag | uint8(OpVersion), ProtoVersion: 1},
		{Type: ReplyFlag | uint8(OpVersion), ProtoVersion: 1, BrokerVersion: strings.Repeat("v", MaxVersionLen+1)},
	} {
		if _, err := EncodeReply(r); !errors.Is(err, ErrInvalid) {
			t.Errorf("EncodeReply(%+v) = %v, want ErrInvalid", r, err)
		}
	}
}

func TestDecodeStrict(t *testing.T) {
	u32 := func(tag uint8, v uint32) []byte {
		return append([]byte{tag, 4}, binary.LittleEndian.AppendUint32(nil, v)...)
	}
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	start := frame(Version, uint8(OpStart))
	tests := []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"short", []byte{0, 2, 1}},
		{"trailing byte", append(append([]byte{}, start...), 0)},
		{"length too long", []byte{0, 3, 1, 2}},
		{"length too short", []byte{0, 1, 1, 2}},
		{"oversize", append(frame(Version, uint8(OpStart)), make([]byte, MaxFrameSize)...)},
		{"version 0", frame(0, uint8(OpStart))},
		{"version 2", frame(2, uint8(OpStart))},
		{"unknown op", frame(Version, 7)},
		{"op 0", frame(Version, 0)},
		{"reply as request", frame(Version, ReplyFlag|uint8(OpStop))},
		{"unknown tag", frame(Version, uint8(OpStart), 1, 1, 1)},
		{"unknown tag 200", frame(Version, uint8(OpStop), 200, 0)},
		{"duplicate tag", frame(Version, uint8(OpResize), cat(u32(2, 1), u32(2, 2))...)},
		{"out of order", frame(Version, uint8(OpResize), cat(u32(3, 1024), u32(2, 2))...)},
		{"truncated field header", frame(Version, uint8(OpStop), 1)},
		{"truncated value", frame(Version, uint8(OpStop), 1, 2, 1)},
		{"bool false encoded", frame(Version, uint8(OpStop), 1, 1, 0)},
		{"bool 2", frame(Version, uint8(OpStop), 1, 1, 2)},
		{"u32 short", frame(Version, uint8(OpResize), 2, 3, 1, 0, 0)},
		{"resize empty", frame(Version, uint8(OpResize))},
		{"resize zero encoded", frame(Version, uint8(OpResize), cat(u32(2, 0), u32(3, 1024))...)},
		{"resize too many CPUs", frame(Version, uint8(OpResize), u32(2, MaxCPUs+1)...)},
		{"handle missing", frame(Version, uint8(OpSetDataLocation))},
		{"handle pseudo", frame(Version, uint8(OpSetDataLocation), append([]byte{4, 8}, bytes.Repeat([]byte{0xff}, 8)...)...)},
		{"handle as u32", frame(Version, uint8(OpSetDataLocation), u32(4, 0x10)...)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if r, err := DecodeRequest(tt.in); !errors.Is(err, ErrInvalid) {
				t.Fatalf("DecodeRequest(%x) = %+v, %v; want ErrInvalid", tt.in, r, err)
			}
		})
	}
	if _, err := DecodeRequest(frame(2, uint8(OpStart))); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("version 2: %v, want ErrUnsupportedVersion", err)
	}
}

func TestDecodeReplyStrict(t *testing.T) {
	for _, in := range [][]byte{
		frame(Version, TypeError),                           // code missing
		frame(Version, TypeError, 10, 2, 0, 0),              // code 0
		frame(Version, TypeError, 10, 2, 99, 0),             // unknown code
		frame(Version, TypeError, 10, 2, 1, 0, 11, 0),       // empty message
		frame(Version, TypeError, 10, 2, 1, 0, 11, 1, 0x80), // non-ASCII message
		frame(Version, ReplyFlag|uint8(OpStatus), 7, 1, 2),  // CPUs, memory missing
		frame(Version, uint8(OpStatus)),                     // request as reply
		frame(Version, ReplyFlag|uint8(OpStop), 8, 16),      // VMID truncated
		frame(Version, ReplyFlag|7),                         // unknown reply
		frame(Version, ReplyFlag|uint8(OpVersion), 5, 1, 1), // broker version missing
	} {
		if r, err := DecodeReply(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("DecodeReply(%x) = %+v, %v; want ErrInvalid", in, r, err)
		}
	}
}

func TestReadFrame(t *testing.T) {
	var buf [MaxFrameSize]byte
	a, _ := EncodeRequest(Request{Op: OpStatus})
	b, _ := EncodeRequest(Request{Op: OpResize, CPUs: 2})
	r := bytes.NewReader(append(append([]byte{}, a...), b...))
	for _, want := range [][]byte{a, b} {
		got, err := ReadFrame(r, &buf)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("ReadFrame = %x, %v; want %x", got, err, want)
		}
	}
	if _, err := ReadFrame(r, &buf); err != io.EOF { //nolint:errorlint // ReadFrame's contract is to return io.EOF and io.ErrUnexpectedEOF themselves, unwrapped, like io.ReadFull
		t.Fatalf("at end: %v, want io.EOF", err)
	}

	// An oversize length is rejected without reading (or waiting for) the body.
	big := bytes.NewReader([]byte{0xff, 0xff, 1, 2, 3})
	if _, err := ReadFrame(big, &buf); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversize: %v", err)
	}
	if big.Len() != 3 {
		t.Fatalf("oversize: read %d body bytes", 3-big.Len())
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{0, 1, 1}), &buf); !errors.Is(err, ErrInvalid) {
		t.Fatalf("length 1: %v", err)
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{0, 4, 1, 2}), &buf); err != io.ErrUnexpectedEOF { //nolint:errorlint // ReadFrame's contract is to return io.EOF and io.ErrUnexpectedEOF themselves, unwrapped, like io.ReadFull
		t.Fatalf("truncated body: %v", err)
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{0}), &buf); err != io.ErrUnexpectedEOF { //nolint:errorlint // ReadFrame's contract is to return io.EOF and io.ErrUnexpectedEOF themselves, unwrapped, like io.ReadFull
		t.Fatalf("truncated length: %v", err)
	}
}

func TestDecodeDoesNotAllocateMuch(t *testing.T) {
	b, _ := EncodeRequest(Request{Op: OpResize, CPUs: 2, MemoryMB: 4096})
	n := testing.AllocsPerRun(100, func() { _, _ = DecodeRequest(b) })
	if n > 1 {
		t.Fatalf("DecodeRequest allocates %v times per call", n)
	}
}
