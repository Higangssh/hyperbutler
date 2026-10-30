// Package guestctl is the hyperbutler host<->guest control channel: a small binary protocol
// over hvsock (host) / AF_VSOCK (guest) on its own service port, ControlPort.
//
// The host dials; the guest daemon (guest/cmd/hyperbutlerd) listens and accepts the host CID
// only. The host sends the free-space guard budget (internal/spaceguard), trim and
// cache-trim requests, pings, and the request for an orderly guest shutdown. The guest answers requests and pushes a periodic
// status report.
//
// # Trust
//
// The guest is not trusted. Everything the host reads from it is decoded strictly, is
// bounded in size and rate, and is used for display, liveness and logging only: no
// guest-reported number feeds the budget or the pause decision (see
// docs/research/guest-services.md). The guest trusts the host, but decodes strictly too.
//
// # Wire format
//
// Every message is one frame:
//
//	u32 BE  n        body length, HeaderLen <= n <= MaxBody
//	body:
//	  u8      version  Version (1)
//	  u8      type     a Type valid for the sender's direction
//	  u16 BE  reserved 0
//	  u32 BE  seq      host requests: the host's sequence number; guest replies echo it;
//	                   guest status reports: the guest's own counter
//	  payload          exactly the fixed size of the type; padding bytes are 0
//
// All integers are big-endian. Decoding is canonical: a frame decodes only if encoding
// the decoded message gives back the same bytes, which the fuzz targets check.
package guestctl

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Higangssh/hyperbutler/internal/spaceguard"
)

const (
	// Version is the only protocol version this package speaks.
	Version = 1
	// ControlPort is the AF_VSOCK port the guest daemon listens on. The Docker API relay
	// uses 2375; this is a separate service so it gets its own HCS service-table entry.
	ControlPort = 2380

	// HeaderLen is the length of the body header (version, type, reserved, seq).
	HeaderLen = 8
	// MaxBody bounds a frame body. The largest message (Status) is far below it.
	MaxBody = 256
	// MaxErrno bounds errno values on the wire: Linux errnos are below 4096.
	MaxErrno = 4095
)

// Type is a message type. Host-to-guest types are below 0x80, guest-to-host types are
// 0x80 and above.
type Type uint8

// Host to guest.
const (
	THello     Type = 0x01 // version negotiation; must be the first host frame
	TBudget    Type = 0x02 // free-space guard budget
	TTrim      Type = 0x03 // run FITRIM on the data file system now
	TCacheTrim Type = 0x04 // run the page-cache policy's reclaim now
	TPing      Type = 0x05 // liveness
	TShutdown  Type = 0x06 // power the guest off in order (stop dockerd, unmount)
)

// Guest to host.
const (
	RHello  Type = 0x81 // reply to THello
	RStatus Type = 0x82 // periodic, unsolicited
	RTrim   Type = 0x83 // reply to TTrim
	RCache  Type = 0x84 // reply to TCacheTrim
	RPong   Type = 0x85 // reply to TPing
	RError  Type = 0x86 // a request could not be served
	// RShutdown accepts a TShutdown: the guest has started powering off. The VM stops
	// when it is done; the host watches for that, not for another message.
	RShutdown Type = 0x87
)

func (t Type) String() string {
	switch t {
	case THello:
		return "hello"
	case TBudget:
		return "budget"
	case TTrim:
		return "trim"
	case TCacheTrim:
		return "cache-trim"
	case TPing:
		return "ping"
	case TShutdown:
		return "shutdown"
	case RHello:
		return "hello-reply"
	case RStatus:
		return "status"
	case RTrim:
		return "trim-result"
	case RCache:
		return "cache-result"
	case RPong:
		return "pong"
	case RError:
		return "error"
	case RShutdown:
		return "shutdown-accepted"
	}
	return fmt.Sprintf("type(0x%02x)", uint8(t))
}

// Direction says who sent a frame.
type Direction int

const (
	FromHost Direction = iota
	FromGuest
)

// Msg is one decoded message.
type Msg interface {
	Type() Type
	// size is the fixed payload size.
	size() int
	put(b []byte)
}

