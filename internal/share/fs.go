// Package share serves Windows directories to the winbox guest as bind-mount sources
// (ADR 0007, REQ-FS-001..004).
//
// The guest kernel's FUSE client talks to this package over one hvsock connection: the
// guest relay (package share/guest) copies /dev/fuse frames to the connection and back,
// unchanged, so the host decodes the kernel's FUSE requests itself, the way virtio-fs
// does over a virtqueue. Everything the guest sends is untrusted.
//
// The FUSE root is a virtual directory whose entries are the registered shares. Each
// share is one host directory; the guest cannot name anything outside it:
//   - names are single components, checked in checkName and mapped in names_windows.go;
//   - every host open is relative to the share's root handle with OBJ_DONT_REPARSE, so
//     no junction, symlink or mount point is followed on the host (symlinks are shown
//     to the guest as symlinks and resolved by the guest kernel, inside the guest);
//   - the server holds no handle to anything but share roots and files the guest has
//     open, so Windows programs can rename and delete freely.
//
// Host changes reach the guest as FUSE invalidations, and as replayed operations so
// that inotify watchers in containers see them (inject.go).
package share

import (
	"errors"
	"io"
)

// FS is one shared host directory. Paths are relative to the share root, as checked
// Linux name components; an empty slice is the root. Implementations must never follow
// a link on the host.
type FS interface {
	// Lstat returns the attributes of the entry itself.
	Lstat(path []string) (Attr, error)
	// ReadDir lists a directory with each entry's attributes.
	ReadDir(path []string) ([]DirEntry, error)
	// Open opens an existing file; flags are Linux open flags.
	Open(path []string, flags uint32) (File, error)
	// Create creates and opens a file.
	Create(path []string, flags, mode uint32, o Owner) (File, Attr, error)
	Mkdir(path []string, mode uint32, o Owner) (Attr, error)
	// Mknod creates a FIFO or socket placeholder.
	Mknod(path []string, mode uint32, o Owner) (Attr, error)
	Symlink(path []string, target string, o Owner) (Attr, error)
	Readlink(path []string) (string, error)
	Link(oldPath, newPath []string) (Attr, error)
	Unlink(path []string) error
	Rmdir(path []string) error
	Rename(oldPath, newPath []string, noReplace bool) error
	// Setattr changes what s.Valid says; f is the open file when the kernel sent one.
	Setattr(path []string, f File, s SetAttr) (Attr, error)
	Statfs() (Statfs, error)
	Close() error
}

// File is an open host file.
type File interface {
	io.ReaderAt
	io.WriterAt
	Sync() error
	Close() error
}

// Owner is the guest user that creates a file.
type Owner struct{ UID, GID uint32 }

// NoID marks an Attr UID or GID the host has no value for; the server fills in
// Options.UID and Options.GID.
const NoID = ^uint32(0)

// DirEntry is one directory entry.
type DirEntry struct {
	Name string
	Attr Attr
}

// SetAttr is a decoded SETATTR.
type SetAttr struct {
	Valid              uint32
	Size               uint64
	Mode, UID, GID     uint32
	Atime, Mtime       int64 // nanoseconds since the Unix epoch; -1 means "now"
	SetAtime, SetMtime bool
	SetSize, SetMode   bool
	SetOwner           bool
}

// Statfs is struct fuse_kstatfs.
type Statfs struct {
	Blocks, Bfree, Bavail, Files, Ffree uint64
	Bsize, NameLen, Frsize              uint32
}

// Errno is a Linux error number returned to the guest.
type Errno int

func (e Errno) Error() string { return "errno " + itoa(int(e)) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// Linux errno values (asm-generic).
const (
	EPERM        Errno = 1
	ENOENT       Errno = 2
	EIO          Errno = 5
	EBADF        Errno = 9
	EAGAIN       Errno = 11
	ENOMEM       Errno = 12
	EACCES       Errno = 13
	EBUSY        Errno = 16
	EEXIST       Errno = 17
	EXDEV        Errno = 18
	ENOTDIR      Errno = 20
	EISDIR       Errno = 21
	EINVAL       Errno = 22
	ENFILE       Errno = 23
	EMFILE       Errno = 24
	EFBIG        Errno = 27
	ENOSPC       Errno = 28
	EROFS        Errno = 30
	EMLINK       Errno = 31
	ERANGE       Errno = 34
	ENAMETOOLONG Errno = 36
	ENOSYS       Errno = 38
	ENOTEMPTY    Errno = 39
	ELOOP        Errno = 40
	ENODATA      Errno = 61
	EOPNOTSUPP   Errno = 95
)

// toErrno maps an error to the errno the guest gets. Anything unexpected is EIO.
func toErrno(err error) int {
	var e Errno
	if errors.As(err, &e) {
		return int(e)
	}
	return int(EIO)
}
