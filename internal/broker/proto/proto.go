// Package proto is the wire format between hyperbutler and the HCS access broker (ADR 0004).
//
// It carries only the ADR 0004 verbs: start (create) the caller's VM, stop it, query its
// status, resize it within fixed limits, set its data location by handle, pause or resume
// it (Implementation decision 2), report the broker version, and open the caller's own
// endpoint (rule 5, served on the shared endpoint only); a status reply on the owner's
// own endpoint carries the running VM's secret (rule 6). There is deliberately no
// field for a path, a command line, a share, an identity, or an HCS document (ADR 0004
// rules 1, 4 and 7). The VM verbs take a slot: none (0) is the caller's Docker VM, 1 to
// MaxSlot an agent sandbox session (ADR 0009). A start carries the VM's features and network as typed values (an
// IPv4 subnet, an MTU in a fixed range, flags), which the broker renders itself
// (Implementation decision 4).
//
// # Frame layout
//
// Every message is one frame:
//
//	length  uint16, big endian: number of bytes that follow (2..MaxFrameSize-2)
//	version uint8: Version
//	type    uint8: a request Op, a reply type (ReplyFlag|Op), or TypeError
//	fields  zero or more of:
//	        tag   uint8
//	        size  uint8: number of value bytes
//	        value [size]byte
//
// Each message type has a fixed schema: the tags it allows, each tag's value kind and
// size, and which tags are required. Decoding is strict and canonical. A frame is rejected
// if it is longer than MaxFrameSize, its length disagrees with the input (including
// trailing bytes), its version or type is unknown, a tag is unknown for its type, tags are
// repeated or out of ascending order, a value has the wrong size or an out-of-range value,
// a required tag is missing, or an optional value is present with its zero value. As a
// result, for every frame b that decodes, encoding the result yields exactly b.
package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/Higangssh/hyperbutler/internal/vmsecret"
)

// Version is the only protocol version this package speaks. A future version must keep
// the frame header and the Version verb (OpVersion request and reply) unchanged, so that
// any client can learn which broker it is talking to.
const Version = 1

// MaxFrameSize is the largest frame, including its 2-byte length prefix.
const MaxFrameSize = 512

const (
	headerSize  = 4 // length, version, type
	minBodySize = 2 // version, type
	maxBodySize = MaxFrameSize - 2
)

// Op is a request verb.
type Op uint8

const (
	OpVersion         Op = 1
	OpStart           Op = 2
	OpStop            Op = 3
	OpStatus          Op = 4
	OpResize          Op = 5
	OpSetDataLocation Op = 6
	// OpPause pauses the caller's running VM, or resumes it with Resume (the free-space
	// guard, REQ-DISK-004 and REQ-DISK-010).
	OpPause Op = 7
	// OpOpenEndpoint asks the shared endpoint for the caller's own endpoint (ADR 0004
	// rule 5): the broker creates it if it does not exist yet and replies with its pipe
	// name. It is the only verb besides OpVersion that the shared endpoint serves, and
	// the only one a per-user endpoint refuses.
	OpOpenEndpoint Op = 8
)

func (o Op) valid() bool { return o >= OpVersion && o <= OpOpenEndpoint }

func (o Op) String() string {
	switch o {
	case OpVersion:
		return "version"
	case OpStart:
		return "start"
	case OpStop:
		return "stop"
	case OpStatus:
		return "status"
	case OpResize:
		return "resize"
	case OpSetDataLocation:
		return "set-data-location"
	case OpPause:
		return "pause"
	case OpOpenEndpoint:
		return "open-endpoint"
	}
	return fmt.Sprintf("op(%d)", uint8(o))
}

const (
	// ReplyFlag marks the type byte of a successful reply: ReplyFlag|op.
	ReplyFlag = 0x80
	// TypeError is the type byte of an error reply, whatever the request was.
	TypeError = 0xFF
)