// Hello is the host's first frame: the protocol versions it speaks.
type Hello struct{ Min, Max uint16 }

// Budget carries a spaceguard.Budget.
type Budget struct{ spaceguard.Budget }

// TrimRequest asks the guest to FITRIM the data file system now.
type TrimRequest struct{}

// CacheTrimRequest asks the guest to run its page-cache reclaim now, idle or not.
type CacheTrimRequest struct{}

// Ping asks for a Pong with the same nonce.
type Ping struct{ Nonce uint64 }

// ShutdownRequest asks the guest to power off in order.
type ShutdownRequest struct{}

// ShutdownAccepted answers a ShutdownRequest.
type ShutdownAccepted struct{}

// HelloReply is the guest's answer to Hello.
type HelloReply struct {
	Version  uint16
	Features Features
	// Instance changes every time the guest daemon starts.
	Instance uint64
}

// Features says which policies the guest daemon runs.
type Features uint32

const (
	FeatureGuard      Features = 1 << 0 // the free-space guard reservation
	FeatureTrim       Features = 1 << 1 // periodic and delete-triggered fstrim
	FeatureCacheTrim  Features = 1 << 2 // the page-cache policy
	FeatureShutdown   Features = 1 << 3 // orderly shutdown on request
	knownFeatures              = FeatureGuard | FeatureTrim | FeatureCacheTrim | FeatureShutdown
	statusPayloadSize          = 8 + 17*8
)

// GuardState is the guest guard's view of its budget.
type GuardState uint8

const (
	GuardOff      GuardState = 0 // the guard is not running
	GuardNoBudget GuardState = 1 // no budget yet or channel lost: failing closed
	GuardStale    GuardState = 2 // the budget is stale: failing closed
	GuardOK       GuardState = 3 // applying the host's budget
	maxGuardState            = GuardOK
)

// Status is the guest's periodic report. Every number in it is guest-reported and
// therefore untrusted: the host shows and logs it, nothing more.
type Status struct {
	Guard      GuardState
	GuardErrno uint32 // errno of the last reservation change, 0 if it worked

	Device, FSSize, FSFree uint64 // bytes
	Reserved, Target       uint64
	Need, Short            uint64

	Trims, Trimmed uint64 // count and total bytes FITRIM reported
	LastTrimAge    uint64 // ms since the last trim finished; MaxUint64 = never

	MemTotal, MemFree uint64 // bytes, /proc/meminfo
	FileCache, Shmem  uint64 // Active(file)+Inactive(file), Shmem
	CacheTrims        uint64 // how often the cache policy reclaimed
	CacheFreed        uint64 // total bytes it freed

	// GuardSteps counts the guard loop's completed steps. It must advance between
	// reports while the guard runs: a frozen counter means the guard is stuck even
	// though the status goroutine still answers.
	GuardSteps uint64
}

// TrimResult answers a TrimRequest.
type TrimResult struct {
	Errno    uint32 // 0 on success
	Trimmed  uint64 // bytes FITRIM reported (fstrim_range.len on return)
	Duration time.Duration
}

// CacheMethod is how the guest reclaimed page cache.
type CacheMethod uint8

const (
	CacheNone      CacheMethod = 0 // nothing to do
	CacheReclaim   CacheMethod = 1 // cgroup v2 memory.reclaim
	CacheDrop      CacheMethod = 2 // vm.drop_caches=1
	maxCacheMethod             = CacheDrop
)

// CacheResult answers a CacheTrimRequest.
type CacheResult struct {
	Errno         uint32
	Method        CacheMethod
	Before, After uint64 // file cache bytes before and after
	Duration      time.Duration
}

// ErrorCode says why the guest refused a request.
type ErrorCode uint32

const (
	ErrBusy        ErrorCode = 1 // the same kind of request is still running
	ErrUnsupported ErrorCode = 2 // the feature is off in this guest
	maxErrorCode             = ErrUnsupported
)

// ErrorReply refuses a request.
type ErrorReply struct {
	Code    ErrorCode
	Request Type
}

