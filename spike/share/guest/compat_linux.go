package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

func unsafePointer(b *byte) unsafe.Pointer { return unsafe.Pointer(b) }

func errStr(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

// compat runs the REQ-FS-004 behaviour checks inside dir and reports what happened.
func compat(dir string) (any, error) {
	d := filepath.Join(dir, "compat")
	os.RemoveAll(d)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return nil, err
	}
	r := map[string]string{}
	p := func(n string) string { return filepath.Join(d, n) }

	// Case sensitivity.
	os.WriteFile(p("Case.txt"), []byte("upper"), 0o644)
	_, err := os.Stat(p("case.txt"))
	r["case: stat other case"] = errStr(err)
	err = os.WriteFile(p("CASE.TXT"), []byte("second"), 0o644)
	b, _ := os.ReadFile(p("Case.txt"))
	r["case: create other case"] = errStr(err) + ", original now " + fmt.Sprintf("%q", b)
	es, _ := os.ReadDir(d)
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	r["case: readdir"] = strings.Join(names, ",")

	// Symlinks.
	err = os.Symlink("Case.txt", p("link"))
	r["symlink: create relative"] = errStr(err)
	t, err := os.Readlink(p("link"))
	r["symlink: readlink"] = t + " " + errStr(err)
	b, err = os.ReadFile(p("link"))
	r["symlink: read through"] = fmt.Sprintf("%q %s", b, errStr(err))
	err = os.Symlink("/etc/hostname", p("abslink"))
	r["symlink: create absolute"] = errStr(err)
	t, err = os.Readlink(p("abslink"))
	r["symlink: readlink absolute"] = t + " " + errStr(err)
	err = os.Symlink("missing", p("dangling"))
	r["symlink: dangling"] = errStr(err)

	// Permissions and ownership.
	os.WriteFile(p("script.sh"), []byte("#!/bin/sh\necho hi\n"), 0o644)
	err = os.Chmod(p("script.sh"), 0o755)
	st, _ := os.Lstat(p("script.sh"))
	r["chmod 755"] = errStr(err) + " mode now " + st.Mode().String()
	err = os.Chmod(p("script.sh"), 0o600)
	st, _ = os.Lstat(p("script.sh"))
	r["chmod 600"] = errStr(err) + " mode now " + st.Mode().String()
	err = os.Lchown(p("script.sh"), 1000, 1000)
	var s unix.Stat_t
	unix.Lstat(p("script.sh"), &s)
	r["chown 1000:1000"] = errStr(err) + fmt.Sprintf(" uid=%d gid=%d", s.Uid, s.Gid)
	unix.Lstat(d, &s)
	r["dir mode"] = fmt.Sprintf("%o uid=%d gid=%d", s.Mode, s.Uid, s.Gid)

	// Names.
	for _, n := range []string{"with space.txt", "유니코드-ünï.txt", "emoji-😀.txt", "colon:name", "back\\slash", "trailing.", "q?mark", "CON", "aux.js"} {
		err := os.WriteFile(p(n), []byte("x"), 0o644)
		if err == nil {
			_, err = os.Stat(p(n))
		}
		r["name "+fmt.Sprintf("%q", n)] = errStr(err)
	}
	long := d
	for i := 0; i < 30; i++ {
		long = filepath.Join(long, "abcdefghij")
	}
	err = os.MkdirAll(long, 0o755)
	if err == nil {
		err = os.WriteFile(filepath.Join(long, "deep.txt"), []byte("deep"), 0o644)
	}
	r[fmt.Sprintf("long path (%d chars)", len(long)-len(d))] = errStr(err)

	// Hard links, rename, unlink while open, fifo.
	err = os.Link(p("Case.txt"), p("hard"))
	unix.Lstat(p("hard"), &s)
	r["hard link"] = errStr(err) + fmt.Sprintf(" nlink=%d", s.Nlink)
	os.WriteFile(p("ren-a"), []byte("a"), 0o644)
	os.WriteFile(p("ren-b"), []byte("b"), 0o644)
	err = os.Rename(p("ren-a"), p("ren-b"))
	b, _ = os.ReadFile(p("ren-b"))
	r["rename over existing"] = errStr(err) + fmt.Sprintf(" content %q", b)
	f, _ := os.Create(p("open-unlink"))
	f.WriteString("still here")
	err = os.Remove(p("open-unlink"))
	buf := make([]byte, 32)
	n, rerr := f.ReadAt(buf, 0)
	f.Close()
	r["unlink while open"] = errStr(err) + fmt.Sprintf(", read after: %q %v", buf[:n], rerr)
	err = unix.Mkfifo(p("fifo"), 0o644)
	r["mkfifo"] = errStr(err)
	err = os.Mkdir(p("..dots"), 0o755)
	r["name ..dots"] = errStr(err)
	err = unix.Setxattr(p("Case.txt"), "user.test", []byte("v"), 0)
	r["setxattr"] = errStr(err)
	var sf unix.Statfs_t
	err = unix.Statfs(d, &sf)
	r["statfs"] = errStr(err) + fmt.Sprintf(" type=%x bsize=%d blocks=%d", sf.Type, sf.Bsize, sf.Blocks)
	return r, nil
}
