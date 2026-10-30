//go:build windows

package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/Higangssh/winbox/internal/broker/proto"
	"github.com/Higangssh/winbox/internal/transport"
	"golang.org/x/sys/windows"
)

// ErrPipeExists is returned by ListenPipe when the pipe name is already taken, for
// example by a process squatting on it. The broker never falls back to another name
// (ADR 0004 rule 5); `winbox doctor` reports this error.
var ErrPipeExists = errors.New("broker: pipe name is already in use")

// clientPipeAccess is what a client is granted on the pipe: FILE_GENERIC_READ and
// FILE_GENERIC_WRITE without FILE_CREATE_PIPE_INSTANCE (0x4), so a client cannot create
// a server instance of the broker's pipe and intercept other connections.
const clientPipeAccess = 0x0012019b

const (
	maxPipeInstances = 16
	pipeBufferSize   = 4096
	// lingerTimeout bounds how long a finished connection waits for the client to read
	// the last reply and close, before the instance is disconnected.
	lingerTimeout = 2 * time.Second
)

// PipeSDDL returns the security descriptor for a broker pipe. The server SID (the
// broker's own account: SYSTEM or its virtual account in production) owns the pipe and
// has full access, which it needs to create further instances. clientSID gets read and
// write access only. If clientSID is empty, interactive users (IU) get that access
// instead, which is the shape of the shared control endpoint. The DACL is protected so no
// inheritable ACE can widen it. Nobody else, including Administrators, can connect.
func PipeSDDL(serverSID, clientSID string) string {
	client := "IU"
	if clientSID != "" {
		client = clientSID
	}
	return fmt.Sprintf("O:%sG:%sD:P(A;;GA;;;%s)(A;;0x%08x;;;%s)",
		serverSID, serverSID, serverSID, clientPipeAccess, client)
}

// PipeConfig configures ListenPipe.
type PipeConfig struct {
	// Name is a local pipe name (transport.ValidatePipeName).
	Name string
	// ClientSID is the only user that may connect. Empty means interactive users.
	ClientSID string
	// Instances is the number of concurrent connections (1..16, default 4).
	Instances int
}

// PipeListener is a set of server instances of one named pipe. The first instance is
// created with FILE_FLAG_FIRST_PIPE_INSTANCE, so the pipe is the broker's own; it is
// byte-mode, overlapped, and rejects remote clients.
type PipeListener struct {
	name  string
	stop  windows.Handle // manual-reset event, set by Close
	insts []*pipeInstance

	mu      sync.Mutex
	closed  bool
	started bool // Serve was called; instances share one OVERLAPPED each, so only once
	serving sync.WaitGroup
}

// ErrAlreadyServing is returned by a second Serve call on the same listener.
var ErrAlreadyServing = errors.New("broker: listener is already being served")

// ListenPipe creates every instance of the pipe up front. It fails with ErrPipeExists if
// the name is taken.
func ListenPipe(cfg PipeConfig) (*PipeListener, error) {
	if err := transport.ValidatePipeName(cfg.Name); err != nil {
		return nil, err
	}
	n := cfg.Instances
	if n == 0 {
		n = 4
	}
	if n < 1 || n > maxPipeInstances {
		return nil, fmt.Errorf("broker: %d pipe instances", n)
	}
	self, err := processSID()
	if err != nil {
		return nil, err
	}
	if cfg.ClientSID != "" {
		if _, err := windows.StringToSid(cfg.ClientSID); err != nil {
			return nil, fmt.Errorf("broker: client SID %q: %w", cfg.ClientSID, err)
		}
	}
	sd, err := windows.SecurityDescriptorFromString(PipeSDDL(self, cfg.ClientSID))
	if err != nil {
		return nil, fmt.Errorf("broker: pipe security descriptor: %w", err)
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	name16, err := windows.UTF16PtrFromString(cfg.Name)
	if err != nil {
		return nil, err
	}
	stop, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return nil, err
	}
	l := &PipeListener{name: cfg.Name, stop: stop}
	for i := 0; i < n; i++ {
		flags := uint32(windows.PIPE_ACCESS_DUPLEX | windows.FILE_FLAG_OVERLAPPED)
		if i == 0 {
			flags |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
		}
		h, err := windows.CreateNamedPipe(name16, flags,
			windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
			uint32(n), pipeBufferSize, pipeBufferSize, 0, sa)
		if err != nil {
			l.closeHandles()
			if i == 0 && (errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_PIPE_BUSY)) {
				return nil, fmt.Errorf("%w: %s: %w", ErrPipeExists, cfg.Name, err)
			}
			return nil, fmt.Errorf("broker: create pipe %s: %w", cfg.Name, err)
		}
		inst, err := newPipeInstance(h, stop)
		if err != nil {
			windows.CloseHandle(h)
			l.closeHandles()
			return nil, err
		}
		l.insts = append(l.insts, inst)
	}
	runtime.KeepAlive(sd)
	return l, nil
}