// Pong answers a Ping.
type Pong struct{ Nonce uint64 }

func (Hello) Type() Type            { return THello }
func (Budget) Type() Type           { return TBudget }
func (TrimRequest) Type() Type      { return TTrim }
func (CacheTrimRequest) Type() Type { return TCacheTrim }
func (Ping) Type() Type             { return TPing }
func (ShutdownRequest) Type() Type  { return TShutdown }
func (ShutdownAccepted) Type() Type { return RShutdown }
func (HelloReply) Type() Type       { return RHello }
func (Status) Type() Type           { return RStatus }
func (TrimResult) Type() Type       { return RTrim }
func (CacheResult) Type() Type      { return RCache }
func (Pong) Type() Type             { return RPong }
func (ErrorReply) Type() Type       { return RError }

func (Hello) size() int            { return 4 }
func (Budget) size() int           { return 24 }
func (TrimRequest) size() int      { return 0 }
func (CacheTrimRequest) size() int { return 0 }
func (Ping) size() int             { return 8 }
func (ShutdownRequest) size() int  { return 0 }
func (ShutdownAccepted) size() int { return 0 }
func (HelloReply) size() int       { return 16 }
func (Status) size() int           { return statusPayloadSize }
func (TrimResult) size() int       { return 24 }
func (CacheResult) size() int      { return 32 }
func (Pong) size() int             { return 8 }
func (ErrorReply) size() int       { return 8 }

var be = binary.BigEndian

func (m Hello) put(b []byte) { be.PutUint16(b, m.Min); be.PutUint16(b[2:], m.Max) }

func (m Budget) put(b []byte) {
	if m.Unlimited {
		b[0] = 1
	}
	be.PutUint64(b[8:], m.Bytes)
	be.PutUint64(b[16:], m.Backed)
}

func (TrimRequest) put([]byte)      {}
func (ShutdownRequest) put([]byte)  {}
func (ShutdownAccepted) put([]byte) {}
func (CacheTrimRequest) put([]byte) {}
func (m Ping) put(b []byte)         { be.PutUint64(b, m.Nonce) }
func (m Pong) put(b []byte)         { be.PutUint64(b, m.Nonce) }

func (m HelloReply) put(b []byte) {
	be.PutUint16(b, m.Version)
	be.PutUint32(b[4:], uint32(m.Features))
	be.PutUint64(b[8:], m.Instance)
}

func (m Status) put(b []byte) {
	b[0] = byte(m.Guard)
	be.PutUint32(b[4:], m.GuardErrno)
	for i, v := range m.fields() {
		be.PutUint64(b[8+8*i:], *v)
	}
}

func (m *Status) fields() [17]*uint64 {
	return [17]*uint64{
		&m.Device, &m.FSSize, &m.FSFree, &m.Reserved, &m.Target, &m.Need, &m.Short,
		&m.Trims, &m.Trimmed, &m.LastTrimAge,
		&m.MemTotal, &m.MemFree, &m.FileCache, &m.Shmem, &m.CacheTrims, &m.CacheFreed,
		&m.GuardSteps,
	}
}

func (m TrimResult) put(b []byte) {
	be.PutUint32(b, m.Errno)
	be.PutUint64(b[8:], m.Trimmed)
	be.PutUint64(b[16:], durMS(m.Duration))
}

func (m CacheResult) put(b []byte) {
	be.PutUint32(b, m.Errno)
	b[4] = byte(m.Method)
	be.PutUint64(b[8:], m.Before)
	be.PutUint64(b[16:], m.After)
	be.PutUint64(b[24:], durMS(m.Duration))
}

func (m ErrorReply) put(b []byte) {
	be.PutUint32(b, uint32(m.Code))
	b[4] = byte(m.Request)
}

// maxDurationMS keeps durations representable as time.Duration after decoding.
const maxDurationMS = uint64(1<<63-1) / uint64(time.Millisecond)

func durMS(d time.Duration) uint64 {
	if d < 0 {
		return 0
	}
	return uint64(d / time.Millisecond)
}

