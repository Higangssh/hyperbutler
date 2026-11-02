//go:build windows

package broker

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/guest/image"
	"github.com/Higangssh/hyperbutler/internal/vm"
	"github.com/Higangssh/hyperbutler/internal/vmtemplate"
)

// fakeHCS is an hcsAPI that runs nothing: it records the documents it is given.
type fakeHCS struct {
	mu      sync.Mutex
	rid     guid.GUID
	created map[string]*vm.Config
	systems map[string]*fakeSystem
	list    []vm.Summary
	// wrongRID makes created systems report another runtime id than Probe.
	wrongRID bool
}

func newFakeHCS() *fakeHCS {
	return &fakeHCS{rid: guid.GUID{Data1: 0x11223344, Data2: 0x5566, Data3: 0x7788, Data4: [8]byte{1, 2, 3, 4, 5, 6, 7, 8}},
		created: map[string]*vm.Config{}, systems: map[string]*fakeSystem{}}
}

func (f *fakeHCS) Create(id string, cfg *vm.Config) (hcsSystem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := cfg.Document(); err != nil {
		return nil, err
	}
	f.created[id] = cfg
	rid := f.rid
	if f.wrongRID {
		rid.Data1++
	}
	s := &fakeSystem{rid: rid, state: "Created"}
	f.systems[id] = s
	return s, nil
}

func (f *fakeHCS) Open(id string) (hcsSystem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &fakeSystem{state: "Running"}
	f.systems[id] = s
	return s, nil
}

func (f *fakeHCS) List() ([]vm.Summary, error) { return f.list, nil }

func (f *fakeHCS) Probe(dir, _ string) (guid.GUID, uint32, error) {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return guid.GUID{}, 0, errors.New("probe: no state directory")
	}
	return f.rid, windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE, nil
}

type fakeSystem struct {
	mu         sync.Mutex
	rid        guid.GUID
	state      string
	terminated bool
	closed     bool
}

func (s *fakeSystem) set(st string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = st
	return nil
}
func (s *fakeSystem) Start() error                  { return s.set("Running") }
func (s *fakeSystem) RuntimeID() (guid.GUID, error) { return s.rid, nil }
func (s *fakeSystem) State() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, nil
}
func (s *fakeSystem) Pause() error    { return s.set("Paused") }
func (s *fakeSystem) Resume() error   { return s.set("Running") }
func (s *fakeSystem) ShutDown() error { return errors.New("not supported") }
func (s *fakeSystem) Terminate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.terminated, s.state = true, "Stopped"
	return nil
}
func (s *fakeSystem) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// writeBrokerImage makes an image folder with a valid manifest and a 1 MiB system disk.
func writeBrokerImage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, b []byte) image.File {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		d, err := image.Describe(name, f)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	footer, err := image.EncodeFooter(1<<20, time.Time{}, [16]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	m := &image.Manifest{
		Version: image.ManifestVersion,
		Kernel:  write("vmlinuz-virt", []byte("kernel")),
		Initrd:  write("initramfs-stage1.gz", []byte("initrd")),
		System:  write("system.vhd", append(make([]byte, 1<<20), footer[:]...)),
	}
	b, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, image.ManifestName), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

type hcsFixture struct {
	be      *HCSBackend
	api     *fakeHCS
	sid     string
	dataDir *DataDir
	disk    string
	image   string
}

func newHCSFixture(t *testing.T, allowUnpinned bool) *hcsFixture {
	t.Helper()
	f := &hcsFixture{api: newFakeHCS(), sid: currentSID(t), image: writeBrokerImage(t)}
	dir := goodDir(t)
	f.disk = writeDisk(t, dir)
	f.dataDir = &DataDir{h: dataDirHandle(t, dir)}
	be, err := NewHCSBackend(HCSConfig{StateDir: t.TempDir(), AllowUnpinned: allowUnpinned, Logf: t.Logf, imageDir: f.image, api: f.api})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { be.Close() })
	f.be = be
	return f
}

func (f *hcsFixture) spec() VMSpec {
	return VMSpec{Owner: f.sid, CPUs: 2, MemoryMB: 2048, DataDir: f.dataDir, Share: true, DaemonConfig: true,
		Net: &vmtemplate.Net{Subnet: netip.MustParsePrefix("192.168.127.0/24"), MTU: 9000, Offload: true, HostGateway: true}}
}

