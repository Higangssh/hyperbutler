//go:build windows

package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/broker/proto"
	"github.com/Higangssh/hyperbutler/internal/transport"
)

// Client is one connection to the broker. Its requests are serialized; it is safe for
// concurrent use. The connection that starts the VM is the VM's lease: closing it
// terminates the VM (Implementation decision 6).
type Client struct {
	mu   sync.Mutex
	c    net.Conn
	buf  [proto.MaxFrameSize]byte
	dead error
}

// ReplyError is an error reply from the broker.
type ReplyError struct {
	Code    proto.Code
	Message string
}

func (e *ReplyError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("broker: error %d", e.Code)
	}
	return fmt.Sprintf("broker: %s (error %d)", e.Message, e.Code)
}

// IsCode reports whether err is a broker error reply with code.
func IsCode(err error, code proto.Code) bool {
	var re *ReplyError
	return errors.As(err, &re) && re.Code == code
}

// clientIOTimeout bounds one request; a start waits longer (the broker's own
// RequestTimeout is 2 minutes).
const (
	clientIOTimeout    = 15 * time.Second
	clientStartTimeout = 3 * time.Minute
)

// Dial connects to the broker's pipe. It connects at impersonation level, which the
// broker requires to identify the caller and take handles from it, and so it first
// checks who created the pipe: a squatter could otherwise impersonate this user. The
// pipe's owner must be SYSTEM, a service account (S-1-5-80-...), or this user (a
// broker run in the foreground by the same user, which can impersonate no one else).
func Dial(ctx context.Context, pipe string) (*Client, error) {
	if err := transport.ValidatePipeName(pipe); err != nil {
		return nil, err
	}
	c, err := winio.DialPipeAccessImpLevel(ctx, pipe, windows.GENERIC_READ|windows.GENERIC_WRITE, winio.PipeImpLevelImpersonation)
	if err != nil {
		return nil, fmt.Errorf("broker: connect to %s: %w", pipe, err)
	}
	if err := checkPipeOwner(c); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("broker: %s: %w", pipe, err)
	}
	return &Client{c: c}, nil
}

// checkPipeOwner checks the owner of the pipe c is connected to. The owner of a new
// object can only be its creator's own user or group SID (without SeRestorePrivilege),
// so it says who made the pipe.
func checkPipeOwner(c net.Conn) error {
	fd, ok := c.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("cannot read the pipe's owner")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(fd.Fd()), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read the pipe's owner: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return fmt.Errorf("read the pipe's owner: %w", err)
	}
	self, err := processSID()
	if err != nil {
		return err
	}
	if !trustedPipeOwner(owner.String(), self) {
		return fmt.Errorf("the pipe belongs to %s, not to the broker", owner)
	}
	return nil
}

// trustedPipeOwner is the owner rule of checkPipeOwner.
func trustedPipeOwner(owner, self string) bool {
	const serviceSIDPrefix = "S-1-5-80-"
	return owner == "S-1-5-18" || owner == self || len(owner) > len(serviceSIDPrefix) && owner[:len(serviceSIDPrefix)] == serviceSIDPrefix
}

// Close closes the connection, which ends its lease. It is safe to call more than once.
// A request in progress fails at once.
func (c *Client) Close() error {
	err := c.c.Close()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead == nil {
		c.dead = net.ErrClosed
	}
	return err
}

// Do sends req and returns the success reply, or a *ReplyError. A transport error ends
// the connection: every later call fails with it.
func (c *Client) Do(req proto.Request) (proto.Reply, error) {
	b, err := proto.EncodeRequest(req)
	if err != nil {
		return proto.Reply{}, err
	}
	timeout := clientIOTimeout
	if req.Op == proto.OpStart {
		timeout = clientStartTimeout
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead != nil {
		return proto.Reply{}, c.dead
	}
	fail := func(err error) (proto.Reply, error) {
		c.dead = fmt.Errorf("broker: connection lost: %w", err)
		_ = c.c.Close()
		return proto.Reply{}, c.dead
	}
	_ = c.c.SetDeadline(time.Now().Add(timeout))
	if _, err := c.c.Write(b); err != nil {
		return fail(err)
	}
	f, err := proto.ReadFrame(c.c, &c.buf)
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return fail(err)
	}
	r, err := proto.DecodeReply(f)
	if err != nil {
		return fail(err)
	}
	if r.IsError() {
		return proto.Reply{}, &ReplyError{Code: r.Code, Message: r.Message}
	}
	if r.Type != proto.ReplyFlag|uint8(req.Op) {
		return fail(fmt.Errorf("reply type 0x%02x to %s", r.Type, req.Op))
	}
	return r, nil
}

// Version returns the broker's protocol and broker versions.
func (c *Client) Version() (uint8, string, error) {
	r, err := c.Do(proto.Request{Op: proto.OpVersion})
	return r.ProtoVersion, r.BrokerVersion, err
}

// SetDataLocation hands the broker the directory handle h of this process (ADR 0004
// rule 2). h must stay open until the call returns.
func (c *Client) SetDataLocation(h windows.Handle) error {
	_, err := c.Do(proto.Request{Op: proto.OpSetDataLocation, Handle: uint64(h)})
	return err
}

// Start starts the caller's VM with req's fields (req.Op is set here); the connection
// becomes its lease.
func (c *Client) Start(req proto.Request) (proto.Reply, error) {
	req.Op = proto.OpStart
	return c.Do(req)
}

// Status returns the caller's VM's status.
func (c *Client) Status() (proto.Reply, error) { return c.Do(proto.Request{Op: proto.OpStatus}) }

// Pause pauses the caller's VM, or resumes it.
func (c *Client) Pause(resume bool) (proto.Reply, error) {
	return c.Do(proto.Request{Op: proto.OpPause, Resume: resume})
}

// Stop stops the caller's VM: at once with force, else through HCS's shutdown request.
func (c *Client) Stop(force bool) error {
	_, err := c.Do(proto.Request{Op: proto.OpStop, Force: force})
	return err
}