// Errors.
var (
	ErrFrameTooLarge = errors.New("guestctl: frame length out of range")
	ErrVersion       = errors.New("guestctl: unsupported protocol version")
	ErrMalformed     = errors.New("guestctl: malformed message")
)

// validate reports a message that cannot be encoded canonically.
func validate(m Msg) error {
	switch v := m.(type) {
	case Hello:
		if v.Min == 0 || v.Min > v.Max {
			return fmt.Errorf("%w: hello versions %d..%d", ErrMalformed, v.Min, v.Max)
		}
	case Budget:
		if v.Unlimited && (v.Bytes != 0 || v.Backed != 0) {
			return fmt.Errorf("%w: unlimited budget with values", ErrMalformed)
		}
	case HelloReply:
		if v.Version != Version {
			return fmt.Errorf("%w: hello reply version %d", ErrVersion, v.Version)
		}
		if v.Features&^knownFeatures != 0 {
			return fmt.Errorf("%w: unknown features 0x%x", ErrMalformed, uint32(v.Features))
		}
	case Status:
		if v.Guard > maxGuardState {
			return fmt.Errorf("%w: guard state %d", ErrMalformed, v.Guard)
		}
		if v.GuardErrno > MaxErrno {
			return fmt.Errorf("%w: errno %d", ErrMalformed, v.GuardErrno)
		}
	case TrimResult:
		if v.Errno > MaxErrno || v.Duration < 0 {
			return fmt.Errorf("%w: trim result", ErrMalformed)
		}
	case CacheResult:
		if v.Errno > MaxErrno || v.Method > maxCacheMethod || v.Duration < 0 {
			return fmt.Errorf("%w: cache result", ErrMalformed)
		}
	case ErrorReply:
		if v.Code == 0 || v.Code > maxErrorCode || !requestType(v.Request) {
			return fmt.Errorf("%w: error reply", ErrMalformed)
		}
	case TrimRequest, CacheTrimRequest, Ping, Pong, ShutdownRequest, ShutdownAccepted:
	default:
		return fmt.Errorf("%w: unknown message %T", ErrMalformed, m)
	}
	return nil
}

func requestType(t Type) bool { return t >= TBudget && t <= TShutdown }

// Append appends the frame for m with sequence number seq to dst.
func Append(dst []byte, seq uint32, m Msg) ([]byte, error) {
	if m == nil {
		return dst, fmt.Errorf("%w: nil message", ErrMalformed)
	}
	if err := validate(m); err != nil {
		return dst, err
	}
	n := HeaderLen + m.size()
	start := len(dst)
	dst = append(dst, make([]byte, 4+n)...)
	f := dst[start:]
	be.PutUint32(f, uint32(n))
	f[4] = Version
	f[5] = byte(m.Type())
	be.PutUint32(f[8:], seq)
	m.put(f[4+HeaderLen:])
	return dst, nil
}

// Encode returns the frame for m.
func Encode(seq uint32, m Msg) ([]byte, error) { return Append(nil, seq, m) }

// ReadFrame reads one frame from r into buf, which must hold MaxBody bytes, and returns
// the body. It never reads past the frame and never allocates.
func ReadFrame(r io.Reader, buf *[MaxBody]byte) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := be.Uint32(hdr[:])
	if n < HeaderLen || n > MaxBody {
		return nil, fmt.Errorf("%w: %d", ErrFrameTooLarge, n)
	}
	body := buf[:n]
	if _, err := io.ReadFull(r, body); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return body, nil
}

// Decode decodes a frame body (without the length prefix) sent in direction dir.
func Decode(body []byte, dir Direction) (uint32, Msg, error) {
	if len(body) < HeaderLen || len(body) > MaxBody {
		return 0, nil, fmt.Errorf("%w: %d", ErrFrameTooLarge, len(body))
	}
	if body[0] != Version {
		return 0, nil, fmt.Errorf("%w: %d", ErrVersion, body[0])
	}
	t := Type(body[1])
	if be.Uint16(body[2:]) != 0 {
		return 0, nil, fmt.Errorf("%w: reserved header bits set", ErrMalformed)
	}
	seq := be.Uint32(body[4:])
	p := body[HeaderLen:]
	m, err := decodePayload(t, p, dir)
	if err != nil {
		return 0, nil, err
	}
	if err := validate(m); err != nil {
		return 0, nil, err
	}
	return seq, m, nil
}

