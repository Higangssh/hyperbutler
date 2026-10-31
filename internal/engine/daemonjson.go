package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Higangssh/hyperbutler/internal/daemoncfg"
	"github.com/Higangssh/hyperbutler/internal/guestctl"
)

// DaemonPusher sends a daemon.json to the guest (guestctl.Client).
type DaemonPusher interface {
	Hello() guestctl.HelloReply
	DaemonConfig(ctx context.Context, data []byte, restart bool) (guestctl.DaemonResult, error)
}

// DaemonSource says where the daemon.json in effect came from.
type DaemonSource string

const (
	DaemonNone     DaemonSource = "none"     // dockerd runs with its defaults
	DaemonFile     DaemonSource = "file"     // the user's daemon.json
	DaemonPrevious DaemonSource = "previous" // the last one that worked (the user's is invalid)
)

// DaemonState is what state.json says about daemon.json (REQ-UX-007).
type DaemonState struct {
	// Generation counts finished deliveries (boot, reconnects and applies); Applies
	// counts finished `config daemon apply` requests, so that command can wait for the
	// result of its own.
	Generation uint64       `json:"generation"`
	Applies    uint64       `json:"applies"`
	Source     DaemonSource `json:"source"`
	// SHA256 and Bytes describe the file in effect; empty and 0 for none.
	SHA256 string `json:"sha256,omitempty"`
	Bytes  int    `json:"bytes"`
	// Restarted says the last delivery restarted dockerd.
	Restarted bool `json:"restarted,omitempty"`
	// Error is why the last delivery did not put the user's file in effect.
	Error   string    `json:"error,omitempty"`
	Updated time.Time `json:"updated"`
}

// Validate reports the first field out of range.
func (d *DaemonState) Validate() error {
	switch {
	case d.Source != DaemonNone && d.Source != DaemonFile && d.Source != DaemonPrevious:
		return fmt.Errorf("daemon state: unknown source %q", d.Source)
	case d.Bytes < 0 || d.Bytes > daemoncfg.MaxSize:
		return fmt.Errorf("daemon state: size %d out of range", d.Bytes)
	case (d.Source == DaemonNone) != (d.SHA256 == "") || (d.SHA256 == "") != (d.Bytes == 0):
		return errors.New("daemon state: source, hash and size disagree")
	case d.SHA256 != "" && !isHexDigest(d.SHA256):
		return fmt.Errorf("daemon state: bad hash %q", d.SHA256)
	case len(d.Error) > maxErrorLen || !utf8.ValidString(d.Error):
		return errors.New("daemon state: error text too long or not UTF-8")
	}
	return nil
}

// ReadDaemonJSON reads and checks the user's daemon.json. It returns (nil, nil) when
// there is none.
func ReadDaemonJSON(l Layout, guestSubnet netip.Prefix) ([]byte, error) {
	b, err := ReadFileBounded(l.DaemonConfigPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", l.DaemonConfigPath(), err)
	}
	if err := daemoncfg.Check(b, daemoncfg.Options{GuestSubnet: guestSubnet}); err != nil {
		return nil, fmt.Errorf("%s: %w", l.DaemonConfigPath(), err)
	}
	return b, nil
}

// DaemonConfig delivers the user's daemon.json to the guest: on every control
// connection (boot and reconnects) and on `config daemon apply`. It keeps a copy of the
// last file that the guest accepted (DaemonAppliedPath), so an invalid user file leaves
// the previous one in effect, across restarts too.
type DaemonConfig struct {
	Layout      Layout
	GuestSubnet netip.Prefix
	Log         *slog.Logger
	// OnState, if set, receives every change (for state.json).
	OnState func(DaemonState)

	once      sync.Once
	delivered chan struct{} // closed when the first delivery is done

	pubMu sync.Mutex
	pub   DaemonState // st as last published, for State

	mu       sync.Mutex
	st       DaemonState
	inEffect []byte // what the guest's dockerd runs with (or will start with)
	known    bool   // inEffect is known (a delivery succeeded on this VM)
	pusher   DaemonPusher
}

func isHexDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && hex.EncodeToString(b) == s
}

