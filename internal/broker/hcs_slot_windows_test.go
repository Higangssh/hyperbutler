//go:build windows

package broker

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/vm"
)

// withSandbox gives the fixture a sandbox image folder and the disk of session slot.
func (f *hcsFixture) withSandbox(t *testing.T, slot uint8) (imageDir, disk string) {
	t.Helper()
	imageDir = writeBrokerImage(t)
	f.be.imageDirs[sandboxImage] = imageDir
	disk = filepath.Join(filepath.Dir(f.disk), SessionDiskName(slot))
	if err := os.WriteFile(disk, testVHDX(false, false), 0o600); err != nil {
		t.Fatal(err)
	}
	me, err := windows.StringToSid(f.sid)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(disk, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, me, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	return imageDir, disk
}

func (f *hcsFixture) sessionSpec(slot uint8) VMSpec {
	spec := f.spec()
	spec.Slot, spec.MemoryMB, spec.Share, spec.DaemonConfig = slot, 1024, false, false
	n := *spec.Net
	n.HostGateway = false
	spec.Net = &n
	return spec
}

func createdFor(t *testing.T, f *hcsFixture, before map[string]bool) (string, *vm.Config) {
	t.Helper()
	for name, cfg := range f.api.created {
		if !before[name] {
			return name, cfg
		}
	}
	t.Fatal("no new compute system")
	return "", nil
}

func names(f *hcsFixture) map[string]bool {
	m := map[string]bool{}
	for n := range f.api.created {
		m[n] = true
	}
	return m
}

// TestHCSBackendSession: a session boots the sandbox image from its own disk, next to
// the Docker VM, and its access entries come and go with it alone (ADR 0009).
func TestHCSBackendSession(t *testing.T) {
	f := newHCSFixture(t, true)
	sbxImage, sbxDisk := f.withSandbox(t, 1)
	ctx := context.Background()

	if _, err := f.be.Start(ctx, f.spec()); err != nil {
		t.Fatal(err)
	}
	dockerName, _ := createdFor(t, f, nil)
	before := names(f)
	st, err := f.be.Start(ctx, f.sessionSpec(1))
	if err != nil {
		t.Fatal(err)
	}
	name, cfg := createdFor(t, f, before)
	if !st.Running || st.MemoryMB != 1024 || cfg.Owner != OwnerTag(f.sid) {
		t.Fatalf("session status %+v, owner %q", st, cfg.Owner)
	}
	if !strings.Contains(cfg.KernelPath, filepath.Base(sbxImage)) || !strings.Contains(cfg.Disks[1].Path, filepath.Base(sbxImage)) {
		t.Fatalf("the session boots %q / %q, not the sandbox image %q", cfg.KernelPath, cfg.Disks[1].Path, sbxImage)
	}
	fa, err1 := os.Stat(cfg.Disks[0].Path)
	fb, err2 := os.Stat(sbxDisk)
	if err1 != nil || err2 != nil || !os.SameFile(fa, fb) {
		t.Fatalf("the session's data disk is %q, want %q", cfg.Disks[0].Path, sbxDisk)
	}
	account := vm.AccountSID(f.api.ridFor(name))
	dockerAccount := vm.AccountSID(f.api.ridFor(dockerName))
	if got, _ := vm.AccountsOnFile(sbxDisk); len(got) != 1 || got[0] != account {
		t.Fatalf("accounts on the session disk: %v", got)
	}
	if got, _ := vm.AccountsOnFile(f.disk); len(got) != 1 || got[0] != dockerAccount {
		t.Fatalf("accounts on the Docker VM's disk: %v", got)
	}
	if got, _ := vm.AccountsOnFile(filepath.Join(sbxImage, "system.vhd")); len(got) != 1 || got[0] != account {
		t.Fatalf("accounts on the sandbox system disk: %v", got)
	}
	if got, _ := vm.AccountsOnFile(filepath.Join(f.image, "system.vhd")); len(got) != 1 || got[0] != dockerAccount {
		t.Fatalf("accounts on the Docker system disk: %v", got)
	}

	// Stopping the session leaves the Docker VM and its entries alone.
	if err := f.be.Stop(ctx, VMKey{Owner: f.sid, Slot: 1}, true); err != nil {
		t.Fatal(err)
	}
	if st, err := f.be.Status(ctx, VMKey{Owner: f.sid}); err != nil || !st.Running {
		t.Fatalf("the Docker VM after the session stopped: %+v, %v", st, err)
	}
	if got, _ := vm.AccountsOnFile(sbxDisk); len(got) != 0 {
		t.Fatalf("accounts left on the session disk: %v", got)
	}
	if got, _ := vm.AccountsOnFile(filepath.Join(sbxImage, "system.vhd")); len(got) != 0 {
		t.Fatalf("accounts left on the sandbox system disk: %v", got)
	}
	if got, _ := vm.AccountsOnFile(f.disk); len(got) != 1 || got[0] != dockerAccount {
		t.Fatalf("the Docker VM's disk lost its entry: %v", got)
	}
}

// TestHCSBackendSessionRefusals: the backend itself refuses what a session must not get,
// a session without its disk, and slots out of range, creating nothing.
func TestHCSBackendSessionRefusals(t *testing.T) {
	f := newHCSFixture(t, true)
	f.withSandbox(t, 1)
	ctx := context.Background()
	for name, mut := range map[string]func(*VMSpec){
		"share":        func(s *VMSpec) { s.Share = true },
		"daemon":       func(s *VMSpec) { s.DaemonConfig = true },
		"host gateway": func(s *VMSpec) { n := *s.Net; n.HostGateway = true; s.Net = &n },
		"slot 5":       func(s *VMSpec) { s.Slot = 5 },
	} {
		spec := f.sessionSpec(1)
		mut(&spec)
		if _, err := f.be.Start(ctx, spec); err == nil {
			t.Errorf("%s: started", name)
		}
	}
	// Slot 2 has no session-2.vhdx; it never falls back to another disk.
	if _, err := f.be.Start(ctx, f.sessionSpec(2)); !errors.Is(err, ErrDiskRejected) || !strings.Contains(err.Error(), SessionDiskName(2)) {
		t.Fatalf("slot 2 without its disk: %v", err)
	}
	if len(f.api.created) != 0 {
		t.Fatalf("%d systems created", len(f.api.created))
	}
}

// TestHCSBackendSessionNeedsItsPin: a broker that refuses unpinned images boots no
// session without the sandbox pin, even with a sandbox image in place.
func TestHCSBackendSessionNeedsItsPin(t *testing.T) {
	f := newHCSFixture(t, false)
	f.withSandbox(t, 1)
	_, err := f.be.Start(context.Background(), f.sessionSpec(1))
	if !errors.Is(err, ErrImageRejected) || !strings.Contains(err.Error(), "sandbox image pin") {
		t.Fatalf("unpinned session: %v", err)
	}
	if len(f.api.created) != 0 {
		t.Fatal("a VM was created")
	}
}

// TestHCSBackendSessionImageNotShared: a sandbox image whose system disk is the Docker
// image's own file is refused, so the two images' access bookkeeping never revokes the
// other's VM.
func TestHCSBackendSessionImageNotShared(t *testing.T) {
	f := newHCSFixture(t, true)
	f.withSandbox(t, 1)
	f.be.imageDirs[sandboxImage] = f.image
	ctx := context.Background()
	if _, err := f.be.Start(ctx, f.spec()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.be.Start(ctx, f.sessionSpec(1)); !errors.Is(err, ErrImageRejected) {
		t.Fatalf("a session on the Docker image's system disk: %v", err)
	}
	if got, _ := vm.AccountsOnFile(filepath.Join(f.image, "system.vhd")); len(got) != 1 {
		t.Fatalf("accounts on the system disk after the refused session: %v", got)
	}
}

// The owner rewrites the disk into a differencing disk after the broker checked it and
// before HCS opens it by path: the check after the start finds it and ends the VM.
func TestHCSBackendDiskRewrittenBeforeOpen(t *testing.T) {
	f := newHCSFixture(t, true)
	f.api.onCreate = func(cfg *vm.Config) {
		b, err := os.ReadFile(f.disk)
		if err != nil {
			t.Error(err)
			return
		}
		meta := 320 << 10
		binary.LittleEndian.PutUint32(b[meta+4100:], vhdxHasParent)
		w, err := os.OpenFile(f.disk, os.O_WRONLY, 0)
		if err != nil {
			t.Errorf("the owner could not open the disk to write while the broker held it: %v", err)
			return
		}
		defer w.Close()
		if _, err := w.WriteAt(b[meta+4100:meta+4104], int64(meta+4100)); err != nil {
			t.Error(err)
		}
	}
	_, err := f.be.Start(context.Background(), f.spec())
	if !errors.Is(err, ErrDiskRejected) || !strings.Contains(err.Error(), "after the VM opened it") {
		t.Fatalf("start with a disk rewritten before HCS opened it: %v", err)
	}
	for _, s := range f.api.systems {
		if !s.terminated {
			t.Fatal("the VM was left running")
		}
	}
	if st, _ := f.be.Status(context.Background(), VMKey{Owner: f.sid}); st.Running {
		t.Fatal("status reports a VM")
	}
}
