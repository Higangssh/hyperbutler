//go:build windows

package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/broker/proto"
	"github.com/Higangssh/hyperbutler/internal/vmsecret"
	"github.com/Higangssh/hyperbutler/internal/vmtemplate"
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

// processUserPeer is a Peer that can name the user of the client process before any
// frame has been read. The shared endpoint uses it for its pending-connection cap only;
// identity always comes from Identify.
type processUserPeer interface {
	ClientProcessUser() (string, error)
}

// VMSpec is what the server asks the backend to start. It is built only from broker
// state, fixed limits and the typed fields of the start request; nothing in it is a
// caller-supplied path, command line or document.
type VMSpec struct {
	Owner string // user SID; the backend tags the VM with it (ADR 0004 rule 9)
	// Slot is 0 for the owner's Docker VM and 1..proto.MaxSlot for a sandbox session
	// (ADR 0009), which boots the sandbox image from a disk of its own.
	Slot     uint8
	CPUs     uint32
	MemoryMB uint32
	DataDir  *DataDir // validated directory handle; open everything below it by handle

	// Net is the guest network, nil for none; vmtemplate renders it.
	Net *vmtemplate.Net
	// Share, DaemonConfig and BlankDisk are the start request's feature bits.
	Share, DaemonConfig, BlankDisk bool
}

// VMStatus is a backend's view of one owner's VM.
type VMStatus struct {
	// Running is set while the VM exists and has not stopped; Paused says it is paused.
	Running  bool
	Paused   bool
	ID       [16]byte
	CPUs     uint32
	MemoryMB uint32
	// Secret is the running VM's secret (ADR 0004 rule 6): made by the backend for each
	// start, given to the guest on its kernel command line, and sent to the owner only,
	// on the owner's own endpoint.
	Secret vmsecret.Secret
}

// ErrNotRunning is returned by Backend.Stop, Backend.Resize and Backend.Pause when the
// owner has no running VM.
var ErrNotRunning = errors.New("broker: VM is not running")

// ErrImageRejected and ErrDiskRejected are wrapped by a Backend.Start that refused the
// broker's guest image or the data disk below the data location. The caller learns only
// which; the detail is logged.
var (
	ErrImageRejected = errors.New("broker: guest image rejected")
	ErrDiskRejected  = errors.New("broker: data disk rejected")
)

// VMKey names one VM: an owner's Docker VM (Slot 0) or one of their sandbox sessions.
type VMKey struct {
	Owner string // user SID
	Slot  uint8
}

// Key is the VMKey of spec.
func (spec *VMSpec) Key() VMKey { return VMKey{Owner: spec.Owner, Slot: spec.Slot} }

// Backend runs VMs on behalf of the server. Every method is keyed by owner SID and slot,
// and an implementation must never let one owner act on another owner's VM, nor one
// slot's request act on another slot's VM.
type Backend interface {
	// Start creates and starts the VM of spec's owner and slot, or returns the status of
	// the one already running.
	Start(ctx context.Context, spec VMSpec) (VMStatus, error)
	Stop(ctx context.Context, key VMKey, force bool) error
	Status(ctx context.Context, key VMKey) (VMStatus, error)
	// Resize changes a running VM. A zero value means unchanged.
	Resize(ctx context.Context, key VMKey, cpus, memoryMB uint32) error
	// Pause pauses a running VM, or resumes it.
	Pause(ctx context.Context, key VMKey, resume bool) error
}

// Config configures a Server. Zero values take the defaults in New.
type Config struct {
	Backend Backend
	// Version is reported by the version verb: 1..64 printable ASCII characters.
	Version string

	// Resource limits that resize enforces, and the defaults of a new user.
	MaxCPUs, DefaultCPUs                    uint32
	MinMemoryMB, MaxMemoryMB, DefaultMemory uint32
	// SessionCPUs and SessionMemory are a sandbox session's defaults (ADR 0009: smaller
	// than the Docker VM's), and MaxTotalMemoryMB caps the memory of all of one user's
	// running VMs together (default MaxMemoryMB), so sessions cannot starve the host.
	SessionCPUs, SessionMemory, MaxTotalMemoryMB uint32

	// AllowedSIDs, if not empty, is the set of user SIDs that may use this server (a
	// per-user endpoint has exactly one). The pipe ACL already enforces this; the check
	// is defense in depth.
	AllowedSIDs []string

	// MaxConnsPerUser caps one user's concurrent identified connections on each endpoint
	// (default 3: the Docker engine, the sandbox manager and a short-lived tool such as
	// doctor), so that one user cannot hold every instance of the shared endpoint.
	MaxConnsPerUser int

	// RequestTimeout bounds each backend call and each data-location check.
	// IdleTimeout bounds the wait for each request frame, and IOTimeout each reply write.
	RequestTimeout, IdleTimeout, IOTimeout time.Duration
	// FirstFrameTimeout bounds the wait for the first frame on the shared endpoint
	// (default 3 s), so connections that send nothing free its instances soon.
	FirstFrameTimeout time.Duration

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
	conns  map[string]int // identified connections by SID
	// opener creates (or finds) the caller's own endpoint and returns its name, for the
	// open-endpoint verb on the shared endpoint; ListenEndpoints sets it.
	opener func(sid string) (string, error)
}

