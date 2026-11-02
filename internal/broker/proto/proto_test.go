package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/vmsecret"
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
		{Op: 9},
		{Op: OpOpenEndpoint, Force: true},
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
		{Type: ReplyFlag | uint8(OpStatus), State: 4, CPUs: 1, MemoryMB: 1024},
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
		{"unknown op", frame(Version, 9)},
		{"open-endpoint with a field", frame(Version, uint8(OpOpenEndpoint), 1, 1, 1)},
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
		frame(Version, ReplyFlag|9),                         // unknown reply
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

// startNet is a start with every feature and the default network.
var startNet = Request{Op: OpStart, CPUs: 2, MemoryMB: 4096,
	Features: FeatureNet | FeatureShare | FeatureDaemonConfig | FeatureBlankDisk,
	NetAddr:  [4]byte{192, 168, 127, 0}, NetPrefix: 24, NetMTU: 9000, NetFlags: NetOffload | NetHostGateway}

func TestStartAndPauseRoundTrip(t *testing.T) {
	for _, r := range []Request{
		startNet,
		{Op: OpStart, Features: FeatureShare},
		{Op: OpStart, MemoryMB: MaxMemoryMB},
		{Op: OpStart, Features: FeatureNet, NetAddr: [4]byte{10, 0, 0, 0}, NetPrefix: MinNetPrefix, NetMTU: MinMTU},
		{Op: OpStart, Features: FeatureNet, NetAddr: [4]byte{172, 31, 255, 248}, NetPrefix: MaxNetPrefix, NetMTU: MaxMTU, NetFlags: NetOffload | NetCRCStrict},
		{Op: OpStart, Features: FeatureNet, NetAddr: [4]byte{10, 20, 30, 64}, NetPrefix: 26, NetMTU: 1500, NetFlags: NetOffload | NetCRCOff},
		{Op: OpPause},
		{Op: OpPause, Resume: true},
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
	for _, st := range []State{StateStopped, StateRunning, StatePaused} {
		r := Reply{Type: ReplyFlag | uint8(OpPause), State: st, CPUs: 2, MemoryMB: 2048, VMID: [16]byte{9}}
		b, err := EncodeReply(r)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := DecodeReply(b); err != nil || got != r {
			t.Fatalf("pause reply %+v: %+v, %v", r, got, err)
		}
	}
	for _, c := range []Code{CodeImageRejected, CodeDiskRejected, CodeLeaseHeld} {
		r := Reply{Type: TypeError, Code: c, Message: "x"}
		b, err := EncodeReply(r)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := DecodeReply(b); err != nil || got != r {
			t.Fatalf("error %d: %+v, %v", c, got, err)
		}
	}
}

func TestStartRejectsInvalid(t *testing.T) {
	net := func(mut func(*Request)) Request {
		r := startNet
		mut(&r)
		return r
	}
	for name, r := range map[string]Request{
		"cpus":                {Op: OpStart, CPUs: MaxCPUs + 1},
		"memory low":          {Op: OpStart, MemoryMB: MinMemoryMB - 1},
		"unknown feature":     {Op: OpStart, Features: 0x10},
		"net without bit":     {Op: OpStart, NetMTU: 1500},
		"addr without bit":    {Op: OpStart, NetAddr: [4]byte{10}},
		"flags without bit":   {Op: OpStart, NetFlags: NetOffload},
		"no prefix":           net(func(r *Request) { r.NetPrefix = 0 }),
		"prefix 7":            net(func(r *Request) { r.NetPrefix = 7 }),
		"prefix 30":           net(func(r *Request) { r.NetPrefix = 30 }),
		"host bits":           net(func(r *Request) { r.NetAddr = [4]byte{192, 168, 127, 1} }),
		"public":              net(func(r *Request) { r.NetAddr = [4]byte{8, 8, 8, 0} }),
		"172.32":              net(func(r *Request) { r.NetAddr, r.NetPrefix = [4]byte{172, 32, 0, 0}, 16 }),
		"no addr":             net(func(r *Request) { r.NetAddr = [4]byte{} }),
		"mtu low":             net(func(r *Request) { r.NetMTU = MinMTU - 1 }),
		"mtu high":            net(func(r *Request) { r.NetMTU = MaxMTU + 1 }),
		"no mtu":              net(func(r *Request) { r.NetMTU = 0 }),
		"unknown flag":        net(func(r *Request) { r.NetFlags |= 0x10 }),
		"both crc":            net(func(r *Request) { r.NetFlags |= NetCRCOff | NetCRCStrict }),
		"crc without offload": net(func(r *Request) { r.NetFlags = NetCRCOff }),
		"resume on start":     net(func(r *Request) { r.Resume = true }),
		"force on pause":      {Op: OpPause, Force: true},
		"cpus on pause":       {Op: OpPause, CPUs: 1},
		"resume on stop":      {Op: OpStop, Resume: true},
		"features on resize":  {Op: OpResize, CPUs: 1, Features: FeatureNet},
	} {
		if _, err := EncodeRequest(r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: EncodeRequest(%+v) = %v, want ErrInvalid", name, r, err)
		}
	}
	good, err := EncodeRequest(startNet)
	if err != nil {
		t.Fatal(err)
	}
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	fields := good[headerSize:]
	for name, in := range map[string][]byte{
		"zero features encoded": frame(Version, uint8(OpStart), tagFeatures, 1, 0),
		"zero flags encoded":    frame(Version, uint8(OpStart), cat(fields, []byte{tagNetFlags, 1, 0})...),
		"zero address":          frame(Version, uint8(OpStart), tagFeatures, 1, FeatureNet, tagNetAddr, 4, 0, 0, 0, 0, tagNetPrefix, 1, 24, tagNetMTU, 2, 0x28, 0x23),
		"address short":         frame(Version, uint8(OpStart), tagFeatures, 1, FeatureNet, tagNetAddr, 3, 10, 0, 0, tagNetPrefix, 1, 24, tagNetMTU, 2, 0x28, 0x23),
		"mtu as u8":             frame(Version, uint8(OpStart), tagFeatures, 1, FeatureNet, tagNetAddr, 4, 10, 0, 0, 0, tagNetPrefix, 1, 24, tagNetMTU, 1, 0x28),
		"zero mtu encoded":      frame(Version, uint8(OpStart), tagFeatures, 1, FeatureNet, tagNetAddr, 4, 10, 0, 0, 0, tagNetPrefix, 1, 24, tagNetMTU, 2, 0, 0),
		"resume false encoded":  frame(Version, uint8(OpPause), tagResume, 1, 0),
		"handle on pause":       frame(Version, uint8(OpPause), append([]byte{tagHandle, 8}, 1, 0, 0, 0, 0, 0, 0, 0)...),
		"net out of order":      frame(Version, uint8(OpStart), tagFeatures, 1, FeatureNet, tagNetPrefix, 1, 24, tagNetAddr, 4, 10, 0, 0, 0, tagNetMTU, 2, 0x28, 0x23),
	} {
		if r, err := DecodeRequest(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: DecodeRequest(%x) = %+v, %v; want ErrInvalid", name, in, r, err)
		}
	}
	// The same frame with the fields in order is accepted.
	ok := frame(Version, uint8(OpStart), tagFeatures, 1, FeatureNet, tagNetAddr, 4, 10, 0, 0, 0, tagNetPrefix, 1, 24, tagNetMTU, 2, 0x28, 0x23)
	if r, err := DecodeRequest(ok); err != nil || r.NetMTU != 9000 || r.NetAddr != [4]byte{10} {
		t.Fatalf("DecodeRequest(%x) = %+v, %v", ok, r, err)
	}
}

func TestDecodeDoesNotAllocateMuch(t *testing.T) {
	b, _ := EncodeRequest(Request{Op: OpResize, CPUs: 2, MemoryMB: 4096})
	n := testing.AllocsPerRun(100, func() { _, _ = DecodeRequest(b) })
	if n > 1 {
		t.Fatalf("DecodeRequest allocates %v times per call", n)
	}
}

func testSecret() vmsecret.Secret {
	var s vmsecret.Secret
	for i := range s {
		s[i] = byte(0xa0 + i)
	}
	return s
}

// The open-endpoint verb and the VM secret (ADR 0004 rules 5 and 6, M3).
func TestEndpointAndSecret(t *testing.T) {
	sec := testSecret()
	id := [16]byte{7}
	for _, r := range []Reply{
		{Type: ReplyFlag | uint8(OpOpenEndpoint), PipeName: `\\.\pipe\hyperbutler-broker-00112233445566778899aabbccddeeff`},
		{Type: ReplyFlag | uint8(OpStart), State: StateRunning, CPUs: 2, MemoryMB: 2048, VMID: id, Secret: sec},
		{Type: ReplyFlag | uint8(OpStatus), State: StatePaused, CPUs: 2, MemoryMB: 2048, VMID: id, DataLocationSet: true, Secret: sec},
		{Type: ReplyFlag | uint8(OpPause), State: StateRunning, CPUs: 2, MemoryMB: 2048, VMID: id, Secret: sec},
		{Type: TypeError, Code: CodeWrongEndpoint, Message: "x"},
		{Type: TypeError, Code: CodeEndpointSquatted},
	} {
		b, err := EncodeReply(r)
		if err != nil {
			t.Fatalf("encode %+v: %v", r, err)
		}
		got, err := DecodeReply(b)
		if err != nil || got != r {
			t.Fatalf("round trip %+v: %+v, %v", r, got, err)
		}
	}
	if b, err := EncodeRequest(Request{Op: OpOpenEndpoint}); err != nil || !bytes.Equal(b, []byte{0, 2, Version, uint8(OpOpenEndpoint)}) {
		t.Fatalf("open-endpoint request %x, %v", b, err)
	}
	for name, r := range map[string]Reply{
		"secret while stopped":    {Type: ReplyFlag | uint8(OpStatus), State: StateStopped, CPUs: 2, MemoryMB: 2048, Secret: sec},
		"secret without VM id":    {Type: ReplyFlag | uint8(OpStatus), State: StateRunning, CPUs: 2, MemoryMB: 2048, Secret: sec},
		"secret on stop":          {Type: ReplyFlag | uint8(OpStop), Secret: sec},
		"secret on version":       {Type: ReplyFlag | uint8(OpVersion), ProtoVersion: 1, BrokerVersion: "v", Secret: sec},
		"secret on error":         {Type: TypeError, Code: CodeBusy, Secret: sec},
		"secret on open-endpoint": {Type: ReplyFlag | uint8(OpOpenEndpoint), PipeName: `\\.\pipe\x`, Secret: sec},
		"no pipe name":            {Type: ReplyFlag | uint8(OpOpenEndpoint)},
		"pipe name on status":     {Type: ReplyFlag | uint8(OpStatus), State: StateStopped, CPUs: 2, MemoryMB: 2048, PipeName: "x"},
		"pipe name too long":      {Type: ReplyFlag | uint8(OpOpenEndpoint), PipeName: strings.Repeat("p", MaxPipeNameLen+1)},
		"pipe name control char":  {Type: ReplyFlag | uint8(OpOpenEndpoint), PipeName: "a\tb"},
	} {
		if _, err := EncodeReply(r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: EncodeReply = %v, want ErrInvalid", name, err)
		}
	}
	running := frame(Version, ReplyFlag|uint8(OpStatus), append(append(fuzzU32(tagCPUs, 2), fuzzU32(tagMemoryMB, 2048)...),
		tagState, 1, uint8(StateRunning), tagVMID, 16, 7, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)...)
	if _, err := DecodeReply(running); err != nil {
		t.Fatal(err)
	}
	withSecret := func(v []byte) []byte {
		b := append(append([]byte{}, running...), tagSecret, uint8(len(v)))
		b = append(b, v...)
		binary.BigEndian.PutUint16(b, uint16(len(b)-2))
		return b
	}
	if r, err := DecodeReply(withSecret(sec[:])); err != nil || r.Secret != sec {
		t.Fatalf("secret reply: %v", err)
	}
	for name, in := range map[string][]byte{
		"zero secret":               withSecret(make([]byte, vmsecret.Size)),
		"short secret":              withSecret(sec[:vmsecret.Size-1]),
		"long secret":               withSecret(append(sec[:], 1)),
		"open reply without a name": frame(Version, ReplyFlag|uint8(OpOpenEndpoint)),
		"open reply empty name":     frame(Version, ReplyFlag|uint8(OpOpenEndpoint), tagPipeName, 0),
	} {
		if r, err := DecodeReply(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: DecodeReply(%x) = %+v, %v; want ErrInvalid", name, in, r, err)
		}
	}
}