func TestHCSBackendStart(t *testing.T) {
	f := newHCSFixture(t, true)
	ctx := context.Background()
	st, err := f.be.Start(ctx, f.spec())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running || st.ID != f.api.rid.ToArray() || st.CPUs != 2 || st.MemoryMB != 2048 {
		t.Fatalf("status %+v", st)
	}
	if len(f.api.created) != 1 {
		t.Fatalf("%d systems created", len(f.api.created))
	}
	var name string
	var cfg *vm.Config
	for name, cfg = range f.api.created {
	}
	if !strings.HasPrefix(name, vmNamePrefix) || cfg.Owner != OwnerTag(f.sid) {
		t.Fatalf("name %q owner %q", name, cfg.Owner)
	}
	// The document is the template's, with the broker's own files and the caller's SID.
	want, err := vmtemplate.Build(&vmtemplate.Spec{
		Owner: OwnerTag(f.sid), UserSID: f.sid, MemoryMB: 2048, CPUs: 2,
		Kernel: cfg.KernelPath, Initrd: cfg.InitrdPath, System: cfg.Disks[1].Path, Data: cfg.Disks[0].Path,
		Net: f.spec().Net, Share: true, DaemonConfig: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := cfg.Document()
	b, _ := want.Document()
	if string(a) != string(b) {
		t.Fatalf("document\n got %s\nwant %s", a, b)
	}
	if cfg.ConsolePipe != "" {
		t.Fatalf("broker VM has a console pipe %q", cfg.ConsolePipe)
	}
	sddl, _ := vm.UserOnlySDDL(f.sid)
	for _, s := range cfg.Services {
		for _, d := range []string{s.ConnectSDDL, s.BindSDDL} {
			if d != "" && d != sddl {
				t.Fatalf("service %s: descriptor %q", s.ID, d)
			}
		}
	}
	for i, p := range []string{cfg.Disks[0].Path, f.disk} {
		fa, err1 := os.Stat(p)
		fb, err2 := os.Stat(f.disk)
		if err1 != nil || err2 != nil || !os.SameFile(fa, fb) {
			t.Fatalf("data disk %d: %q", i, p)
		}
	}
	if !strings.HasPrefix(strings.ToLower(cfg.KernelPath), strings.ToLower(f.image)) && !strings.Contains(cfg.KernelPath, filepath.Base(f.image)) {
		t.Fatalf("kernel %q is not from the image folder %q", cfg.KernelPath, f.image)
	}
	// The VM's account is on the data disk (and the system disk) while it runs.
	account := vm.AccountSID(f.api.rid)
	if got, _ := vm.AccountsOnFile(f.disk); len(got) != 1 || got[0] != account {
		t.Fatalf("accounts on the data disk: %v", got)
	}
	if got, _ := vm.AccountsOnFile(filepath.Join(f.image, "system.vhd")); len(got) != 1 || got[0] != account {
		t.Fatalf("accounts on the system disk: %v", got)
	}
	// The image cannot be changed while the VM runs.
	if err := os.WriteFile(filepath.Join(f.image, "system.vhd"), []byte("x"), 0o600); err == nil {
		t.Fatal("the system disk was rewritten under a running VM")
	}

	// A second start reports the same VM.
	if st2, err := f.be.Start(ctx, f.spec()); err != nil || st2 != st || len(f.api.created) != 1 {
		t.Fatalf("second start: %+v, %v (%d created)", st2, err, len(f.api.created))
	}
	if err := f.be.Pause(ctx, f.sid, false); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.be.Status(ctx, f.sid); !st.Paused {
		t.Fatalf("after pause: %+v", st)
	}
	if err := f.be.Pause(ctx, f.sid, true); err != nil {
		t.Fatal(err)
	}
	if err := f.be.Stop(ctx, f.sid, false); err == nil {
		t.Fatal("an orderly stop through HCS succeeded on a guest without the integration service")
	}
	if err := f.be.Stop(ctx, f.sid, true); err != nil {
		t.Fatal(err)
	}
	if !f.api.systems[name].terminated || !f.api.systems[name].closed {
		t.Fatal("stop did not terminate and close the system")
	}
	if got, _ := vm.AccountsOnFile(f.disk); len(got) != 0 {
		t.Fatalf("accounts left on the data disk: %v", got)
	}
	if got, _ := vm.AccountsOnFile(filepath.Join(f.image, "system.vhd")); len(got) != 0 {
		t.Fatalf("accounts left on the system disk: %v", got)
	}
	if err := f.be.Stop(ctx, f.sid, true); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("second stop: %v", err)
	}
	if err := os.Rename(f.disk, f.disk+".moved"); err != nil {
		t.Fatalf("the data disk is still held after the stop: %v", err)
	}
}

// A VM that stopped on its own is found by status, and its files are released.
func TestHCSBackendStoppedOnItsOwn(t *testing.T) {
	f := newHCSFixture(t, true)
	ctx := context.Background()
	if _, err := f.be.Start(ctx, f.spec()); err != nil {
		t.Fatal(err)
	}
	for _, s := range f.api.systems {
		_ = s.set("Stopped")
	}
	if st, err := f.be.Status(ctx, f.sid); err != nil || st.Running {
		t.Fatalf("status %+v, %v", st, err)
	}
	if got, _ := vm.AccountsOnFile(f.disk); len(got) != 0 {
		t.Fatalf("accounts left: %v", got)
	}
	if err := f.be.Pause(ctx, f.sid, false); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("pause: %v", err)
	}
}

