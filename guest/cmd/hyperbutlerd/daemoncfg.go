package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Higangssh/hyperbutler/internal/guestctl"
)

// dockerdConfig installs the host's daemon.json (REQ-UX-007) and restarts dockerd on
// request. The file lives on the /run tmpfs: the system disk is read-only, and the host
// sends the file again on every boot, so nothing has to persist in the guest.
//
// The init (guest/prototype/init) starts dockerd with --config-file=path when the file
// exists. On SIGHUP it stops dockerd and starts it again, and it writes "N PID" to gen
// for every dockerd it starts. While hold exists, a dockerd that exits does not power
// the VM off: the init waits for the next SIGHUP instead, which is how a dockerd that
// does not start with a new file is rolled back.
type dockerdConfig struct {
	path  string // the installed file
	ready string // created after the first successful commit (the init waits for it)
	gen   string // the init's dockerd generation file
	hold  string // see above
	sock  string // dockerd's API socket

	// validate runs `dockerd --validate --config-file=path`; it returns dockerd's
	// complaint on failure.
	validate func(ctx context.Context, path string) (string, error)
	// restart asks the init to restart dockerd (SIGHUP).
	restart func() error
	// alive reports whether pid still runs.
	alive func(pid int) bool
	// upTimeout bounds the wait for a restarted dockerd to answer (90 s).
	upTimeout time.Duration
	// pollEvery is the readiness poll period (200 ms).
	pollEvery time.Duration
	// ping checks that dockerd answers (GET /_ping on sock if nil).
	ping func(ctx context.Context) error

	mu        sync.Mutex
	readyDone bool
}

func (d *dockerdConfig) apply(ctx context.Context, data []byte, restart bool) (res guestctl.DaemonResult) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t0 := time.Now()
	defer func() { res.Duration = time.Since(t0) }()
	fail := func(code guestctl.DaemonCode, format string, a ...any) guestctl.DaemonResult {
		r := guestctl.DaemonResult{Code: code, Message: clip(fmt.Sprintf(format, a...))}
		logf("daemon.json: %s", r.Message)
		return r
	}

	prev, err := os.ReadFile(d.path)
	prevExists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fail(guestctl.DaemonFailed, "read the installed daemon.json: %v", err)
	}
	changed := prevExists != (len(data) > 0) || !bytes.Equal(prev, data)
	if changed {
		if err := d.install(ctx, data); err != nil {
			var rej *rejectedError
			if errors.As(err, &rej) {
				return fail(guestctl.DaemonRejected, "%s", rej.msg)
			}
			return fail(guestctl.DaemonFailed, "%v", err)
		}
		res.Flags |= guestctl.DaemonChanged
		logf("daemon.json: installed %d bytes", len(data))
	}
	gen, _, err := readGen(d.gen)
	boot := errors.Is(err, fs.ErrNotExist) // dockerd has not started yet
	if err != nil && !boot {
		return fail(guestctl.DaemonFailed, "%v", err)
	}
	// A changed file on a running dockerd takes effect only with a restart: never
	// report it as installed while dockerd runs another (the init's boot wait timed out,
	// or a reconnect re-sends what the host believes is in effect).
	if changed && !boot {
		restart = true
	}
	// The watch below must not end with the control connection: a reconnect during a
	// restart is no reason to roll back. upTimeout bounds it.
	ctx = context.WithoutCancel(ctx)
	// At boot the init starts dockerd once the ready file exists. If it starts with a
	// new file, watch it come up as after a restart: a file that dockerd --validate
	// passes can still keep dockerd from starting, and that must not power the VM off.
	watch := restart || boot && changed && len(data) > 0
	if !watch {
		d.markReady()
		return res
	}
	if err := os.WriteFile(d.hold, nil, 0o600); err != nil {
		d.markReady()
		return fail(guestctl.DaemonFailed, "hold file: %v", err)
	}
	defer os.Remove(d.hold)
	d.markReady()
	if boot {
		gen, err = d.waitUp(ctx, 0)
	} else {
		gen, err = d.restartDockerd(ctx, gen)
	}
	if err == nil {
		if !boot {
			res.Flags |= guestctl.DaemonRestarted
			logf("daemon.json: dockerd restarted")
		}
		return res
	}
	if !changed {
		return fail(guestctl.DaemonFailed, "dockerd did not come back after a restart: %v", err)
	}
	// Put the previous file back and try again with it.
	logf("daemon.json: dockerd did not start with the new file (%v); restoring the previous one", err)
	var old []byte
	if prevExists {
		old = prev
	}
	if ierr := d.put(old); ierr != nil {
		return fail(guestctl.DaemonFailed, "dockerd did not start with the new daemon.json (%v), and the previous one could not be restored: %v", err, ierr)
	}
	if _, rerr := d.restartDockerd(ctx, gen); rerr != nil {
		return fail(guestctl.DaemonFailed, "dockerd did not start with the new daemon.json (%v), nor with the previous one (%v)", err, rerr)
	}
	r := fail(guestctl.DaemonRolledBack, "dockerd did not start with the new daemon.json (%v); the previous one is back in effect", err)
	r.Flags = guestctl.DaemonChanged | guestctl.DaemonRestarted
	return r
}

