//go:build windows

package winipc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// PipeConfig configures ListenPipe.
type PipeConfig struct {
	// SecurityDescriptor is the pipe's security descriptor in SDDL. Empty means the
	// default named-pipe DACL (the creator, SYSTEM and Administrators full control,
	// Everyone and anonymous read).
	SecurityDescriptor string
	// MessageMode creates a message-type pipe (read in byte mode on both ends), which
	// is what lets CloseWrite deliver EOF: a zero-byte message.
	MessageMode bool
	// InputBufferSize and OutputBufferSize are the pipe's buffer quotas in bytes. With
	// zero, a write completes only as the peer reads it.
	InputBufferSize, OutputBufferSize uint32
}

// pipeAddr is a pipe's name as a net.Addr.
type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }

// PipeListener accepts connections on a local named pipe.
//
// ListenPipe creates the pipe with FILE_CREATE (it fails if the name exists, so a
// squatter cannot share it) through a placeholder instance with no read or write
// access, which holds the name and the security descriptor for the listener's life and
// which clients cannot connect to. Connections arrive on further instances: one is
// always waiting, so a client that connects between two Accept calls waits for the
// next one instead of getting ERROR_PIPE_BUSY. Every instance rejects remote clients
// (FILE_PIPE_REJECT_REMOTE_CLIENTS).
type PipeListener struct {
	name   string
	ntPath *windows.NTUnicodeString
	cfg    PipeConfig
	holder windows.Handle

	acceptMu sync.Mutex // one Accept at a time

	mu      sync.Mutex
	closed  bool
	pending *file // the instance waiting for the next client, or nil
}

// ListenPipe listens on the named pipe name (\\.\pipe\...). The listener's Accept
// returns *PipeConn values.
func ListenPipe(name string, cfg *PipeConfig) (net.Listener, error) {
	l, err := listenPipe(name, cfg)
	if err != nil {
		return nil, err
	}
	return l, nil
}

func listenPipe(name string, cfg *PipeConfig) (*PipeListener, error) {
	if cfg == nil {
		cfg = &PipeConfig{}
	}
	opErr := func(err error) error {
		return &net.OpError{Op: "listen", Net: "pipe", Addr: pipeAddr(name), Err: err}
	}
	nt, err := ntPipePath(name)
	if err != nil {
		return nil, opErr(err)
	}
	var sd *windows.SECURITY_DESCRIPTOR
	if cfg.SecurityDescriptor != "" {
		sd, err = windows.SecurityDescriptorFromString(cfg.SecurityDescriptor)
		if err != nil {
			return nil, opErr(fmt.Errorf("security descriptor: %w", err))
		}
	} else {
		var dacl *windows.ACL
		if err := windows.RtlDefaultNpAcl(&dacl); err != nil {
			return nil, opErr(fmt.Errorf("default pipe ACL: %w", ntErr(err)))
		}
		defer func() { _, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(dacl))) }()
		sd, err = windows.NewSecurityDescriptor()
		if err != nil {
			return nil, opErr(err)
		}
		if err := sd.SetDACL(dacl, true, false); err != nil {
			return nil, opErr(err)
		}
	}
	l := &PipeListener{name: name, ntPath: nt, cfg: *cfg}
	l.holder, err = l.createInstance(sd, true)
	if err != nil {
		return nil, opErr(err)
	}
	// Have the first connectable instance ready before returning, so a client that
	// dials right after ListenPipe does not find the pipe busy.
	if l.pending, err = l.newInstance(); err != nil {
		_ = windows.CloseHandle(l.holder)
		return nil, opErr(err)
	}
	return l, nil
}

// ntPipePath converts a Win32 pipe path to the NT path NtCreateNamedPipeFile takes.
func ntPipePath(name string) (*windows.NTUnicodeString, error) {
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	var nt windows.NTUnicodeString
	if err := windows.RtlDosPathNameToNtPathName(name16, &nt, nil, nil); err != nil {
		return nil, ntErr(err)
	}
	s := windows.UTF16PtrToString(nt.Buffer)
	_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(nt.Buffer)))
	return windows.NewNTUnicodeString(s)
}

// ntErr converts an NTSTATUS error to the Win32 code callers test with errors.Is.
func ntErr(err error) error {
	var st windows.NTStatus
	if errors.As(err, &st) {
		return st.Errno()
	}
	return err
}

