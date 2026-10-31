//go:build windows

package transport

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// pipeBufferSize matches what dockerd uses for its own named pipe.
const pipeBufferSize = 64 << 10

// CurrentUserSDDL returns the security descriptor applied to the listening pipe: owned by
// the current user, with a protected DACL that grants GENERIC_ALL to that user and to no
// one else. SYSTEM, Administrators, and other users get no access; remote clients are
// already rejected by the pipe type (PIPE_REJECT_REMOTE_CLIENTS, set by go-winio).
func CurrentUserSDDL() (string, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("transport: query token user: %w", err)
	}
	sid := tu.User.Sid.String()
	if sid == "" {
		return "", errors.New("transport: current user has no SID")
	}
	// O: pins the owner to the user even when the process is elevated, where the default
	// owner would be BUILTIN\Administrators (which would grant every admin WRITE_DAC).
	// D:P makes the DACL protected so no inheritable ACE can widen it.
	return "O:" + sid + "D:P(A;;GA;;;" + sid + ")", nil
}

// ListenPipe listens on the local named pipe name (see ValidatePipeName) with a security
// descriptor from CurrentUserSDDL. The pipe is created in message mode so that clients
// using go-winio (the Docker CLI and SDKs) get a connection that supports CloseWrite.
//
// Creation fails if a pipe with the same name already exists, including one created by
// another process to squat on the name.
func ListenPipe(name string) (net.Listener, error) {
	if err := ValidatePipeName(name); err != nil {
		return nil, err
	}
	sddl, err := CurrentUserSDDL()
	if err != nil {
		return nil, err
	}
	ln, err := winio.ListenPipe(name, &winio.PipeConfig{
		SecurityDescriptor: sddl,
		MessageMode:        true,
		InputBufferSize:    pipeBufferSize,
		OutputBufferSize:   pipeBufferSize,
	})
	if err != nil {
		return nil, fmt.Errorf("transport: listen on %s: %w", name, err)
	}
	return &pipeListener{Listener: ln}, nil
}

type pipeListener struct {
	net.Listener
}

func (l *pipeListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	fd, ok := c.(interface{ Fd() uintptr })
	if !ok {
		_ = c.Close()
		return nil, errors.New("transport: accepted pipe has no handle")
	}
	return &pipeConn{Conn: c, h: windows.Handle(fd.Fd())}, nil
}

// pipeConn is the server end of one named-pipe connection.
//
// It replaces go-winio's CloseWrite, which calls FlushFileBuffers on go-winio's handle
// field. That flush blocks until the client has read everything, Close does not reliably
// cancel it (a client that stops reading would hang shutdown), and it reads the handle
// field while a concurrent Close clears it (a data race).
//
// The flush itself is required: the client reads in byte mode, and a zero-byte message
// queued behind unread data is consumed silently by the read that drains that data. So
// CloseWrite flushes on a duplicate of the handle, which Close cancels, and then queues
// the zero-byte message that go-winio clients read as io.EOF.
//
// Every use of the raw handle h happens under mu with closed checked first, so the
// handle value is never used after Close has released it to the OS.
type pipeConn struct {
	net.Conn
	h windows.Handle

	writeClosed atomic.Bool

	mu     sync.Mutex
	closed bool
	flushH windows.Handle      // duplicate handle of an in-progress flush, or 0
	eofEv  windows.Handle      // event of a pending zero-byte write, or 0
	eofOv  *windows.Overlapped // kept alive until that write completes
}

func (c *pipeConn) Write(b []byte) (int, error) {
	if c.writeClosed.Load() {
		return 0, io.ErrClosedPipe
	}
	if len(b) == 0 {
		// A zero-byte write would be read by the client as EOF.
		return 0, nil
	}
	return c.Conn.Write(b)
}

