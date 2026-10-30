//go:build windows

package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Higangssh/winbox/internal/broker/proto"
	"golang.org/x/sys/windows"
)

// Identity is the caller of one connection, as reported by the OS (ADR 0004 rule 4). It
// never comes from a message.
type Identity struct {
	// SID is the caller's user SID in canonical string form (S-1-5-21-...). Per-user
	// state is keyed by it.
	SID string
}

// Peer is the authenticated client end of one connection. The named-pipe implementation
// takes everything from the pipe's client token and client process; tests substitute
// their own.
type Peer interface {
	// Identify returns the caller's identity. The server calls it once, after the first
	// frame has been read (a named pipe cannot impersonate before that).
	Identify() (Identity, error)
	// DuplicateHandle copies the handle with value v out of the client process into the
	// broker process, with the same access the client holds. The caller closes it.
	DuplicateHandle(v uint64) (windows.Handle, error)
	// Impersonate runs fn on an OS thread that impersonates the client, and reverts
	// before returning.
	Impersonate(fn func() error) error
}

// VMSpec is what the server asks the backend to start. It is built only from broker
// state and fixed limits; nothing in it is a caller-supplied path or document.
type VMSpec struct {
	Owner    string // user SID; the backend tags the VM with it (ADR 0004 rule 9)
	CPUs     uint32
	MemoryMB uint32
	DataDir  *DataDir // validated directory handle; open everything below it by handle
}

// VMStatus is a backend's view of one owner's VM.
type VMStatus struct {
	Running  bool
	ID       [16]byte
	CPUs     uint32
	MemoryMB uint32
}

// ErrNotRunning is returned by Backend.Stop and Backend.Resize when the owner has no
// running VM.
var ErrNotRunning = errors.New("broker: VM is not running")

// Backend runs VMs on behalf of the server. Every method is keyed by the owner SID, and
// an implementation must never let one owner act on another owner's VM.
type Backend interface {
	// Start creates and starts the owner's VM, or returns the status of the one already
	// running.
	Start(ctx context.Context, spec VMSpec) (VMStatus, error)
	Stop(ctx context.Context, owner string, force bool) error
	Status(ctx context.Context, owner string) (VMStatus, error)
	// Resize changes a running VM. A zero value means unchanged.
	Resize(ctx context.Context, owner string, cpus, memoryMB uint32) error
}

// Config configures a Server. Zero values take the defaults in New.
type Config struct {
	Backend Backend
	// Version is reported by the version verb: 1..64 printable ASCII characters.
	Version string

	// Resource limits that resize enforces, and the defaults of a new user.
	MaxCPUs, DefaultCPUs                    uint32
	MinMemoryMB, MaxMemoryMB, DefaultMemory uint32

	// AllowedSIDs, if not empty, is the set of user SIDs that may use this server (a
	// per-user endpoint has exactly one). The pipe ACL already enforces this; the check
	// is defence in depth.
	AllowedSIDs []string

	// RequestTimeout bounds each backend call. IdleTimeout bounds the wait for each
	// request frame, and IOTimeout each reply write.
	RequestTimeout, IdleTimeout, IOTimeout time.Duration

	// Logf receives internal errors that are not sent to the caller. Nil discards them.
	Logf func(format string, args ...any)

	// validateDataDir replaces ValidateDataDir in tests that use SIDs which own nothing.
	validateDataDir func(h windows.Handle, sid string) error
}

// Server handles broker requests. It is safe for concurrent use by many connections.
type Server struct {
	cfg Config

	mu     sync.Mutex
	closed bool
	users  map[string]*userState
}

type userState struct {
	mu       sync.Mutex
	cpus     uint32
	memoryMB uint32
	dataDir  *DataDir
}

// New returns a Server for cfg.
func New(cfg Config) (*Server, error) {
	if cfg.Backend == nil {
		return nil, errors.New("broker: no backend")
	}
	def := func(v *uint32, d uint32) {
		if *v == 0 {
			*v = d
		}
	}
	def(&cfg.MaxCPUs, 8)
	def(&cfg.DefaultCPUs, 2)
	def(&cfg.MinMemoryMB, 512)
	def(&cfg.MaxMemoryMB, 16<<10)
	def(&cfg.DefaultMemory, 2048)
	if cfg.MaxCPUs > proto.MaxCPUs || cfg.DefaultCPUs > cfg.MaxCPUs ||
		cfg.MinMemoryMB < proto.MinMemoryMB || cfg.MaxMemoryMB > proto.MaxMemoryMB ||
		cfg.MinMemoryMB > cfg.MaxMemoryMB ||
		cfg.DefaultMemory < cfg.MinMemoryMB || cfg.DefaultMemory > cfg.MaxMemoryMB {
		return nil, errors.New("broker: resource limits out of range")
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	if _, err := proto.EncodeReply(proto.Reply{Type: okType(proto.OpVersion), ProtoVersion: proto.Version, BrokerVersion: cfg.Version}); err != nil {
		return nil, fmt.Errorf("broker: bad version string: %w", err)
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 2 * time.Minute
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 30 * time.Second
	}
	if cfg.IOTimeout == 0 {
		cfg.IOTimeout = 5 * time.Second
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.validateDataDir == nil {
		cfg.validateDataDir = ValidateDataDir
	}
	return &Server{cfg: cfg, users: make(map[string]*userState)}, nil
}

// Close releases every data directory handle the server holds. It does not stop VMs.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for _, u := range s.users {
		u.mu.Lock()
		if u.dataDir != nil {
			_ = u.dataDir.Close()
			u.dataDir = nil
		}
		u.mu.Unlock()
	}
	return nil
}

