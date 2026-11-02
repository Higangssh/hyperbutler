// Package vmsecret is the per-VM secret of ADR 0004 rule 6: 32 random bytes made for
// each VM start, given to the guest on its kernel command line (CmdlineKey) and to the
// VM's owner only, which the host sends as the first bytes of every hvsock connection
// it makes to the guest. The guest's services read those bytes before they serve
// anything, compare them in constant time, and close the connection on a mismatch
// without writing a byte, so a peer learns nothing but that it was refused.
//
// The hvsock connect descriptors already allow only the owner's SID; the secret is the
// barrier that remains if a platform does not enforce them.
//
// A Secret never prints: every fmt verb renders it as "[redacted]", so it cannot reach a
// log through a %v of a struct that holds it. Hex is the one way to get its text, for
// the kernel command line. That holds for exported fields only: fmt does not call the
// methods of an unexported field's value, so a struct that keeps a Secret in an
// unexported field prints its bytes under %v and must never be formatted (or must
// implement fmt.Formatter itself).
//
// The package builds on every platform and depends only on the standard library, so
// the broker, the engine and the guest share it.
package vmsecret

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Size is the length of a secret in bytes.
const Size = 32

// CmdlineKey is the kernel command line argument that carries the secret, as
// CmdlineKey=<64 lowercase hex digits>.
const CmdlineKey = "hyperbutler.secret"

// Secret is one VM's secret. The zero value is no secret: it matches nothing.
type Secret [Size]byte

// Errors returned by Parse, FromCmdline and Check.
var (
	// ErrMissing: the command line carries no secret.
	ErrMissing = errors.New("vmsecret: no " + CmdlineKey + " on the kernel command line")
	// ErrMalformed: the text is not 64 lowercase hex digits of a non-zero secret, or the
	// command line carries the key more than once.
	ErrMalformed = errors.New("vmsecret: malformed secret")
	// ErrMismatch: a peer did not send the expected secret.
	ErrMismatch = errors.New("vmsecret: the peer did not present the VM secret")
)

// New returns a fresh secret from crypto/rand.
func New() (Secret, error) {
	var s Secret
	if _, err := rand.Read(s[:]); err != nil {
		return Secret{}, fmt.Errorf("vmsecret: %w", err)
	}
	if s.IsZero() {
		// 2^-256; a broken generator rather than chance.
		return Secret{}, errors.New("vmsecret: the random generator returned zeros")
	}
	return s, nil
}

// IsZero reports whether s is the zero (absent) secret.
func (s Secret) IsZero() bool { return s == Secret{} }

// Hex returns s as 64 lowercase hex digits, the form CmdlineKey carries.
func (s Secret) Hex() string { return hex.EncodeToString(s[:]) }

// String hides the secret.
func (Secret) String() string { return "[redacted]" }

// GoString hides the secret from %#v.
func (Secret) GoString() string { return "vmsecret.Secret{[redacted]}" }

// Format hides the secret from every fmt verb, including %x and %d, which would
// otherwise print an array's bytes.
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, "[redacted]") }

// Equal compares a and b in constant time.
func Equal(a, b Secret) bool { return subtle.ConstantTimeCompare(a[:], b[:]) == 1 }

// Parse parses 64 lowercase hex digits of a non-zero secret.
func Parse(s string) (Secret, error) {
	var out Secret
	if len(s) != 2*Size {
		return Secret{}, ErrMalformed
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return Secret{}, ErrMalformed
		}
	}
	if _, err := hex.Decode(out[:], []byte(s)); err != nil || out.IsZero() {
		return Secret{}, ErrMalformed
	}
	return out, nil
}

// maxCmdline bounds the command line FromCmdline looks at (the kernel's own limit on
// x86 is 2048 bytes; /proc/cmdline adds a newline).
const maxCmdline = 64 << 10

// FromCmdline returns the secret of a kernel command line (the text of /proc/cmdline).
// It fails with ErrMissing without CmdlineKey, and with ErrMalformed if the key appears
// more than once or its value is not a well-formed secret: a second copy is refused
// rather than one of them chosen.
func FromCmdline(cmdline string) (Secret, error) {
	if len(cmdline) > maxCmdline {
		return Secret{}, ErrMalformed
	}
	const prefix = CmdlineKey + "="
	var (
		found bool
		val   string
	)
	for _, arg := range strings.Fields(cmdline) {
		if arg != CmdlineKey && !strings.HasPrefix(arg, prefix) {
			continue
		}
		if found {
			return Secret{}, ErrMalformed
		}
		found, val = true, strings.TrimPrefix(arg, prefix)
		if arg == CmdlineKey {
			val = ""
		}
	}
	if !found {
		return Secret{}, ErrMissing
	}
	return Parse(val)
}