func TestHCSBackendRefusesUnpinned(t *testing.T) {
	f := newHCSFixture(t, false)
	_, err := f.be.Start(context.Background(), f.spec())
	if !errors.Is(err, ErrImageRejected) || len(f.api.created) != 0 {
		t.Fatalf("unpinned start: %v (%d created)", err, len(f.api.created))
	}
}

func TestHCSBackendRejectsImage(t *testing.T) {
	f := newHCSFixture(t, true)
	if err := os.WriteFile(filepath.Join(f.image, "vmlinuz-virt"), []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := f.be.Start(context.Background(), f.spec())
	if !errors.Is(err, ErrImageRejected) || len(f.api.created) != 0 {
		t.Fatalf("tampered image: %v", err)
	}
}

func TestHCSBackendRejectsDisk(t *testing.T) {
	f := newHCSFixture(t, true)
	other := filepath.Join(newDir(t), "link.vhdx")
	if err := os.Link(f.disk, other); err != nil {
		t.Fatal(err)
	}
	_, err := f.be.Start(context.Background(), f.spec())
	if !errors.Is(err, ErrDiskRejected) || len(f.api.created) != 0 {
		t.Fatalf("hard-linked disk: %v", err)
	}
	// Nothing is left held: the image can be replaced again.
	if err := os.WriteFile(filepath.Join(f.image, "vmlinuz-virt"), []byte("kernel"), 0o600); err != nil {
		t.Fatalf("image still held after a refused start: %v", err)
	}
}

func TestHCSBackendRuntimeIDMismatch(t *testing.T) {
	f := newHCSFixture(t, true)
	f.api.wrongRID = true
	if _, err := f.be.Start(context.Background(), f.spec()); err == nil {
		t.Fatal("started a VM whose account is not the one granted")
	}
	for _, s := range f.api.systems {
		if !s.terminated || !s.closed {
			t.Fatal("the VM was left running")
		}
	}
	if got, _ := vm.AccountsOnFile(f.disk); len(got) != 0 {
		t.Fatalf("accounts left: %v", got)
	}
}

func TestHCSBackendNetPort(t *testing.T) {
	f := newHCSFixture(t, true)
	s := f.spec()
	s.Net.Port = 4000
	if _, err := f.be.Start(context.Background(), s); err == nil {
		t.Fatal("started with another network port")
	}
}

// The sweep terminates VMs with the broker's owner tag that it does not run, and
// nothing else.
func TestHCSBackendSweep(t *testing.T) {
	f := newHCSFixture(t, true)
	if _, err := f.be.Start(context.Background(), f.spec()); err != nil {
		t.Fatal(err)
	}
	var mine string
	for mine = range f.api.created {
	}
	f.api.list = []vm.Summary{
		{ID: mine, Owner: OwnerTag(f.sid), State: "Running"},
		{ID: "hyperbutler-broker-0011", Owner: OwnerTag(sidB), State: "Running"},
		{ID: "hyperbutler-0a1b2c3d-112233", Owner: "hyperbutler", State: "Running"},
		{ID: "wsl", Owner: "WSL", State: "Running"},
	}
	if n := f.be.Sweep(); n != 1 {
		t.Fatalf("swept %d", n)
	}
	if s := f.api.systems["hyperbutler-broker-0011"]; s == nil || !s.terminated {
		t.Fatal("the untracked broker VM was not terminated")
	}
	for _, id := range []string{"hyperbutler-0a1b2c3d-112233", "wsl"} {
		if _, opened := f.api.systems[id]; opened {
			t.Fatalf("%s was touched", id)
		}
	}
	if f.api.systems[mine].terminated {
		t.Fatal("the tracked VM was terminated")
	}
}