// Absolute resource limits. The broker applies its own, narrower, configured limits; these
// only bound what can appear on the wire.
const (
	MaxCPUs     = 64
	MinMemoryMB = 256
	MaxMemoryMB = 256 << 10
	// MaxHandle bounds a handle value. Real handle values fit in 32 bits; 64-bit
	// pseudo-handles (-1 current process, -2 current thread, ...) are sign-extended to
	// 0xFFFFFFFFFFFFFFFx, so this bound is what rejects them on the wire. (A 32-bit value
	// such as 0xFFFFFFFF passes here and fails the broker's object type check instead.)
	MaxHandle     = 0xFFFFFFFF
	MaxVersionLen = 64
	MaxMessageLen = 200
	// MaxSlot is the highest slot: the number of sandbox sessions one user may have.
	MaxSlot = 4
	// MaxPipeNameLen bounds the pipe name of an open-endpoint reply.
	MaxPipeNameLen = 128
	// MinMTU and MaxMTU bound the guest link MTU (the network stack's own range).
	MinMTU = 576
	MaxMTU = 65520
	// MinNetPrefix and MaxNetPrefix bound the guest subnet's prefix length.
	MinNetPrefix = 8
	MaxNetPrefix = 29
)

// Feature bits of a start request.
const (
	// FeatureNet gives the guest the user-mode network; the Net fields describe it.
	FeatureNet uint8 = 1 << iota
	// FeatureShare adds the bind-mount file server's service (ADR 0007).
	FeatureShare
	// FeatureDaemonConfig makes the guest wait for daemon.json on the control channel.
	FeatureDaemonConfig
	// FeatureBlankDisk says the data disk was never written: the guest may format it.
	FeatureBlankDisk
	allFeatures = FeatureNet | FeatureShare | FeatureDaemonConfig | FeatureBlankDisk
)

// Network flags of a start request with FeatureNet.
const (
	// NetOffload is the offload link (the host dials the guest).
	NetOffload uint8 = 1 << iota
	// NetHostGateway tells the guest the host gateway's address.
	NetHostGateway
	// NetCRCOff and NetCRCStrict set the offload link's CRC mode (on by default). At
	// most one is set, and only with NetOffload.
	NetCRCOff
	NetCRCStrict
	allNetFlags = NetOffload | NetHostGateway | NetCRCOff | NetCRCStrict
)

// State is a VM state in a status reply.
type State uint8

const (
	StateStopped State = 1
	StateRunning State = 2
	StatePaused  State = 3
)

func (s State) valid() bool { return s >= StateStopped && s <= StatePaused }

// Code is the error code of an error reply.
type Code uint16

const (
	CodeBadRequest           Code = 1 // malformed or invalid request
	CodeUnsupportedVersion   Code = 2 // request version is not Version
	CodeAccessDenied         Code = 3 // caller identity rejected
	CodeInvalidHandle        Code = 4 // the handle could not be taken from the caller
	CodeDataLocationRejected Code = 5 // the handle failed the ADR 0004 rule 2 checks
	CodeNoDataLocation       Code = 6 // start before a data location was set
	CodeOutOfRange           Code = 7 // resize outside the broker's limits
	CodeNotRunning           Code = 8 // stop of a VM that is not running
	CodeBusy                 Code = 9 // operation not allowed in the VM's current state
	CodeInternal             Code = 10
	CodeImageRejected        Code = 11 // the broker's guest image failed its checks
	CodeDiskRejected         Code = 12 // the data disk below the data location failed its checks
	CodeLeaseHeld            Code = 13 // the VM runs under another connection's lease
	// CodeWrongEndpoint: the verb is not served on this endpoint. The shared endpoint
	// serves only version and open-endpoint; a per-user endpoint serves everything else.
	CodeWrongEndpoint Code = 14
	// CodeEndpointSquatted: the caller's per-user pipe name is already taken by a pipe
	// the broker did not create. The broker does not fall back to another name.
	CodeEndpointSquatted Code = 15
	maxCode                   = CodeEndpointSquatted
)

func (c Code) valid() bool { return c >= CodeBadRequest && c <= maxCode }

