//go:build windows

package broker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/broker/proto"
	"github.com/Higangssh/hyperbutler/internal/hvaddr"
)

// Client is one connection to the broker. Its requests are serialized; it is safe for
// concurrent use. The connection that starts the VM is the VM's lease: closing it
// terminates the VM (Implementation decision 6), and so does any error that leaves the
// stream out of step; a reply that is only late (ErrNoReply) ends nothing.
type Client struct {
	mu sync.Mutex
	c  net.Conn
	// in holds the reply frame being read, have bytes of it so far: they are kept when
	// the wait for the rest times out.
	in   [proto.MaxFrameSize]byte
	have int
	buf  [proto.MaxFrameSize]byte
	// owed is the request whose reply a timeout left unread; the next request reads
	// that reply first, so the stream stays in step.
	owed  proto.Op
	owing bool
	dead  error
	// awake is the host's awake time: awakeTime, or a fake in tests.
	awake func() time.Duration
}

// ErrLost is what every error that ended a Client's connection wraps: the broker
// closed it or exited, Close was called, a write failed, or a reply broke the protocol.
// The connection's lease, and its VM, end with it.
var ErrLost = errors.New("broker: connection lost")

// ErrNoReply is what Do returns when the broker sent no reply within the request's
// timeout of the host's awake time. The connection stays usable and keeps its lease:
// the next request reads the late reply first. It wraps os.ErrDeadlineExceeded.
var ErrNoReply = fmt.Errorf("broker: no reply in time: %w", os.ErrDeadlineExceeded)

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

// ErrUntrustedPipe is what Dial's errors wrap when a pipe it reached is not the
// broker's: its owner is not SYSTEM, a service or this user, the shared endpoint named
// a pipe not of this user's form, or the two pipes have different owners. Another
// program then holds the name (rule 5); the client sends it nothing.
var ErrUntrustedPipe = errors.New("not the broker's pipe")

// clientIOTimeout bounds one request's wait for its reply, in the host's awake time; a
// start waits longer (the broker's own RequestTimeout is 2 minutes).
const (
	clientIOTimeout    = 15 * time.Second
	clientStartTimeout = 3 * time.Minute
)

// minRewait is the least awake time left of a request's timeout for which a wait that a
// host's sleep cut short is made again; with less left the request ends with ErrNoReply.
const minRewait = time.Second

var procQueryUnbiasedInterruptTime = windows.NewLazySystemDLL("kernel32.dll").NewProc("QueryUnbiasedInterruptTime")

// processStart is where awakeTime's fallback counts from.
var processStart = time.Now()

// unbiasedWorks is whether QueryUnbiasedInterruptTime answered its first call. It is
// decided once, so the clock awakeTime reads never changes under a wait.
var unbiasedWorks = sync.OnceValue(func() bool {
	var t uint64
	r, _, _ := procQueryUnbiasedInterruptTime.Call(uintptr(unsafe.Pointer(&t)))
	return r != 0
})

// awakeTime is how long the host has been awake: Windows's unbiased interrupt time,
// which stops while the host sleeps or hibernates. Go's monotonic clock runs on (it
// reads the interrupt time), which is why a deadline set before a sleep has passed when
// the host wakes. If the call does not work the monotonic clock stands in, and a sleep
// then counts as awake time.
func awakeTime() time.Duration {
	if !unbiasedWorks() {
		return time.Since(processStart)
	}
	var t uint64 // 100 ns units
	_, _, _ = procQueryUnbiasedInterruptTime.Call(uintptr(unsafe.Pointer(&t)))
	return time.Duration(t) * 100 // 2^63 ns is 292 years of uptime
}

// Dial connects to this user's own endpoint of the broker whose shared endpoint is
// shared (ADR 0004 rule 5): it asks the shared endpoint to open it, checks that the
// name it got back has the form of this user's endpoint (ValidUserPipeName: its SID's
// hash and a random suffix), connects there, and checks that the same account owns both
// pipes. Every VM verb is served on the user's own endpoint only. Nothing is cached: the
// name changes when the broker restarts or retires the endpoint.
//
// Both connections are made at impersonation level, which the broker requires to
// identify the caller and take handles from it, and so each first checks who created
// the pipe: a squatter could otherwise impersonate this user. The pipe's owner must be
// SYSTEM, a service account (S-1-5-80-...), or this user (a broker run in the foreground
// by the same user, which can impersonate no one else).
func Dial(ctx context.Context, shared string) (*Client, error) {
	if err := hvaddr.ValidatePipeName(shared); err != nil {
		return nil, err
	}
	self, err := processSID()
	if err != nil {
		return nil, err
	}
	r, brokerSID, err := openEndpoint(ctx, shared)
	if err != nil {
		return nil, err
	}
	if !ValidUserPipeName(shared, self, r.PipeName) || hvaddr.ValidatePipeName(r.PipeName) != nil {
		// The client connects only to a name of this user's own form.
		return nil, fmt.Errorf("broker: %s named endpoint %q, which is not this user's (%w)", shared, r.PipeName, ErrUntrustedPipe)
	}
	c, owner, err := dialPipe(ctx, r.PipeName)
	if err != nil {
		return nil, err
	}
	if owner != brokerSID {
		_ = c.Close()
		return nil, fmt.Errorf("broker: %s belongs to %s, the shared endpoint to %s (%w)", r.PipeName, owner, brokerSID, ErrUntrustedPipe)
	}
	return c, nil
}

