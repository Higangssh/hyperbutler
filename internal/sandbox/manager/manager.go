//go:build windows

package manager

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Higangssh/hyperbutler/internal/netstack"
	"github.com/Higangssh/hyperbutler/internal/sandbox/apply"
	"github.com/Higangssh/hyperbutler/internal/sandbox/copyin"
	"github.com/Higangssh/hyperbutler/internal/sandbox/ctl"
	"github.com/Higangssh/hyperbutler/internal/sandbox/egress"
	"github.com/Higangssh/hyperbutler/internal/sandbox/review"
	"github.com/Higangssh/hyperbutler/internal/sandbox/session"
	"github.com/Higangssh/hyperbutler/internal/sandbox/wire"
	"github.com/Higangssh/hyperbutler/internal/transport"
)

// Defaults of a new session. Memory is below the Docker VM's (ADR 0009); the disk is
// sparse, so its size costs nothing until it is used.
const (
	DefaultMemoryMB = 2048
	DefaultCPUs     = 2
	DefaultDiskSize = 64 << 30

	agentWait    = 60 * time.Second // from the VM's start until sbxd answers
	shutdownWait = 20 * time.Second
	reviewWait   = 10 * time.Minute
	copyWait     = 30 * time.Minute
	// maxBundleOut bounds the commits a session sends back.
	maxBundleOut = 2 << 30
	// maxReviewText bounds the diff text of one review; the rest is listed without it.
	maxReviewText = 16 << 20
)

// VM is a running session's VM.
type VM interface {
	Dialer(port uint32, wait time.Duration) transport.Dialer
	Gone() <-chan struct{}
	Terminate() error
	Close() error
}

// BootRequest is what Host.Boot gets.
type BootRequest struct {
	Name           string
	Slot           uint8
	CPUs, MemoryMB int
	DiskMeta       uint64
	Egress         netstack.Egress
}

// Host is what the manager needs of the machine: VMs and their disks.
type Host interface {
	Boot(ctx context.Context, r BootRequest) (VM, error)
	CreateDisk(slot uint8, size uint64) (metadataBytes uint64, err error)
	RemoveDisk(slot uint8) error
}

// Manager holds a user's sessions.
type Manager struct {
	host  Host
	store store
	log   *slog.Logger
	// git is the git executable the project's history goes through.
	git string

	// unreadable counts records that could not be read: each may own a disk.
	unreadable int

	mu       sync.Mutex
	sessions map[string]*sess
	lastBusy time.Time
	// active counts clients being served; the manager does not end under one.
	active int
	// quit, if set, ends the manager (OpQuit).
	quit context.CancelFunc
}

// sess is one session. mu serializes what changes it: booting, reviewing, applying,
// stopping. Reads for a listing take only Manager.mu and running.
type sess struct {
	rec     *Record
	running atomic.Bool
	terms   atomic.Uint32

	mu     sync.Mutex
	vm     VM
	policy *egress.Policy
	sent   map[string]copyin.Copied
	// items is the last review: what apply writes, and nothing else ever is. shown names
	// the ones a diff showed whole, which is what --all may take.
	items []session.Item
	shown map[string]bool
}

// New loads the sessions recorded in dir, none of them running. Problems with a record are
// logged: the rest still work.
func New(host Host, dir string, log *slog.Logger) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	m := &Manager{host: host, store: store{dir: dir}, log: log, git: "git", sessions: map[string]*sess{}, lastBusy: time.Now()}
	recs, errs := m.store.load()
	for _, err := range errs {
		log.Warn("a session's record could not be read", "err", err)
	}
	m.unreadable = len(errs)
	var used [MaxSlot + 1]bool
	for _, r := range recs {
		if used[r.Slot] {
			// Removing either would remove the other's disk. Only a crash or a hand reaches this.
			log.Warn("two sessions' records name one disk; this one is ignored", "session", r.Name, "slot", r.Slot)
			m.unreadable++
			continue
		}
		used[r.Slot] = true
		m.sessions[r.Name] = &sess{rec: r, sent: sentFromRecord(r.Sent)}
	}
	return m, nil
}

// IdleSince reports since when no session has run, zero while one does.
func (m *Manager) IdleSince() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active > 0 {
		m.lastBusy = time.Now()
		return time.Time{}
	}
	for _, s := range m.sessions {
		if s.running.Load() {
			m.lastBusy = time.Now()
			return time.Time{}
		}
	}
	return m.lastBusy
}

