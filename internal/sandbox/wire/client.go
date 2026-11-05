package wire

import (
	"errors"
	"fmt"
	"io"
	"syscall"
	"time"
)

// Conn is a connection to a session's guest agent.
type Conn interface {
	io.ReadWriter
	SetDeadline(time.Time) error
}

// Client asks one session's guest agent for its changes. Calls are not concurrent.
type Client struct {
	c   Conn
	seq uint32
	buf []byte
	// Timeout bounds each request's whole exchange.
	Timeout time.Duration
}

// NewClient returns a client on c.
func NewClient(c Conn) *Client {
	return &Client{c: c, buf: make([]byte, MaxBody), Timeout: time.Minute}
}

// GuestError is an Error frame: an errno and the guest's text (guest-controlled; show
// it escaped).
type GuestError struct {
	Errno uint16
	Text  string
}

func (e *GuestError) Error() string {
	return fmt.Sprintf("session: %s (errno %d)", syscall.Errno(e.Errno), e.Errno)
}

func (c *Client) send(m Msg) error {
	c.seq++
	m.Seq = c.seq
	b, err := Encode(m)
	if err != nil {
		return err
	}
	if err := c.c.SetDeadline(time.Now().Add(c.Timeout)); err != nil {
		return err
	}
	_, err = c.c.Write(b)
	return err
}

func (c *Client) recv() (Msg, error) {
	body, err := ReadFrame(c.c, c.buf)
	if err != nil {
		return Msg{}, err
	}
	m, err := Decode(body, true)
	if err != nil {
		return Msg{}, err
	}
	if m.Seq != c.seq {
		return Msg{}, bad("reply to %d, want %d", m.Seq, c.seq)
	}
	if m.Type == TypeError {
		return Msg{}, &GuestError{Errno: m.Errno, Text: m.Text}
	}
	return m, nil
}

// List returns the session's changed files. A path appears at most once, and the count
// in the End frame must match.
func (c *Client) List() ([]Msg, error) {
	if err := c.send(Msg{Type: TypeList}); err != nil {
		return nil, err
	}
	var out []Msg
	seen := map[string]bool{}
	for {
		m, err := c.recv()
		if err != nil {
			return nil, err
		}
		switch m.Type {
		case TypeEntry:
			if len(out) >= MaxEntries {
				return nil, bad("more than %d entries", MaxEntries)
			}
			if seen[m.Path] {
				return nil, bad("path listed twice")
			}
			seen[m.Path] = true
			out = append(out, m)
		case TypeEnd:
			if int(m.Count) != len(out) {
				return nil, bad("end says %d entries, got %d", m.Count, len(out))
			}
			return out, nil
		default:
			return nil, bad("type %#x in a list", m.Type)
		}
	}
}

// ErrTooLarge: a file over the caller's limit. Like a GuestError, it leaves the client
// usable: every request got its reply.
var ErrTooLarge = errors.New("session file is larger than the limit")

// ReadFile reads all of path, refusing more than max bytes. The guest's Size is not
// trusted: the read stops at max whatever the list said.
func (c *Client) ReadFile(path string, max int64) ([]byte, error) {
	var out []byte
	for {
		want := uint32(MaxData)
		if left := max + 1 - int64(len(out)); left < int64(want) {
			want = uint32(left) //nolint:gosec // G115: below MaxData
		}
		if err := c.send(Msg{Type: TypeRead, Off: uint64(len(out)), Len: want, Path: path}); err != nil {
			return nil, err
		}
		m, err := c.recv()
		if err != nil {
			return nil, err
		}
		if m.Type != TypeData || len(m.Data) > int(want) {
			return nil, bad("read answered with type %#x, %d bytes", m.Type, len(m.Data))
		}
		out = append(out, m.Data...)
		if int64(len(out)) > max {
			return nil, fmt.Errorf("%w (%d bytes)", ErrTooLarge, max)
		}
		if m.EOF {
			return out, nil
		}
		if len(m.Data) == 0 {
			return nil, bad("empty read before the end of the file")
		}
	}
}
