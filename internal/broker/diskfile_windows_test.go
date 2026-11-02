//go:build windows

package broker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/vm"
)

// dataDirHandle opens dir as the broker does after set-data-location.
func dataDirHandle(t *testing.T, dir string) windows.Handle {
	t.Helper()
	h, err := reopenDirectory(openDir(t, dir, true, 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { windows.CloseHandle(h) })
	return h
}

func writeDisk(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, DataDiskName)
	if err := os.WriteFile(p, []byte("vhdx"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func wantDiskReject(t *testing.T, err error, reason string) {
	t.Helper()
	if !errors.Is(err, ErrDiskRejected) || !strings.Contains(err.Error(), reason) {
		t.Fatalf("err = %v, want ErrDiskRejected with %q", err, reason)
	}
}

func TestDataDiskAccepted(t *testing.T) {
	dir := goodDir(t)
	p := writeDisk(t, dir)
	d, err := openDataDisk(dataDirHandle(t, dir), DataDiskName, currentSID(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	a, _ := os.Stat(p)
	b, err := os.Stat(d.Path())
	if err != nil || !os.SameFile(a, b) {
		t.Fatalf("final path %q is not the disk: %v", d.Path(), err)
	}
	// Held without FILE_SHARE_DELETE: the disk cannot be renamed or deleted under HCS,
	// but can still be opened for reading and writing (as HCS does).
	if err := os.Rename(p, p+".moved"); err == nil {
		t.Fatal("the disk was renamed while held")
	}
	if err := os.Remove(p); err == nil {
		t.Fatal("the disk was deleted while held")
	}
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("read-write open while held: %v", err)
	}
	f.Close()
	d.Close()
	if err := os.Rename(p, p+".moved"); err != nil {
		t.Fatalf("rename after close: %v", err)
	}
}

func TestDataDiskWrongOwner(t *testing.T) {
	dir := goodDir(t)
	writeDisk(t, dir)
	_, err := openDataDisk(dataDirHandle(t, dir), DataDiskName, sidB)
	wantDiskReject(t, err, "not owned by the caller")
}

func TestDataDiskMissing(t *testing.T) {
	dir := goodDir(t)
	_, err := openDataDisk(dataDirHandle(t, dir), DataDiskName, currentSID(t))
	wantDiskReject(t, err, "open")
}

func TestDataDiskDirectory(t *testing.T) {
	dir := goodDir(t)
	if err := os.Mkdir(filepath.Join(dir, DataDiskName), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := openDataDisk(dataDirHandle(t, dir), DataDiskName, currentSID(t))
	if !errors.Is(err, ErrDiskRejected) {
		t.Fatalf("a directory named %s: %v", DataDiskName, err)
	}
}

// A junction named data.vhdx is opened as itself (not followed) and refused.
func TestDataDiskJunction(t *testing.T) {
	dir := goodDir(t)
	target := goodDir(t)
	writeDisk(t, target)
	mklinkJ(t, filepath.Join(dir, DataDiskName), target)
	_, err := openDataDisk(dataDirHandle(t, dir), DataDiskName, currentSID(t))
	if !errors.Is(err, ErrDiskRejected) {
		t.Fatalf("junction: %v", err)
	}
}

// A file symbolic link to a disk elsewhere is a reparse point and refused.
func TestDataDiskSymlink(t *testing.T) {
	dir := goodDir(t)
	target := writeDisk(t, goodDir(t))
	if err := os.Symlink(target, filepath.Join(dir, DataDiskName)); err != nil {
		t.Skipf("symlinks need Developer Mode or the privilege: %v", err)
	}
	_, err := openDataDisk(dataDirHandle(t, dir), DataDiskName, currentSID(t))
	wantDiskReject(t, err, "reparse point")
}

// A hard link gives the file a second name, possibly in a directory the caller does not
// own; a disk with two links is refused, from either name.
func TestDataDiskHardLink(t *testing.T) {
	dir := goodDir(t)
	p := writeDisk(t, dir)
	other := filepath.Join(newDir(t), "other.vhdx")
	if err := os.Link(p, other); err != nil {
		t.Fatal(err)
	}
	_, err := openDataDisk(dataDirHandle(t, dir), DataDiskName, currentSID(t))
	wantDiskReject(t, err, "has 2 links")
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}
	d, err := openDataDisk(dataDirHandle(t, dir), DataDiskName, currentSID(t))
	if err != nil {
		t.Fatalf("after the second link is gone: %v", err)
	}
	d.Close()
}

func TestDataDiskName(t *testing.T) {
	dir := goodDir(t)
	h := dataDirHandle(t, dir)
	for _, name := range []string{"", ".", "..", `sub\data.vhdx`, "sub/data.vhdx", "data.vhdx:stream", `..\data.vhdx`} {
		if _, err := openDataDisk(h, name, currentSID(t)); !errors.Is(err, ErrDiskRejected) {
			t.Errorf("name %q: %v", name, err)
		}
	}
	if _, err := openDataDisk(0, DataDiskName, currentSID(t)); !errors.Is(err, ErrDiskRejected) {
		t.Errorf("no directory: %v", err)
	}
}

func TestCheckDiskInfo(t *testing.T) {
	me, err := windows.StringToSid(sidA)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := windows.StringToSid(sidB)
	good := func() *diskInfo {
		return &diskInfo{attrs: windows.FILE_ATTRIBUTE_ARCHIVE, links: 1, fileType: windows.FILE_TYPE_DISK, owner: me, deviceType: fileDeviceDisk}
	}
	if err := checkDiskInfo(good(), me); err != nil {
		t.Fatal(err)
	}
	for reason, mut := range map[string]func(*diskInfo){
		"reparse point":     func(i *diskInfo) { i.attrs |= windows.FILE_ATTRIBUTE_REPARSE_POINT },
		"is a directory":    func(i *diskInfo) { i.directory = true },
		"not a regular":     func(i *diskInfo) { i.fileType = windows.FILE_TYPE_PIPE },
		"device":            func(i *diskInfo) { i.attrs |= windows.FILE_ATTRIBUTE_DEVICE },
		"local disk":        func(i *diskInfo) { i.remote = true },
		"local disk (type)": func(i *diskInfo) { i.deviceType = 0x14 },
		"links":             func(i *diskInfo) { i.links = 2 },
		"no links":          func(i *diskInfo) { i.links = 0 },
		"owned":             func(i *diskInfo) { i.owner = other },
		"no owner":          func(i *diskInfo) { i.owner = nil },
	} {
		i := good()
		mut(i)
		if err := checkDiskInfo(i, me); err == nil {
			t.Errorf("%s: accepted", reason)
		}
	}
}

// The VM account's access is set and removed on the handle, and entries of VM accounts
// a crashed run left behind are removed with it.
func TestDataDiskGrantByHandle(t *testing.T) {
	dir := goodDir(t)
	p := writeDisk(t, dir)
	d, err := openDataDisk(dataDirHandle(t, dir), DataDiskName, currentSID(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	const stale = vm.AccountPrefix + "1-2-3-4"
	const account = vm.AccountPrefix + "11-22-33-44"
	if err := d.grant(stale, windows.FILE_GENERIC_READ); err != nil {
		t.Fatal(err)
	}
	const mask = windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE
	if err := d.grant(account, mask); err != nil {
		t.Fatal(err)
	}
	got, err := vm.AccountsOnFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != account {
		t.Fatalf("VM accounts on the disk: %v, want only %s", got, account)
	}
	if err := d.grant(currentSID(t), mask); err == nil {
		t.Fatal("granted a user account")
	}
	if err := d.revoke(account); err != nil {
		t.Fatal(err)
	}
	if got, _ := vm.AccountsOnFile(p); len(got) != 0 {
		t.Fatalf("after revoke: %v", got)
	}
	// The owner still has its access.
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("owner lost access: %v", err)
	}
	f.Close()
}