// openEndpoint asks the shared endpoint for this user's own endpoint. A busy broker or
// a connection the broker dropped (it gives instances back at once to clients over their
// cap, or silent for too long) is tried again until ctx ends; any other error is final.
// The broker can drop an instance before the owner check reads it, which then fails
// with ERROR_PIPE_NOT_CONNECTED: that is a dropped connection too.
func openEndpoint(ctx context.Context, shared string) (proto.Reply, string, error) {
	for wait := 50 * time.Millisecond; ; wait = min(2*wait, time.Second) {
		sc, owner, err := dialPipe(ctx, shared)
		if err != nil {
			if !errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED) {
				return proto.Reply{}, "", err
			}
			select {
			case <-ctx.Done():
				return proto.Reply{}, "", fmt.Errorf("%w (gave up: %w)", err, ctx.Err())
			case <-time.After(wait):
			}
			continue
		}
		r, err := sc.Do(proto.Request{Op: proto.OpOpenEndpoint})
		_ = sc.Close()
		if err == nil {
			return r, owner, nil
		}
		var re *ReplyError
		if errors.As(err, &re) && re.Code != proto.CodeBusy {
			return proto.Reply{}, "", fmt.Errorf("broker: open this user's endpoint: %w", err)
		}
		select {
		case <-ctx.Done():
			return proto.Reply{}, "", fmt.Errorf("broker: open this user's endpoint: %w", err)
		case <-time.After(wait):
		}
	}
}

// dialPipe connects to one broker pipe after checking its owner, which it returns.
func dialPipe(ctx context.Context, pipe string) (*Client, string, error) {
	c, err := dialPipeClient(ctx, pipe)
	if err != nil {
		return nil, "", fmt.Errorf("broker: connect to %s: %w", pipe, err)
	}
	owner, err := checkPipeOwner(c)
	if err != nil {
		_ = c.Close()
		return nil, "", fmt.Errorf("broker: %s: %w", pipe, err)
	}
	return &Client{c: c}, owner, nil
}

// checkPipeOwner checks the owner of the pipe c is connected to and returns it. The
// owner of a new object can only be its creator's own user or group SID (without
// SeRestorePrivilege), so it says who made the pipe.
func checkPipeOwner(c net.Conn) (string, error) {
	fd, ok := c.(interface{ Fd() uintptr })
	if !ok {
		return "", errors.New("cannot read the pipe's owner")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(fd.Fd()), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return "", fmt.Errorf("read the pipe's owner: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return "", fmt.Errorf("read the pipe's owner: %w", err)
	}
	self, err := processSID()
	if err != nil {
		return "", err
	}
	if !trustedPipeOwner(owner.String(), self) {
		return "", fmt.Errorf("the pipe belongs to %s, not to the broker (%w)", owner, ErrUntrustedPipe)
	}
	return owner.String(), nil
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
		c.dead = fmt.Errorf("%w: %w", ErrLost, net.ErrClosed)
	}
	return err
}

