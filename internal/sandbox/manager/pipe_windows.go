//go:build windows

package manager

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/winipc"
)

// idleExit is how long the manager stays after its last session stopped.
const idleExit = 5 * time.Minute

// PipeName is the manager's pipe for an installation, by Layout.Tag.
func PipeName(tag string) string { return `\\.\pipe\hyperbutler_sandbox_` + tag }

// ListenAndServe serves m on pipe until ctx ends or no session has run for idleExit. Only
// this user can open the pipe: whoever can, can run commands in their sessions.
func ListenAndServe(ctx context.Context, m *Manager, pipe string) error {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	l, err := winipc.ListenPipe(pipe, &winipc.PipeConfig{
		SecurityDescriptor: "O:" + tu.User.Sid.String() + "D:P(A;;GA;;;" + tu.User.Sid.String() + ")",
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if since := m.IdleSince(); !since.IsZero() && time.Since(since) > idleExit {
					cancel()
					return
				}
			}
		}
	}()
	context.AfterFunc(ctx, func() { _ = l.Close() })
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go m.Serve(ctx, c)
	}
}

// ErrNotRunning says no manager serves the pipe.
var ErrNotRunning = errors.New("the sandbox manager is not running")

// Client is one request's connection to the manager.
type Client struct {
	c  net.Conn
	br *bufio.Reader
}

// Dial connects to the manager. A pipe another account made first is refused before any
// byte of the request crosses it: requests carry project paths and environment variables.
func Dial(ctx context.Context, pipe string) (*Client, error) {
	c, err := winipc.DialPipe(ctx, pipe)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return nil, ErrNotRunning
		}
		return nil, err
	}
	if err := checkOwner(c); err != nil {
		_ = c.Close()
		return nil, err
	}
	return &Client{c: c, br: bufio.NewReader(c)}, nil
}

func checkOwner(c net.Conn) error {
	h, ok := c.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("the sandbox manager's pipe has no handle")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(h.Fd()), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read the pipe's owner: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return fmt.Errorf("read the pipe's owner: %w", err)
	}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	if !owner.Equals(tu.User.Sid) {
		return fmt.Errorf("the sandbox manager's pipe belongs to %s, not to this account: another program holds its name", owner)
	}
	return nil
}

// Close ends the connection.
func (c *Client) Close() error { return c.c.Close() }

// Do sends req and returns its reply, telling progress what the manager says meanwhile.
func (c *Client) Do(req *Request, progress func(string)) (Reply, error) {
	j, err := json.Marshal(req)
	if err != nil {
		return Reply{}, err
	}
	if _, err := c.c.Write(append(j, '\n')); err != nil {
		return Reply{}, err
	}
	dec := json.NewDecoder(c.br)
	for {
		var r Reply
		if err := dec.Decode(&r); err != nil {
			return Reply{}, fmt.Errorf("the sandbox manager: %w", err)
		}
		if r.Progress != "" {
			if progress != nil {
				progress(r.Progress)
			}
			continue
		}
		if r.Error != "" {
			return r, errors.New(r.Error)
		}
		// What the decoder read past the reply belongs to a terminal, after the reply's own
		// end of line, which Serve writes with it.
		c.br = bufio.NewReader(io.MultiReader(dec.Buffered(), c.c))
		if b, err := c.br.ReadByte(); err != nil || b != '\n' {
			return Reply{}, fmt.Errorf("the sandbox manager: a reply without an end of line: %w", err)
		}
		return r, nil
	}
}

// Stream is the connection after OpTerm's reply: reads get the guest's ctl frames, writes
// go to the guest.
func (c *Client) Stream() io.ReadWriter {
	return struct {
		io.Reader
		io.Writer
	}{c.br, c.c}
}