func (s *Server) user(sid string) (*userState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false
	}
	u, ok := s.users[sid]
	if !ok {
		u = &userState{cpus: s.cfg.DefaultCPUs, memoryMB: s.cfg.DefaultMemory}
		s.users[sid] = u
	}
	return u, true
}

// deadliner is implemented by connections that support per-operation deadlines.
type deadliner interface {
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

// internalErrorFrame is sent if a reply fails to encode, which would be a server bug.
var internalErrorFrame = func() []byte {
	b, err := proto.EncodeReply(proto.Reply{Type: proto.TypeError, Code: proto.CodeInternal})
	if err != nil {
		panic(err)
	}
	return b
}()

// ServeConn serves requests on one connection until the client closes it, a frame is
// malformed, the caller is rejected, or ctx is done. Every frame it writes is a valid
// reply. After a malformed frame it sends one error reply and returns, because the
// stream can no longer be trusted to be in sync.
func (s *Server) ServeConn(ctx context.Context, rw io.ReadWriter, p Peer) error {
	var (
		buf [proto.MaxFrameSize]byte
		id  *Identity
	)
	dl, _ := rw.(deadliner)
	write := func(r proto.Reply) error {
		b, err := proto.EncodeReply(r)
		if err != nil {
			s.cfg.Logf("broker: encode reply %+v: %v", r, err)
			b = internalErrorFrame
		}
		if dl != nil {
			_ = dl.SetWriteDeadline(time.Now().Add(s.cfg.IOTimeout))
		}
		_, err = rw.Write(b)
		return err
	}
	fail := func(code proto.Code, msg string, cause error) error {
		if err := write(errorReply(code, msg)); err != nil {
			return err
		}
		return cause
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if dl != nil {
			_ = dl.SetReadDeadline(time.Now().Add(s.cfg.IdleTimeout))
		}
		b, err := proto.ReadFrame(rw, &buf)
		switch {
		case err == io.EOF:
			return nil
		case errors.Is(err, proto.ErrFrameTooLarge), errors.Is(err, proto.ErrInvalid):
			return fail(proto.CodeBadRequest, "malformed frame", err)
		case err != nil:
			return err
		}
		if id == nil {
			got, err := p.Identify()
			if err != nil {
				s.cfg.Logf("broker: identify caller: %v", err)
				return fail(proto.CodeAccessDenied, "caller identity rejected", err)
			}
			if !s.allowed(got.SID) {
				return fail(proto.CodeAccessDenied, "caller not allowed on this endpoint",
					fmt.Errorf("broker: caller %s not allowed", got.SID))
			}
			id = &got
		}
		req, err := proto.DecodeRequest(b)
		if err != nil {
			if errors.Is(err, proto.ErrUnsupportedVersion) {
				return fail(proto.CodeUnsupportedVersion, "unsupported protocol version", err)
			}
			return fail(proto.CodeBadRequest, "malformed request", err)
		}
		if err := write(s.handle(ctx, *id, p, req)); err != nil {
			return err
		}
	}
}

func (s *Server) allowed(sid string) bool {
	if sid == "" {
		return false
	}
	if len(s.cfg.AllowedSIDs) == 0 {
		return true
	}
	for _, a := range s.cfg.AllowedSIDs {
		if a == sid {
			return true
		}
	}
	return false
}

func okType(op proto.Op) uint8 { return proto.ReplyFlag | uint8(op) }

func errorReply(code proto.Code, msg string) proto.Reply {
	return proto.Reply{Type: proto.TypeError, Code: code, Message: msg}
}

// handle executes one decoded request for id. It always returns a reply.
func (s *Server) handle(ctx context.Context, id Identity, p Peer, req proto.Request) proto.Reply {
	if req.Op == proto.OpVersion {
		return proto.Reply{Type: okType(req.Op), ProtoVersion: proto.Version, BrokerVersion: s.cfg.Version}
	}
	u, ok := s.user(id.SID)
	if !ok {
		return errorReply(proto.CodeBusy, "broker is shutting down")
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()

	switch req.Op {
	case proto.OpStatus:
		st, err := s.cfg.Backend.Status(ctx, id.SID)
		if err != nil {
			return s.internal("status", err)
		}
		return s.statusReply(req.Op, u, st)

	case proto.OpStart:
		if u.dataDir == nil {
			return errorReply(proto.CodeNoDataLocation, "set a data location first")
		}
		// The owner can change the directory's ACL after it was accepted, so check it
		// again on every start.
		if err := u.dataDir.revalidate(id.SID, s.cfg.validateDataDir); err != nil {
			return rejectReply(err)
		}
		st, err := s.cfg.Backend.Start(ctx, VMSpec{Owner: id.SID, CPUs: u.cpus, MemoryMB: u.memoryMB, DataDir: u.dataDir})
		if err != nil {
			return s.internal("start", err)
		}
		return s.statusReply(req.Op, u, st)

	case proto.OpStop:
		err := s.cfg.Backend.Stop(ctx, id.SID, req.Force)
		switch {
		case errors.Is(err, ErrNotRunning):
			return errorReply(proto.CodeNotRunning, "VM is not running")
		case err != nil:
			return s.internal("stop", err)
		}
		return proto.Reply{Type: okType(req.Op)}

	case proto.OpResize:
		cpus, mem := u.cpus, u.memoryMB
		if req.CPUs != 0 {
			cpus = req.CPUs
		}
		if req.MemoryMB != 0 {
			mem = req.MemoryMB
		}
		if cpus < 1 || cpus > s.cfg.MaxCPUs || mem < s.cfg.MinMemoryMB || mem > s.cfg.MaxMemoryMB {
			return errorReply(proto.CodeOutOfRange, "resize outside the broker's limits")
		}
		st, err := s.cfg.Backend.Status(ctx, id.SID)
		if err != nil {
			return s.internal("status", err)
		}
		if st.Running {
			err := s.cfg.Backend.Resize(ctx, id.SID, req.CPUs, req.MemoryMB)
			switch {
			case errors.Is(err, ErrNotRunning):
				// Stopped in between; the new values apply at the next start.
			case err != nil:
				return s.internal("resize", err)
			}
			if st, err = s.cfg.Backend.Status(ctx, id.SID); err != nil {
				return s.internal("status", err)
			}
		}
		u.cpus, u.memoryMB = cpus, mem
		return s.statusReply(req.Op, u, st)

	case proto.OpSetDataLocation:
		st, err := s.cfg.Backend.Status(ctx, id.SID)
		if err != nil {
			return s.internal("status", err)
		}
		if st.Running {
			return errorReply(proto.CodeBusy, "stop the VM before moving its data")
		}
		d, err := openDataDir(p, req.Handle, id.SID, s.cfg.validateDataDir)
		if err != nil {
			return rejectReply(err)
		}
		if u.dataDir != nil {
			_ = u.dataDir.Close()
		}
		u.dataDir = d
		return proto.Reply{Type: okType(req.Op)}
	}
	return errorReply(proto.CodeBadRequest, "unknown op")
}

func (s *Server) internal(what string, err error) proto.Reply {
	// Backend errors can carry host paths and HCS detail; log them, don't send them.
	s.cfg.Logf("broker: %s: %v", what, err)
	return errorReply(proto.CodeInternal, what+" failed")
}

func (s *Server) statusReply(op proto.Op, u *userState, st VMStatus) proto.Reply {
	r := proto.Reply{
		Type:            okType(op),
		State:           proto.StateStopped,
		CPUs:            u.cpus,
		MemoryMB:        u.memoryMB,
		DataLocationSet: u.dataDir != nil,
	}
	if st.Running {
		r.State, r.VMID = proto.StateRunning, st.ID
		if st.CPUs != 0 {
			r.CPUs = st.CPUs
		}
		if st.MemoryMB != 0 {
			r.MemoryMB = st.MemoryMB
		}
	}
	if err := r.Validate(); err != nil {
		return s.internal("status reply", err)
	}
	return r
}

// rejectError is a data-location rejection with a fixed, caller-safe reason.
type rejectError struct {
	code   proto.Code
	reason string
	err    error
}

func (e *rejectError) Error() string {
	if e.err != nil {
		return "broker: data location: " + e.reason + ": " + e.err.Error()
	}
	return "broker: data location: " + e.reason
}

func (e *rejectError) Unwrap() error { return e.err }

func reject(reason string, err error) error {
	return &rejectError{code: proto.CodeDataLocationRejected, reason: reason, err: err}
}

func rejectReply(err error) proto.Reply {
	var re *rejectError
	if errors.As(err, &re) {
		return errorReply(re.code, re.reason)
	}
	return errorReply(proto.CodeDataLocationRejected, "data location rejected")
}