func processSID() (string, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("broker: query process user: %w", err)
	}
	return tu.User.Sid.String(), nil
}

// Name returns the pipe name.
func (l *PipeListener) Name() string { return l.name }

// Close stops accepting, interrupts every connection, waits for Serve to return, and
// closes the pipe.
func (l *PipeListener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()
	_ = windows.SetEvent(l.stop)
	l.serving.Wait()
	l.closeHandles()
	return nil
}

func (l *PipeListener) closeHandles() {
	for _, inst := range l.insts {
		inst.close()
	}
	l.insts = nil
	if l.stop != 0 {
		windows.CloseHandle(l.stop)
		l.stop = 0
	}
}

// Serve serves connections on every instance of l until l is closed. It returns nil
// after Close. Each instance serves one connection at a time, so l's instance count
// bounds concurrency.
func (s *Server) Serve(l *PipeListener) error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return ErrListenerClosed
	}
	if l.started {
		l.mu.Unlock()
		return ErrAlreadyServing
	}
	l.started = true
	l.serving.Add(1)
	l.mu.Unlock()
	defer l.serving.Done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for _, inst := range l.insts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.serveInstance(ctx, inst)
		}()
	}
	wg.Wait()
	return nil
}

// ErrListenerClosed is returned by Serve on a closed listener.
var ErrListenerClosed = errors.New("broker: listener closed")

func (s *Server) serveInstance(ctx context.Context, inst *pipeInstance) {
	backoff := 100 * time.Millisecond
	for {
		err := inst.connect()
		if errors.Is(err, errStopped) {
			return
		}
		if errors.Is(err, windows.ERROR_NO_DATA) {
			// The client connected and closed before the server saw it.
			_ = windows.DisconnectNamedPipe(inst.h)
			continue
		}
		if err != nil {
			s.cfg.Logf("broker: connect pipe: %v", err)
			_ = windows.DisconnectNamedPipe(inst.h)
			// Back off if the instance is broken, so it neither spins nor floods the log;
			// stop still interrupts the wait.
			if inst.sleep(backoff) {
				return
			}
			backoff = min(2*backoff, 5*time.Second)
			continue
		}
		backoff = 100 * time.Millisecond
		peer := &pipePeer{h: inst.h}
		if err := s.ServeConn(ctx, inst, peer); err != nil && !errors.Is(err, errStopped) {
			s.cfg.Logf("broker: connection: %v", err)
		}
		inst.linger()
		// A data-location check abandoned after its timeout may still hold the peer. Once
		// the peer is closed it can no longer impersonate or query the pipe, so it can
		// never act as the next client of this instance.
		peer.close()
		_ = windows.DisconnectNamedPipe(inst.h)
	}
}

var (
	errStopped = errors.New("broker: pipe listener stopped")
	errTimeout = fmt.Errorf("broker: pipe I/O: %w", os.ErrDeadlineExceeded)
)

// pipeInstance is one server instance of the pipe and the connection it currently
// serves. Its OVERLAPPED and buffers are heap fields that the kernel writes into; every
// operation waits for its I/O to complete (or be cancelled) before returning, so they are
// never reused or freed while the kernel still owns them.
type pipeInstance struct {
	h    windows.Handle
	stop windows.Handle
	ov   *windows.Overlapped

	readDeadline, writeDeadline time.Time

	rbuf [proto.MaxFrameSize]byte
	wbuf [proto.MaxFrameSize]byte
}

func newPipeInstance(h, stop windows.Handle) (*pipeInstance, error) {
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return nil, err
	}
	return &pipeInstance{h: h, stop: stop, ov: &windows.Overlapped{HEvent: ev}}, nil
}

func (p *pipeInstance) close() {
	if p.h != 0 {
		windows.CloseHandle(p.h)
		p.h = 0
	}
	if p.ov != nil && p.ov.HEvent != 0 {
		windows.CloseHandle(p.ov.HEvent)
		p.ov.HEvent = 0
	}
}

func (p *pipeInstance) sleep(d time.Duration) (stopped bool) {
	s, _ := windows.WaitForSingleObject(p.stop, uint32(d/time.Millisecond))
	return s == windows.WAIT_OBJECT_0
}

