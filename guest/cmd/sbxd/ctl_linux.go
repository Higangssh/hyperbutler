//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Higangssh/hyperbutler/internal/sandbox/ctl"
	"github.com/Higangssh/hyperbutler/internal/vmsecret"
)

const (
	baseCommitFile = "/var/lib/hyperbutler/base-commit"
	inBundleFile   = "/var/lib/hyperbutler/in.bundle"
	stopPipe       = "/run/hyperbutler/stop"
	// ctlConns bounds connections on the port: every terminal, and a few for the bundles.
	ctlConns = ctl.MaxTerminals + 4
	// screenKept is how much of a terminal's output waits for the next attach.
	screenKept = 64 << 10
	// ptyDrain is how long output is read after the process exits; a background process
	// can hold the terminal open forever.
	ptyDrain = time.Second
)

var sessionEnv = []string{
	"HOME=/home/agent", "USER=agent", "LOGNAME=agent", "SHELL=/bin/bash",
	"PATH=/usr/local/bin:/usr/bin:/bin", "TERM=xterm-256color", "LANG=C.UTF-8",
}

// listenCtl serves the ctl port. Unlike the other two, its connections run at once: a
// terminal stays attached for as long as the user works.
func (s *state) listenCtl() error {
	l, err := listenVsock(ctl.Port)
	if err != nil {
		return err
	}
	sem := make(chan struct{}, ctlConns)
	for {
		c, cid, err := l.accept()
		if err != nil {
			if errors.Is(err, unix.ECONNABORTED) || errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		if cid != unix.VMADDR_CID_HOST {
			_ = c.Close()
			continue
		}
		select {
		case sem <- struct{}{}:
		default:
			_ = c.Close()
			continue
		}
		go func() {
			defer func() { <-sem; _ = c.Close() }()
			if err := vmsecret.Admit(c, s.secret, 5*time.Second); err != nil {
				return
			}
			if err := s.serveCtl(c); err != nil && !errors.Is(err, io.EOF) {
				slog.Warn("sbxd ctl", "err", err)
				_ = ctl.WriteFrame(c, ctl.TypeError, []byte(limit(err.Error(), 4096)))
			}
		}()
	}
}

func limit(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (s *state) serveCtl(c *vsockConn) error {
	t, data, err := ctl.ReadFrame(c)
	if err != nil {
		return err
	}
	switch t {
	case ctl.TypeTerm:
		var req ctl.TermRequest
		if err := json.Unmarshal(data, &req); err != nil {
			return err
		}
		return s.terms.serve(c, &req)
	case ctl.TypeBundleIn:
		var req ctl.BundleRequest
		if err := json.Unmarshal(data, &req); err != nil {
			return err
		}
		return bundleIn(c, &req)
	case ctl.TypeBundleOut:
		return bundleOut(c)
	case ctl.TypeShutdown:
		return shutdown(c)
	case ctl.TypeHead:
		return headCommit(c)
	}
	return fmt.Errorf("frame type %d first", t)
}

// terminals are a session's: they outlive their connection, so closing the host's window
// does not stop what the agent is doing.
type terminals struct {
	mu sync.Mutex
	m  map[string]*terminal
}

type terminal struct {
	id   string
	ptmx *os.File
	// fd is ptmx's descriptor: calling Fd() would take the file off the runtime poller.
	fd int

	mu      sync.Mutex
	screen  []byte
	viewer  *vsockConn
	exited  bool
	code    int32
	drained chan struct{}
}

func (ts *terminals) serve(c *vsockConn, req *ctl.TermRequest) error {
	t, err := ts.find(req)
	if err != nil {
		return err
	}
	if req.Cols != 0 && req.Rows != 0 {
		setSize(t.fd, req.Cols, req.Rows)
	}
	if done := t.attach(c); done {
		ts.mu.Lock()
		delete(ts.m, t.id)
		ts.mu.Unlock()
		return nil
	}
	defer t.detach(c)
	for {
		ty, data, err := ctl.ReadFrame(c)
		if err != nil {
			return nil //nolint:nilerr // the host went away: a detach, the terminal stays
		}
		switch ty {
		case ctl.TypeInput:
			if _, err := t.ptmx.Write(data); err != nil {
				<-t.drained
				return nil //nolint:nilerr // the process ended: the pump told the viewer
			}
		case ctl.TypeResize:
			if cols, rows, err := ctl.ParseResize(data); err == nil && cols != 0 && rows != 0 {
				setSize(t.fd, cols, rows)
			}
		default:
			return fmt.Errorf("frame type %d on a terminal", ty)
		}
	}
}

func (ts *terminals) find(req *ctl.TermRequest) (*terminal, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.m == nil {
		ts.m = map[string]*terminal{}
	}
	if req.Attach != "" {
		t := ts.m[req.Attach]
		if t == nil {
			return nil, fmt.Errorf("no terminal %q", req.Attach)
		}
		return t, nil
	}
	if req.ID == "" || ts.m[req.ID] != nil {
		return nil, fmt.Errorf("a terminal needs a new id, not %q", req.ID)
	}
	if len(ts.m) >= ctl.MaxTerminals {
		return nil, fmt.Errorf("this session already has %d terminals", ctl.MaxTerminals)
	}
	t, err := startTerminal(req)
	if err != nil {
		return nil, err
	}
	ts.m[req.ID] = t
	return t, nil
}

// startTerminal starts the process on a new pseudo-terminal, in its own session.
func startTerminal(req *ctl.TermRequest) (*terminal, error) {
	if len(req.Argv) > ctl.MaxArgs || len(req.Env) > ctl.MaxEnv {
		return nil, errors.New("too many arguments or variables")
	}
	mfd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	ptmx := os.NewFile(uintptr(mfd), "ptmx")
	if err := unix.IoctlSetPointerInt(mfd, unix.TIOCSPTLCK, 0); err != nil {
		_ = ptmx.Close()
		return nil, fmt.Errorf("unlock the terminal: %w", err)
	}
	sfd, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(mfd), unix.TIOCGPTPEER, uintptr(unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC))
	if errno != 0 {
		_ = ptmx.Close()
		return nil, fmt.Errorf("open the terminal's other end: %w", errno)
	}
	pts := os.NewFile(sfd, "pts")
	defer func() { _ = pts.Close() }()
	if req.Cols != 0 && req.Rows != 0 {
		setSize(mfd, req.Cols, req.Rows)
	}
	argv := req.Argv
	if len(argv) == 0 {
		argv = []string{"/bin/bash", "-l"}
	}
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // G204: this agent runs what the user's session asks for, as the agent user
	cmd.Dir = workspaceDir
	cmd.Env = append(append([]string(nil), sessionEnv...), req.Env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pts, pts, pts
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		_ = ptmx.Close()
		return nil, err
	}
	t := &terminal{id: req.ID, ptmx: ptmx, fd: mfd, drained: make(chan struct{})}
	pumped := make(chan struct{})
	go func() { t.pump(); close(pumped) }()
	go func() {
		code := int32(0)
		if err := cmd.Wait(); err != nil {
			code = 1
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = int32(ee.ExitCode()) //nolint:gosec // G115: an exit status
			}
		}
		select {
		case <-pumped:
		case <-time.After(ptyDrain):
		}
		_ = ptmx.Close()
		t.finish(code)
	}()
	return t, nil
}