func decodePayload(t Type, p []byte, dir Direction) (Msg, error) {
	var m Msg
	switch {
	case dir == FromHost && t == THello:
		m = Hello{}
	case dir == FromHost && t == TBudget:
		m = Budget{}
	case dir == FromHost && t == TTrim:
		m = TrimRequest{}
	case dir == FromHost && t == TCacheTrim:
		m = CacheTrimRequest{}
	case dir == FromHost && t == TPing:
		m = Ping{}
	case dir == FromHost && t == TShutdown:
		m = ShutdownRequest{}
	case dir == FromGuest && t == RHello:
		m = HelloReply{}
	case dir == FromGuest && t == RStatus:
		m = Status{}
	case dir == FromGuest && t == RTrim:
		m = TrimResult{}
	case dir == FromGuest && t == RCache:
		m = CacheResult{}
	case dir == FromGuest && t == RPong:
		m = Pong{}
	case dir == FromGuest && t == RError:
		m = ErrorReply{}
	case dir == FromGuest && t == RShutdown:
		m = ShutdownAccepted{}
	default:
		return nil, fmt.Errorf("%w: type %v not valid in this direction", ErrMalformed, t)
	}
	if len(p) != m.size() {
		return nil, fmt.Errorf("%w: %v payload is %d bytes, want %d", ErrMalformed, t, len(p), m.size())
	}
	zero := func(b []byte) bool {
		for _, c := range b {
			if c != 0 {
				return false
			}
		}
		return true
	}
	dur := func(b []byte) (time.Duration, bool) {
		ms := be.Uint64(b)
		if ms > maxDurationMS {
			return 0, false
		}
		return time.Duration(ms) * time.Millisecond, true
	}
	bad := fmt.Errorf("%w: %v padding or flag bits set", ErrMalformed, t)
	switch m.(type) {
	case Hello:
		return Hello{Min: be.Uint16(p), Max: be.Uint16(p[2:])}, nil
	case Budget:
		if p[0] > 1 || !zero(p[1:8]) {
			return nil, bad
		}
		return Budget{spaceguard.Budget{Unlimited: p[0] == 1, Bytes: be.Uint64(p[8:]), Backed: be.Uint64(p[16:])}}, nil
	case Ping:
		return Ping{Nonce: be.Uint64(p)}, nil
	case Pong:
		return Pong{Nonce: be.Uint64(p)}, nil
	case HelloReply:
		if !zero(p[2:4]) {
			return nil, bad
		}
		return HelloReply{Version: be.Uint16(p), Features: Features(be.Uint32(p[4:])), Instance: be.Uint64(p[8:])}, nil
	case Status:
		if !zero(p[1:4]) {
			return nil, bad
		}
		s := Status{Guard: GuardState(p[0]), GuardErrno: be.Uint32(p[4:])}
		for i, v := range s.fields() {
			*v = be.Uint64(p[8+8*i:])
		}
		return s, nil
	case TrimResult:
		d, ok := dur(p[16:])
		if !zero(p[4:8]) || !ok {
			return nil, bad
		}
		return TrimResult{Errno: be.Uint32(p), Trimmed: be.Uint64(p[8:]), Duration: d}, nil
	case CacheResult:
		d, ok := dur(p[24:])
		if !zero(p[5:8]) || !ok {
			return nil, bad
		}
		return CacheResult{Errno: be.Uint32(p), Method: CacheMethod(p[4]), Before: be.Uint64(p[8:]), After: be.Uint64(p[16:]), Duration: d}, nil
	case ErrorReply:
		if !zero(p[5:8]) {
			return nil, bad
		}
		return ErrorReply{Code: ErrorCode(be.Uint32(p)), Request: Type(p[4])}, nil
	}
	return m, nil // TrimRequest, CacheTrimRequest, ShutdownRequest, ShutdownAccepted: no payload
}