// do starts one overlapped operation and waits until it completes, the deadline passes,
// or the listener stops. On timeout or stop it cancels the operation and waits for the
// cancellation to complete.
func (p *pipeInstance) do(deadline time.Time, start func(ov *windows.Overlapped) error) (uint32, error) {
	ov := p.ov
	ev := ov.HEvent
	*ov = windows.Overlapped{HEvent: ev}
	if err := windows.ResetEvent(ev); err != nil {
		return 0, err
	}
	err := start(ov)
	if err != nil && !errors.Is(err, windows.ERROR_IO_PENDING) {
		return 0, err
	}
	var n uint32
	if err == nil {
		err = windows.GetOverlappedResult(p.h, ov, &n, true)
		return n, err
	}
	wait := uint32(windows.INFINITE)
	if !deadline.IsZero() {
		d := time.Until(deadline)
		if d < 0 {
			d = 0
		}
		wait = uint32(d / time.Millisecond)
	}
	which, werr := windows.WaitForMultipleObjects([]windows.Handle{ev, p.stop}, false, wait)
	if werr == nil && which == windows.WAIT_OBJECT_0 {
		err = windows.GetOverlappedResult(p.h, ov, &n, false)
		return n, err
	}
	_ = windows.CancelIoEx(p.h, ov)
	err = windows.GetOverlappedResult(p.h, ov, &n, true)
	if err == nil {
		// Completed before the cancel took effect.
		return n, nil
	}
	if werr == nil && which == windows.WAIT_OBJECT_0+1 {
		return 0, errStopped
	}
	return 0, errTimeout
}

func (p *pipeInstance) connect() error {
	_, err := p.do(time.Time{}, func(ov *windows.Overlapped) error {
		return windows.ConnectNamedPipe(p.h, ov)
	})
	if errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
		return nil
	}
	return err
}

func (p *pipeInstance) SetReadDeadline(t time.Time) error  { p.readDeadline = t; return nil }
func (p *pipeInstance) SetWriteDeadline(t time.Time) error { p.writeDeadline = t; return nil }

func (p *pipeInstance) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	if len(b) > len(p.rbuf) {
		b = b[:len(p.rbuf)]
	}
	n, err := p.do(p.readDeadline, func(ov *windows.Overlapped) error {
		var done uint32
		return windows.ReadFile(p.h, p.rbuf[:len(b)], &done, ov)
	})
	if err != nil {
		if errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED) {
			return 0, io.EOF
		}
		return 0, err
	}
	if n == 0 {
		return 0, io.EOF
	}
	return copy(b, p.rbuf[:n]), nil
}

func (p *pipeInstance) Write(b []byte) (int, error) {
	total := 0
	for len(b) > 0 {
		chunk := copy(p.wbuf[:], b)
		n, err := p.do(p.writeDeadline, func(ov *windows.Overlapped) error {
			var done uint32
			return windows.WriteFile(p.h, p.wbuf[:chunk], &done, ov)
		})
		total += int(n)
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
		b = b[n:]
	}
	return total, nil
}

// linger waits a bounded time for the client to close, discarding anything it sends,
// so that DisconnectNamedPipe does not throw away a reply the client has not read yet.
func (p *pipeInstance) linger() {
	deadline := time.Now().Add(lingerTimeout)
	var b [64]byte
	p.readDeadline = deadline
	for time.Now().Before(deadline) {
		if _, err := p.Read(b[:]); err != nil {
			return
		}
	}
}

// pipePeer identifies the client of a connected pipe instance from its token and
// process. Nothing it returns comes from the client's messages.
type pipePeer struct {
	h windows.Handle
	// sid is the identified user; set by Identify.
	sid string

	// mu guards every use of h. After close, h belongs to the instance's next client.
	mu     sync.Mutex
	closed bool
}

var errPeerClosed = errors.New("broker: connection closed")

func (p *pipePeer) close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
}

// withPipe runs fn with the pipe handle while the connection is still this peer's.
func (p *pipePeer) withPipe(fn func(h windows.Handle) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errPeerClosed
	}
	return fn(p.h)
}

// TokenIsAppContainer (TOKEN_INFORMATION_CLASS) and SECURITY_MANDATORY_MEDIUM_RID.
const (
	tokenIsAppContainer = 29
	mandatoryMediumRID  = 0x2000
)

var (
	modadvapi32                    = windows.NewLazySystemDLL("advapi32.dll")
	procImpersonateNamedPipeClient = modadvapi32.NewProc("ImpersonateNamedPipeClient")
)

func impersonateNamedPipeClient(h windows.Handle) error {
	r, _, err := procImpersonateNamedPipeClient.Call(uintptr(h))
	if r == 0 {
		return err
	}
	return nil
}

// impersonate runs fn on a locked OS thread that impersonates the pipe client. If
// RevertToSelf fails, the goroutine exits without unlocking the thread, so the runtime
// destroys the thread instead of reusing it with the client's token.
//
// Only ImpersonateNamedPipeClient needs the pipe; once it has run, the thread holds this
// client's token whatever happens to the pipe, so fn runs outside the peer's lock.
func impersonate(p *pipePeer, fn func() error) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := p.withPipe(impersonateNamedPipeClient); err != nil {
			runtime.UnlockOSThread()
			done <- fmt.Errorf("broker: impersonate pipe client: %w", err)
			return
		}
		err := fn()
		if rerr := windows.RevertToSelf(); rerr != nil {
			done <- fmt.Errorf("broker: revert impersonation: %w", rerr)
			return // thread stays locked and is destroyed
		}
		runtime.UnlockOSThread()
		done <- err
	}()
	return <-done
}