type rejectedError struct{ msg string }

func (e *rejectedError) Error() string { return e.msg }

// install validates data and makes it the installed file (or removes that, for empty
// data). On any error the installed file is unchanged.
func (d *dockerdConfig) install(ctx context.Context, data []byte) error {
	if len(data) == 0 {
		return d.put(nil)
	}
	cand := d.path + ".new"
	if err := os.WriteFile(cand, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", cand, err)
	}
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	msg, err := d.validate(vctx, cand)
	cancel()
	if err != nil {
		_ = os.Remove(cand)
		msg = strings.ReplaceAll(msg, cand, "daemon.json")
		if msg == "" {
			msg = err.Error()
		}
		return &rejectedError{msg: "dockerd refused daemon.json: " + msg}
	}
	if err := os.Rename(cand, d.path); err != nil {
		_ = os.Remove(cand)
		return fmt.Errorf("install daemon.json: %w", err)
	}
	return nil
}

// put installs data without validation (a file that was valid before), or removes the
// installed file.
func (d *dockerdConfig) put(data []byte) error {
	if data == nil {
		if err := os.Remove(d.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	tmp := d.path + ".new"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, d.path)
}

func (d *dockerdConfig) markReady() {
	if d.readyDone {
		return
	}
	if err := os.WriteFile(d.ready, nil, 0o644); err != nil {
		logf("daemon.json ready file: %v", err)
	}
	d.readyDone = true
}

// restartDockerd asks the init for a new dockerd and waits until it answers. It returns
// the new generation.
func (d *dockerdConfig) restartDockerd(ctx context.Context, gen uint64) (uint64, error) {
	if err := d.restart(); err != nil {
		return gen, fmt.Errorf("signal the init: %w", err)
	}
	return d.waitUp(ctx, gen)
}

// waitUp waits until a dockerd newer than generation gen answers.
func (d *dockerdConfig) waitUp(ctx context.Context, gen uint64) (uint64, error) {
	deadline := time.Now().Add(d.upTimeout)
	for {
		g, pid, err := readGen(d.gen)
		if err == nil && g > gen {
			ping := d.ping
			if ping == nil {
				ping = func(ctx context.Context) error { return pingDocker(ctx, d.sock) }
			}
			if ping(ctx) == nil {
				return g, nil
			}
			if !d.alive(pid) {
				return g, errors.New("dockerd exited")
			}
		}
		if time.Now().After(deadline) {
			return max(g, gen), fmt.Errorf("dockerd did not answer within %v", d.upTimeout)
		}
		select {
		case <-ctx.Done():
			return max(g, gen), ctx.Err()
		case <-time.After(d.pollEvery):
		}
	}
}

// readGen parses the init's "N PID" line.
func readGen(path string) (gen uint64, pid int, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	f := strings.Fields(string(b))
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("%s: malformed", path)
	}
	gen, err1 := strconv.ParseUint(f[0], 10, 64)
	p, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || p <= 0 {
		return 0, 0, fmt.Errorf("%s: malformed", path)
	}
	return gen, p, nil
}

// pingDocker sends GET /_ping to dockerd's socket and wants a 200.
func pingDocker(ctx context.Context, sock string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var dl net.Dialer
	c, err := dl.DialContext(ctx, "unix", sock)
	if err != nil {
		return err
	}
	defer c.Close()
	deadline, _ := ctx.Deadline()
	_ = c.SetDeadline(deadline)
	if _, err := c.Write([]byte("GET /_ping HTTP/1.0\r\nHost: docker\r\n\r\n")); err != nil {
		return err
	}
	line, err := bufio.NewReaderSize(c, 256).ReadString('\n')
	if err != nil {
		return err
	}
	if f := strings.Fields(line); len(f) < 2 || f[1] != "200" {
		return fmt.Errorf("dockerd answered %q", strings.TrimSpace(line))
	}
	return nil
}

// clip shortens s to guestctl.MaxDaemonMessage bytes on a rune boundary.
func clip(s string) string {
	s = strings.ToValidUTF8(s, "?")
	if len(s) <= guestctl.MaxDaemonMessage {
		return s
	}
	s = s[:guestctl.MaxDaemonMessage]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// lastLine is the last non-empty line of dockerd's output, without its log prefix.
func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	l := strings.TrimSpace(lines[len(lines)-1])
	// "unable to configure the Docker daemon with file X: reason"
	if i := strings.Index(l, "daemon with file "); i >= 0 {
		if j := strings.Index(l[i:], ": "); j >= 0 {
			l = l[i+j+2:]
		}
	}
	return l
}

// logf is log.Printf, replaceable in tests.
var logf = log.Printf