// DefaultCmdlinePath is where a Linux guest reads its kernel command line.
const DefaultCmdlinePath = "/proc/cmdline"

// Load reads the kernel command line at path (DefaultCmdlinePath in a guest) and
// returns its secret, as FromCmdline. A guest service that gets an error keeps the zero
// secret and so refuses every connection.
func Load(path string) (Secret, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the guest service's own flag names its kernel command line (/proc/cmdline)
	if err != nil {
		return Secret{}, fmt.Errorf("vmsecret: %w", err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxCmdline+1))
	if err != nil {
		return Secret{}, fmt.Errorf("vmsecret: %w", err)
	}
	return FromCmdline(string(b))
}

// Check reads exactly Size bytes from r and reports whether they are want, compared in
// constant time. A zero want matches nothing, so a guest that has no secret refuses
// every connection. It reads nothing past the secret.
func Check(r io.Reader, want Secret) error {
	var got Secret
	if _, err := io.ReadFull(r, got[:]); err != nil {
		return fmt.Errorf("%w: %w", ErrMismatch, err)
	}
	ok := Equal(got, want)
	if !ok || want.IsZero() {
		return ErrMismatch
	}
	return nil
}

// Send writes s as the first bytes of a connection. A zero secret is refused.
func Send(w io.Writer, s Secret) error {
	if s.IsZero() {
		return errors.New("vmsecret: no secret to send")
	}
	_, err := w.Write(s[:])
	return err
}

// DefaultTimeout is how long Admit waits for the secret. The host writes it in the same
// dial call that connects (transport.HvsockDialer), before the connection is used, so a
// peer that has not sent 32 bytes by then is not the host.
const DefaultTimeout = time.Second

// DeadlineReader is a connection whose reads can be bounded.
type DeadlineReader interface {
	io.Reader
	SetReadDeadline(t time.Time) error
}

// Admit checks the secret at the start of c within timeout (DefaultTimeout if zero) and
// clears the read deadline again. On an error the caller closes c without writing
// anything to it.
func Admit(c DeadlineReader, want Secret, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if err := c.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("%w: %w", ErrMismatch, err)
	}
	if err := Check(c, want); err != nil {
		return err
	}
	return c.SetReadDeadline(time.Time{})
}

// Conn is a connection a Gate admits.
type Conn interface {
	DeadlineReader
	io.Closer
}

// ErrBusy is passed to a Gate's callback for a connection that arrived while MaxPending
// others were still being checked; it was closed at once.
var ErrBusy = errors.New("vmsecret: too many connections waiting to be admitted")

// DefaultMaxPending is a Gate's default bound on connections being checked at once.
const DefaultMaxPending = 8

// Gate admits connections concurrently, so a peer that connects and sends nothing holds
// up no one else: each connection is checked (Admit) on a goroutine of its own, at most
// MaxPending at a time, and one over that bound is closed at once. The zero value admits
// no one (its Secret is zero).
type Gate struct {
	Secret     Secret
	Timeout    time.Duration // DefaultTimeout if zero
	MaxPending int           // DefaultMaxPending if zero

	once sync.Once
	sem  chan struct{}
}

// Go checks c in the background and then calls done on that goroutine: with nil when c
// was admitted (the caller then owns c), or with the error after c was closed, without a
// byte written to it. done is called exactly once; for ErrBusy, before Go returns.
func (g *Gate) Go(c Conn, done func(err error)) {
	g.once.Do(func() {
		n := g.MaxPending
		if n <= 0 {
			n = DefaultMaxPending
		}
		g.sem = make(chan struct{}, n)
	})
	select {
	case g.sem <- struct{}{}:
	default:
		_ = c.Close()
		done(ErrBusy)
		return
	}
	go func() {
		err := Admit(c, g.Secret, g.Timeout)
		<-g.sem
		if err != nil {
			_ = c.Close()
		}
		done(err)
	}()
}