// Request is a decoded request. Only the fields of its Op are meaningful; all others are
// zero.
type Request struct {
	Op Op

	// OpStart, OpStop, OpStatus, OpResize and OpPause: the VM the verb is for, 0 for the
	// caller's Docker VM or 1..MaxSlot for a sandbox session (ADR 0009).
	Slot uint8

	// OpStop.
	Force bool

	// OpResize: zero means unchanged; at least one is set. OpStart: zero means the
	// caller's current values.
	CPUs     uint32
	MemoryMB uint32

	// OpStart: Feature* bits, and with FeatureNet the guest network: the subnet's
	// address (masked, private) and prefix length, the link MTU, and Net* flags.
	Features  uint8
	NetAddr   [4]byte
	NetPrefix uint8
	NetMTU    uint16
	NetFlags  uint8

	// OpPause: resume instead of pause.
	Resume bool

	// OpSetDataLocation: the value of a directory handle in the caller's own process.
	// The broker duplicates it from the authenticated client process.
	Handle uint64
}

// Reply is a decoded reply. Type is ReplyFlag|op for success and TypeError for failure.
// Only the fields of its type are meaningful; all others are zero.
type Reply struct {
	Type uint8

	// TypeError.
	Code    Code
	Message string

	// OpVersion.
	ProtoVersion  uint8
	BrokerVersion string

	// OpStart, OpStatus, OpResize and OpPause.
	State           State
	CPUs            uint32
	MemoryMB        uint32
	VMID            [16]byte // the runtime id in RFC 4122 byte order; zero when no VM exists
	DataLocationSet bool
	// Secret is the running VM's secret (ADR 0004 rule 6), which the owner's hvsock
	// connections to the guest begin with. Only the owner's own endpoint sends it; it is
	// zero when no VM runs, and never printed (vmsecret.Secret).
	Secret vmsecret.Secret

	// OpOpenEndpoint: the caller's own endpoint.
	PipeName string
}

// IsError reports whether r is an error reply.
func (r *Reply) IsError() bool { return r.Type == TypeError }