// createInstance creates one server instance of the pipe. The placeholder (first) has
// only SYNCHRONIZE access, which leaves it disconnected: no client can connect to it.
func (l *PipeListener) createInstance(sd *windows.SECURITY_DESCRIPTOR, first bool) (windows.Handle, error) {
	oa := windows.OBJECT_ATTRIBUTES{ObjectName: l.ntPath, Attributes: windows.OBJ_CASE_INSENSITIVE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	typ := uint32(windows.FILE_PIPE_REJECT_REMOTE_CLIENTS)
	if l.cfg.MessageMode {
		typ |= windows.FILE_PIPE_MESSAGE_TYPE
	}
	disposition := uint32(windows.FILE_OPEN)
	access := uint32(windows.GENERIC_READ | windows.GENERIC_WRITE | windows.SYNCHRONIZE)
	if first {
		disposition = windows.FILE_CREATE
		access = windows.SYNCHRONIZE
		oa.SecurityDescriptor = sd
	}
	timeout := int64(-50 * 10000) // the default client wait, 50 ms
	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	err := windows.NtCreateNamedPipeFile(&h, access, &oa, &iosb,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, disposition,
		0, // no FILE_SYNCHRONOUS_IO_*: an overlapped handle
		typ,
		windows.FILE_PIPE_BYTE_STREAM_MODE,
		windows.FILE_PIPE_QUEUE_OPERATION,
		0xffffffff, // unlimited instances
		l.cfg.InputBufferSize, l.cfg.OutputBufferSize, &timeout)
	if err != nil {
		return 0, os.NewSyscallError("NtCreateNamedPipeFile", ntErr(err))
	}
	return h, nil
}

func (l *PipeListener) newInstance() (*file, error) {
	h, err := l.createInstance(nil, false)
	if err != nil {
		return nil, err
	}
	f, err := newFile(h, false)
	if err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	return f, nil
}

// Accept waits for a client and returns its connection, a *PipeConn. After Close it
// returns an error that matches net.ErrClosed.
func (l *PipeListener) Accept() (net.Conn, error) {
	c, err := l.accept()
	if err != nil {
		return nil, &net.OpError{Op: "accept", Net: "pipe", Addr: pipeAddr(l.name), Err: err}
	}
	return c, nil
}

func (l *PipeListener) accept() (*PipeConn, error) {
	l.acceptMu.Lock()
	defer l.acceptMu.Unlock()
	for {
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			return nil, net.ErrClosed
		}
		p := l.pending
		if p == nil {
			var err error
			if p, err = l.newInstance(); err != nil {
				l.mu.Unlock()
				return nil, err
			}
			l.pending = p
		}
		l.mu.Unlock()

		// Close closes p, which ends the wait with net.ErrClosed.
		err := connectPipe(p)

		l.mu.Lock()
		if l.closed {
			// Close took p (and closed it), whatever connectPipe reported.
			l.mu.Unlock()
			return nil, net.ErrClosed
		}
		switch {
		case err == nil:
			// p is the client's now. Have the next instance waiting before
			// returning; if that fails, put p back (the client sees a broken pipe)
			// and report the error, so the caller can retry later.
			next, nerr := l.newInstance()
			if nerr != nil {
				_ = windows.DisconnectNamedPipe(p.h)
				l.mu.Unlock()
				return nil, nerr
			}
			l.pending = next
			l.mu.Unlock()
			return &PipeConn{f: p, name: l.name, server: true, msg: l.cfg.MessageMode}, nil
		case errors.Is(err, windows.ERROR_NO_DATA):
			// The client connected and left before we saw it; serve the next one.
			_ = windows.DisconnectNamedPipe(p.h)
			l.mu.Unlock()
		default:
			_ = windows.DisconnectNamedPipe(p.h)
			l.mu.Unlock()
			return nil, err
		}
	}
}

// connectPipe waits for a client on p (ConnectNamedPipe).
func connectPipe(p *file) error {
	if err := p.begin(); err != nil {
		return err
	}
	defer p.end()
	p.rmu.Lock()
	defer p.rmu.Unlock()
	_, err := p.do(p.rop, nil, nil, func(o *windows.Overlapped) (uint32, error) {
		return 0, windows.ConnectNamedPipe(p.h, o)
	})
	if errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
		// A client connected between the instance's creation and now.
		return nil
	}
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return os.NewSyscallError("ConnectNamedPipe", err)
	}
	return err
}

// Close stops listening. Connections already accepted stay open; once they are closed
// too, the pipe name is free.
func (l *PipeListener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	p := l.pending
	l.pending = nil
	l.mu.Unlock()
	if p != nil {
		_ = p.close()
	}
	return os.NewSyscallError("CloseHandle", windows.CloseHandle(l.holder))
}

// Addr returns the pipe's name.
func (l *PipeListener) Addr() net.Addr { return pipeAddr(l.name) }

// ImpLevel is the impersonation level a pipe client grants the server
// (SECURITY_QUALITY_OF_SERVICE).
type ImpLevel uint32