func (p *pipePeer) Impersonate(fn func() error) error { return impersonate(p, fn) }

func (p *pipePeer) Identify() (Identity, error) {
	var id Identity
	err := impersonate(p, func() error {
		var tok windows.Token
		if err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &tok); err != nil {
			return fmt.Errorf("broker: open client token: %w", err)
		}
		defer tok.Close()
		sid, err := checkClientToken(tok)
		if err != nil {
			return err
		}
		id.SID = sid
		return nil
	})
	if err != nil {
		return Identity{}, err
	}
	p.sid = id.SID
	return id, nil
}

// checkClientToken returns the token's user SID, or an error if the token is anonymous,
// an AppContainer, restricted, or below medium integrity. A sandboxed process of the
// right user must not be able to drive the broker.
func checkClientToken(tok windows.Token) (string, error) {
	tu, err := tok.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("broker: client token user: %w", err)
	}
	if tu.User.Sid.IsWellKnown(windows.WinAnonymousSid) {
		return "", errors.New("broker: anonymous client")
	}
	var appContainer, n uint32
	if err := windows.GetTokenInformation(tok, tokenIsAppContainer, (*byte)(unsafe.Pointer(&appContainer)), 4, &n); err != nil {
		return "", fmt.Errorf("broker: client token: %w", err)
	}
	if appContainer != 0 {
		return "", errors.New("broker: AppContainer client")
	}
	restricted, err := tok.IsRestricted()
	if err != nil {
		return "", fmt.Errorf("broker: client token: %w", err)
	}
	if restricted {
		return "", errors.New("broker: restricted client token")
	}
	var buf [128]byte
	if err := windows.GetTokenInformation(tok, windows.TokenIntegrityLevel, &buf[0], uint32(len(buf)), &n); err != nil {
		return "", fmt.Errorf("broker: client integrity level: %w", err)
	}
	label := (*windows.Tokenmandatorylabel)(unsafe.Pointer(&buf[0]))
	il := label.Label.Sid
	if c := il.SubAuthorityCount(); c == 0 || il.SubAuthority(uint32(c)-1) < mandatoryMediumRID {
		return "", errors.New("broker: client integrity below medium")
	}
	return tu.User.Sid.String(), nil
}

// DuplicateHandle copies handle value v out of the pipe's client process. The client
// process is opened while impersonating the client, so the OS access check is the
// caller's own: a caller can only take handles from processes it could already open with
// PROCESS_DUP_HANDLE. The process's user must be the identified caller, and the pipe's
// client process ID is checked again after opening, which catches the client exiting and
// its ID being reused in between.
func (p *pipePeer) DuplicateHandle(v uint64) (windows.Handle, error) {
	if p.sid == "" {
		return 0, errors.New("broker: caller not identified")
	}
	var pid uint32
	if err := p.withPipe(func(h windows.Handle) error {
		return windows.GetNamedPipeClientProcessId(h, &pid)
	}); err != nil {
		return 0, fmt.Errorf("broker: client process: %w", err)
	}
	var proc windows.Handle
	err := p.Impersonate(func() error {
		var err error
		proc, err = windows.OpenProcess(windows.PROCESS_DUP_HANDLE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
		if err != nil {
			return fmt.Errorf("broker: open client process: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(proc)

	var tok windows.Token
	if err := windows.OpenProcessToken(proc, windows.TOKEN_QUERY, &tok); err != nil {
		return 0, fmt.Errorf("broker: client process token: %w", err)
	}
	tu, err := tok.GetTokenUser()
	tok.Close()
	if err != nil {
		return 0, fmt.Errorf("broker: client process user: %w", err)
	}
	if tu.User.Sid.String() != p.sid {
		return 0, errors.New("broker: client process belongs to another user")
	}
	// The re-check and the duplication run under the peer's lock, so the connection
	// cannot end (and the instance cannot move to another client) in between.
	var dup windows.Handle
	err = p.withPipe(func(h windows.Handle) error {
		var again uint32
		if err := windows.GetNamedPipeClientProcessId(h, &again); err != nil || again != pid {
			return errors.New("broker: client process changed")
		}
		if err := windows.DuplicateHandle(proc, windows.Handle(v), windows.CurrentProcess(), &dup,
			0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
			return fmt.Errorf("broker: duplicate client handle: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return dup, nil
}
