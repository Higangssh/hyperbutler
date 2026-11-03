package broker

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

// pipeClient is the client end of a broker pipe, with only x/sys: the broker package may
// not link go-winio (ADR 0004 rule 10). It reuses pipeInstance's overlapped I/O; Close
// from another goroutine ends an operation in progress at once (through the stop event)
// and closes the handle only after that operation returned, as the kernel owns its
// OVERLAPPED until then.
type pipeClient struct {
	name string
	io   sync.Mutex // held for each Read or Write
	inst *pipeInstance

	closeOnce sync.Once
}

// pipeDialRetry is how long dialPipeClient waits between tries while every instance of
// the pipe is busy.
const pipeDialRetry = 10 * time.Millisecond

// dialPipeClient opens the local pipe name for reading and writing, overlapped, at
// impersonation level (the broker identifies the caller from its token, and takes
// handles from its process). While every instance is busy it tries again until ctx ends.
func dialPipeClient(ctx context.Context, name string) (*pipeClient, error) {
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	var h windows.Handle
	for {
		h, err = windows.CreateFile(name16, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_OVERLAPPED|windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IMPERSONATION, 0)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pipeDialRetry):
		}
	}
	stop, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	inst, err := newPipeInstance(h, stop)
	if err != nil {
		_ = windows.CloseHandle(stop)
		_ = windows.CloseHandle(h)
		return nil, err
	}
	return &pipeClient{name: name, inst: inst}, nil
}

func (c *pipeClient) Read(b []byte) (int, error) {
	c.io.Lock()
	defer c.io.Unlock()
	if c.inst.h == 0 {
		return 0, net.ErrClosed
	}
	n, err := c.inst.Read(b)
	if errors.Is(err, errStopped) {
		err = net.ErrClosed
	}
	return n, err
}

func (c *pipeClient) Write(b []byte) (int, error) {
	c.io.Lock()
	defer c.io.Unlock()
	if c.inst.h == 0 {
		return 0, net.ErrClosed
	}
	n, err := c.inst.Write(b)
	if errors.Is(err, errStopped) {
		err = net.ErrClosed
	}
	return n, err
}

// Close ends any Read or Write in progress and closes the pipe. It is safe to call more
// than once and from any goroutine.
func (c *pipeClient) Close() error {
	c.closeOnce.Do(func() {
		_ = windows.SetEvent(c.inst.stop)
		c.io.Lock()
		defer c.io.Unlock()
		stop := c.inst.stop
		c.inst.close()
		_ = windows.CloseHandle(stop)
	})
	return nil
}

// Fd is the pipe handle, for checkPipeOwner.
func (c *pipeClient) Fd() uintptr { return uintptr(c.inst.h) }

func (c *pipeClient) SetDeadline(t time.Time) error {
	c.inst.readDeadline, c.inst.writeDeadline = t, t
	return nil
}

func (c *pipeClient) SetReadDeadline(t time.Time) error  { c.inst.readDeadline = t; return nil }
func (c *pipeClient) SetWriteDeadline(t time.Time) error { c.inst.writeDeadline = t; return nil }

// pipeAddr is a pipe's name as a net.Addr.
type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }

func (c *pipeClient) LocalAddr() net.Addr  { return pipeAddr(c.name) }
func (c *pipeClient) RemoteAddr() net.Addr { return pipeAddr(c.name) }

var _ net.Conn = (*pipeClient)(nil)