const (
	// ImpAnonymous lets the server neither identify nor impersonate the client.
	ImpAnonymous ImpLevel = windows.SECURITY_ANONYMOUS
	// ImpIdentification lets the server identify the client but not act as it.
	ImpIdentification ImpLevel = windows.SECURITY_IDENTIFICATION
	// ImpImpersonation lets the server act as the client on this machine.
	ImpImpersonation ImpLevel = windows.SECURITY_IMPERSONATION
)

// pipeBusyRetry is how long a dial waits before trying again while every instance of
// the pipe is busy.
const pipeBusyRetry = 10 * time.Millisecond

// DialPipe connects to the named pipe name anonymously (the server can neither
// identify nor impersonate the caller). While every instance is busy it retries until
// ctx ends; any other failure, such as a pipe that does not exist, returns at once.
func DialPipe(ctx context.Context, name string) (net.Conn, error) {
	return DialPipeImp(ctx, name, ImpAnonymous)
}

// DialPipeImp is DialPipe with the given impersonation level.
func DialPipeImp(ctx context.Context, name string, level ImpLevel) (net.Conn, error) {
	c, err := dialPipe(ctx, name, level)
	if err != nil {
		return nil, &net.OpError{Op: "dial", Net: "pipe", Addr: pipeAddr(name), Err: err}
	}
	return c, nil
}

func dialPipe(ctx context.Context, name string, level ImpLevel) (*PipeConn, error) {
	switch level {
	case ImpAnonymous, ImpIdentification, ImpImpersonation:
	default:
		return nil, fmt.Errorf("impersonation level %#x", uint32(level))
	}
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	var h windows.Handle
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		h, err = windows.CreateFile(name16, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_OVERLAPPED|windows.SECURITY_SQOS_PRESENT|uint32(level), 0)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return nil, os.NewSyscallError("CreateFile", err)
		}
		t := time.NewTimer(pipeBusyRetry)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
	var flags uint32
	if err := windows.GetNamedPipeInfo(h, &flags, nil, nil, nil); err != nil {
		_ = windows.CloseHandle(h)
		return nil, os.NewSyscallError("GetNamedPipeInfo", err)
	}
	f, err := newFile(h, false)
	if err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	return &PipeConn{f: f, name: name, msg: flags&windows.PIPE_TYPE_MESSAGE != 0}, nil
}

// PipeConn is one end of a named-pipe connection: a server instance from
// PipeListener.Accept or a client from DialPipe. Both read in byte mode.
//
// A read of zero bytes means the peer closed its write side (a zero-byte message) and
// is reported as io.EOF, as is a disconnected pipe; after that every Read returns
// io.EOF. Data the peer has not read survives Close.
type PipeConn struct {
	f      *file
	name   string
	server bool
	msg    bool // a message-type pipe: CloseWrite is supported

	readEOF     bool // under f.rmu
	writeClosed bool // under f.wmu
}

func (c *PipeConn) opErr(op string, err error) error {
	return &net.OpError{Op: op, Net: "pipe", Source: c.LocalAddr(), Addr: c.RemoteAddr(), Err: err}
}

// isPipeGone reports whether err means the other end has disconnected.
func isPipeGone(err error) bool {
	return errors.Is(err, windows.ERROR_BROKEN_PIPE) ||
		errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED) ||
		errors.Is(err, windows.ERROR_NO_DATA)
}

func (c *PipeConn) Read(b []byte) (int, error) {
	f := c.f
	f.rmu.Lock()
	defer f.rmu.Unlock()
	if err := f.begin(); err != nil {
		return 0, c.opErr("read", err)
	}
	defer f.end()
	if c.readEOF {
		return 0, io.EOF
	}
	if len(b) == 0 {
		return 0, nil
	}
	op := f.rop
	op.buf = b
	n, err := f.do(op, &f.rd, nil, func(o *windows.Overlapped) (uint32, error) {
		var n uint32
		err := windows.ReadFile(f.h, b, &n, o)
		return n, err
	})
	op.buf = nil
	switch {
	case err == nil && n == 0, isPipeGone(err):
		c.readEOF = true
		return 0, io.EOF
	case errors.Is(err, windows.ERROR_MORE_DATA):
		// Only in message read mode, which this package never sets: the data is valid.
		return int(n), nil
	case err != nil:
		return int(n), c.opErr("read", wrapSyscall("ReadFile", err))
	}
	return int(n), nil
}