// Close ends every running session, in order if the guest answers soon; their disks and
// records stay.
func (m *Manager) Close() {
	m.mu.Lock()
	all := make([]*sess, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		s.mu.Lock()
		if s.vm != nil {
			if askShutdown(context.Background(), s.vm) == nil {
				select {
				case <-s.vm.Gone():
				case <-time.After(5 * time.Second):
				}
			}
			m.endVM(s)
		}
		s.mu.Unlock()
	}
}

func (m *Manager) find(name string) (*sess, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[name]; s != nil {
		return s, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrNoSession, name)
}

// Handle answers one request. progress is told what a long one is doing.
func (m *Manager) Handle(ctx context.Context, req *Request, progress func(string)) (Reply, error) {
	switch req.Op {
	case OpPing:
		return Reply{}, nil
	case OpQuit:
		m.mu.Lock()
		q := m.quit
		m.mu.Unlock()
		if q != nil {
			defer q()
		}
		return Reply{}, nil
	case OpRun:
		return m.run(ctx, req, progress)
	case OpList:
		return m.list(), nil
	}
	s, err := m.find(req.Name)
	if err != nil {
		return Reply{}, err
	}
	switch req.Op {
	case OpStart:
		return m.start(ctx, s, progress)
	case OpStop:
		return m.stop(ctx, s, req.Discard, false, progress)
	case OpRemove:
		return m.stop(ctx, s, req.Discard, true, progress)
	case OpDiff:
		return m.diff(ctx, s, req.StatOnly)
	case OpApply:
		return m.apply(s, req)
	case OpCommits:
		return m.commits(ctx, s, req.Branch)
	case OpAllow:
		return m.allow(s, req.Allow)
	case OpBlocked:
		return m.blocked(s)
	}
	return Reply{}, fmt.Errorf("unknown request %q", req.Op)
}

func info(s *sess) Info {
	r := s.rec
	return Info{Name: r.Name, Dir: r.Dir, Running: s.running.Load(), Kept: r.Kept, Mode: r.Mode, MemoryMB: r.MemoryMB, Created: r.Created}
}

func (m *Manager) list() Reply {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, info(s))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return Reply{Sessions: out}
}

// policyFor makes a session's egress hook: nil for open.
func policyFor(mode string, allow []string) (netstack.Egress, *egress.Policy, error) {
	md, err := egress.ParseMode(mode)
	if err != nil {
		return nil, nil, err
	}
	var entries []egress.Entry
	if md == egress.Dev {
		entries = egress.DevPreset()
	}
	for _, a := range allow {
		e, err := egress.ParseEntry(a)
		if err != nil {
			return nil, nil, err
		}
		entries = append(entries, e)
	}
	return egress.Hook(md, entries, egress.Options{})
}

// reserve picks the session's name and slot and makes it known, so two runs at once
// cannot take the same ones.
func (m *Manager) reserve(req *Request) (*sess, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	name := req.Name
	if name == "" {
		var b [3]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		name = "sbx-" + hex.EncodeToString(b[:])
	}
	if err := CheckName(name); err != nil {
		return nil, err
	}
	if m.sessions[name] != nil {
		return nil, fmt.Errorf("a session named %s exists; `hyperbutler sandbox start %s` runs it, `hyperbutler sandbox rm %s` removes it", name, name, name)
	}
	var used [MaxSlot + 1]bool
	for _, s := range m.sessions {
		used[s.rec.Slot] = true
	}
	for slot := uint8(1); slot <= MaxSlot; slot++ {
		if !used[slot] {
			mem, cpus := req.MemoryMB, req.CPUs
			if mem == 0 {
				mem = DefaultMemoryMB
			}
			if cpus == 0 {
				cpus = DefaultCPUs
			}
			s := &sess{rec: &Record{
				Name: name, Slot: slot, Dir: req.Dir, Kept: req.Kept, Mode: req.Mode, Allow: req.Allow,
				CPUs: cpus, MemoryMB: mem, Created: time.Now().UTC().Truncate(time.Second),
			}}
			m.sessions[name] = s
			return s, nil
		}
	}
	return nil, fmt.Errorf("you already have %d sessions; `hyperbutler sandbox rm NAME` removes one", MaxSlot)
}

func (m *Manager) forget(s *sess) {
	m.mu.Lock()
	delete(m.sessions, s.rec.Name)
	m.mu.Unlock()
}