func digest(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// result records the outcome of a delivery.
func (d *DaemonConfig) result(src DaemonSource, data []byte, restarted bool, errText string) {
	d.st.Generation++
	d.st.Source, d.st.SHA256, d.st.Bytes = src, digest(data), len(data)
	if len(data) == 0 {
		d.st.Source = DaemonNone
	}
	d.st.Restarted = restarted
	d.st.Error = strings.ToValidUTF8(errText, "?")
	if len(d.st.Error) > maxErrorLen {
		d.st.Error = d.st.Error[:maxErrorLen]
	}
	d.st.Updated = time.Now().UTC().Truncate(time.Millisecond)
	d.pubMu.Lock()
	d.pub = d.st
	d.pubMu.Unlock()
	if d.OnState != nil {
		d.OnState(d.st)
	}
}

func (d *DaemonConfig) deliveredChan() chan struct{} {
	d.once.Do(func() { d.delivered = make(chan struct{}) })
	return d.delivered
}

// WaitDelivered waits until the first control connection's delivery is done (dockerd
// then runs with its final file at boot), or ctx ends.
func (d *DaemonConfig) WaitDelivered(ctx context.Context) error {
	select {
	case <-d.deliveredChan():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// State returns the state of the last finished delivery. It does not wait for one in
// progress.
func (d *DaemonConfig) State() DaemonState {
	d.pubMu.Lock()
	defer d.pubMu.Unlock()
	return d.pub
}

func (d *DaemonConfig) log() *slog.Logger {
	if d.Log == nil {
		return slog.Default()
	}
	return d.Log
}

// guestText escapes what the guest said for logs and state.json.
func guestText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || invisibleRune(r) {
			fmt.Fprintf(&b, `\u%04x`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// invisibleRune reports format characters that can reorder or hide text on a terminal:
// bidi controls, zero-width characters and the byte order mark.
func invisibleRune(r rune) bool {
	return r >= 0x200b && r <= 0x200f || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 || r == 0xfeff || r == 0x2028 || r == 0x2029
}

func describe(r guestctl.DaemonResult) string {
	msg := guestText(r.Message)
	switch r.Code {
	case guestctl.DaemonRejected:
		return msg
	case guestctl.DaemonBadTransfer:
		return "the guest received a damaged daemon.json (" + msg + ")"
	case guestctl.DaemonRolledBack:
		return msg
	}
	return "the guest could not apply daemon.json: " + msg
}

// Connected delivers the file for a new control connection, without restarting dockerd:
// at boot the guest's init waits for it before it starts dockerd. On a reconnect the
// file already in effect is sent again (a no-op for the guest).
func (d *DaemonConfig) Connected(ctx context.Context, p DaemonPusher) {
	d.mu.Lock()
	defer d.mu.Unlock()
	defer func() {
		select {
		case <-d.deliveredChan():
		default:
			close(d.deliveredChan())
		}
	}()
	d.pusher = p
	if p.Hello().Features&guestctl.FeatureDaemonConfig == 0 {
		d.log().Warn("the guest image cannot take a daemon.json; dockerd runs with its defaults")
		d.result(DaemonNone, nil, false, "the guest image does not support daemon.json; update it")
		return
	}
	type cand struct {
		src  DaemonSource
		data []byte
	}
	var cands []cand
	var why string
	if d.known {
		cands = []cand{{d.st.Source, d.inEffect}}
	} else {
		user, uerr := ReadDaemonJSON(d.Layout, d.GuestSubnet)
		prev, perr := ReadFileBounded(d.Layout.DaemonAppliedPath())
		havePrev := perr == nil && daemoncfg.Check(prev, daemoncfg.Options{GuestSubnet: d.GuestSubnet}) == nil
		switch {
		case uerr != nil:
			why = uerr.Error() + "; the previous daemon.json stays in effect"
			if havePrev {
				cands = append(cands, cand{DaemonPrevious, prev})
			}
		case user != nil:
			cands = append(cands, cand{DaemonFile, user})
			if havePrev && !bytes.Equal(prev, user) {
				cands = append(cands, cand{DaemonPrevious, prev})
			}
		}
		cands = append(cands, cand{DaemonNone, nil})
	}
	// restart is set once the guest's dockerd runs: after a boot-time rollback, the next
	// candidate needs a dockerd restart (with the guest's own rollback) to take effect.
	restart := false
	for _, c := range cands {
		if restart && c.data == nil {
			break // dockerd already runs without a file
		}
		r, err := p.DaemonConfig(ctx, c.data, restart)
		if err != nil {
			d.log().Warn("daemon.json: delivery failed", "err", err)
			d.result(d.st.Source, d.inEffect, false, "could not deliver daemon.json: "+err.Error())
			return
		}
		if r.Code == guestctl.DaemonOK {
			d.inEffect, d.known = c.data, true
			if c.src == DaemonFile {
				d.saveApplied(c.data)
			} else if c.src == DaemonNone && why == "" {
				// No user file: forget the old one too.
				_ = os.Remove(d.Layout.DaemonAppliedPath())
			}
			d.log().Info("daemon.json delivered", "source", c.src, "bytes", len(c.data), "dockerdRestarted", restart)
			d.result(c.src, c.data, restart, why)
			return
		}
		why = describe(r)
		if r.Code == guestctl.DaemonRolledBack && !restart {
			// At boot dockerd did not start with it; the guest started it without one.
			d.inEffect, d.known, restart = nil, true, true
		}
		if c.src == DaemonFile && r.Code != guestctl.DaemonRolledBack {
			why += "; the previous daemon.json stays in effect"
		}
		d.log().Warn("daemon.json refused by the guest", "source", c.src, "why", why)
	}
	src := d.st.Source
	if d.known && d.inEffect == nil {
		src = DaemonNone
	}
	d.result(src, d.inEffect, restart, why)
}

func (d *DaemonConfig) saveApplied(b []byte) {
	if err := WriteFileAtomic(d.Layout.DaemonAppliedPath(), b); err != nil {
		d.log().Warn("daemon.json: save the applied copy", "err", err)
	}
}

// ErrNoGuest is returned by Apply while no control connection is up.
var ErrNoGuest = errors.New("the guest's control channel is not connected")

// Apply reads the user's daemon.json again and puts it in effect, restarting dockerd
// (not the VM). An invalid file, or one dockerd refuses, leaves the previous one in
// effect. The outcome is in State as well as the error.
func (d *DaemonConfig) Apply(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.st.Applies++
	fail := func(err error) error {
		d.result(d.st.Source, d.inEffect, false, err.Error())
		return err
	}
	p := d.pusher
	if p != nil && p.Hello().Features&guestctl.FeatureDaemonConfig == 0 {
		return fail(errors.New("the guest image does not support daemon.json; update it"))
	}
	if p == nil || !d.known {
		return fail(ErrNoGuest)
	}
	data, err := ReadDaemonJSON(d.Layout, d.GuestSubnet)
	if err != nil {
		return fail(fmt.Errorf("%w; the previous daemon.json stays in effect", err))
	}
	r, err := p.DaemonConfig(ctx, data, true)
	if err != nil {
		return fail(fmt.Errorf("could not deliver daemon.json: %w", err))
	}
	switch r.Code {
	case guestctl.DaemonOK:
		d.inEffect = data
		src := DaemonFile
		if data == nil {
			src = DaemonNone
			_ = os.Remove(d.Layout.DaemonAppliedPath())
		} else {
			d.saveApplied(data)
		}
		d.log().Info("daemon.json applied", "bytes", len(data), "dockerdRestarted", r.Flags&guestctl.DaemonRestarted != 0, "took", r.Duration)
		d.result(src, data, r.Flags&guestctl.DaemonRestarted != 0, "")
		return nil
	case guestctl.DaemonRolledBack:
		err := errors.New(describe(r))
		d.log().Warn("daemon.json rolled back", "why", err)
		d.result(d.st.Source, d.inEffect, true, err.Error())
		return err
	}
	err = errors.New(describe(r))
	if r.Code == guestctl.DaemonRejected {
		err = fmt.Errorf("%w; the previous daemon.json stays in effect", err)
	}
	return fail(err)
}