// Do sends req and returns the success reply, or a *ReplyError. An error that leaves the
// stream out of step (the pipe closed or broken, a write that failed or timed out, a
// malformed or unexpected reply) ends the connection: it wraps ErrLost, and every later
// call fails with it. A reply that does not come within the request's timeout is
// ErrNoReply and ends nothing. The timeout counts the host's awake time, so a host's
// sleep of any length while a request waits neither ends the connection nor uses up
// the request's time.
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
	if c.owing {
		// The reply an earlier request gave up on; this request's time covers it.
		if _, err := c.await(c.owed, timeout); err != nil {
			return proto.Reply{}, err
		}
	}
	// A write that timed out may have sent part of the request, so it ends the
	// connection like any other failed write. The request is one frame, which the
	// pipe's buffer takes at once (the broker reads each request before it answers it,
	// and this client sends one at a time): a write waits, and so can meet a host's
	// sleep, only when the broker has stopped reading.
	_ = c.c.SetWriteDeadline(time.Now().Add(timeout))
	if _, err := c.c.Write(b); err != nil {
		return c.fail(err)
	}
	c.owed, c.owing = req.Op, true
	r, err := c.await(req.Op, timeout)
	if err != nil {
		return proto.Reply{}, err
	}
	if r.IsError() {
		return proto.Reply{}, &ReplyError{Code: r.Code, Message: r.Message}
	}
	return r, nil
}

// fail ends the connection with err.
func (c *Client) fail(err error) (proto.Reply, error) {
	c.dead = fmt.Errorf("%w: %w", ErrLost, err)
	_ = c.c.Close()
	return proto.Reply{}, c.dead
}

// await reads the reply to op, the request owed. A read deadline is on a clock that
// runs while the host sleeps, so one that passes when less than timeout of awake time
// has gone by since the wait began was cut short by a sleep: the wait is made again,
// for the awake time left, on the same reply. When the awake time is used up the reply
// stays owed and await returns ErrNoReply.
func (c *Client) await(op proto.Op, timeout time.Duration) (proto.Reply, error) {
	awake := c.awake
	if awake == nil {
		awake = awakeTime
	}
	began := awake()
	left := timeout
	var f []byte
	for {
		_ = c.c.SetReadDeadline(time.Now().Add(left))
		var err error
		f, err = c.readFrame()
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return c.fail(err)
		}
		if left = timeout - (awake() - began); left < minRewait {
			return proto.Reply{}, fmt.Errorf("%w to %s", ErrNoReply, op)
		}
	}
	c.owing = false
	r, err := proto.DecodeReply(f)
	if err != nil {
		return c.fail(err)
	}
	if !r.IsError() && r.Type != proto.ReplyFlag|uint8(op) {
		return c.fail(fmt.Errorf("reply type 0x%02x to %s", r.Type, op))
	}
	return r, nil
}

// readFrame reads one frame, going on with one a timeout cut: what it read so far is
// kept in c.in, so the stream stays in step. It reads no further than the frame's end.
func (c *Client) readFrame() ([]byte, error) {
	for {
		need := 2
		if c.have >= 2 {
			need += int(binary.BigEndian.Uint16(c.in[:2]))
			if need > len(c.in) {
				return nil, proto.ErrFrameTooLarge
			}
			if c.have == need {
				break
			}
		}
		n, err := c.c.Read(c.in[c.have:need])
		c.have += n
		if err != nil {
			return nil, err
		}
	}
	n := c.have
	c.have = 0
	// ReadFrame checks the frame's length as the broker's own reads do.
	return proto.ReadFrame(bytes.NewReader(c.in[:n]), &c.buf)
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

// Status returns the caller's Docker VM's status.
func (c *Client) Status() (proto.Reply, error) { return c.StatusSlot(0) }

// StatusSlot returns the status of the caller's VM in slot: 0 is the Docker VM, 1 to
// proto.MaxSlot a sandbox session (ADR 0009).
func (c *Client) StatusSlot(slot uint8) (proto.Reply, error) {
	return c.Do(proto.Request{Op: proto.OpStatus, Slot: slot})
}

// Pause pauses the caller's Docker VM, or resumes it.
func (c *Client) Pause(resume bool) (proto.Reply, error) { return c.PauseSlot(0, resume) }

// PauseSlot pauses the caller's VM in slot, or resumes it.
func (c *Client) PauseSlot(slot uint8, resume bool) (proto.Reply, error) {
	return c.Do(proto.Request{Op: proto.OpPause, Resume: resume, Slot: slot})
}

// Stop stops the caller's Docker VM: at once with force, else through HCS's shutdown
// request.
func (c *Client) Stop(force bool) error { return c.StopSlot(0, force) }

// StopSlot stops the caller's VM in slot, as Stop does.
func (c *Client) StopSlot(slot uint8, force bool) error {
	_, err := c.Do(proto.Request{Op: proto.OpStop, Force: force, Slot: slot})
	return err
}

// Resize changes the CPUs and memory of the caller's VM in slot.
func (c *Client) Resize(slot uint8, cpus, memoryMB uint32) (proto.Reply, error) {
	return c.Do(proto.Request{Op: proto.OpResize, Slot: slot, CPUs: cpus, MemoryMB: memoryMB})
}