func (m *Manager) run(ctx context.Context, req *Request, progress func(string)) (_ Reply, err error) {
	if !filepath.IsAbs(req.Dir) {
		return Reply{}, fmt.Errorf("the project %q is not an absolute path", req.Dir)
	}
	if fi, err := os.Stat(req.Dir); err != nil {
		return Reply{}, err
	} else if !fi.IsDir() {
		return Reply{}, fmt.Errorf("%s is not a folder", req.Dir)
	}
	if req.Mode == "" {
		req.Mode = string(egress.Dev)
	}
	hook, pol, err := policyFor(req.Mode, req.Allow)
	if err != nil {
		return Reply{}, err
	}
	plan, err := copyin.Select(req.Dir, copyin.Options{Include: req.Include, Git: m.git})
	if err != nil {
		return Reply{}, err
	}
	if !req.LargeOK && (plan.Total > copyin.DefaultMaxTotal || len(plan.TooBig) > 0) {
		return Reply{}, fmt.Errorf("the project is %d MiB in %d files, %d of them over the %d MiB limit; --large copies it anyway", plan.Total>>20, len(plan.Files), len(plan.TooBig), copyin.DefaultMaxFile>>20)
	}
	s, err := m.reserve(req)
	if err != nil {
		return Reply{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policy = pol
	disk := false
	defer func() {
		if err == nil {
			return
		}
		m.endVM(s)
		if disk {
			if rerr := m.host.RemoveDisk(s.rec.Slot); rerr != nil {
				m.log.Warn("remove a failed session's disk", "session", s.rec.Name, "err", rerr)
			}
		}
		_ = m.store.remove(s.rec.Name)
		m.forget(s)
	}()
	size := req.DiskSize
	if size == 0 {
		size = DefaultDiskSize
	}
	// No record owns this slot, so a disk there is what a failed run left. With a record
	// that could not be read it may be someone's work, and CreateDisk refuses it.
	if m.unreadable == 0 {
		if err := m.host.RemoveDisk(s.rec.Slot); err != nil {
			return Reply{}, err
		}
	}
	if s.rec.DiskMeta, err = m.host.CreateDisk(s.rec.Slot, size); err != nil {
		return Reply{}, err
	}
	disk = true
	// On disk before the VM: a crash from here on leaves a disk with a record that owns it.
	if err := m.store.save(s.rec); err != nil {
		return Reply{}, err
	}
	progress("starting the session's VM")
	if err := m.boot(ctx, s, hook); err != nil {
		return Reply{}, err
	}
	progress(fmt.Sprintf("copying %d files (%d MiB) into the session", len(plan.Files), plan.Total>>20))
	cctx, cancel := context.WithTimeout(ctx, copyWait)
	defer cancel()
	sent, err := m.sessionOf(s).CopyIn(cctx, req.Dir, plan)
	if err != nil {
		return Reply{}, err
	}
	if plan.FromGit {
		git, err := m.history(cctx, s, req.Dir, progress)
		if err != nil {
			return Reply{}, err
		}
		s.rec.Git = git
	}
	s.sent = sent
	s.rec.Sent = sentToRecord(sent)
	if err := m.store.save(s.rec); err != nil {
		return Reply{}, err
	}
	r := Reply{Files: len(plan.Files), Bytes: plan.Total}
	for _, sk := range plan.Skipped {
		r.Skipped = append(r.Skipped, Skipped{Path: review.Escape(sk.Path), Reason: sk.Reason})
	}
	in := info(s)
	r.Session = &in
	return r, nil
}

func (m *Manager) sessionOf(s *sess) *session.Session {
	return &session.Session{Copy: s.vm.Dialer(wire.CopyPort, agentWait), Changes: s.vm.Dialer(wire.Port, agentWait)}
}

// boot starts s's VM and watches for its end. The caller holds s.mu.
func (m *Manager) boot(ctx context.Context, s *sess, hook netstack.Egress) error {
	vm, err := m.host.Boot(ctx, BootRequest{Name: s.rec.Name, Slot: s.rec.Slot, CPUs: s.rec.CPUs, MemoryMB: s.rec.MemoryMB, DiskMeta: s.rec.DiskMeta, Egress: hook})
	if err != nil {
		return err
	}
	s.vm = vm
	s.running.Store(true)
	s.terms.Store(0) // the guest's terminals went with its last boot
	go func() {
		<-vm.Gone()
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.vm == vm {
			m.endVM(s)
		}
	}()
	return nil
}

// endVM releases s's VM. The caller holds s.mu.
func (m *Manager) endVM(s *sess) {
	if s.vm != nil {
		_ = s.vm.Terminate()
		if err := s.vm.Close(); err != nil {
			m.log.Warn("close a session's VM", "session", s.rec.Name, "err", err)
		}
		s.vm = nil
	}
	s.running.Store(false)
	s.items = nil
}

// history brings the project's branches and tags in as a bundle: no reflog, stash,
// dangling objects or .git/config (ADR 0009). It reports false, and the session has no
// history, when dir is not the top of its repository: commits could not come back to it.
func (m *Manager) history(ctx context.Context, s *sess, dir string, progress func(string)) (bool, error) {
	top, err := m.gitOut(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return false, err
	}
	if same, err := sameDir(strings.TrimSpace(top), dir); err != nil || !same {
		return false, err
	}
	req := ctl.BundleRequest{}
	// An identity is only what the agent's commits are signed with, in the guest.
	if v, err := m.gitOut(ctx, dir, "config", "user.name"); err == nil {
		req.Name = strings.TrimSpace(v)
	}
	if v, err := m.gitOut(ctx, dir, "config", "user.email"); err == nil {
		req.Email = strings.TrimSpace(v)
	}
	if v, err := m.gitOut(ctx, dir, "config", "core.autocrlf"); err == nil {
		switch v = strings.TrimSpace(v); strings.ToLower(v) {
		case "true", "input", "false":
			req.AutoCRLF = v
		}
	}
	var bundle *os.File
	if commit, err := m.gitOut(ctx, dir, "rev-parse", "--verify", "-q", "HEAD^{commit}"); err == nil {
		req.Commit = strings.TrimSpace(commit)
		if head, err := m.gitOut(ctx, dir, "symbolic-ref", "-q", "HEAD"); err == nil {
			req.Head = strings.TrimSpace(head)
		}
		progress("copying the git history into the session")
		tmp, err := os.CreateTemp(m.store.dir, s.rec.Name+".*.bundle")
		if err != nil {
			return false, err
		}
		name := tmp.Name()
		_ = tmp.Close()
		defer func() { _ = os.Remove(name) }()
		// HEAD names a detached head too; bundling it needs no ref of its own.
		if _, err := m.gitOut(ctx, dir, "bundle", "create", "-q", name, "--branches", "--tags", "HEAD"); err != nil {
			return false, err
		}
		if bundle, err = os.Open(name); err != nil { //nolint:gosec // G304: the temporary file above
			return false, err
		}
		defer func() { _ = bundle.Close() }()
	}
	c, err := s.vm.Dialer(ctl.Port, agentWait).DialContext(ctx)
	if err != nil {
		return false, fmt.Errorf("session ctl port: %w", err)
	}
	defer func() { _ = c.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	j, err := json.Marshal(req)
	if err != nil {
		return false, err
	}
	if err := ctl.WriteFrame(c, ctl.TypeBundleIn, j); err != nil {
		return false, err
	}
	if bundle != nil {
		buf := make([]byte, ctl.MaxData)
		for {
			n, rerr := bundle.Read(buf)
			if n > 0 {
				if err := ctl.WriteFrame(c, ctl.TypeInput, buf[:n]); err != nil {
					return false, err
				}
			}
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				return false, rerr
			}
		}
	}
	if err := ctl.WriteFrame(c, ctl.TypeInput, nil); err != nil {
		return false, err
	}
	if _, err := readEnd(c, io.Discard, 0); err != nil {
		return false, fmt.Errorf("the session did not take the history: %w", err)
	}
	return true, nil
}

// readEnd copies a ctl stream's output to w, at most limit bytes when limit is above
// zero, and returns the exit code that ends it. The guest's text comes back escaped.
func readEnd(r io.Reader, w io.Writer, limit int64) (int32, error) {
	var n int64
	for {
		t, data, err := ctl.ReadFrame(r)
		if err != nil {
			return 0, err
		}
		switch t {
		case ctl.TypeOutput:
			if n += int64(len(data)); limit > 0 && n > limit {
				return 0, fmt.Errorf("more than %d bytes", limit)
			}
			if _, err := w.Write(data); err != nil {
				return 0, err
			}
		case ctl.TypeExit:
			return ctl.ParseExit(data)
		case ctl.TypeError:
			return 0, errors.New(review.Shorten(string(data), 1024))
		default:
			return 0, fmt.Errorf("frame type %d", t)
		}
	}
}

func (m *Manager) gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, m.git, append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: git with fixed subcommands; values checked by the callers
	cmd.Env = gitEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, review.Shorten(strings.TrimSpace(stderr.String()), 1024))
	}
	return string(out), nil
}

