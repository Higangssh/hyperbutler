//go:build windows

package sandboxmsi

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.msi")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLockKeepsTheFile is the race itself: from the hash until Close, nobody gets to write,
// delete or rename the file, nor to rename the folder above it, and it stays readable, which
// msiexec needs.
func TestLockKeepsTheFile(t *testing.T) {
	path := writeTemp(t, "msi")
	l, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	sum, err := l.SHA256()
	if err != nil || sum != hashOf([]byte("msi")) {
		t.Fatalf("%s %v", sum, err)
	}
	if _, err := os.OpenFile(path, os.O_WRONLY, 0); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Errorf("open to write: %v", err)
	}
	if err := os.WriteFile(path, []byte("evil"), 0o600); err == nil {
		t.Error("overwrite succeeded")
	}
	if err := os.Remove(path); err == nil {
		t.Error("delete succeeded")
	}
	if err := os.Rename(path, path+".moved"); err == nil {
		t.Error("rename succeeded")
	}
	dir := filepath.Dir(path)
	if err := os.Rename(dir, dir+".moved"); err == nil {
		t.Error("the folder could be renamed")
	}
	// Replace by rename from elsewhere.
	other := writeTemp(t, "evil")
	if err := os.Rename(other, path); err == nil {
		t.Error("a file was renamed over it")
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "msi" {
		t.Errorf("read while locked: %q %v", b, err)
	}
	// The hash again, after all that, is the same, through the same handle.
	if again, err := l.SHA256(); err != nil || again != sum {
		t.Errorf("%s %v", again, err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Errorf("after Close: %v", err)
	}
}

// TestLockFailsWithAWriter: a program that opened the file before the lock keeps nothing:
// the lock fails while its handle is open.
func TestLockFailsWithAWriter(t *testing.T) {
	path := writeTemp(t, "msi")
	w, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if l, err := Lock(path); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		if err == nil {
			_ = l.Close()
		}
		t.Errorf("with a writer open: %v", err)
	}
}

// TestLockFailsWithAWritableMapping: closing the handle does not help a program that keeps
// a writable view of the file, which is the one way to write without a handle.
func TestLockFailsWithAWritableMapping(t *testing.T) {
	path := writeTemp(t, "msi")
	w, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	m, err := windows.CreateFileMapping(windows.Handle(w.Fd()), nil, windows.PAGE_READWRITE, 0, 0, nil)
	if err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(m) }()
	v, err := windows.MapViewOfFile(m, windows.FILE_MAP_WRITE, 0, 0, 0)
	if err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	defer func() { _ = windows.UnmapViewOfFile(v) }()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if l, err := Lock(path); err == nil {
		_ = l.Close()
		t.Error("the lock succeeded with a writable view of the file open")
	}
}

// TestLockResolvesLinks: msiexec gets the real path, so swapping a link does not change what
// it opens, and a file that is a link is refused outright.
func TestLockResolvesLinks(t *testing.T) {
	real := t.TempDir()
	if err := os.WriteFile(filepath.Join(real, "x.msi"), []byte("msi"), 0o600); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "junction")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, real).CombinedOutput(); err != nil {
		t.Skipf("no junction: %v %s", err, out)
	}
	defer func() { _ = os.Remove(link) }()
	l, err := Lock(filepath.Join(link, "x.msi"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if !strings.EqualFold(l.Path(), filepath.Join(real, "x.msi")) {
		t.Errorf("locked path %q, want below %q", l.Path(), real)
	}
	if strings.Contains(strings.ToLower(l.Path()), "junction") {
		t.Errorf("the path still goes through the link: %s", l.Path())
	}
}

func TestLockRefuses(t *testing.T) {
	dir := t.TempDir()
	if l, err := Lock(dir); err == nil {
		_ = l.Close()
		t.Error("a folder was locked")
	}
	if l, err := Lock(filepath.Join(dir, "missing.msi")); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			_ = l.Close()
		}
		t.Errorf("missing: %v", err)
	}
	// A second name: a program holding a link could not write either, but nothing that
	// is not this run makes one, and the check costs nothing.
	p := writeTemp(t, "msi")
	if err := os.Link(p, p+".2"); err != nil {
		t.Skipf("no hard link: %v", err)
	}
	if l, err := Lock(p); err == nil || !strings.Contains(err.Error(), "names") {
		if err == nil {
			_ = l.Close()
		}
		t.Errorf("two names: %v", err)
	}
}

func TestCheckInstallerPath(t *testing.T) {
	fixed := func(string) uint32 { return windows.DRIVE_FIXED }
	remote := func(string) uint32 { return windows.DRIVE_REMOTE }
	removable := func(string) uint32 { return windows.DRIVE_REMOVABLE }
	if err := checkInstallerPath(`C:\Users\a\x.msi`, fixed); err != nil {
		t.Error(err)
	}
	for name, c := range map[string]struct {
		p string
		d func(string) uint32
	}{
		"unc":       {`\\server\share\x.msi`, fixed},
		"remote":    {`Z:\x.msi`, remote},
		"removable": {`E:\x.msi`, removable},
		"long":      {`C:\` + strings.Repeat("a", windows.MAX_PATH), fixed},
		"relative":  {`x.msi`, fixed},
	} {
		if err := checkInstallerPath(c.p, c.d); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestMsiexecArgs(t *testing.T) {
	a, err := msiexecArgs(`C:\Users\a b\x.msi`, false)
	if err != nil || a != `/i "C:\Users\a b\x.msi" /passive /norestart` {
		t.Errorf("%s %v", a, err)
	}
	a, err = msiexecArgs(`C:\x.msi`, true)
	if err != nil || a != `/i "C:\x.msi" /passive /norestart REINSTALL=ALL REINSTALLMODE=amus` {
		t.Errorf("%s %v", a, err)
	}
	// No log on the command line: an elevated process would write it where this user points.
	if strings.Contains(a, "/l") {
		t.Errorf("a log file: %s", a)
	}
	for _, bad := range []string{`C:\x".msi`, "C:\\x\n.msi"} {
		if a, err := msiexecArgs(bad, false); err == nil {
			t.Errorf("%q: %s", bad, a)
		}
	}
}

func TestShellExecuteInfoSize(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip(runtime.GOARCH)
	}
	if n := unsafe.Sizeof(shellExecuteInfo{}); n != 112 {
		t.Errorf("SHELLEXECUTEINFOW is 112 bytes, this struct %d", n)
	}
	if off := unsafe.Offsetof(shellExecuteInfo{}.hProcess); off != 104 {
		t.Errorf("hProcess at %d, want 104", off)
	}
}
