//go:build windows

package engine

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Microsoft/go-winio/pkg/guid"

	"github.com/Higangssh/hyperbutler/internal/guestctl"
	"github.com/Higangssh/hyperbutler/internal/spaceguard"
	"github.com/Higangssh/hyperbutler/internal/vm"
)

func TestPrepareDisk(t *testing.T) {
	dir := testDir(t)
	l := Layout{Home: dir, Data: filepath.Join(dir, "data")}
	d, err := prepareDisk(l, MinDiskSize)
	if err != nil {
		t.Fatal(err)
	}
	if !d.created || d.meta.SizeBytes != MinDiskSize || d.meta.BlockSize != 1<<20 {
		t.Fatalf("first prepare: %+v %+v", d, d.meta)
	}
	u, err := vm.Usage(d.path)
	if err != nil || !u.Sparse || u.Allocated > 64<<20 || d.meta.MetadataBytes != u.Allocated {
		t.Fatalf("new disk: %+v %v, meta %d", u, err, d.meta.MetadataBytes)
	}
	if _, err := os.Stat(d.path + ".new"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file left: %v", err)
	}
	// The second start reuses it, whatever size is asked for now.
	d2, err := prepareDisk(l, 2*MinDiskSize)
	if err != nil || d2.created || d2.meta.SizeBytes != MinDiskSize {
		t.Fatalf("second prepare: %+v %v", d2, err)
	}

	// A crash between disk.json and the rename is completed.
	if err := os.Rename(d.path, d.path+".new"); err != nil {
		t.Fatal(err)
	}
	if d3, err := prepareDisk(l, MinDiskSize); err != nil || !d3.created {
		t.Fatalf("after a crash before the rename: %+v %v", d3, err)
	}

	// A disk removed behind hyperbutler's back is an error, not a silent reformat.
	if err := os.Remove(d.path); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareDisk(l, MinDiskSize); ErrorCode(err) != CodeDataDisk {
		t.Fatalf("missing disk: %v", err)
	}
	if _, err := os.Stat(d.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a new disk was created over a missing one")
	}
	// And so is a disk without its disk.json.
	if err := os.WriteFile(d.path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(l.DiskMetaPath()); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareDisk(l, MinDiskSize); ErrorCode(err) != CodeDataDisk {
		t.Fatalf("disk without meta: %v", err)
	}
}

func TestPrepareDiskRefusesNonSparse(t *testing.T) {
	// As hyperbutler makes it (a temp folder may let other accounts in, and disk.json
	// is read only from a folder that does not).
	dir := filepath.Join(testDir(t), "data")
	if err := MkdirPrivate(dir); err != nil {
		t.Fatal(err)
	}
	l := Layout{Home: dir, Data: dir}
	if err := saveDiskMeta(l, &DiskMeta{SizeBytes: MinDiskSize, BlockSize: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.DiskPath(), []byte("not a vhdx"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareDisk(l, MinDiskSize); ErrorCode(err) != CodeDataDisk || !strings.Contains(err.Error(), "sparse") {
		t.Fatalf("non-sparse disk: %v", err)
	}
}

func TestMkdirPrivate(t *testing.T) {
	dir := testDir(t)
	p := filepath.Join(dir, "a", "b")
	if err := MkdirPrivate(p); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		t.Fatal(err)
	}
	if err := MkdirPrivate(p); err != nil {
		t.Fatalf("existing dir: %v", err)
	}
	if err := MkdirPrivate("relative"); err == nil {
		t.Fatal("accepted a relative path")
	}
	f := filepath.Join(dir, "file")
	_ = os.WriteFile(f, nil, 0o600)
	if err := MkdirPrivate(f); err == nil {
		t.Fatal("accepted a file")
	}
	link := filepath.Join(dir, "link")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, p).CombinedOutput(); err != nil {
		t.Skipf("mklink /J: %v %s", err, out)
	}
	if err := MkdirPrivate(link); err == nil {
		t.Fatal("accepted a junction as the data directory")
	}
}

// fakeMachine records calls.
type fakeMachine struct {
	mu                    sync.Mutex
	pauses, resumes, term int
	state                 string
}

func (f *fakeMachine) Start() error                  { return nil }
func (f *fakeMachine) RuntimeID() (guid.GUID, error) { return guid.NewV4() }
func (f *fakeMachine) State() (string, error)        { return f.state, nil }
func (f *fakeMachine) ShutDown() error               { return errors.New("not supported") }
func (f *fakeMachine) Close() error                  { return nil }
func (f *fakeMachine) Pause() error                  { f.mu.Lock(); f.pauses++; f.mu.Unlock(); return nil }
func (f *fakeMachine) Resume() error                 { f.mu.Lock(); f.resumes++; f.mu.Unlock(); return nil }
func (f *fakeMachine) Terminate() error              { f.mu.Lock(); f.term++; f.mu.Unlock(); return nil }
func (f *fakeMachine) counts() (p, r, t int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pauses, f.resumes, f.term
}

type fakeBackend struct {
	list   []vm.Summary
	opened map[string]*fakeMachine
	calls  []string
}

func (b *fakeBackend) Create(string, *vm.Config) (Machine, error) { return nil, errors.New("no") }
func (b *fakeBackend) Open(id string) (Machine, error) {
	m := &fakeMachine{}
	b.opened[id] = m
	return m, nil
}
func (b *fakeBackend) List(owner string) ([]vm.Summary, error) {
	var out []vm.Summary
	for _, s := range b.list {
		if s.Owner == owner {
			out = append(out, s)
		}
	}
	return out, nil
}
func (b *fakeBackend) GrantAccess(id, path string) error {
	b.calls = append(b.calls, "grant "+id+" "+path)
	return nil
}
func (b *fakeBackend) RevokeAccess(string, string) error { return nil }
func (b *fakeBackend) RuntimeIDOf(id string) (guid.GUID, error) {
	b.calls = append(b.calls, "runtime id "+id)
	return guid.NewV5(guid.GUID{}, []byte(id))
}

func TestCleanupOrphans(t *testing.T) {
	l := Layout{Home: `C:\h`}
	b := &fakeBackend{opened: map[string]*fakeMachine{}, list: []vm.Summary{
		{ID: l.VMPrefix() + "aaaa", Owner: VMOwner},
		{ID: l.VMPrefix() + "bbbb", Owner: VMOwner},
		{ID: "hyperbutler-00000000-cccc", Owner: VMOwner}, // another installation
		{ID: l.VMPrefix() + "dddd", Owner: "someone"},     // not hyperbutler's
		{ID: "hyperbutler-dev-1234", Owner: "hyperbutler-dev"},
	}}
	if n := CleanupOrphans(b, l.VMPrefix(), slog.New(slog.DiscardHandler)); n != 2 {
		t.Fatalf("terminated %d, want 2", n)
	}
	if len(b.opened) != 2 || b.opened[l.VMPrefix()+"aaaa"] == nil || b.opened[l.VMPrefix()+"bbbb"] == nil {
		t.Fatalf("opened %v", b.opened)
	}
	for id, m := range b.opened {
		if _, _, term := m.counts(); term != 1 {
			t.Errorf("%s terminated %d times", id, term)
		}
	}
}

// The host guard pauses below the floor, resumes above floor + gap, and pauses at the
// margin instead when the guest guard stops showing progress.
func TestHostGuardPause(t *testing.T) {
	dir := testDir(t)
	vhdx := filepath.Join(dir, "data.vhdx")
	if err := os.WriteFile(vhdx, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	free, err := vm.FreeBytes(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := &fakeMachine{}
	sup := &guestctl.Supervisor{}
	disk := &dataDisk{path: vhdx, meta: &DiskMeta{SizeBytes: MinDiskSize, BlockSize: 1 << 20, MetadataBytes: 1 << 20}}
	// A floor above the real free space: pause.
	cfg := spaceguard.Config{Margin: free + 1<<40, Floor: free + 1<<30, ResumeGap: 1 << 20, Hysteresis: 1 << 20, Quantum: 1 << 20}
	g, err := newHostGuard(cfg, m, sup, disk, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	g.tick()
	if p, r, _ := m.counts(); p != 1 || r != 0 || !g.paused {
		t.Fatalf("below the floor: pauses %d resumes %d", p, r)
	}
	g.tick()
	if p, _, _ := m.counts(); p != 1 {
		t.Fatal("paused twice")
	}
	// Space came back: resume.
	g.cfg = spaceguard.Config{Margin: 2 << 20, Floor: 1 << 20, ResumeGap: 1 << 20, Hysteresis: 1 << 20, Quantum: 1 << 20}
	g.tick()
	if _, r, _ := m.counts(); r != 1 || g.paused {
		t.Fatalf("resumes %d paused %v", r, g.paused)
	}
	// A guard that went silent: pause at the margin, although free is above the floor.
	g.cfg = spaceguard.Config{Margin: free + 1<<40, Floor: 1 << 20, ResumeGap: 1 << 20, Hysteresis: 1 << 20, Quantum: 1 << 20}
	g.tick()
	if p, _, _ := m.counts(); p != 1 {
		t.Fatal("paused at the margin although the guard is live")
	}
	g.live.Store(time.Now().Add(-time.Minute).UnixNano())
	g.tick()
	if p, _, _ := m.counts(); p != 2 {
		t.Fatal("a silent guard did not pause at the margin")
	}
	// Status that shows progress revives it; status without progress does not.
	g.live.Store(0)
	g.onStatus(guestctl.Status{Guard: guestctl.GuardOK, GuardSteps: 5})
	if time.Since(time.Unix(0, g.live.Load())) > time.Second {
		t.Fatal("progress not counted")
	}
	g.live.Store(0)
	g.onStatus(guestctl.Status{Guard: guestctl.GuardOK, GuardSteps: 5})
	g.onStatus(guestctl.Status{Guard: guestctl.GuardNoBudget, GuardSteps: 6})
	if g.live.Load() != 0 {
		t.Fatal("a stuck or failing guard counted as live")
	}
}

// A measurement failure fails closed: an unreadable VHDX gives budget 0, and an
// unreadable volume reads as 0 bytes free, which pauses.
func TestHostGuardFailsClosed(t *testing.T) {
	dir := testDir(t)
	m := &fakeMachine{}
	disk := &dataDisk{path: filepath.Join(dir, "missing", "data.vhdx"), meta: &DiskMeta{SizeBytes: MinDiskSize, BlockSize: 1 << 20}}
	g, err := newHostGuard(spaceguard.DefaultConfig, m, &guestctl.Supervisor{}, disk, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	g.dir = dir
	g.tick() // the VHDX is missing, the volume is fine: budget 0, no pause
	if p, _, _ := m.counts(); p != 0 {
		t.Fatal("paused although the volume has space")
	}
	g.dir = filepath.Join(dir, "no-such-volume-dir")
	g.tick()
	if p, _, _ := m.counts(); p != 1 {
		t.Fatal("an unreadable volume did not pause")
	}
}

// TestGuestStatsFstrimFailClosed: the trim the guest guard runs before failing closed
// must not log the reservation-filled free space as the guest's fsFree.
func TestGuestStatsFstrimFailClosed(t *testing.T) {
	var buf strings.Builder
	g := &guestStats{log: slog.New(slog.NewTextHandler(&buf, nil))}
	g.onStatus(guestctl.Status{Guard: guestctl.GuardNoBudget})
	g.onStatus(guestctl.Status{Guard: guestctl.GuardNoBudget, FSSize: 64 << 30, FSFree: 16 << 20, Trims: 1, Trimmed: 62 << 30})
	if s := buf.String(); !strings.Contains(s, `msg="guest fstrim"`) || strings.Contains(s, "fsFree") {
		t.Fatalf("fail-closed trim logged as:\n%s", s)
	}
	buf.Reset()
	g.onStatus(guestctl.Status{Guard: guestctl.GuardOK, FSSize: 64 << 30, FSFree: 60 << 30, Trims: 2, Trimmed: 63 << 30})
	if s := buf.String(); !strings.Contains(s, `fsFree="60.0 GiB"`) {
		t.Fatalf("trim under a budget logged as:\n%s", s)
	}
}
