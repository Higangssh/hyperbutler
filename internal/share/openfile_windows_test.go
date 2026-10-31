//go:build windows

package share

// Opens without round trips against real NTFS: what Windows programs can still do to
// files the guest has (or had) open, O_TRUNC, and POSIX unlink of open files.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// openExclusive opens path for read and write sharing nothing, as some Windows
// programs (editors, installers) do.
func openExclusive(path string) (windows.Handle, error) {
	p, _ := windows.UTF16PtrFromString(path)
	return windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
}

// classicDelete deletes path without POSIX semantics: the name stays (delete pending)
// until every handle to the file is closed.
func classicDelete(path string) error {
	p, _ := windows.UTF16PtrFromString(path)
	h, err := windows.CreateFile(p, windows.DELETE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	del := byte(1)
	return windows.SetFileInformationByHandle(h, windows.FileDispositionInfo, &del, 1)
}

func eventually(t *testing.T, within time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %v", what, within)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Windows programs can edit, rename and delete a file the guest is reading or writing
// (cached handle open), and get it exclusively once the handle is idle.
func TestNoOpenWindowsKeepsAccess(t *testing.T) {
	dir := testDir(t)
	p := filepath.Join(dir, "f.txt")
	os.WriteFile(p, []byte("hello from windows"), 0o644)
	idle := 100 * time.Millisecond
	s, c, root := newTestServer(t, dir, Options{NoWatch: true, FileIdle: idle}, nil)
	en := c.mustLookup(root, "f.txt")
	if e, b := c.read(en.node, 0, 0, 100); e != 0 || string(b) != "hello from windows" {
		t.Fatalf("read: %d %q", e, b)
	}
	if e := c.write(en.node, 0, 0, []byte("HELLO")); e != 0 {
		t.Fatalf("write: %d", e)
	}
	if n, _ := s.CachedFiles(); n != 1 {
		t.Fatalf("%d cached", n)
	}
	// With the write handle cached: Windows reads, writes, renames and deletes.
	if b, err := os.ReadFile(p); err != nil || string(b) != "HELLO from windows" {
		t.Fatalf("Windows read: %v %q", err, b)
	}
	if err := os.WriteFile(p, []byte("edited on windows"), 0o644); err != nil {
		t.Fatalf("Windows write: %v", err)
	}
	if e, b := c.read(en.node, 0, 0, 100); e != 0 || string(b) != "edited on windows" {
		t.Fatalf("guest read after the Windows edit: %d %q", e, b)
	}
	q := filepath.Join(dir, "renamed.txt")
	if err := os.Rename(p, q); err != nil {
		t.Fatalf("Windows rename: %v", err)
	}
	if err := os.Rename(q, p); err != nil {
		t.Fatalf("Windows rename back: %v", err)
	}
	// Exclusive access waits for the idle handle (at most FileIdle and a sweep).
	eventually(t, 4*idle+time.Second, "exclusive open", func() bool {
		h, err := openExclusive(p)
		if err == nil {
			windows.CloseHandle(h)
		}
		return err == nil
	})
	if n, _ := s.CachedFiles(); n != 0 {
		t.Fatalf("%d cached after idle", n)
	}
	// Reading again caches a read-only handle: Windows programs that share only reads
	// can still open the file.
	c.read(en.node, 0, 0, 100)
	sp, _ := windows.UTF16PtrFromString(p)
	h, err := windows.CreateFile(sp, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatalf("share-read open next to a cached read handle: %v", err)
	}
	windows.CloseHandle(h)
	// A POSIX delete (os.Remove) works at once, and the name can be reused at once.
	if err := os.Remove(p); err != nil {
		t.Fatalf("Windows delete: %v", err)
	}
	if err := os.WriteFile(p, []byte("new"), 0o644); err != nil {
		t.Fatalf("Windows re-create after delete: %v", err)
	}
}

// A classic delete stays pending while any handle is open: the cached handle must
// close soon, with no help from the watcher.
func TestNoOpenClassicDeleteCompletes(t *testing.T) {
	dir := testDir(t)
	p := filepath.Join(dir, "f.txt")
	os.WriteFile(p, []byte("x"), 0o644)
	idle := 100 * time.Millisecond
	_, c, root := newTestServer(t, dir, Options{NoWatch: true, FileIdle: idle}, nil)
	en := c.mustLookup(root, "f.txt")
	c.read(en.node, 0, 0, 10)
	if err := classicDelete(p); err != nil {
		t.Fatalf("classic delete: %v", err)
	}
	eventually(t, 4*idle+time.Second, "name free after a classic delete", func() bool {
		_, err := os.Lstat(p)
		return errors.Is(err, os.ErrNotExist)
	})
	if err := os.WriteFile(p, []byte("again"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// With the watcher on, a Windows delete closes the cached handle at once.
func TestNoOpenHostDeleteClosesHandle(t *testing.T) {
	dir := testDir(t)
	p := filepath.Join(dir, "f.txt")
	os.WriteFile(p, []byte("x"), 0o644)
	s, c, root := newTestServer(t, dir, Options{FileIdle: time.Hour}, nil)
	en := c.mustLookup(root, "f.txt")
	c.read(en.node, 0, 0, 10)
	time.Sleep(100 * time.Millisecond) // the watcher's first ReadDirectoryChangesW
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "handle closed on a host delete", func() bool {
		n, _ := s.CachedFiles()
		return n == 0
	})
}

func TestNoOpenTruncAndAppend(t *testing.T) {
	dir := testDir(t)
	p := filepath.Join(dir, "f.txt")
	os.WriteFile(p, []byte("hello world"), 0o644)
	_, c, root := newTestServer(t, dir, Options{NoWatch: true}, nil)
	en := c.mustLookup(root, "f.txt")
	c.read(en.node, 0, 0, 100) // a read handle is cached
	// open(O_WRONLY|O_TRUNC): the kernel sends a path SETATTR (no FUSE_ATOMIC_O_TRUNC).
	if e, _ := c.open(en.node, oWRONLY|oTRUNC); e != 0 {
		t.Fatalf("open O_TRUNC: %d", e)
	}
	if b, _ := os.ReadFile(p); len(b) != 0 {
		t.Fatalf("not truncated: %q", b)
	}
	if e, b := c.read(en.node, 0, 0, 100); e != 0 || len(b) != 0 {
		t.Fatalf("cached handle sees %q", b)
	}
	if e := c.write(en.node, 0, 0, []byte("abc")); e != 0 {
		t.Fatal(e)
	}
	// O_APPEND: the kernel writes at its i_size, as it did with OPEN.
	os.WriteFile(p+".x", nil, 0o644)
	if e := c.write(en.node, 0, 3, []byte("def")); e != 0 {
		t.Fatal(e)
	}
	if b, _ := os.ReadFile(p); string(b) != "abcdef" {
		t.Fatalf("host has %q", b)
	}
}

// The guest unlinks (or renames over) a file a process is using: the name is gone on
// Windows at once (its directory can be removed), and the open file keeps working
// through its pinned handle until the kernel forgets it.
func TestNoOpenUnlinkWhileOpen(t *testing.T) {
	dir := testDir(t)
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	p := filepath.Join(dir, "sub", "f.txt")
	os.WriteFile(p, []byte("old data"), 0o644)
	s, c, root := newTestServer(t, dir, Options{NoWatch: true, FileIdle: time.Millisecond}, nil)
	sub := c.mustLookup(root, "sub")
	en := c.mustLookup(sub.node, "f.txt")
	if e := c.write(en.node, 0, 0, []byte("o")); e != 0 { // in use: a cached handle
		t.Fatal(e)
	}
	if e, _ := c.do(opUnlink, sub.node, (&wbuf{}).str("f.txt").b); e != 0 {
		t.Fatalf("unlink: %d", e)
	}
	if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("name still there on Windows: %v", err)
	}
	if n, pins := s.CachedFiles(); n != 1 || pins != 1 {
		t.Fatalf("cached %d pinned %d", n, pins)
	}
	time.Sleep(20 * time.Millisecond) // longer than FileIdle: pins stay
	if e, b := c.read(en.node, 0, 0, 100); e != 0 || string(b) != "old data" {
		t.Fatalf("read of the unlinked open file: %d %q", e, b)
	}
	if e := c.write(en.node, 0, 0, []byte("OLD")); e != 0 {
		t.Fatalf("write to the unlinked open file: %d", e)
	}
	if err := os.Remove(filepath.Join(dir, "sub")); err != nil {
		t.Fatalf("Windows rmdir of the parent: %v", err)
	}
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	if err := os.WriteFile(p, []byte("new"), 0o644); err != nil {
		t.Fatalf("re-create: %v", err)
	}
	c.do(opForget, en.node, (&wbuf{}).u64(1).b)
	if n, pins := s.CachedFiles(); n != 0 || pins != 0 {
		t.Fatalf("after FORGET: cached %d pinned %d", n, pins)
	}

	// Rename over a file a process is reading.
	os.WriteFile(filepath.Join(dir, "a"), []byte("replacement"), 0o644)
	os.WriteFile(filepath.Join(dir, "b"), []byte("original"), 0o644)
	c.mustLookup(root, "a")
	b := c.mustLookup(root, "b")
	c.read(b.node, 0, 0, 1)
	body := (&wbuf{}).u64(root).u32(0).u32(0).str("a").str("b").b
	if e, _ := c.do(opRename2, root, body); e != 0 {
		t.Fatalf("rename: %d", e)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "b")); string(got) != "replacement" {
		t.Fatalf("host b is %q", got)
	}
	time.Sleep(20 * time.Millisecond)
	if e, got := c.read(b.node, 0, 0, 100); e != 0 || string(got) != "original" {
		t.Fatalf("replaced open file reads %d %q", e, got)
	}
	if err := os.Remove(filepath.Join(dir, "b")); err != nil {
		t.Fatalf("Windows delete of the new b: %v", err)
	}

	// A file idle past FileIdle has no handle to keep: ESTALE, never another file.
	os.WriteFile(filepath.Join(dir, "c"), []byte("c"), 0o644)
	cn := c.mustLookup(root, "c")
	c.read(cn.node, 0, 0, 1)
	eventually(t, 5*time.Second, "idle close", func() bool { n, _ := s.CachedFiles(); return n <= 1 })
	c.do(opUnlink, root, (&wbuf{}).str("c").b)
	os.WriteFile(filepath.Join(dir, "c"), []byte("other"), 0o644)
	if e, _ := c.read(cn.node, 0, 0, 10); e != int(ESTALE) {
		t.Fatalf("read of an idle unlinked file: %d", e)
	}
}

// A read-only host file (FILE_ATTRIBUTE_READONLY) in use can be removed by the guest
// and still read.
func TestNoOpenUnlinkReadOnlyFile(t *testing.T) {
	dir := testDir(t)
	p := filepath.Join(dir, "ro.txt")
	os.WriteFile(p, []byte("ro"), 0o444)
	_, c, root := newTestServer(t, dir, Options{NoWatch: true}, nil)
	en := c.mustLookup(root, "ro.txt")
	c.read(en.node, 0, 0, 1)
	if e, _ := c.do(opUnlink, root, (&wbuf{}).str("ro.txt").b); e != 0 {
		t.Fatalf("unlink: %d", e)
	}
	if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("still there: %v", err)
	}
	if e, b := c.read(en.node, 0, 0, 10); e != 0 || string(b) != "ro" {
		t.Fatalf("read: %d %q", e, b)
	}
}