func setSize(fd int, cols, rows uint16) {
	_ = unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: rows})
}

// pump keeps what the process prints and shows it to whoever is attached. It ends when
// every holder of the terminal's other end is gone (EIO on Linux).
func (t *terminal) pump() {
	buf := make([]byte, ctl.MaxData)
	for {
		n, err := t.ptmx.Read(buf)
		if n > 0 {
			t.mu.Lock()
			t.screen = append(t.screen, buf[:n]...)
			if over := len(t.screen) - screenKept; over > 0 {
				t.screen = append(t.screen[:0], t.screen[over:]...)
			}
			if v := t.viewer; v != nil {
				_ = v.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if ctl.WriteFrame(v, ctl.TypeOutput, buf[:n]) != nil {
					// The host stopped reading: it can attach again.
					t.viewer = nil
					_ = v.Close()
				}
			}
			t.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// attach makes c the viewer, after what the terminal printed so far. It reports whether
// the terminal has ended, in which case c has been told and nothing more follows.
func (t *terminal) attach(c *vsockConn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for b := t.screen; len(b) > 0; {
		n := min(len(b), ctl.MaxData)
		if ctl.WriteFrame(c, ctl.TypeOutput, b[:n]) != nil {
			return false
		}
		b = b[n:]
	}
	if t.exited {
		_ = ctl.WriteFrame(c, ctl.TypeExit, ctl.Exit(t.code))
		return true
	}
	if t.viewer != nil {
		// The newest window wins; an older one is closed rather than left to go stale.
		_ = t.viewer.Close()
	}
	t.viewer = c
	return false
}

func (t *terminal) detach(c *vsockConn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.viewer == c {
		t.viewer = nil
	}
}

func (t *terminal) finish(code int32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.exited, t.code = true, code
	if v := t.viewer; v != nil {
		_ = ctl.WriteFrame(v, ctl.TypeExit, ctl.Exit(code))
		t.viewer = nil
	}
	close(t.drained)
}

// shutdown tells the init to end the session: this agent has no rights over it, and the
// init listens on a pipe the agent user may write to. An agent that does the same only
// ends its own VM.
func shutdown(c *vsockConn) error {
	f, err := os.OpenFile(stopPipe, os.O_WRONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString("stop\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return ctl.WriteFrame(c, ctl.TypeExit, ctl.Exit(0))
}

var (
	gitOID    = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	// The name comes from the host's git, which has judged it; this only keeps it one word.
	gitBranch = regexp.MustCompile(`^refs/heads/[^\x00-\x20\x7f]{1,240}$`)
)

// bundleIn turns the workspace into a repository with the project's branches and tags
// and a generated config. The working tree stays as the copy left it, so the user's
// uncommitted work shows as such in the session (ADR 0009, "Bringing the project in").
func bundleIn(c *vsockConn, req *ctl.BundleRequest) error {
	f, err := os.OpenFile(inBundleFile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(inBundleFile) }()
	size := 0
	for {
		t, data, err := ctl.ReadFrame(c)
		if err == nil && t != ctl.TypeInput {
			err = fmt.Errorf("frame type %d in a bundle", t)
		}
		if err != nil {
			_ = f.Close()
			return err
		}
		if len(data) == 0 {
			break
		}
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			return err
		}
		size += len(data)
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Modes on Windows are a guess from the file's name, so they mean nothing to compare.
	steps := [][]string{{"init", "-q"}, {"config", "core.fileMode", "false"}}
	for _, kv := range [][2]string{{"user.name", req.Name}, {"user.email", req.Email}, {"core.autocrlf", req.AutoCRLF}} {
		if kv[1] != "" {
			steps = append(steps, []string{"config", kv[0], kv[1]})
		}
	}
	if size > 0 {
		if !gitOID.MatchString(req.Commit) || req.Head != "" && !gitBranch.MatchString(req.Head) {
			return errors.New("a bundle's head is not a commit and a branch")
		}
		steps = append(steps, []string{"fetch", "-q", "--update-head-ok", inBundleFile, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"})
		if req.Head != "" {
			steps = append(steps, []string{"symbolic-ref", "HEAD", req.Head})
		} else {
			steps = append(steps, []string{"update-ref", "--no-deref", "HEAD", req.Commit})
		}
		steps = append(steps, []string{"reset", "-q", "--mixed", "HEAD"})
	}
	for _, s := range steps {
		if out, err := git(s...).CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w: %s", s[0], err, limit(string(out), 2048))
		}
	}
	if size > 0 {
		if err := os.WriteFile(baseCommitFile, []byte(req.Commit), 0o600); err != nil {
			return err
		}
	}
	return ctl.WriteFrame(c, ctl.TypeExit, ctl.Exit(0))
}

// headCommit tells the host where HEAD is, when that is past what was copied in: enough
// for it to know whether commits are waiting, without the bundle.
func headCommit(c *vsockConn) error {
	head, err := git("rev-parse", "--verify", "-q", "HEAD^{commit}").Output()
	if err != nil {
		return ctl.WriteFrame(c, ctl.TypeExit, ctl.Exit(ctl.ExitNoCommits))
	}
	oid := strings.TrimSpace(string(head))
	if b, err := os.ReadFile(baseCommitFile); err == nil {
		base := string(bytes.TrimSpace(b))
		n, err := git("rev-list", "--count", base+"..HEAD").Output()
		if base == oid || err == nil && strings.TrimSpace(string(n)) == "0" {
			return ctl.WriteFrame(c, ctl.TypeExit, ctl.Exit(ctl.ExitNoCommits))
		}
	}
	if err := ctl.WriteFrame(c, ctl.TypeOutput, []byte(oid)); err != nil {
		return err
	}
	return ctl.WriteFrame(c, ctl.TypeExit, ctl.Exit(0))
}

// bundleOut sends the commits HEAD has beyond what was copied in. The agent controls the
// repository and everything git does with it here; the host checks what it gets.
func bundleOut(c *vsockConn) error {
	head, err := git("rev-parse", "--verify", "-q", "HEAD^{commit}").Output()
	if err != nil {
		return ctl.WriteFrame(c, ctl.TypeExit, ctl.Exit(ctl.ExitNoCommits))
	}
	args := []string{"bundle", "create", "-", "HEAD"}
	if b, err := os.ReadFile(baseCommitFile); err == nil {
		base := string(bytes.TrimSpace(b))
		if base == strings.TrimSpace(string(head)) {
			return ctl.WriteFrame(c, ctl.TypeExit, ctl.Exit(ctl.ExitNoCommits))
		}
		n, err := git("rev-list", "--count", base+"..HEAD").Output()
		if err == nil && strings.TrimSpace(string(n)) == "0" {
			return ctl.WriteFrame(c, ctl.TypeExit, ctl.Exit(ctl.ExitNoCommits))
		}
		args = append(args, "^"+base)
	}
	cmd := git(args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	buf := make([]byte, ctl.MaxData)
	for {
		n, rerr := out.Read(buf)
		if n > 0 {
			if err := ctl.WriteFrame(c, ctl.TypeOutput, buf[:n]); err != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return err
			}
		}
		if rerr != nil {
			break
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git bundle: %w: %s", err, limit(stderr.String(), 2048))
	}
	return ctl.WriteFrame(c, ctl.TypeExit, ctl.Exit(0))
}

func git(args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-C", workspaceDir}, args...)...) //nolint:gosec // G204: fixed subcommands, and values checked above
	cmd.Env = sessionEnv
	return cmd
}
