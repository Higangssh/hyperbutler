//go:build linux

package relay

import (
	"errors"
	"path"

	"golang.org/x/sys/unix"
)

// replay performs one host change on the mount so inotify watchers see it. Every path
// is resolved with openat2 beneath the mount point and without following symlinks: a
// container may have replaced a directory with a symlink since the host saw the
// change, and guest root must not then act outside the mount.
func (r *Relay) replay(b []byte) error {
	f, ok := parseInject(b)
	if !ok {
		return errors.New("bad frame")
	}
	if _, err := safeJoin(r.cfg.Mountpoint, f.path); err != nil {
		return err
	}
	dfd, leaf, err := r.parentAt(f.path)
	if err != nil {
		return err
	}
	defer unix.Close(dfd)
	switch f.kind {
	case injModify:
		// O_WRONLY open + ftruncate to the current size + close: IN_OPEN, IN_MODIFY,
		// IN_CLOSE_WRITE, without changing a byte (the host ignores these requests).
		fd, err := unix.Openat(dfd, leaf, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		var st unix.Stat_t
		if unix.Fstat(fd, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFREG {
			unix.Ftruncate(fd, st.Size)
		}
		return unix.Close(fd)
	case injCreate:
		switch f.mode & unix.S_IFMT {
		case unix.S_IFDIR:
			return unix.Mkdirat(dfd, leaf, 0o755)
		case unix.S_IFLNK:
			return unix.Symlinkat("hyperbutler-replay", dfd, leaf)
		case unix.S_IFIFO, unix.S_IFSOCK:
			return unix.Mknodat(dfd, leaf, f.mode&unix.S_IFMT|0o644, 0)
		default:
			return unix.Mknodat(dfd, leaf, unix.S_IFREG|0o644, 0)
		}
	case injDelete:
		err := unix.Unlinkat(dfd, leaf, 0)
		if err == unix.EISDIR {
			err = unix.Unlinkat(dfd, leaf, unix.AT_REMOVEDIR)
		}
		return err
	case injRename:
		if _, err := safeJoin(r.cfg.Mountpoint, f.newPath); err != nil {
			return err
		}
		nfd, nleaf, err := r.parentAt(f.newPath)
		if err != nil {
			return err
		}
		defer unix.Close(nfd)
		return unix.Renameat(dfd, leaf, nfd, nleaf)
	}
	return errors.New("unknown replay kind")
}

// parentAt opens the directory holding p (relative to the mount) beneath the mount.
func (r *Relay) parentAt(p string) (int, string, error) {
	root, err := unix.Open(r.cfg.Mountpoint, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", err
	}
	defer unix.Close(root)
	dir, leaf := path.Split(p)
	if dir == "" {
		dir = "."
	}
	fd, err := unix.Openat2(root, dir, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV,
	})
	if err != nil {
		return -1, "", err
	}
	return fd, leaf, nil
}