// Errors returned by the decoder. Every decode failure wraps ErrInvalid; an unsupported
// version also wraps ErrUnsupportedVersion so the broker can say so.
var (
	ErrInvalid            = errors.New("proto: invalid message")
	ErrUnsupportedVersion = errors.New("proto: unsupported protocol version")
	ErrFrameTooLarge      = errors.New("proto: frame too large")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Field tags. A tag means the same thing in every type that allows it.
const (
	tagForce           = 1
	tagCPUs            = 2
	tagMemoryMB        = 3
	tagHandle          = 4
	tagProtoVersion    = 5
	tagBrokerVersion   = 6
	tagState           = 7
	tagVMID            = 8
	tagDataLocationSet = 9
	tagCode            = 10
	tagMessage         = 11
	tagFeatures        = 12
	tagNetAddr         = 13
	tagNetPrefix       = 14
	tagNetMTU          = 15
	tagNetFlags        = 16
	tagResume          = 17
	tagPipeName        = 18
	tagSecret          = 19
	tagSlot            = 20
	maxTag             = tagSlot
)

type kind uint8

const (
	kindBool kind = iota + 1 // 1 byte, must be 1 (false is encoded by omission)
	kindU8
	kindU16
	kindU32
	kindU64
	kindGUID   // 16 bytes, not all zero
	kindIPv4   // 4 bytes in network order, not all zero
	kindString // 1..max bytes of printable ASCII
	kindSecret // vmsecret.Size bytes, not all zero
)

type fieldSpec struct {
	kind     kind
	required bool
	max      int // kindString: maximum length
}

// schema maps a type byte to the fields it allows, indexed by tag.
type schema [maxTag + 1]*fieldSpec

var (
	optBool   = &fieldSpec{kind: kindBool}
	optU8     = &fieldSpec{kind: kindU8}
	optU16    = &fieldSpec{kind: kindU16}
	optU32    = &fieldSpec{kind: kindU32}
	optIPv4   = &fieldSpec{kind: kindIPv4}
	reqU8     = &fieldSpec{kind: kindU8, required: true}
	reqU16    = &fieldSpec{kind: kindU16, required: true}
	reqU32    = &fieldSpec{kind: kindU32, required: true}
	reqU64    = &fieldSpec{kind: kindU64, required: true}
	optGUID   = &fieldSpec{kind: kindGUID}
	reqVerStr = &fieldSpec{kind: kindString, required: true, max: MaxVersionLen}
	optMsgStr = &fieldSpec{kind: kindString, max: MaxMessageLen}
	reqPipe   = &fieldSpec{kind: kindString, required: true, max: MaxPipeNameLen}
	optSecret = &fieldSpec{kind: kindSecret}
)

var statusSchema = schema{
	tagState:           reqU8,
	tagCPUs:            reqU32,
	tagMemoryMB:        reqU32,
	tagVMID:            optGUID,
	tagDataLocationSet: optBool,
	tagSecret:          optSecret,
}

var schemas = map[uint8]*schema{
	uint8(OpVersion): {},
	uint8(OpStart): {tagCPUs: optU32, tagMemoryMB: optU32, tagFeatures: optU8,
		tagNetAddr: optIPv4, tagNetPrefix: optU8, tagNetMTU: optU16, tagNetFlags: optU8, tagSlot: optU8},
	uint8(OpStop):            {tagForce: optBool, tagSlot: optU8},
	uint8(OpStatus):          {tagSlot: optU8},
	uint8(OpResize):          {tagCPUs: optU32, tagMemoryMB: optU32, tagSlot: optU8},
	uint8(OpSetDataLocation): {tagHandle: reqU64},
	uint8(OpPause):           {tagResume: optBool, tagSlot: optU8},
	uint8(OpOpenEndpoint):    {},

	ReplyFlag | uint8(OpVersion):         {tagProtoVersion: reqU8, tagBrokerVersion: reqVerStr},
	ReplyFlag | uint8(OpStart):           &statusSchema,
	ReplyFlag | uint8(OpStop):            {},
	ReplyFlag | uint8(OpStatus):          &statusSchema,
	ReplyFlag | uint8(OpResize):          &statusSchema,
	ReplyFlag | uint8(OpSetDataLocation): {},
	ReplyFlag | uint8(OpPause):           &statusSchema,
	ReplyFlag | uint8(OpOpenEndpoint):    {tagPipeName: reqPipe},
	TypeError:                            {tagCode: reqU16, tagMessage: optMsgStr},
}

// fields holds raw decoded values by tag, before typed validation.
type fields struct {
	present [maxTag + 1]bool
	num     [maxTag + 1]uint64
	str     [maxTag + 1]string
	guid    [16]byte
	ipv4    [4]byte
	secret  vmsecret.Secret
}

// The u8, u16 and u32 accessors narrow a decoded number to its tag's wire width.
// decodeFrame only stores a tag's number after checking that its value is exactly
// kindSize(kind) bytes, so the stored uint64 never exceeds that width and the
// conversions below cannot truncate. Each accessor must only be used for tags whose
// schema kind is at most that wide.

func (f *fields) u8(tag int) uint8 {
	return uint8(f.num[tag]) //nolint:gosec // G115: decodeFrame stored at most one byte for this tag
}

func (f *fields) u16(tag int) uint16 {
	return uint16(f.num[tag]) //nolint:gosec // G115: decodeFrame stored at most two bytes for this tag
}

func (f *fields) u32(tag int) uint32 {
	return uint32(f.num[tag]) //nolint:gosec // G115: decodeFrame stored at most four bytes for this tag
}

func kindSize(k kind) int {
	switch k {
	case kindBool, kindU8:
		return 1
	case kindU16:
		return 2
	case kindU32:
		return 4
	case kindU64:
		return 8
	case kindGUID:
		return 16
	case kindIPv4:
		return 4
	case kindSecret:
		return vmsecret.Size
	}
	return -1
}

func printableASCII(s []byte) bool {
	for _, c := range s {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// decodeFrame checks the frame header and parses the fields against the type's schema.
// It does not interpret values beyond their encoding.
func decodeFrame(b []byte) (uint8, *fields, error) {
	if len(b) > MaxFrameSize {
		return 0, nil, fmt.Errorf("%w: %w", ErrInvalid, ErrFrameTooLarge)
	}
	if len(b) < headerSize {
		return 0, nil, invalid("short frame")
	}
	n := int(binary.BigEndian.Uint16(b))
	if n != len(b)-2 {
		return 0, nil, invalid("length %d does not match %d bytes of input", n, len(b)-2)
	}
	if b[2] != Version {
		return 0, nil, fmt.Errorf("%w: %w: %d", ErrInvalid, ErrUnsupportedVersion, b[2])
	}
	typ := b[3]
	sc, ok := schemas[typ]
	if !ok {
		return 0, nil, invalid("unknown type 0x%02x", typ)
	}
	f := new(fields)
	last := 0
	p := b[headerSize:]
	for len(p) > 0 {
		if len(p) < 2 {
			return 0, nil, invalid("truncated field header")
		}
		tag, size := int(p[0]), int(p[1])
		p = p[2:]
		if tag <= last {
			return 0, nil, invalid("tag %d repeated or out of order", tag)
		}
		last = tag
		if tag > maxTag || sc[tag] == nil {
			return 0, nil, invalid("unknown tag %d for type 0x%02x", tag, typ)
		}
		spec := sc[tag]
		if size > len(p) {
			return 0, nil, invalid("tag %d: value truncated", tag)
		}
		v := p[:size]
		p = p[size:]
		if spec.kind == kindString {
			if size == 0 || size > spec.max || !printableASCII(v) {
				return 0, nil, invalid("tag %d: bad string", tag)
			}
			f.str[tag] = string(v)
		} else {
			if size != kindSize(spec.kind) {
				return 0, nil, invalid("tag %d: size %d", tag, size)
			}
			switch spec.kind {
			case kindBool:
				if v[0] != 1 {
					return 0, nil, invalid("tag %d: bool must be encoded as 1", tag)
				}
				f.num[tag] = 1
			case kindU8:
				f.num[tag] = uint64(v[0])
			case kindU16:
				f.num[tag] = uint64(binary.LittleEndian.Uint16(v))
			case kindU32:
				f.num[tag] = uint64(binary.LittleEndian.Uint32(v))
			case kindU64:
				f.num[tag] = binary.LittleEndian.Uint64(v)
			case kindGUID:
				copy(f.guid[:], v)
				if f.guid == ([16]byte{}) {
					return 0, nil, invalid("tag %d: zero GUID", tag)
				}
			case kindIPv4:
				copy(f.ipv4[:], v)
				if f.ipv4 == ([4]byte{}) {
					return 0, nil, invalid("tag %d: zero address", tag)
				}
			case kindSecret:
				copy(f.secret[:], v)
				if f.secret.IsZero() {
					return 0, nil, invalid("tag %d: zero secret", tag)
				}
			}
			// An optional number is encoded only when it is not zero (canonical form).
			if !spec.required && spec.kind >= kindU8 && spec.kind <= kindU64 && f.num[tag] == 0 {
				return 0, nil, invalid("tag %d: zero value must be omitted", tag)
			}
		}
		f.present[tag] = true
	}
	for tag, spec := range sc {
		if spec != nil && spec.required && !f.present[tag] {
			return 0, nil, invalid("type 0x%02x: missing tag %d", typ, tag)
		}
	}
	return typ, f, nil
}

// DecodeRequest decodes exactly one request frame. b must hold the whole frame,
// including its length prefix, and nothing else.
func DecodeRequest(b []byte) (Request, error) {
	typ, f, err := decodeFrame(b)
	if err != nil {
		return Request{}, err
	}
	if typ&ReplyFlag != 0 {
		return Request{}, invalid("type 0x%02x is not a request", typ)
	}
	r := Request{
		Op:        Op(typ),
		Force:     f.num[tagForce] == 1,
		CPUs:      f.u32(tagCPUs),
		MemoryMB:  f.u32(tagMemoryMB),
		Handle:    f.num[tagHandle],
		Features:  f.u8(tagFeatures),
		NetAddr:   f.ipv4,
		NetPrefix: f.u8(tagNetPrefix),
		NetMTU:    f.u16(tagNetMTU),
		NetFlags:  f.u8(tagNetFlags),
		Resume:    f.num[tagResume] == 1,
		Slot:      f.u8(tagSlot),
	}
	// decodeFrame rejected every optional number encoded with its zero value, so a
	// zero here (CPUs or MemoryMB unchanged) was omitted, as the canonical form wants.
	if err := r.Validate(); err != nil {
		return Request{}, err
	}
	return r, nil
}

// privateIPv4 reports whether a is in an RFC 1918 range.
func privateIPv4(a [4]byte) bool {
	return a[0] == 10 || a[0] == 172 && a[1]&0xf0 == 16 || a[0] == 192 && a[1] == 168
}

// validateNet checks a start's network fields.
func (r *Request) validateNet() error {
	if r.Features&FeatureNet == 0 {
		if r.NetAddr != ([4]byte{}) || r.NetPrefix != 0 || r.NetMTU != 0 || r.NetFlags != 0 {
			return invalid("start: network fields without FeatureNet")
		}
		return nil
	}
	if r.NetPrefix < MinNetPrefix || r.NetPrefix > MaxNetPrefix {
		return invalid("start: prefix length %d", r.NetPrefix)
	}
	addr := binary.BigEndian.Uint32(r.NetAddr[:])
	if addr == 0 || addr&(uint32(1)<<(32-r.NetPrefix)-1) != 0 || !privateIPv4(r.NetAddr) {
		return invalid("start: subnet %v/%d is not a masked private IPv4 subnet", r.NetAddr, r.NetPrefix)
	}
	if r.NetMTU < MinMTU || r.NetMTU > MaxMTU {
		return invalid("start: MTU %d", r.NetMTU)
	}
	if r.NetFlags&^allNetFlags != 0 {
		return invalid("start: network flags 0x%02x", r.NetFlags)
	}
	crc := r.NetFlags & (NetCRCOff | NetCRCStrict)
	if crc == NetCRCOff|NetCRCStrict || crc != 0 && r.NetFlags&NetOffload == 0 {
		return invalid("start: CRC flags 0x%02x", r.NetFlags)
	}
	return nil
}

// Validate checks r against its Op's rules. EncodeRequest and DecodeRequest both apply it.
func (r *Request) Validate() error {
	if !r.Op.valid() {
		return invalid("unknown op %d", r.Op)
	}
	zero := Request{Op: r.Op}
	switch r.Op {
	case OpStart, OpStop, OpStatus, OpResize, OpPause:
		zero.Slot = r.Slot
		if r.Slot > MaxSlot {
			return invalid("%s: slot %d", r.Op, r.Slot)
		}
	}
	switch r.Op {
	case OpStop:
		zero.Force = r.Force
	case OpResize:
		zero.CPUs, zero.MemoryMB = r.CPUs, r.MemoryMB
		if r.CPUs == 0 && r.MemoryMB == 0 {
			return invalid("resize: nothing to change")
		}
		if r.CPUs > MaxCPUs {
			return invalid("resize: %d CPUs", r.CPUs)
		}
		if r.MemoryMB != 0 && (r.MemoryMB < MinMemoryMB || r.MemoryMB > MaxMemoryMB) {
			return invalid("resize: %d MB", r.MemoryMB)
		}
	case OpSetDataLocation:
		zero.Handle = r.Handle
		if r.Handle == 0 || r.Handle > MaxHandle {
			return invalid("set-data-location: handle 0x%x", r.Handle)
		}
	case OpStart:
		zero.CPUs, zero.MemoryMB = r.CPUs, r.MemoryMB
		zero.Features, zero.NetAddr, zero.NetPrefix, zero.NetMTU, zero.NetFlags = r.Features, r.NetAddr, r.NetPrefix, r.NetMTU, r.NetFlags
		if r.CPUs > MaxCPUs {
			return invalid("start: %d CPUs", r.CPUs)
		}
		if r.MemoryMB != 0 && (r.MemoryMB < MinMemoryMB || r.MemoryMB > MaxMemoryMB) {
			return invalid("start: %d MB", r.MemoryMB)
		}
		if r.Features&^allFeatures != 0 {
			return invalid("start: features 0x%02x", r.Features)
		}
		if err := r.validateNet(); err != nil {
			return err
		}
		// A sandbox session gets neither the bind-mount file server, nor dockerd's
		// configuration, nor the host gateway (ADR 0009: the project is copied in, and a
		// session never reaches the host's loopback).
		if r.Slot != 0 && (r.Features&(FeatureShare|FeatureDaemonConfig) != 0 || r.NetFlags&NetHostGateway != 0) {
			return invalid("start: slot %d: share, daemon config and host gateway are for the Docker VM only", r.Slot)
		}
	case OpPause:
		zero.Resume = r.Resume
	}
	if *r != zero {
		return invalid("%s: field not allowed", r.Op)
	}
	return nil
}

// DecodeReply decodes exactly one reply frame.
func DecodeReply(b []byte) (Reply, error) {
	typ, f, err := decodeFrame(b)
	if err != nil {
		return Reply{}, err
	}
	if typ&ReplyFlag == 0 {
		return Reply{}, invalid("type 0x%02x is not a reply", typ)
	}
	r := Reply{
		Type:            typ,
		Code:            Code(f.u16(tagCode)),
		Message:         f.str[tagMessage],
		ProtoVersion:    f.u8(tagProtoVersion),
		BrokerVersion:   f.str[tagBrokerVersion],
		State:           State(f.u8(tagState)),
		CPUs:            f.u32(tagCPUs),
		MemoryMB:        f.u32(tagMemoryMB),
		VMID:            f.guid,
		DataLocationSet: f.num[tagDataLocationSet] == 1,
		Secret:          f.secret,
		PipeName:        f.str[tagPipeName],
	}
	if err := r.Validate(); err != nil {
		return Reply{}, err
	}
	return r, nil
}

// Validate checks r against its type's rules. EncodeReply and DecodeReply both apply it.
func (r *Reply) Validate() error {
	sc, ok := schemas[r.Type]
	if !ok || r.Type&ReplyFlag == 0 {
		return invalid("unknown reply type 0x%02x", r.Type)
	}
	// Every field that is set must be allowed by the schema.
	set := [maxTag + 1]bool{
		tagCode:            r.Code != 0,
		tagMessage:         r.Message != "",
		tagProtoVersion:    r.ProtoVersion != 0,
		tagBrokerVersion:   r.BrokerVersion != "",
		tagState:           r.State != 0,
		tagCPUs:            r.CPUs != 0,
		tagMemoryMB:        r.MemoryMB != 0,
		tagVMID:            r.VMID != [16]byte{},
		tagDataLocationSet: r.DataLocationSet,
		tagPipeName:        r.PipeName != "",
		tagSecret:          !r.Secret.IsZero(),
	}
	for tag, isSet := range set {
		if isSet && sc[tag] == nil {
			return invalid("reply 0x%02x: tag %d not allowed", r.Type, tag)
		}
	}
	if !printableASCII([]byte(r.Message)) || len(r.Message) > MaxMessageLen {
		return invalid("bad message")
	}
	if !printableASCII([]byte(r.BrokerVersion)) || len(r.BrokerVersion) > MaxVersionLen {
		return invalid("bad broker version")
	}
	if !printableASCII([]byte(r.PipeName)) || len(r.PipeName) > MaxPipeNameLen {
		return invalid("bad pipe name")
	}
	switch {
	case r.Type == TypeError:
		if !r.Code.valid() {
			return invalid("error code %d", r.Code)
		}
	case sc == &statusSchema:
		if !r.State.valid() {
			return invalid("state %d", r.State)
		}
		if r.CPUs == 0 || r.CPUs > MaxCPUs || r.MemoryMB < MinMemoryMB || r.MemoryMB > MaxMemoryMB {
			return invalid("status: %d CPUs, %d MB", r.CPUs, r.MemoryMB)
		}
		// A secret belongs to a VM that exists.
		if !r.Secret.IsZero() && (r.State == StateStopped || r.VMID == [16]byte{}) {
			return invalid("status: a secret without a running VM")
		}
	case r.Type == ReplyFlag|uint8(OpVersion):
		if r.ProtoVersion == 0 || r.BrokerVersion == "" {
			return invalid("version reply: missing fields")
		}
	case r.Type == ReplyFlag|uint8(OpOpenEndpoint):
		if r.PipeName == "" {
			return invalid("open-endpoint reply: no pipe name")
		}
	}
	return nil
}

type encoder struct{ b []byte }

func (e *encoder) field(tag uint8, v []byte) {
	// Every value is a fixed-width number, a GUID, or a string Validate bounded by its
	// schema maximum (far below 256), and finish rejects an oversized frame anyway.
	e.b = append(e.b, tag, uint8(len(v))) //nolint:gosec // G115: field values are shorter than 256 bytes
	e.b = append(e.b, v...)
}

func (e *encoder) boolean(tag uint8, v bool) {
	if v {
		e.field(tag, []byte{1})
	}
}

func (e *encoder) u8(tag uint8, v uint8) {
	if v != 0 {
		e.field(tag, []byte{v})
	}
}

func (e *encoder) u16(tag uint8, v uint16) {
	e.field(tag, binary.LittleEndian.AppendUint16(nil, v))
}

func (e *encoder) u32(tag uint8, v uint32) {
	if v != 0 {
		e.field(tag, binary.LittleEndian.AppendUint32(nil, v))
	}
}

func (e *encoder) str(tag uint8, s string) {
	if s != "" {
		e.field(tag, []byte(s))
	}
}

func newEncoder(typ uint8) *encoder {
	e := &encoder{b: make([]byte, headerSize, 64)}
	e.b[2], e.b[3] = Version, typ
	return e
}

func (e *encoder) finish() ([]byte, error) {
	if len(e.b) > MaxFrameSize {
		return nil, ErrFrameTooLarge
	}
	binary.BigEndian.PutUint16(e.b, uint16(len(e.b)-2)) //nolint:gosec // G115: len(e.b) <= MaxFrameSize (512) was checked above
	return e.b, nil
}

// EncodeRequest validates r and returns its frame. Fields are written in ascending tag
// order, so the output is the canonical form DecodeRequest accepts.
func EncodeRequest(r Request) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	e := newEncoder(uint8(r.Op))
	e.boolean(tagForce, r.Force)
	e.u32(tagCPUs, r.CPUs)
	e.u32(tagMemoryMB, r.MemoryMB)
	if r.Handle != 0 {
		e.field(tagHandle, binary.LittleEndian.AppendUint64(nil, r.Handle))
	}
	e.u8(tagFeatures, r.Features)
	if r.NetAddr != ([4]byte{}) {
		e.field(tagNetAddr, r.NetAddr[:])
	}
	e.u8(tagNetPrefix, r.NetPrefix)
	if r.NetMTU != 0 {
		e.u16(tagNetMTU, r.NetMTU)
	}
	e.u8(tagNetFlags, r.NetFlags)
	e.boolean(tagResume, r.Resume)
	e.u8(tagSlot, r.Slot)
	return e.finish()
}

// EncodeReply validates r and returns its frame in canonical form.
func EncodeReply(r Reply) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	e := newEncoder(r.Type)
	// Fields go in ascending tag order. Validate has already checked that every required
	// field is set and that no field outside the schema is.
	e.u32(tagCPUs, r.CPUs)
	e.u32(tagMemoryMB, r.MemoryMB)
	e.u8(tagProtoVersion, r.ProtoVersion)
	e.str(tagBrokerVersion, r.BrokerVersion)
	e.u8(tagState, uint8(r.State))
	if r.VMID != ([16]byte{}) {
		e.field(tagVMID, r.VMID[:])
	}
	e.boolean(tagDataLocationSet, r.DataLocationSet)
	if r.Code != 0 {
		e.u16(tagCode, uint16(r.Code))
	}
	e.str(tagMessage, r.Message)
	e.str(tagPipeName, r.PipeName)
	if !r.Secret.IsZero() {
		e.field(tagSecret, r.Secret[:])
	}
	return e.finish()
}

// ReadFrame reads one frame from r into buf and returns it (a sub-slice of buf, including
// the length prefix). It never reads past the frame and never allocates. A length prefix
// outside 2..MaxFrameSize-2 returns ErrFrameTooLarge or ErrInvalid without reading the
// body. A clean EOF before the first byte returns io.EOF.
func ReadFrame(r io.Reader, buf *[MaxFrameSize]byte) ([]byte, error) {
	if _, err := io.ReadFull(r, buf[:2]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(buf[:2]))
	if n > maxBodySize {
		return nil, ErrFrameTooLarge
	}
	if n < minBodySize {
		return nil, invalid("frame length %d", n)
	}
	if _, err := io.ReadFull(r, buf[2:2+n]); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return buf[:2+n], nil
}