func (c *PipeConn) Write(b []byte) (int, error) {
	f := c.f
	f.wmu.Lock()
	defer f.wmu.Unlock()
	if err := f.begin(); err != nil {
		return 0, c.opErr("write", err)
	}
	defer f.end()
	if c.writeClosed {
		return 0, c.opErr("write", io.ErrClosedPipe)
	}
	// A zero-byte write would be read as EOF on a message pipe; it is a no-op.
	total := 0
	for len(b) > 0 {
		op := f.wop
		op.buf = b
		n, err := f.do(op, &f.wd, nil, func(o *windows.Overlapped) (uint32, error) {
			var n uint32
			err := windows.WriteFile(f.h, b, &n, o)
			return n, err
		})
		op.buf = nil
		total += int(n)
		if err != nil {
			return total, c.opErr("write", wrapSyscall("WriteFile", err))
		}
		if n == 0 {
			return total, c.opErr("write", io.ErrShortWrite)
		}
		b = b[n:]
	}
	return total, nil
}

// wrapSyscall names the failing call, except for the package's own sentinel errors.
func wrapSyscall(call string, err error) error {
	var errno windows.Errno
	if errors.As(err, &errno) {
		return os.NewSyscallError(call, errno)
	}
	return err
}

// errNoHalfClose is CloseWrite's error on a byte-type pipe, where a zero-byte write
// carries nothing the peer could see.
var errNoHalfClose = errors.New("winipc: CloseWrite needs a message-type pipe")

// CloseWrite waits until the peer has read everything written so far (the zero-byte
// message would otherwise be swallowed by the read that drains that data), then sends
// the zero-byte message the peer reads as EOF. Reads still work. Close interrupts the
// wait, and CloseWrite then returns an error matching net.ErrClosed. It waits for any
// Write in progress.
func (c *PipeConn) CloseWrite() error {
	f := c.f
	f.wmu.Lock()
	defer f.wmu.Unlock()
	if !c.msg {
		return c.opErr("close", errNoHalfClose)
	}
	if err := f.begin(); err != nil {
		return c.opErr("close", err)
	}
	if c.writeClosed {
		f.end()
		return c.opErr("close", io.ErrClosedPipe)
	}
	if err := f.flush(); err != nil {
		f.end()
		return c.opErr("close", wrapSyscall("FlushFileBuffers", err))
	}
	// The zero-byte write may stay pending until the peer reads it; it must not hold up
	// CloseWrite (or the write lock) for that long, so it completes in the background.
	// It uses its own ioOp, and close cancels it.
	op := newIOOp()
	var n uint32
	err := windows.WriteFile(f.h, nil, &n, &op.o)
	if err != windows.ERROR_IO_PENDING { //nolint:errorlint // bare Errno from x/sys
		f.end()
		if err != nil {
			return c.opErr("close", wrapSyscall("WriteFile", err))
		}
		c.writeClosed = true
		return nil
	}
	c.writeClosed = true
	go func() {
		defer f.end()
		select {
		case <-op.ch:
			return
		case <-f.closing:
		}
		_ = windows.CancelIoEx(f.h, &op.o)
		<-op.ch
	}()
	return nil
}

// PeerClosed reports, without reading, whether the other end has disconnected. A peer
// that only closed its write side is still connected. It is true after Close.
func (c *PipeConn) PeerClosed() bool {
	if err := c.f.begin(); err != nil {
		return true
	}
	defer c.f.end()
	var avail uint32
	r, _, err := procPeekNamedPipe.Call(uintptr(c.f.h), 0, 0, 0, uintptr(unsafe.Pointer(&avail)), 0)
	if r != 0 {
		return false
	}
	return isPipeGone(err) || errors.Is(err, windows.ERROR_INVALID_HANDLE)
}

var procPeekNamedPipe = modkernel32.NewProc("PeekNamedPipe")

// Close cancels any Read, Write or CloseWrite in progress (they return net.ErrClosed)
// and closes the handle. Data already written stays readable by the peer.
func (c *PipeConn) Close() error {
	if err := c.f.close(); err != nil {
		return c.opErr("close", err)
	}
	return nil
}

// Fd returns the pipe handle. It is valid until Close.
func (c *PipeConn) Fd() uintptr { return uintptr(c.f.h) }

// IsServer reports whether c is a server instance.
func (c *PipeConn) IsServer() bool { return c.server }

func (c *PipeConn) LocalAddr() net.Addr  { return pipeAddr(c.name) }
func (c *PipeConn) RemoteAddr() net.Addr { return pipeAddr(c.name) }

func (c *PipeConn) SetDeadline(t time.Time) error {
	c.f.rd.set(t)
	c.f.wd.set(t)
	return nil
}

func (c *PipeConn) SetReadDeadline(t time.Time) error  { c.f.rd.set(t); return nil }
func (c *PipeConn) SetWriteDeadline(t time.Time) error { c.f.wd.set(t); return nil }

var (
	_ net.Conn     = (*PipeConn)(nil)
	_ net.Listener = (*PipeListener)(nil)
)