// gitEnv is this process's environment without what could point git elsewhere.
func gitEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT":
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GIT_TERMINAL_PROMPT=0")
}

func sameDir(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(fa, fb), nil
}

func (m *Manager) start(ctx context.Context, s *sess, progress func(string)) (Reply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vm != nil {
		in := info(s)
		return Reply{Session: &in}, nil
	}
	hook, pol, err := policyFor(s.rec.Mode, s.rec.Allow)
	if err != nil {
		return Reply{}, err
	}
	s.policy = pol
	progress("starting the session's VM")
	if err := m.boot(ctx, s, hook); err != nil {
		return Reply{}, err
	}
	in := info(s)
	return Reply{Session: &in}, nil
}

// stop ends s's VM, in an orderly way when it can. An unnamed session and one being
// removed are deleted with it, which refuses while there are changes nobody applied:
// losing an agent's work to a command meant to tidy up costs more than a second command.
func (m *Manager) stop(ctx context.Context, s *sess, discard, remove bool, progress func(string)) (Reply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gone := remove || !s.rec.Kept
	if gone && !discard {
		if s.vm == nil {
			return Reply{}, fmt.Errorf("%s is stopped, so its changes cannot be listed; `hyperbutler sandbox start %s` runs it, --discard removes it with whatever it holds", s.rec.Name, s.rec.Name)
		}
		items, err := m.reviewLocked(ctx, s)
		if err != nil {
			return Reply{}, fmt.Errorf("list the session's changes: %w; --discard removes it anyway", err)
		}
		n := 0
		for _, it := range items {
			if it.Refused == "" {
				n++
			}
		}
		if n > 0 {
			return Reply{}, fmt.Errorf("%s has %d changes nobody applied; `hyperbutler sandbox diff %s` shows them, --discard removes it anyway", s.rec.Name, n, s.rec.Name)
		}
		if s.rec.Git {
			head, err := headCommit(ctx, s.vm)
			if err != nil {
				return Reply{}, fmt.Errorf("list the session's commits: %w; --discard removes it anyway", err)
			}
			if head != "" && head != s.rec.Fetched {
				return Reply{}, fmt.Errorf("%s has commits nobody fetched; `hyperbutler sandbox commits %s` brings them back, --discard removes it anyway", s.rec.Name, s.rec.Name)
			}
		}
	}
	if s.vm != nil {
		progress("stopping the session's VM")
		vm := s.vm
		if err := askShutdown(ctx, vm); err != nil {
			m.log.Warn("ask a session's guest to shut down", "session", s.rec.Name, "err", err)
		} else {
			select {
			case <-vm.Gone():
			case <-time.After(shutdownWait):
				m.log.Warn("a session's guest did not shut down in time", "session", s.rec.Name)
			}
		}
		m.endVM(s)
	}
	s.items = nil
	if gone {
		// The record last: a crash before it leaves a session that can be removed again.
		if err := m.host.RemoveDisk(s.rec.Slot); err != nil {
			return Reply{}, err
		}
		if err := m.store.remove(s.rec.Name); err != nil {
			return Reply{}, err
		}
		m.forget(s)
		s.sent, s.rec.Sent = nil, nil
	}
	return Reply{}, nil
}