type userState struct {
	mu      sync.Mutex
	dataDir *DataDir // shared by the user's Docker VM and sessions
	vms     [proto.MaxSlot + 1]vmState
}

// vmState is the broker's state of one slot of a user.
type vmState struct {
	cpus     uint32
	memoryMB uint32
	// lease is the connection whose start created the running VM (Implementation
	// decision 6): when it ends, the VM is terminated. One connection may hold the
	// leases of several slots.
	lease *lease
}

// lease identifies one connection; only its pointer identity matters.
type lease struct{ _ byte }

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
	def(&cfg.SessionCPUs, 2)
	def(&cfg.SessionMemory, 1024)
	def(&cfg.MaxTotalMemoryMB, cfg.MaxMemoryMB)
	if cfg.MaxCPUs > proto.MaxCPUs || cfg.DefaultCPUs > cfg.MaxCPUs || cfg.SessionCPUs > cfg.MaxCPUs ||
		cfg.MinMemoryMB < proto.MinMemoryMB || cfg.MaxMemoryMB > proto.MaxMemoryMB ||
		cfg.MinMemoryMB > cfg.MaxMemoryMB ||
		cfg.DefaultMemory < cfg.MinMemoryMB || cfg.DefaultMemory > cfg.MaxMemoryMB ||
		cfg.SessionMemory < cfg.MinMemoryMB || cfg.SessionMemory > cfg.MaxMemoryMB ||
		cfg.MaxTotalMemoryMB < max(cfg.DefaultMemory, cfg.SessionMemory) {
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
	if cfg.FirstFrameTimeout == 0 {
		cfg.FirstFrameTimeout = 3 * time.Second
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
	if cfg.MaxConnsPerUser == 0 {
		cfg.MaxConnsPerUser = 3
	}
	return &Server{cfg: cfg, users: make(map[string]*userState), conns: make(map[string]int)}, nil
}

func (s *Server) addConn(sid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns[sid] >= s.cfg.MaxConnsPerUser {
		return false
	}
	s.conns[sid]++
	return true
}

func (s *Server) dropConn(sid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns[sid]--; s.conns[sid] <= 0 {
		delete(s.conns, sid)
	}
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
		u = &userState{}
		u.vms[0] = vmState{cpus: s.cfg.DefaultCPUs, memoryMB: s.cfg.DefaultMemory}
		for i := 1; i < len(u.vms); i++ {
			u.vms[i] = vmState{cpus: s.cfg.SessionCPUs, memoryMB: s.cfg.SessionMemory}
		}
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
//
// owner says which endpoint the connection came in on (ADR 0004 rule 5). With owner
// empty it is the shared endpoint, which any allowed caller may use for the version
// and open-endpoint verbs only; every VM verb is refused there, so no VM state and no
// secret is ever sent on it. Otherwise it is owner's own endpoint: the caller's token
// SID must be owner, every verb but open-endpoint is served, and status replies carry
// the running VM's secret.
func (s *Server) ServeConn(ctx context.Context, rw io.ReadWriter, p Peer, owner string) error {
	var (
		buf    [proto.MaxFrameSize]byte
		id     *Identity
		own    = new(lease)
		shared = owner == ""
	)
	dl, _ := rw.(deadliner)
	write := func(r proto.Reply) error {
		b, err := proto.EncodeReply(r)
		if err != nil {
			s.cfg.Logf("broker: encode reply %+v: %v", r, err)
			b = internalErrorFrame
		}
		if shared && !r.Secret.IsZero() {
			// handle never builds one; this is the last line of defense.
			s.cfg.Logf("broker: refused to send a VM secret on the shared endpoint (type %#x)", r.Type)
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
	if shared {
		// Before the first frame no token identifies the caller, so the per-user cap
		// below cannot apply yet; count the client process's user instead (a cap only,
		// never an identity), so one user's silent connections cannot hold every
		// instance of the shared endpoint.
		if pi, ok := p.(processUserPeer); ok {
			if sid, err := pi.ClientProcessUser(); err == nil {
				key := "process/" + sid
				if !s.addConn(key) {
					return fail(proto.CodeBusy, "too many connections for this user",
						fmt.Errorf("%w: process user %s over its connection limit on the shared endpoint", errRefusedEarly, sid))
				}
				defer s.dropConn(key)
			}
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if dl != nil {
			wait := s.cfg.IdleTimeout
			if shared && id == nil {
				// A client of the shared endpoint sends its request at once.
				wait = s.cfg.FirstFrameTimeout
			}
			deadline := time.Now().Add(wait)
			if id != nil && s.holds(id.SID, own) {
				// The lease's connection waits without a deadline: the client's exit
				// closes the pipe and ends it. An idle limit would end the lease, and
				// so the VM, when the host wakes from sleep, where the wait has
				// expired before the engine's next status request can run.
				deadline = time.Time{}
			}
			_ = dl.SetReadDeadline(deadline)
		}
		b, err := proto.ReadFrame(rw, &buf)
		switch {
		case errors.Is(err, io.EOF):
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
			if !s.allowed(got.SID) || !shared && got.SID != owner {
				return fail(proto.CodeAccessDenied, "caller not allowed on this endpoint",
					fmt.Errorf("broker: caller %s not allowed", got.SID))
			}
			// The cap is per endpoint: on the shared one it stops one user holding every
			// instance; a user's own endpoint is theirs alone.
			key := got.SID
			if shared {
				key = "shared/" + got.SID
			}
			if !s.addConn(key) {
				return fail(proto.CodeBusy, "too many connections for this user",
					fmt.Errorf("broker: caller %s over its connection limit", got.SID))
			}
			defer s.dropConn(key)
			if !shared {
				defer s.release(got.SID, own)
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
		var r proto.Reply
		if shared {
			r = s.handleShared(id.SID, req)
		} else {
			r = s.handle(ctx, *id, p, own, req)
		}
		if err := write(r); err != nil {
			return err
		}
	}
}

// handleShared executes one request on the shared endpoint: only the version and
// open-endpoint verbs are served there.
func (s *Server) handleShared(sid string, req proto.Request) proto.Reply {
	switch req.Op {
	case proto.OpVersion:
		return proto.Reply{Type: okType(req.Op), ProtoVersion: proto.Version, BrokerVersion: s.cfg.Version}
	case proto.OpOpenEndpoint:
		s.mu.Lock()
		open, closed := s.opener, s.closed
		s.mu.Unlock()
		if closed {
			return errorReply(proto.CodeBusy, "broker is shutting down")
		}
		if open == nil {
			return s.internal("open endpoint", errors.New("no per-user endpoints on this server"))
		}
		name, err := open(sid)
		switch {
		case errors.Is(err, ErrPipeExists):
			// Someone created the caller's pipe name before the broker did (ADR 0004 rule
			// 5): fail loudly, never fall back to another name.
			s.cfg.Logf("broker: the endpoint of %s is squatted: %v", sid, err)
			return errorReply(proto.CodeEndpointSquatted, "your endpoint's pipe name is taken by another process")
		case errors.Is(err, ErrListenerClosed):
			return errorReply(proto.CodeBusy, "broker is shutting down")
		case errors.Is(err, errTooManyEndpoints):
			s.cfg.Logf("broker: open endpoint for %s: %v", sid, err)
			return errorReply(proto.CodeBusy, "the broker serves too many users")
		case err != nil:
			return s.internal("open endpoint", err)
		}
		return proto.Reply{Type: okType(req.Op), PipeName: name}
	}
	return errorReply(proto.CodeWrongEndpoint, "open your own endpoint first; the shared endpoint serves only version and open-endpoint")
}

// errRefusedEarly wraps the error of a connection refused before its first frame, whose
// pipe instance is given back without lingering.
var errRefusedEarly = errors.New("broker: connection refused before its first request")

// errTooManyEndpoints is returned by an opener that has reached its per-user endpoint
// limit.
var errTooManyEndpoints = errors.New("broker: too many per-user endpoints")

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

// holds reports whether connection own holds the lease on any of sid's VMs.
func (s *Server) holds(sid string, own *lease) bool {
	s.mu.Lock()
	u := s.users[sid]
	s.mu.Unlock()
	if u == nil {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := range u.vms {
		if u.vms[i].lease == own {
			return true
		}
	}
	return false
}

// release ends every lease that connection own holds on sid's VMs: each such VM is
// terminated (Implementation decision 6, as the engine's exit does in developer mode).
func (s *Server) release(sid string, own *lease) {
	s.mu.Lock()
	u := s.users[sid]
	s.mu.Unlock()
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := range u.vms {
		if u.vms[i].lease != own {
			continue
		}
		u.vms[i].lease = nil
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.RequestTimeout)
		err := s.cfg.Backend.Stop(ctx, VMKey{Owner: sid, Slot: uint8(i)}, true)
		cancel()
		if err != nil && !errors.Is(err, ErrNotRunning) {
			s.cfg.Logf("broker: terminate the VM of a closed lease (slot %d): %v", i, err)
		}
	}
}

// runningMemory is the memory of sid's running VMs other than skip, as their backend
// reports it. u.mu is held.
func (s *Server) runningMemory(ctx context.Context, sid string, u *userState, skip uint8) (uint64, error) {
	var sum uint64
	for i := range u.vms {
		if uint8(i) == skip {
			continue
		}
		st, err := s.cfg.Backend.Status(ctx, VMKey{Owner: sid, Slot: uint8(i)})
		if err != nil {
			return 0, err
		}
		if st.Running {
			m := st.MemoryMB
			if m == 0 {
				m = u.vms[i].memoryMB
			}
			sum += uint64(m)
		}
	}
	return sum, nil
}

// handle executes one decoded request for id on the connection own. It always returns a
// reply.
func (s *Server) handle(ctx context.Context, id Identity, p Peer, own *lease, req proto.Request) proto.Reply {
	switch req.Op {
	case proto.OpVersion:
		return proto.Reply{Type: okType(req.Op), ProtoVersion: proto.Version, BrokerVersion: s.cfg.Version}
	case proto.OpOpenEndpoint:
		return errorReply(proto.CodeWrongEndpoint, "this is already your own endpoint")
	}
	u, ok := s.user(id.SID)
	if !ok {
		return errorReply(proto.CodeBusy, "broker is shutting down")
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()

	if int(req.Slot) >= len(u.vms) {
		// The decoder bounds Slot already; an index panic would end the service.
		return errorReply(proto.CodeBadRequest, "no such slot")
	}
	key := VMKey{Owner: id.SID, Slot: req.Slot}
	vs := &u.vms[req.Slot]

	switch req.Op {
	case proto.OpStatus:
		st, err := s.cfg.Backend.Status(ctx, key)
		if err != nil {
			return s.internal("status", err)
		}
		return s.statusReply(req.Op, u, vs, st)

	case proto.OpStart:
		if u.dataDir == nil {
			return errorReply(proto.CodeNoDataLocation, "set a data location first")
		}
		cpus, mem := vs.cpus, vs.memoryMB
		if req.CPUs != 0 {
			cpus = req.CPUs
		}
		if req.MemoryMB != 0 {
			mem = req.MemoryMB
		}
		if cpus < 1 || cpus > s.cfg.MaxCPUs || mem < s.cfg.MinMemoryMB || mem > s.cfg.MaxMemoryMB {
			return errorReply(proto.CodeOutOfRange, "start outside the broker's limits")
		}
		st, err := s.cfg.Backend.Status(ctx, key)
		if err != nil {
			return s.internal("status", err)
		}
		if st.Running {
			// One VM per slot, and its lifetime is its lease: a second start on the
			// same connection reports it, any other connection is refused.
			if vs.lease == own {
				return s.statusReply(req.Op, u, vs, st)
			}
			return errorReply(proto.CodeLeaseHeld, "the VM runs under another connection's lease")
		}
		others, err := s.runningMemory(ctx, id.SID, u, req.Slot)
		if err != nil {
			return s.internal("status", err)
		}
		if others+uint64(mem) > uint64(s.cfg.MaxTotalMemoryMB) {
			return errorReply(proto.CodeOutOfRange, "your running VMs would use more memory than the broker allows")
		}
		// The owner can change the directory's ACL after it was accepted, so check it
		// again on every start.
		if err := u.dataDir.revalidate(id.SID, s.cfg.validateDataDir); err != nil {
			return rejectReply(err)
		}
		spec := VMSpec{Owner: id.SID, Slot: req.Slot, CPUs: cpus, MemoryMB: mem, DataDir: u.dataDir,
			Net: startNet(req), Share: req.Features&proto.FeatureShare != 0,
			DaemonConfig: req.Features&proto.FeatureDaemonConfig != 0, BlankDisk: req.Features&proto.FeatureBlankDisk != 0}
		st, err = s.cfg.Backend.Start(ctx, spec)
		switch {
		case errors.Is(err, ErrImageRejected):
			s.cfg.Logf("broker: start: %v", err)
			return errorReply(proto.CodeImageRejected, "the broker's guest image failed its checks")
		case errors.Is(err, ErrDiskRejected):
			s.cfg.Logf("broker: start: %v", err)
			return errorReply(proto.CodeDiskRejected, "the data disk failed its checks")
		case err != nil:
			return s.internal("start", err)
		}
		vs.cpus, vs.memoryMB, vs.lease = cpus, mem, own
		return s.statusReply(req.Op, u, vs, st)

	case proto.OpPause:
		err := s.cfg.Backend.Pause(ctx, key, req.Resume)
		switch {
		case errors.Is(err, ErrNotRunning):
			return errorReply(proto.CodeNotRunning, "VM is not running")
		case err != nil:
			return s.internal("pause", err)
		}
		st, err := s.cfg.Backend.Status(ctx, key)
		if err != nil {
			return s.internal("status", err)
		}
		return s.statusReply(req.Op, u, vs, st)

	case proto.OpStop:
		err := s.cfg.Backend.Stop(ctx, key, req.Force)
		switch {
		case errors.Is(err, ErrNotRunning):
			vs.lease = nil
			return errorReply(proto.CodeNotRunning, "VM is not running")
		case err != nil:
			return s.internal("stop", err)
		}
		if req.Force {
			// Gone now; an orderly stop keeps the lease until the VM is down, so that the
			// lease holder's close still terminates one that does not power off.
			vs.lease = nil
		}
		return proto.Reply{Type: okType(req.Op)}

	case proto.OpResize:
		cpus, mem := vs.cpus, vs.memoryMB
		if req.CPUs != 0 {
			cpus = req.CPUs
		}
		if req.MemoryMB != 0 {
			mem = req.MemoryMB
		}
		if cpus < 1 || cpus > s.cfg.MaxCPUs || mem < s.cfg.MinMemoryMB || mem > s.cfg.MaxMemoryMB {
			return errorReply(proto.CodeOutOfRange, "resize outside the broker's limits")
		}
		st, err := s.cfg.Backend.Status(ctx, key)
		if err != nil {
			return s.internal("status", err)
		}
		if st.Running {
			others, err := s.runningMemory(ctx, id.SID, u, req.Slot)
			if err != nil {
				return s.internal("status", err)
			}
			if others+uint64(mem) > uint64(s.cfg.MaxTotalMemoryMB) {
				return errorReply(proto.CodeOutOfRange, "your running VMs would use more memory than the broker allows")
			}
			err = s.cfg.Backend.Resize(ctx, key, req.CPUs, req.MemoryMB)
			switch {
			case errors.Is(err, ErrNotRunning):
				// Stopped in between; the new values apply at the next start.
			case err != nil:
				return s.internal("resize", err)
			}
			if st, err = s.cfg.Backend.Status(ctx, key); err != nil {
				return s.internal("status", err)
			}
		}
		vs.cpus, vs.memoryMB = cpus, mem
		return s.statusReply(req.Op, u, vs, st)

	case proto.OpSetDataLocation:
		// Every slot's disk lives below the data location.
		running, err := s.runningMemory(ctx, id.SID, u, noSlot)
		if err != nil {
			return s.internal("status", err)
		}
		dctx, dcancel := context.WithTimeout(ctx, dataDirTimeout)
		defer dcancel()
		d, err := s.openDataDirBounded(dctx, p, req.Handle, id.SID)
		if err != nil {
			if running != 0 {
				return errorReply(proto.CodeBusy, "stop your VMs before moving their data")
			}
			// The reply carries the fixed reason only; the cause is for the log.
			s.cfg.Logf("broker: set data location for %s: %v", id.SID, err)
			return rejectReply(err)
		}
		if running != 0 {
			// The Docker engine and the sandbox manager are two processes of one user, and
			// each sends the location before its first start. The same folder moves nothing.
			same := u.dataDir != nil && u.dataDir.same(d)
			_ = d.Close()
			if !same {
				return errorReply(proto.CodeBusy, "stop your VMs before moving their data")
			}
			return proto.Reply{Type: okType(req.Op)}
		}
		if u.dataDir != nil {
			_ = u.dataDir.Close()
		}
		u.dataDir = d
		return proto.Reply{Type: okType(req.Op)}
	}
	return errorReply(proto.CodeBadRequest, "unknown op")
}

// dataDirTimeout caps a data-location check below RequestTimeout: a local NTFS directory
// answers in milliseconds.
const dataDirTimeout = 10 * time.Second

// openDataDirBounded runs openDataDir but stops waiting when ctx is done. Reopening a
// caller-chosen object goes through whatever file system or filter serves it (a remote
// share, a ProjFS or cloud-files provider the caller runs), which may never answer, and
// those calls cannot be canceled. On timeout the caller gets a rejection, the per-user
// lock and the pipe instance are released, and the abandoned goroutine closes the handle
// if the call ever returns.
func (s *Server) openDataDirBounded(ctx context.Context, p Peer, v uint64, sid string) (*DataDir, error) {
	type result struct {
		d   *DataDir
		err error
	}
	ch := make(chan result, 1)
	var mu sync.Mutex
	abandoned := false
	go func() {
		d, err := openDataDir(p, v, sid, s.cfg.validateDataDir)
		mu.Lock()
		defer mu.Unlock()
		if abandoned {
			if d != nil {
				_ = d.Close()
			}
			s.cfg.Logf("broker: data location check finished after timeout: %v", err)
			return
		}
		ch <- result{d, err}
	}()
	select {
	case r := <-ch:
		return r.d, r.err
	case <-ctx.Done():
		mu.Lock()
		defer mu.Unlock()
		select {
		case r := <-ch:
			return r.d, r.err
		default:
		}
		abandoned = true
		return nil, &rejectError{code: proto.CodeInvalidHandle, reason: "data location check timed out", err: ctx.Err()}
	}
}

// startNet is the guest network of a start request, nil without FeatureNet. The request
// was validated by the decoder: a masked private IPv4 subnet, an MTU in range, known
// flags.
func startNet(req proto.Request) *vmtemplate.Net {
	if req.Features&proto.FeatureNet == 0 {
		return nil
	}
	n := &vmtemplate.Net{
		Subnet:      netip.PrefixFrom(netip.AddrFrom4(req.NetAddr), int(req.NetPrefix)),
		MTU:         int(req.NetMTU),
		Offload:     req.NetFlags&proto.NetOffload != 0,
		HostGateway: req.NetFlags&proto.NetHostGateway != 0,
	}
	switch {
	case req.NetFlags&proto.NetCRCOff != 0:
		n.CRC = vmtemplate.CRCOff
	case req.NetFlags&proto.NetCRCStrict != 0:
		n.CRC = vmtemplate.CRCStrict
	}
	return n
}

func (s *Server) internal(what string, err error) proto.Reply {
	// Backend errors can carry host paths and HCS detail; log them, don't send them.
	s.cfg.Logf("broker: %s: %v", what, err)
	return errorReply(proto.CodeInternal, what+" failed")
}

// noSlot is a slot number no VM has, for runningMemory to count every slot.
const noSlot = proto.MaxSlot + 1

func (s *Server) statusReply(op proto.Op, u *userState, vs *vmState, st VMStatus) proto.Reply {
	r := proto.Reply{
		Type:            okType(op),
		State:           proto.StateStopped,
		CPUs:            vs.cpus,
		MemoryMB:        vs.memoryMB,
		DataLocationSet: u.dataDir != nil,
	}
	if st.Running {
		// statusReply serves the owner's own endpoint only (handle); the secret goes to
		// the owner and nobody else.
		r.State, r.VMID, r.Secret = proto.StateRunning, st.ID, st.Secret
		if st.Paused {
			r.State = proto.StatePaused
		}
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
