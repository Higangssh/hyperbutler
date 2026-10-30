package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/unix"
)

// notify watches dir and its subdirectories with inotify (no polling) and prints one line
// per event as soon as it arrives: {"name":"event","path":...,"mask":...}. It prints
// {"name":"ready"} once every watch is in place.
func notify(dir string) error {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		return err
	}
	const mask = unix.IN_MODIFY | unix.IN_CLOSE_WRITE | unix.IN_CREATE | unix.IN_DELETE |
		unix.IN_MOVED_FROM | unix.IN_MOVED_TO | unix.IN_ATTRIB
	dirs := map[int]string{}
	add := func(p string) error {
		wd, err := unix.InotifyAddWatch(fd, p, mask)
		if err != nil {
			return fmt.Errorf("watch %s: %w", p, err)
		}
		dirs[wd] = p
		return nil
	}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return add(p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	emit("ready", map[string]any{"watches": len(dirs)})
	buf := make([]byte, 64<<10)
	for {
		n, err := unix.Read(fd, buf)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
			name := ""
			if ev.Len > 0 {
				b := buf[off+unix.SizeofInotifyEvent : off+unix.SizeofInotifyEvent+int(ev.Len)]
				for i, c := range b {
					if c == 0 {
						b = b[:i]
						break
					}
				}
				name = string(b)
			}
			p := filepath.Join(dirs[int(ev.Wd)], name)
			if ev.Mask&unix.IN_CREATE != 0 && ev.Mask&unix.IN_ISDIR != 0 {
				add(p)
			}
			emit("event", map[string]any{"path": p, "mask": ev.Mask})
			off += unix.SizeofInotifyEvent + int(ev.Len)
		}
		os.Stdout.Sync()
	}
}