// headCommit asks the guest where HEAD is, "" when that is not past the copy.
func headCommit(ctx context.Context, vm VM) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, agentWait)
	defer cancel()
	c, err := vm.Dialer(ctl.Port, agentWait).DialContext(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	if err := ctl.WriteFrame(c, ctl.TypeHead, nil); err != nil {
		return "", err
	}
	var out bytes.Buffer
	code, err := readEnd(c, &out, 128)
	if err != nil {
		return "", err
	}
	if code == ctl.ExitNoCommits {
		return "", nil
	}
	if code != 0 {
		return "", fmt.Errorf("code %d", code)
	}
	return out.String(), nil
}

// askShutdown asks the guest to shut down through sbxd. The VM offers no Hyper-V shutdown
// component, and a guest that does not answer is terminated: the host has nothing in it
// that it needs.
func askShutdown(ctx context.Context, vm VM) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := vm.Dialer(ctl.Port, 3*time.Second).DialContext(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	if err := ctl.WriteFrame(c, ctl.TypeShutdown, nil); err != nil {
		return err
	}
	_, err = readEnd(c, io.Discard, 0)
	return err
}

// reviewLocked reads the session's changes once and keeps them as the review: what apply
// writes. A change this computer's file already matches is left out: it was applied, or
// the user made the same edit. The caller holds s.mu.
func (m *Manager) reviewLocked(ctx context.Context, s *sess) ([]session.Item, error) {
	if s.vm == nil {
		return nil, fmt.Errorf("%s is stopped; `hyperbutler sandbox start %s` runs it", s.rec.Name, s.rec.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, reviewWait)
	defer cancel()
	items, err := m.sessionOf(s).Review(ctx, s.sent)
	if err != nil {
		return nil, err
	}
	s.shown = nil
	root, err := os.OpenRoot(s.rec.Dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	kept := items[:0]
	for _, it := range items {
		if it.Refused == "" {
			host, ok := hostFile(root, it.Path)
			switch {
			case it.Change.Delete && !ok:
				continue
			case !it.Change.Delete && ok && bytes.Equal(host, it.Change.Data):
				continue
			}
		}
		kept = append(kept, it)
	}
	s.items = kept
	return kept, nil
}

// hostFile reads a project file for comparison, not following a link out of the project.
func hostFile(root *os.Root, p string) ([]byte, bool) {
	fi, err := root.Lstat(filepath.FromSlash(p))
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > session.DefaultMaxFile {
		return nil, false
	}
	b, err := root.ReadFile(filepath.FromSlash(p))
	if err != nil {
		return nil, false
	}
	return b, true
}

var kindNames = map[wire.Kind]string{wire.Added: "added", wire.Modified: "modified", wire.Deleted: "deleted"}

func (m *Manager) diff(ctx context.Context, s *sess, statOnly bool) (Reply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := m.reviewLocked(ctx, s)
	if err != nil {
		return Reply{}, err
	}
	root, err := os.OpenRoot(s.rec.Dir)
	if err != nil {
		return Reply{}, err
	}
	defer func() { _ = root.Close() }()
	r := Reply{Items: make([]DiffItem, 0, len(items))}
	shown := make(map[string]bool, len(items))
	text := 0
	for _, it := range items {
		d := DiffItem{Path: review.Escape(it.Path), Shown: it.Shown, Kind: kindNames[it.Kind], Risk: string(it.Risk), Refused: it.Refused}
		seen := false
		if it.Refused == "" {
			host, ok := hostFile(root, it.Path)
			switch {
			case it.Change.Base == nil:
				d.Conflict = ok
			case !ok:
				d.Conflict = true
			default:
				d.Conflict = sha256.Sum256(host) != *it.Change.Base
			}
			td := review.TextDiff(host, it.Change.Data)
			d.Added, d.Removed, d.Binary = td.Added, td.Removed, td.Binary
			if !statOnly && text < maxReviewText {
				d.Text = td.Text
				text += len(td.Text)
				seen = !td.Cut
			}
			d.Unshown = !seen
		}
		if seen {
			shown[it.Path] = true
		}
		r.Items = append(r.Items, d)
	}
	s.shown = shown
	return r, nil
}

func (m *Manager) apply(s *sess, req *Request) (Reply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		return Reply{}, fmt.Errorf("nothing was reviewed; `hyperbutler sandbox diff %s` first: apply writes what the review showed", s.rec.Name)
	}
	want := map[string]bool{}
	for _, p := range req.Paths {
		want[p] = true
	}
	var r Reply
	var changes []apply.Change
	for _, it := range s.items {
		named := want[review.Escape(it.Path)]
		delete(want, review.Escape(it.Path))
		switch {
		case !named && !req.All:
		case it.Refused != "":
			if named {
				r.Outcomes = append(r.Outcomes, Outcome{Path: it.Shown, Error: it.Refused})
			}
		case it.Risk != "" && !named:
			// Each of these can run code later: --all does not take them.
			r.Outcomes = append(r.Outcomes, Outcome{Path: it.Shown, Error: "not applied: it can run code or redirect a tool (" + string(it.Risk) + "); name it with --path after reading it"})
		case !named && !s.shown[it.Path]:
			r.Outcomes = append(r.Outcomes, Outcome{Path: it.Shown, Error: "not applied: the review did not show all of it; run diff without --stat, or name it with --path"})
		default:
			changes = append(changes, it.Change)
		}
	}
	for p := range want {
		r.Outcomes = append(r.Outcomes, Outcome{Path: review.Escape(p), Error: "not in the last review"})
	}
	done, err := apply.Apply(s.rec.Dir, changes)
	if err != nil {
		return Reply{}, err
	}
	applied := map[string]bool{}
	for _, o := range done {
		out := Outcome{Path: review.Escape(o.Path)}
		if o.Err != nil {
			out.Error = review.Shorten(o.Err.Error(), 400)
		} else {
			applied[o.Path] = true
		}
		r.Outcomes = append(r.Outcomes, out)
	}
	// What is on this computer now is the base of the next conflict check.
	kept := s.items[:0]
	for _, it := range s.items {
		if !applied[it.Path] {
			kept = append(kept, it)
			continue
		}
		if it.Change.Delete {
			delete(s.sent, it.Path)
		} else {
			s.sent[it.Path] = copyin.Copied{Sum: it.Sum}
		}
	}
	s.items = kept
	s.rec.Sent = sentToRecord(s.sent)
	if err := m.store.save(s.rec); err != nil {
		return r, err
	}
	return r, nil
}

// commitsFile is a bundle of the session's commits that passed CheckBundle.
type commitsFile struct{ path, commit string }

// readCommits fetches the bundle of the commits HEAD has beyond the copy into a temporary
// file the caller removes. It returns nil when there are none.
func (m *Manager) readCommits(ctx context.Context, s *sess) (_ *commitsFile, err error) {
	c, err := s.vm.Dialer(ctl.Port, agentWait).DialContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("session ctl port: %w", err)
	}
	defer func() { _ = c.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	if err := ctl.WriteFrame(c, ctl.TypeBundleOut, nil); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(m.store.dir, s.rec.Name+".*.out")
	if err != nil {
		return nil, err
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(name)
		}
	}()
	code, err := readEnd(c, tmp, maxBundleOut)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("read the session's commits: %w", err)
	}
	if code == ctl.ExitNoCommits {
		_ = os.Remove(name)
		return nil, nil
	}
	if code != 0 {
		return nil, fmt.Errorf("the session could not bundle its commits (code %d)", code)
	}
	f, err := os.Open(name) //nolint:gosec // G304: the temporary file above
	if err != nil {
		return nil, err
	}
	head := make([]byte, ctl.MaxBundleHead())
	n, rerr := io.ReadFull(f, head)
	_ = f.Close()
	if rerr != nil && !errors.Is(rerr, io.ErrUnexpectedEOF) && !errors.Is(rerr, io.EOF) {
		return nil, rerr
	}
	if err := ctl.CheckBundle(head[:n]); err != nil {
		return nil, err
	}
	return &commitsFile{path: name, commit: ctl.BundleCommit(head[:n])}, nil
}

var branchRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,100}$`)

// commits fetches the commits the agent made into a branch of the project: never a
// checkout, and no ref but that branch (ADR 0009). The bundle is the guest's, so its head
// is checked first, fsck is on, and no hook of the project runs.
func (m *Manager) commits(ctx context.Context, s *sess, branch string) (Reply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vm == nil {
		return Reply{}, fmt.Errorf("%s is stopped; `hyperbutler sandbox start %s` runs it", s.rec.Name, s.rec.Name)
	}
	if !s.rec.Git {
		return Reply{}, fmt.Errorf("%s is not the top of a git repository, so the session has no history to bring back", s.rec.Dir)
	}
	if branch == "" {
		branch = "sandbox/" + s.rec.Name
	}
	if !branchRE.MatchString(branch) || strings.Contains(branch, "..") || strings.HasSuffix(branch, ".lock") || strings.HasSuffix(branch, "/") {
		return Reply{}, fmt.Errorf("%q is not a branch name hyperbutler takes", branch)
	}
	ctx, cancel := context.WithTimeout(ctx, copyWait)
	defer cancel()
	b, err := m.readCommits(ctx, s)
	if err != nil {
		return Reply{}, err
	}
	if b == nil {
		return Reply{Commits: "no new commits"}, nil
	}
	name := b.path
	defer func() { _ = os.Remove(name) }()
	hooks, err := os.MkdirTemp(m.store.dir, "nohooks")
	if err != nil {
		return Reply{}, err
	}
	defer func() { _ = os.Remove(hooks) }()
	// No force: a branch that moved on is an error, never overwritten.
	if _, err := m.gitOut(ctx, s.rec.Dir,
		"-c", "core.hooksPath="+hooks, "-c", "transfer.fsckObjects=true", "-c", "fetch.fsckObjects=true",
		"-c", "gc.auto=0", "-c", "fetch.recurseSubmodules=false",
		"fetch", "--no-tags", "--no-write-fetch-head", name, "HEAD:refs/heads/"+branch); err != nil {
		return Reply{}, fmt.Errorf("%w (a branch that moved on is not overwritten; --branch names another)", err)
	}
	s.rec.Fetched = b.commit
	if err := m.store.save(s.rec); err != nil {
		return Reply{}, err
	}
	log, err := m.gitOut(ctx, s.rec.Dir, "log", "-n", "20", "--format=%h %s", "refs/heads/"+branch, "^HEAD")
	if err != nil {
		return Reply{Branch: branch}, nil //nolint:nilerr // the fetch worked; the list is a courtesy
	}
	var sb strings.Builder
	for line := range strings.SplitSeq(strings.TrimRight(log, "\n"), "\n") {
		sb.WriteString(review.Escape(line))
		sb.WriteByte('\n')
	}
	return Reply{Branch: branch, Commits: sb.String()}, nil
}

func (m *Manager) allow(s *sess, entries []string) (Reply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if egress.Mode(s.rec.Mode) == egress.Open {
		return Reply{}, fmt.Errorf("%s is in the open mode: nothing is blocked", s.rec.Name)
	}
	parsed := make([]egress.Entry, 0, len(entries))
	for _, a := range entries {
		e, err := egress.ParseEntry(a)
		if err != nil {
			return Reply{}, err
		}
		parsed = append(parsed, e)
	}
	if s.policy != nil {
		for _, e := range parsed {
			if err := s.policy.Allow(e); err != nil {
				return Reply{}, err
			}
		}
	}
	s.rec.Allow = append(s.rec.Allow, entries...)
	return Reply{}, m.store.save(s.rec)
}

func (m *Manager) blocked(s *sess) (Reply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.policy == nil {
		return Reply{}, nil
	}
	r := Reply{BlockedLost: s.policy.BlockedLost()}
	for _, b := range s.policy.Blocked() {
		r.Blocked = append(r.Blocked, Blocked{Name: b.Name, Kind: b.Kind, Count: b.Count})
	}
	return r, nil
}

// Term connects a terminal of the session and returns the guest's connection, already
// told what it is for; after that it carries ctl frames the client reads.
func (m *Manager) Term(ctx context.Context, req *Request) (net.Conn, error) {
	s, err := m.find(req.Name)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	vm := s.vm
	s.mu.Unlock()
	if vm == nil {
		return nil, fmt.Errorf("%s is stopped; `hyperbutler sandbox start %s` runs it", s.rec.Name, s.rec.Name)
	}
	if len(req.Env) > ctl.MaxEnv || len(req.Argv) > ctl.MaxArgs {
		return nil, errors.New("too many arguments or variables for a terminal")
	}
	c, err := vm.Dialer(ctl.Port, agentWait).DialContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("session ctl port: %w", err)
	}
	t := ctl.TermRequest{Attach: req.Attach, Argv: req.Argv, Env: req.Env, Cols: req.Cols, Rows: req.Rows}
	if t.Attach == "" {
		// The first terminal of a boot is the one `attach` goes back to.
		if n := s.terms.Add(1); n == 1 {
			t.ID = "main"
		} else {
			t.ID = fmt.Sprintf("sh-%d", n)
		}
	}
	j, err := json.Marshal(t)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	if len(j) > ctl.MaxData {
		_ = c.Close()
		return nil, errors.New("a terminal's arguments and variables are too long")
	}
	if err := ctl.WriteFrame(c, ctl.TypeTerm, j); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// Serve answers one client on c: one request, and for OpTerm the terminal until either end
// closes.
func (m *Manager) Serve(ctx context.Context, c net.Conn) {
	m.mu.Lock()
	m.active++
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.active--
		m.lastBusy = time.Now()
		m.mu.Unlock()
	}()
	defer func() { _ = c.Close() }()
	br := bufio.NewReaderSize(c, 64<<10)
	line, err := readLine(br, maxRequest)
	if err != nil {
		return
	}
	var req Request
	enc := json.NewEncoder(c)
	if err := json.Unmarshal(line, &req); err != nil {
		_ = enc.Encode(Reply{Error: "a bad request: " + err.Error()})
		return
	}
	if req.Op == OpTerm {
		g, err := m.Term(ctx, &req)
		if err != nil {
			_ = enc.Encode(Reply{Error: err.Error()})
			return
		}
		defer func() { _ = g.Close() }()
		if err := enc.Encode(Reply{}); err != nil {
			return
		}
		// The frames go through unread: the client decodes them, bounded (ctl.ReadFrame).
		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(g, br); done <- struct{}{} }()
		go func() { _, _ = io.Copy(c, g); done <- struct{}{} }()
		<-done
		return
	}
	r, err := m.Handle(ctx, &req, func(p string) { _ = enc.Encode(Reply{Progress: p}) })
	if err != nil {
		r.Error = err.Error()
	}
	_ = enc.Encode(r)
}

const maxRequest = 8 << 20

func readLine(r *bufio.Reader, limit int) ([]byte, error) {
	var out []byte
	for {
		b, err := r.ReadSlice('\n')
		out = append(out, b...)
		if len(out) > limit {
			return nil, fs.ErrInvalid
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return out, err
	}
}