// CloseWrite waits until the client has read everything written so far, then sends the
// zero-byte message that go-winio clients read as EOF. Close interrupts the wait, and
// CloseWrite then returns net.ErrClosed. It must not be called concurrently with Write
// or with itself.
func (c *pipeConn) CloseWrite() error {
	if c.writeClosed.Load() {
		return io.ErrClosedPipe
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return net.ErrClosed
	}
	p := windows.CurrentProcess()
	var dup windows.Handle
	if err := windows.DuplicateHandle(p, c.h, p, &dup, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("transport: duplicate pipe handle: %w", err)
	}
	c.flushH = dup
	c.mu.Unlock()

	ferr := windows.FlushFileBuffers(dup)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushH = 0
	_ = windows.CloseHandle(dup)
	if c.closed {
		return net.ErrClosed
	}
	if ferr != nil {
		return fmt.Errorf("transport: pipe flush: %w", ferr)
	}

	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return fmt.Errorf("transport: CreateEvent: %w", err)
	}
	ov := &windows.Overlapped{
		// The low bit stops the completion from being queued to go-winio's I/O
		// completion port, which would otherwise treat our OVERLAPPED as its own.
		HEvent: ev | 1,
	}
	// done must be non-nil: x/sys dereferences it in race-enabled builds.
	var done uint32
	err = windows.WriteFile(c.h, nil, &done, ov)
	switch {
	case err == nil:
		_ = windows.CloseHandle(ev)
	case errors.Is(err, windows.ERROR_IO_PENDING):
		c.eofEv, c.eofOv = ev, ov
	default:
		_ = windows.CloseHandle(ev)
		return fmt.Errorf("transport: pipe half-close: %w", err)
	}
	c.writeClosed.Store(true)
	return nil
}

// closeWriteWithoutLock marks pipeConn's CloseWrite as safe to run concurrently with
// Close (Close interrupts it), so halfConn must not hold its lock around it.
func (c *pipeConn) closeWriteWithoutLock() {}

// peerClosed reports whether the client has closed its end of the pipe. It does not
// read, so it is safe after a half-close.
func (c *pipeConn) peerClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return true
	}
	err := peekNamedPipe(c.h)
	return errors.Is(err, windows.ERROR_BROKEN_PIPE) ||
		errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED) ||
		errors.Is(err, windows.ERROR_NO_DATA) ||
		errors.Is(err, windows.ERROR_INVALID_HANDLE)
}

// maxCancelWait bounds how long Close keeps canceling an in-progress flush.
const maxCancelWait = 5 * time.Second

func (c *pipeConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	ev, ov := c.eofEv, c.eofOv
	c.eofEv, c.eofOv = 0, nil
	c.mu.Unlock()

	// go-winio's Close cancels all I/O on the file (CancelIoEx with no OVERLAPPED; this
	// also covers our zero-byte write and a flush on the duplicate handle, which shares
	// the file object) and then closes its handle. Unread data stays readable.
	err := c.Conn.Close()

	// A CloseWrite that duplicated the handle just before Close may enter
	// FlushFileBuffers after that single cancel. Keep canceling until it has returned.
	deadline := time.Now().Add(maxCancelWait)
	for {
		c.mu.Lock()
		h := c.flushH
		if h != 0 {
			_ = windows.CancelIoEx(h, nil)
		}
		c.mu.Unlock()
		if h == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}

	if ev != 0 {
		// The canceled or completed write signals ev. Wait a bounded time; if the OS
		// never signals, keep the OVERLAPPED reachable forever rather than free memory
		// the kernel may still write to.
		if s, _ := windows.WaitForSingleObject(ev, uint32(maxCancelWait/time.Millisecond)); s == windows.WAIT_OBJECT_0 {
			_ = windows.CloseHandle(ev)
		} else {
			leaked.mu.Lock()
			leaked.ovs = append(leaked.ovs, ov)
			leaked.mu.Unlock()
		}
	}
	return err
}

// leaked keeps OVERLAPPED structures alive whose write never completed, so the kernel
// never writes into freed memory. Tests assert it stays empty.
var leaked struct {
	mu  sync.Mutex
	ovs []*windows.Overlapped
}

var (
	modkernel32       = windows.NewLazySystemDLL("kernel32.dll")
	procPeekNamedPipe = modkernel32.NewProc("PeekNamedPipe")
)

func peekNamedPipe(h windows.Handle) error {
	var avail uint32
	r, _, err := procPeekNamedPipe.Call(uintptr(h), 0, 0, 0, uintptr(unsafe.Pointer(&avail)), 0)
	if r == 0 {
		return err
	}
	return nil
}

// Compile-time interface checks.
var (
	_ closeWriter         = (*pipeConn)(nil)
	_ unlockedCloseWriter = (*pipeConn)(nil)
	_ peerProber          = (*pipeConn)(nil)
)
