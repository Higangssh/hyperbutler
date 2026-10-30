//go:build windows

package share

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// NTFS serves one host directory. Every open is relative to the root handle, carries
// OBJ_DONT_REPARSE (no reparse point is followed anywhere in the path) and
// FILE_OPEN_REPARSE_POINT (the last component is opened as itself). When a path crosses
// a reparse point that is not a link (a OneDrive placeholder directory, say), the
// one-shot open fails and openAt walks it one component at a time, refusing links.
//
// POSIX metadata (mode, owner) lives in the same extended attributes WSL uses ($LXMOD,
// $LXUID, $LXGID), and symlinks, FIFOs and sockets are WSL's reparse tags, so the two
// see each other's files the same way.
type NTFS struct {
	root     windows.Handle
	rootPath string // final path of the root, for statfs and link targets
}

var (
	modntdll                     = windows.NewLazySystemDLL("ntdll.dll")
	procNtQueryInformationByName = modntdll.NewProc("NtQueryInformationByName")
	procNtSetEaFile              = modntdll.NewProc("NtSetEaFile")
)

// NT constants not in x/sys/windows.
const (
	fileStatLxInformation        = 70
	fileBasicInformation         = 4
	fileEndOfFileInformation     = 20
	fileDispositionInformation   = 13
	fileDispositionInformationEx = 64
	fileRenameInformationEx      = 65
	fileRenameInformation        = 10
	fileLinkInformationEx        = 72
	fileLinkInformation          = 11

	dispositionDelete         = 0x1
	dispositionPosix          = 0x2
	dispositionIgnoreReadonly = 0x10
	renameReplace             = 0x1
	renamePosix               = 0x2
	renameIgnoreReadonly      = 0x40
	linkPosix                 = 0x2

	lxHasUID  = 0x1
	lxHasGID  = 0x2
	lxHasMode = 0x4

	tagSymlink    = 0xA000000C
	tagMountPoint = 0xA0000003
	tagLxSymlink  = 0xA000001D
	tagAfUnix     = 0x80000023
	tagLxFifo     = 0x80000024
	tagLxChr      = 0x80000025
	tagLxBlk      = 0x80000026
	tagSurrogate  = 0x20000000

	fsctlSetReparsePoint = 0x000900A4
	fsctlGetReparsePoint = 0x000900A8

	fileAttributeTagInfo = 9 // FILE_INFO_BY_HANDLE_CLASS
	fileAddFile          = 0x2
	fileAddSubdir        = 0x4

	// maxDirEntries bounds one directory listing held for the guest.
	maxDirEntries = 1 << 20
	// maxNTPath is the longest relative name NtCreateFile takes (UNICODE_STRING).
	maxNTPath = 32000
)

// OpenNTFS opens dir for sharing. dir itself may be reached through links (it is what
// the user asked for); nothing below it is.
func OpenNTFS(dir string) (*NTFS, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.FILE_TRAVERSE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, fmt.Errorf("share: open %s: %w", dir, err)
	}
	var bi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &bi); err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	if bi.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("share: %s is not a directory", dir)
	}
	buf := make([]uint16, 1024)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
	if err != nil || int(n) >= len(buf) {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("share: final path of %s: %v", dir, err)
	}
	return &NTFS{root: h, rootPath: windows.UTF16ToString(buf[:n])}, nil
}

// Close releases the root handle.
func (f *NTFS) Close() error { return windows.CloseHandle(f.root) }

// RootPath is the final path of the share root.
func (f *NTFS) RootPath() string { return f.rootPath }

func ntJoin(path []string) (string, error) {
	var b strings.Builder
	for i, c := range path {
		n, err := toNT(c)
		if err != nil {
			return "", err
		}
		if i > 0 {
			b.WriteByte('\\')
		}
		b.WriteString(n)
	}
	if b.Len() > maxNTPath {
		return "", ENAMETOOLONG
	}
	return b.String(), nil
}

// ntErr maps an NTSTATUS or Win32 error to the errno the guest sees.
func ntErr(err error) error {
	if err == nil {
		return nil
	}
	var st windows.NTStatus
	if errors.As(err, &st) {
		switch st {
		case windows.STATUS_OBJECT_NAME_NOT_FOUND, windows.STATUS_OBJECT_PATH_NOT_FOUND, windows.STATUS_DELETE_PENDING,
			windows.STATUS_NO_SUCH_FILE, windows.STATUS_FILE_DELETED:
			return ENOENT
		case windows.STATUS_OBJECT_NAME_COLLISION:
			return EEXIST
		case windows.STATUS_ACCESS_DENIED, windows.STATUS_PRIVILEGE_NOT_HELD:
			return EACCES
		case windows.STATUS_CANNOT_DELETE:
			return EPERM
		case windows.STATUS_SHARING_VIOLATION, windows.STATUS_FILE_LOCK_CONFLICT, windows.STATUS_LOCK_NOT_GRANTED:
			return EBUSY
		case windows.STATUS_DIRECTORY_NOT_EMPTY:
			return ENOTEMPTY
		case windows.STATUS_NOT_A_DIRECTORY:
			return ENOTDIR
		case windows.STATUS_FILE_IS_A_DIRECTORY:
			return EISDIR
		case windows.STATUS_DISK_FULL, windows.STATUS_QUOTA_EXCEEDED:
			return ENOSPC
		case windows.STATUS_OBJECT_NAME_INVALID, windows.STATUS_OBJECT_PATH_SYNTAX_BAD, windows.STATUS_INVALID_PARAMETER:
			return EINVAL
		case windows.STATUS_NAME_TOO_LONG:
			return ENAMETOOLONG
		case windows.STATUS_REPARSE_POINT_ENCOUNTERED, windows.STATUS_STOPPED_ON_SYMLINK, windows.STATUS_IO_REPARSE_TAG_NOT_HANDLED:
			return ELOOP
		case windows.STATUS_NOT_SAME_DEVICE:
			return EXDEV
		case windows.STATUS_TOO_MANY_LINKS:
			return EMLINK
		case windows.STATUS_MEDIA_WRITE_PROTECTED:
			return EROFS
		case windows.STATUS_EAS_NOT_SUPPORTED, windows.STATUS_NOT_SUPPORTED, windows.STATUS_INVALID_DEVICE_REQUEST:
			return EOPNOTSUPP
		case windows.STATUS_INSUFFICIENT_RESOURCES, windows.STATUS_NO_MEMORY:
			return ENOMEM
		}
		return EIO
	}
	var en syscall.Errno
	if errors.As(err, &en) {
		switch en {
		case windows.ERROR_FILE_NOT_FOUND, windows.ERROR_PATH_NOT_FOUND:
			return ENOENT
		case windows.ERROR_ALREADY_EXISTS, windows.ERROR_FILE_EXISTS:
			return EEXIST
		case windows.ERROR_ACCESS_DENIED:
			return EACCES
		case windows.ERROR_SHARING_VIOLATION, windows.ERROR_LOCK_VIOLATION:
			return EBUSY
		case windows.ERROR_DIR_NOT_EMPTY:
			return ENOTEMPTY
		case windows.ERROR_DISK_FULL, windows.ERROR_HANDLE_DISK_FULL:
			return ENOSPC
		case windows.ERROR_NOT_SAME_DEVICE:
			return EXDEV
		case windows.ERROR_INVALID_NAME, windows.ERROR_INVALID_PARAMETER:
			return EINVAL
		case windows.ERROR_NOT_SUPPORTED, windows.ERROR_INVALID_FUNCTION:
			return EOPNOTSUPP
		case windows.ERROR_NOT_A_REPARSE_POINT:
			return EINVAL
		}
		return EIO
	}
	var e Errno
	if errors.As(err, &e) {
		return e
	}
	return EIO
}

const shareAll = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

// ntCreate is one NtCreateFile relative to dir.
func ntCreate(dir windows.Handle, name string, access, disp, opts, objAttrs uint32, ea []byte) (windows.Handle, error) {
	us, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, EINVAL
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: dir, ObjectName: us, Attributes: objAttrs}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	var eaPtr uintptr
	if len(ea) > 0 {
		eaPtr = uintptr(unsafe.Pointer(&ea[0]))
	}
	err = windows.NtCreateFile(&h, access|windows.SYNCHRONIZE, &oa, &iosb, nil, windows.FILE_ATTRIBUTE_NORMAL, shareAll, disp,
		opts|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, eaPtr, uint32(len(ea)))
	if err != nil {
		return 0, err
	}
	return h, nil
}

func reparseTag(h windows.Handle) (attrs, tag uint32, err error) {
	var info [2]uint32
	if err := windows.GetFileInformationByHandleEx(h, fileAttributeTagInfo, (*byte)(unsafe.Pointer(&info[0])), 8); err != nil {
		return 0, 0, err
	}
	return info[0], info[1], nil
}

// openAt opens path below the root without following any link.
func (f *NTFS) openAt(path []string, access, disp, opts uint32, ea []byte) (windows.Handle, error) {
	name, err := ntJoin(path)
	if err != nil {
		return 0, err
	}
	h, err := ntCreate(f.root, name, access, disp, opts, windows.OBJ_CASE_INSENSITIVE|windows.OBJ_DONT_REPARSE, ea)
	if err == nil {
		return h, nil
	}
	if !errors.Is(err, windows.STATUS_REPARSE_POINT_ENCOUNTERED) || len(path) == 0 {
		return 0, ntErr(err)
	}
	return f.walkAt(path, access, disp, opts, ea)
}

// walkAt opens path one component at a time. Every component is opened as itself
// (FILE_OPEN_REPARSE_POINT) by a single name relative to its verified parent, so no
// link is ever followed: a directory component that is a link (a name surrogate) is
// refused, any other reparse point (cloud placeholder, dedup) is entered as the
// directory it is.
func (f *NTFS) walkAt(path []string, access, disp, opts uint32, ea []byte) (windows.Handle, error) {
	dir := f.root
	closeDir := func() {
		if dir != f.root {
			windows.CloseHandle(dir)
		}
	}
	defer closeDir()
	for _, c := range path[:len(path)-1] {
		n, err := toNT(c)
		if err != nil {
			return 0, err
		}
		h, err := ntCreate(dir, n, windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE, windows.OBJ_CASE_INSENSITIVE, nil)
		if err != nil {
			return 0, ntErr(err)
		}
		attrs, tag, err := reparseTag(h)
		if err != nil || (attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 && tag&tagSurrogate != 0) {
			windows.CloseHandle(h)
			return 0, ENOTDIR
		}
		closeDir()
		dir = h
	}
	n, err := toNT(path[len(path)-1])
	if err != nil {
		return 0, err
	}
	h, err := ntCreate(dir, n, access, disp, opts, windows.OBJ_CASE_INSENSITIVE, ea)
	if err != nil {
		return 0, ntErr(err)
	}
	return h, nil
}

// filetime converts NT time (100 ns since 1601) to Unix seconds and nanoseconds.
func filetime(t int64) (uint64, uint32) {
	const epoch = 116444736000000000
	if t < epoch {
		return 0, 0
	}
	ns := (t - epoch) * 100
	return uint64(ns / 1e9), uint32(ns % 1e9)
}

func toFiletime(unixNs int64) int64 {
	const epoch = 116444736000000000
	return unixNs/100 + epoch
}

// typeOf picks the Linux file type for NTFS attributes and a reparse tag.
func typeOf(attrs, tag uint32) uint32 {
	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		switch tag {
		case tagLxFifo:
			return sIFIFO
		case tagAfUnix:
			return sIFSOCK
		case tagLxChr, tagLxBlk:
			// Device nodes are shown as empty regular files: a host file must never
			// become a device in the guest.
			return sIFREG
		}
		if tag&tagSurrogate != 0 {
			return sIFLNK
		}
	}
	if attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return sIFDIR
	}
	return sIFREG
}

func defaultPerm(typ, attrs uint32) uint32 {
	perm := uint32(0o777)
	if typ == sIFREG && attrs&windows.FILE_ATTRIBUTE_READONLY != 0 {
		perm &^= 0o222
	}
	return perm
}

// statLx decodes FILE_STAT_LX_INFORMATION.
func statLx(b []byte) Attr {
	le := binary.LittleEndian
	attrs, tag := le.Uint32(b[56:]), le.Uint32(b[60:])
	typ := typeOf(attrs, tag)
	a := Attr{
		Ino:    le.Uint64(b[0:]),
		Size:   le.Uint64(b[48:]),
		Blocks: le.Uint64(b[40:]) / 512,
		Nlink:  le.Uint32(b[64:]),
		UID:    NoID,
		GID:    NoID,
	}
	a.Atime, a.AtimeNsec = filetime(int64(le.Uint64(b[16:])))
	a.Mtime, a.MtimeNsec = filetime(int64(le.Uint64(b[24:])))
	a.Ctime, a.CtimeNsec = filetime(int64(le.Uint64(b[32:])))
	flags := le.Uint32(b[72:])
	perm := defaultPerm(typ, attrs)
	if flags&lxHasMode != 0 {
		perm = le.Uint32(b[84:]) & 0o7777
	}
	if flags&lxHasUID != 0 {
		a.UID = le.Uint32(b[76:])
	}
	if flags&lxHasGID != 0 {
		a.GID = le.Uint32(b[80:])
	}
	if typ != sIFREG && typ != sIFDIR {
		a.Size = 0
	}
	if typ == sIFREG && attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 && (tag == tagLxChr || tag == tagLxBlk) {
		a.Size = 0
	}
	a.Mode = typ | perm
	return a
}

const statLxLen = 96

func statHandle(h windows.Handle) (Attr, error) {
	var b [statLxLen]byte
	var iosb windows.IO_STATUS_BLOCK
	if err := windows.NtQueryInformationFile(h, &iosb, &b[0], statLxLen, fileStatLxInformation); err != nil {
		return Attr{}, ntErr(err)
	}
	return statLx(b[:]), nil
}

// Lstat queries by name in one call when it can (no handle is opened, so no AV scan or
// oplock break), and falls back to a handle.
func (f *NTFS) Lstat(path []string) (Attr, error) {
	name, err := ntJoin(path)
	if err != nil {
		return Attr{}, err
	}
	if procNtQueryInformationByName.Find() == nil {
		us, err := windows.NewNTUnicodeString(name)
		if err != nil {
			return Attr{}, EINVAL
		}
		oa := windows.OBJECT_ATTRIBUTES{RootDirectory: f.root, ObjectName: us, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE}
		oa.Length = uint32(unsafe.Sizeof(oa))
		var b [statLxLen]byte
		var iosb windows.IO_STATUS_BLOCK
		r, _, _ := procNtQueryInformationByName.Call(uintptr(unsafe.Pointer(&oa)), uintptr(unsafe.Pointer(&iosb)), uintptr(unsafe.Pointer(&b[0])), statLxLen, fileStatLxInformation)
		if r == 0 {
			return statLx(b[:]), nil
		}
		st := windows.NTStatus(r)
		if st != windows.STATUS_REPARSE_POINT_ENCOUNTERED && st != windows.STATUS_INVALID_INFO_CLASS && st != windows.STATUS_NOT_IMPLEMENTED && st != windows.STATUS_INVALID_PARAMETER {
			return Attr{}, ntErr(st)
		}
	}
	h, err := f.openAt(path, windows.FILE_READ_ATTRIBUTES|windows.FILE_READ_EA, windows.FILE_OPEN, 0, nil)
	if err != nil {
		return Attr{}, err
	}
	defer windows.CloseHandle(h)
	return statHandle(h)
}

// ReadDir lists a directory with FileIdBothDirectoryInfo. Entries with extended
// attributes (possible POSIX metadata) are stat'ed one by one.
func (f *NTFS) ReadDir(path []string) ([]DirEntry, error) {
	h, err := f.openAt(path, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE, nil)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h)
	if _, tag, err := reparseTag(h); err == nil && tag&tagSurrogate != 0 {
		return nil, ENOTDIR
	}
	buf := make([]byte, 64<<10)
	class := uint32(windows.FileIdBothDirectoryRestartInfo)
	var out []DirEntry
	le := binary.LittleEndian
	for {
		err := windows.GetFileInformationByHandleEx(h, class, &buf[0], uint32(len(buf)))
		class = windows.FileIdBothDirectoryInfo
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return out, nil
		}
		if err != nil {
			return nil, ntErr(err)
		}
		for off := 0; ; {
			e := buf[off:]
			if len(e) < 104 {
				return nil, EIO
			}
			nameLen := int(le.Uint32(e[60:]))
			if 104+nameLen > len(e) {
				return nil, EIO
			}
			u := make([]uint16, nameLen/2)
			for i := range u {
				u[i] = le.Uint16(e[104+2*i:])
			}
			ntName := string(utf16.Decode(u))
			if ntName != "." && ntName != ".." {
				attrs := le.Uint32(e[56:])
				eaOrTag := le.Uint32(e[64:])
				var tag uint32
				if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
					tag = eaOrTag
				}
				typ := typeOf(attrs, tag)
				a := Attr{
					Ino:    le.Uint64(e[96:]),
					Size:   le.Uint64(e[40:]),
					Blocks: le.Uint64(e[48:]) / 512,
					Nlink:  1,
					UID:    NoID,
					GID:    NoID,
					Mode:   typ | defaultPerm(typ, attrs),
				}
				a.Atime, a.AtimeNsec = filetime(int64(le.Uint64(e[16:])))
				a.Mtime, a.MtimeNsec = filetime(int64(le.Uint64(e[24:])))
				a.Ctime, a.CtimeNsec = filetime(int64(le.Uint64(e[32:])))
				if typ != sIFREG && typ != sIFDIR {
					a.Size = 0
				}
				name := fromNT(ntName)
				if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 && eaOrTag != 0 {
					if full, err := f.Lstat(append(append([]string(nil), path...), name)); err == nil {
						a = full
					}
				}
				if checkName(name) == nil {
					out = append(out, DirEntry{Name: name, Attr: a})
					if len(out) > maxDirEntries {
						return nil, ENOMEM
					}
				}
			}
			next := int(le.Uint32(e[0:]))
			if next == 0 {
				break
			}
			off += next
			if off >= len(buf) {
				return nil, EIO
			}
		}
	}
}

// ntFile is an open host file. The handle is synchronous; positional I/O passes the
// offset in an OVERLAPPED.
type ntFile struct {
	h      windows.Handle
	access uint32
}

func (n *ntFile) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	ov := windows.Overlapped{Offset: uint32(off), OffsetHigh: uint32(off >> 32)}
	var done uint32
	err := windows.ReadFile(n.h, p, &done, &ov)
	if errors.Is(err, windows.ERROR_HANDLE_EOF) || (err == nil && done == 0) {
		return int(done), io.EOF
	}
	if err != nil {
		return int(done), ntErr(err)
	}
	return int(done), nil
}

func (n *ntFile) WriteAt(p []byte, off int64) (int, error) {
	ov := windows.Overlapped{Offset: uint32(off), OffsetHigh: uint32(off >> 32)}
	var done uint32
	if err := windows.WriteFile(n.h, p, &done, &ov); err != nil {
		return int(done), ntErr(err)
	}
	return int(done), nil
}

func (n *ntFile) Sync() error  { return ntErr(windows.FlushFileBuffers(n.h)) }
func (n *ntFile) Close() error { return ntErr(windows.CloseHandle(n.h)) }

func accessFor(flags uint32) uint32 {
	a := uint32(windows.FILE_READ_ATTRIBUTES | windows.FILE_READ_EA)
	switch flags & oACCMODE {
	case oRDONLY:
		a |= windows.FILE_GENERIC_READ
	case oWRONLY:
		a |= windows.FILE_GENERIC_WRITE
	default:
		a |= windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE
	}
	return a
}

// Open opens an existing file.
func (f *NTFS) Open(path []string, flags uint32) (File, error) {
	disp := uint32(windows.FILE_OPEN)
	if flags&oTRUNC != 0 && flags&oACCMODE != oRDONLY {
		disp = windows.FILE_OVERWRITE
	}
	acc := accessFor(flags)
	h, err := f.openAt(path, acc, disp, windows.FILE_NON_DIRECTORY_FILE, nil)
	if err != nil {
		return nil, err
	}
	return &ntFile{h: h, access: acc}, nil
}

// lxEA builds a FILE_FULL_EA_INFORMATION list with WSL's metadata attributes.
func lxEA(mode uint32, o *Owner) []byte {
	var b []byte
	add := func(name string, v uint32, last bool) {
		start := len(b)
		entry := make([]byte, 8+len(name)+1+4)
		entry[5] = byte(len(name))
		binary.LittleEndian.PutUint16(entry[6:], 4)
		copy(entry[8:], name)
		binary.LittleEndian.PutUint32(entry[8+len(name)+1:], v)
		b = append(b, entry...)
		if !last {
			for len(b)%4 != 0 {
				b = append(b, 0)
			}
			binary.LittleEndian.PutUint32(b[start:], uint32(len(b)-start))
		}
	}
	if o != nil {
		add("$LXUID", o.UID, false)
		add("$LXGID", o.GID, false)
	}
	add("$LXMOD", mode, true)
	return b
}

func lxOwnerEA(uid, gid uint32) []byte {
	var b []byte
	for i, kv := range []struct {
		n string
		v uint32
	}{{"$LXUID", uid}, {"$LXGID", gid}} {
		start := len(b)
		entry := make([]byte, 8+len(kv.n)+1+4)
		entry[5] = byte(len(kv.n))
		binary.LittleEndian.PutUint16(entry[6:], 4)
		copy(entry[8:], kv.n)
		binary.LittleEndian.PutUint32(entry[8+len(kv.n)+1:], kv.v)
		b = append(b, entry...)
		if i == 0 {
			for len(b)%4 != 0 {
				b = append(b, 0)
			}
			binary.LittleEndian.PutUint32(b[start:], uint32(len(b)-start))
		}
	}
	return b
}

func setEA(h windows.Handle, ea []byte) error {
	var iosb windows.IO_STATUS_BLOCK
	r, _, _ := procNtSetEaFile.Call(uintptr(h), uintptr(unsafe.Pointer(&iosb)), uintptr(unsafe.Pointer(&ea[0])), uintptr(len(ea)))
	if r != 0 {
		return ntErr(windows.NTStatus(r))
	}
	return nil
}

// Create creates (or, without O_EXCL, opens) a regular file.
func (f *NTFS) Create(path []string, flags, mode uint32, o Owner) (File, Attr, error) {
	var disp uint32
	switch {
	case flags&oEXCL != 0:
		disp = windows.FILE_CREATE
	case flags&oTRUNC != 0:
		disp = windows.FILE_OVERWRITE_IF
	default:
		disp = windows.FILE_OPEN_IF
	}
	acc := accessFor(flags)
	h, err := f.openAt(path, acc, disp, windows.FILE_NON_DIRECTORY_FILE, lxEA(sIFREG|mode&0o7777, &o))
	if err != nil {
		return nil, Attr{}, err
	}
	a, err := statHandle(h)
	if err != nil {
		windows.CloseHandle(h)
		return nil, Attr{}, err
	}
	return &ntFile{h: h, access: acc}, a, nil
}

// Mkdir creates a directory.
func (f *NTFS) Mkdir(path []string, mode uint32, o Owner) (Attr, error) {
	h, err := f.openAt(path, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.FILE_READ_EA, windows.FILE_CREATE, windows.FILE_DIRECTORY_FILE, lxEA(sIFDIR|mode&0o7777, &o))
	if err != nil {
		return Attr{}, err
	}
	defer windows.CloseHandle(h)
	return statHandle(h)
}

func setReparse(h windows.Handle, tag uint32, data []byte) error {
	b := make([]byte, 8+len(data))
	binary.LittleEndian.PutUint32(b[0:], tag)
	binary.LittleEndian.PutUint16(b[4:], uint16(len(data)))
	copy(b[8:], data)
	var n uint32
	return windows.DeviceIoControl(h, fsctlSetReparsePoint, &b[0], uint32(len(b)), nil, 0, &n, nil)
}

// special creates an empty file and makes it a WSL-style reparse point.
func (f *NTFS) special(path []string, mode uint32, o Owner, tag uint32, data []byte) (Attr, error) {
	h, err := f.openAt(path, windows.FILE_GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES|windows.FILE_READ_EA|windows.DELETE, windows.FILE_CREATE, windows.FILE_NON_DIRECTORY_FILE, lxEA(mode, &o))
	if err != nil {
		return Attr{}, err
	}
	defer windows.CloseHandle(h)
	if err := setReparse(h, tag, data); err != nil {
		deleteHandle(h)
		return Attr{}, ntErr(err)
	}
	return statHandle(h)
}

// Mknod creates a FIFO or a socket placeholder (the socket itself lives in the guest).
func (f *NTFS) Mknod(path []string, mode uint32, o Owner) (Attr, error) {
	switch mode & sIFMT {
	case sIFIFO:
		return f.special(path, mode, o, tagLxFifo, nil)
	case sIFSOCK:
		return f.special(path, mode, o, tagAfUnix, nil)
	}
	return Attr{}, EPERM
}

// Symlink creates a WSL symlink (IO_REPARSE_TAG_LX_SYMLINK). It needs no privilege,
// unlike an NTFS symlink, and the host never follows it.
func (f *NTFS) Symlink(path []string, target string, o Owner) (Attr, error) {
	if len(target) == 0 || len(target) > maxSymlinkLen || strings.IndexByte(target, 0) >= 0 {
		return Attr{}, EINVAL
	}
	data := make([]byte, 4+len(target))
	binary.LittleEndian.PutUint32(data, 2)
	copy(data[4:], target)
	a, err := f.special(path, sIFLNK|0o777, o, tagLxSymlink, data)
	if err == nil {
		a.Size = uint64(len(target))
	}
	return a, err
}

// Readlink reads a WSL symlink, an NTFS symlink or a junction. NTFS links are shown
// with '/' separators; an absolute target inside the share becomes relative, any other
// absolute target is shown as is and dangles in the guest.
func (f *NTFS) Readlink(path []string) (string, error) {
	h, err := f.openAt(path, windows.FILE_READ_ATTRIBUTES, windows.FILE_OPEN, 0, nil)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]byte, 16<<10)
	var n uint32
	if err := windows.DeviceIoControl(h, fsctlGetReparsePoint, nil, 0, &buf[0], uint32(len(buf)), &n, nil); err != nil {
		return "", ntErr(err)
	}
	return parseReparse(buf[:n], path, f.rootPath)
}

// parseReparse decodes a reparse buffer into a Linux symlink target. linkPath is the
// share-relative path of the link and root the share root's final path.
func parseReparse(b []byte, linkPath []string, root string) (string, error) {
	if len(b) < 8 {
		return "", EIO
	}
	le := binary.LittleEndian
	tag := le.Uint32(b)
	dlen := int(le.Uint16(b[4:]))
	if 8+dlen > len(b) {
		return "", EIO
	}
	d := b[8 : 8+dlen]
	switch tag {
	case tagLxSymlink:
		if len(d) < 4 || len(d) == 4 || len(d)-4 > maxSymlinkLen {
			return "", EIO
		}
		return string(d[4:]), nil
	case tagSymlink, tagMountPoint:
		hdr := 8
		if tag == tagSymlink {
			hdr = 12
		}
		if len(d) < hdr {
			return "", EIO
		}
		subOff, subLen := int(le.Uint16(d[0:])), int(le.Uint16(d[2:]))
		prOff, prLen := int(le.Uint16(d[4:])), int(le.Uint16(d[6:]))
		relative := tag == tagSymlink && le.Uint32(d[8:])&1 != 0
		pb := d[hdr:]
		get := func(off, n int) (string, bool) {
			if off%2 != 0 || n%2 != 0 || off+n > len(pb) {
				return "", false
			}
			u := make([]uint16, n/2)
			for i := range u {
				u[i] = le.Uint16(pb[off+2*i:])
			}
			return string(utf16.Decode(u)), true
		}
		target, ok := get(prOff, prLen)
		if !ok || target == "" {
			if target, ok = get(subOff, subLen); !ok {
				return "", EIO
			}
		}
		if target == "" || strings.IndexByte(target, 0) >= 0 || len(target) > maxSymlinkLen {
			// A crafted buffer; a NUL would silently truncate the guest's link
			// (found by FuzzReparse).
			return "", EIO
		}
		if relative {
			return slashes(target), nil
		}
		target = strings.TrimPrefix(target, `\??\`)
		if rel, ok := insideRoot(target, root); ok {
			return relLink(linkPath, rel), nil
		}
		return slashes(target), nil
	}
	return "", EINVAL
}

func slashes(win string) string {
	parts := strings.Split(win, `\`)
	for i, p := range parts {
		parts[i] = fromNT(p)
	}
	return strings.Join(parts, "/")
}

// insideRoot reports whether the absolute Windows path target is the share root or
// below it, and returns its components relative to the root.
func insideRoot(target, root string) ([]string, bool) {
	r := strings.TrimPrefix(root, `\\?\`)
	t := strings.TrimPrefix(target, `\\?\`)
	if len(t) < len(r) || !strings.EqualFold(t[:len(r)], r) {
		return nil, false
	}
	rest := t[len(r):]
	if rest != "" && rest[0] != '\\' {
		return nil, false
	}
	var out []string
	for _, p := range strings.Split(rest, `\`) {
		if p == "" || p == "." {
			continue
		}
		if p == ".." || strings.ContainsAny(p, "/\x00") {
			// Not an NTFS name; a crafted target (found by FuzzStatAndRoot).
			return nil, false
		}
		out = append(out, fromNT(p))
	}
	return out, true
}

// relLink is the relative path from the directory holding link to target, both
// share-relative.
func relLink(link, target []string) string {
	dir := link[:len(link)-1]
	common := 0
	for common < len(dir) && common < len(target) && strings.EqualFold(dir[common], target[common]) {
		common++
	}
	var parts []string
	for i := common; i < len(dir); i++ {
		parts = append(parts, "..")
	}
	parts = append(parts, target[common:]...)
	if len(parts) == 0 {
		return "."
	}
	return strings.Join(parts, "/")
}

func (f *NTFS) openParent(path []string) (windows.Handle, string, error) {
	var dir windows.Handle
	var err error
	if len(path) == 1 {
		dir, err = f.openAt(nil, windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|fileAddFile|fileAddSubdir, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE, nil)
	} else {
		dir, err = f.openAt(path[:len(path)-1], windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|fileAddFile|fileAddSubdir, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE, nil)
	}
	if err != nil {
		return 0, "", err
	}
	if _, tag, err := reparseTag(dir); err != nil || tag&tagSurrogate != 0 {
		windows.CloseHandle(dir)
		return 0, "", ENOTDIR
	}
	name, err := toNT(path[len(path)-1])
	if err != nil {
		windows.CloseHandle(dir)
		return 0, "", err
	}
	return dir, name, nil
}

// renameBuf builds FILE_RENAME_INFORMATION / FILE_LINK_INFORMATION (same layout):
// flags (or the ReplaceIfExists byte), RootDirectory, FileNameLength, FileName.
func renameBuf(flags uint32, dir windows.Handle, name string) []byte {
	u := utf16.Encode([]rune(name))
	b := make([]byte, 20+2*len(u)+2)
	binary.LittleEndian.PutUint32(b[0:], flags)
	binary.LittleEndian.PutUint64(b[8:], uint64(dir))
	binary.LittleEndian.PutUint32(b[16:], uint32(2*len(u)))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[20+2*i:], c)
	}
	return b
}

// Link makes newPath a hard link to oldPath. The new name is resolved by a single
// component relative to its verified parent directory.
func (f *NTFS) Link(oldPath, newPath []string) (Attr, error) {
	h, err := f.openAt(oldPath, windows.FILE_READ_ATTRIBUTES|windows.FILE_READ_EA|windows.FILE_WRITE_ATTRIBUTES, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE, nil)
	if err != nil {
		return Attr{}, err
	}
	defer windows.CloseHandle(h)
	dir, name, err := f.openParent(newPath)
	if err != nil {
		return Attr{}, err
	}
	defer windows.CloseHandle(dir)
	var iosb windows.IO_STATUS_BLOCK
	b := renameBuf(linkPosix, dir, name)
	err = windows.NtSetInformationFile(h, &iosb, &b[0], uint32(len(b)), fileLinkInformationEx)
	if errors.Is(err, windows.STATUS_INVALID_INFO_CLASS) || errors.Is(err, windows.STATUS_NOT_SUPPORTED) {
		b = renameBuf(0, dir, name)
		err = windows.NtSetInformationFile(h, &iosb, &b[0], uint32(len(b)), fileLinkInformation)
	}
	if err != nil {
		return Attr{}, ntErr(err)
	}
	return statHandle(h)
}

func deleteHandle(h windows.Handle) error {
	var iosb windows.IO_STATUS_BLOCK
	flags := uint32(dispositionDelete | dispositionPosix | dispositionIgnoreReadonly)
	err := windows.NtSetInformationFile(h, &iosb, (*byte)(unsafe.Pointer(&flags)), 4, fileDispositionInformationEx)
	if errors.Is(err, windows.STATUS_INVALID_INFO_CLASS) || errors.Is(err, windows.STATUS_NOT_SUPPORTED) || errors.Is(err, windows.STATUS_INVALID_PARAMETER) {
		del := byte(1)
		err = windows.NtSetInformationFile(h, &iosb, &del, 1, fileDispositionInformation)
	}
	return ntErr(err)
}

// Unlink removes a file, symlink, junction or special file (POSIX semantics: the name
// goes away at once even if Windows programs have it open with FILE_SHARE_DELETE).
func (f *NTFS) Unlink(path []string) error {
	h, err := f.openAt(path, windows.DELETE|windows.FILE_READ_ATTRIBUTES, windows.FILE_OPEN, 0, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	attrs, tag, err := reparseTag(h)
	if err != nil {
		return ntErr(err)
	}
	if typeOf(attrs, tag) == sIFDIR {
		return EISDIR
	}
	return deleteHandle(h)
}

// Rmdir removes an empty directory.
func (f *NTFS) Rmdir(path []string) error {
	h, err := f.openAt(path, windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.FILE_LIST_DIRECTORY, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	attrs, tag, err := reparseTag(h)
	if err != nil {
		return ntErr(err)
	}
	if typeOf(attrs, tag) != sIFDIR {
		return ENOTDIR
	}
	return deleteHandle(h)
}

// Rename moves oldPath to newPath, replacing the target unless noReplace.
func (f *NTFS) Rename(oldPath, newPath []string, noReplace bool) error {
	h, err := f.openAt(oldPath, windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.FILE_READ_EA, windows.FILE_OPEN, 0, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	dir, name, err := f.openParent(newPath)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(dir)
	flags := uint32(renamePosix | renameIgnoreReadonly)
	if !noReplace {
		flags |= renameReplace
	}
	var iosb windows.IO_STATUS_BLOCK
	b := renameBuf(flags, dir, name)
	err = windows.NtSetInformationFile(h, &iosb, &b[0], uint32(len(b)), fileRenameInformationEx)
	if errors.Is(err, windows.STATUS_INVALID_INFO_CLASS) || errors.Is(err, windows.STATUS_NOT_SUPPORTED) {
		var rep uint32
		if !noReplace {
			rep = 1
		}
		b = renameBuf(rep, dir, name)
		err = windows.NtSetInformationFile(h, &iosb, &b[0], uint32(len(b)), fileRenameInformation)
	}
	if errors.Is(err, windows.STATUS_ACCESS_DENIED) && !noReplace {
		// NTFS will not replace a directory; POSIX rename replaces an empty one.
		if t, terr := f.Lstat(newPath); terr == nil && t.isDir() {
			if s, serr := statHandle(h); serr == nil && !s.isDir() {
				return EISDIR
			}
			if rerr := f.Rmdir(newPath); rerr != nil {
				return rerr
			}
			err = windows.NtSetInformationFile(h, &iosb, &b[0], uint32(len(b)), fileRenameInformationEx)
		}
	}
	return ntErr(err)
}

// Setattr changes size, times, mode and owner.
func (f *NTFS) Setattr(path []string, file File, s SetAttr) (Attr, error) {
	acc := uint32(windows.FILE_READ_ATTRIBUTES | windows.FILE_READ_EA | windows.FILE_WRITE_ATTRIBUTES)
	if s.SetSize {
		acc |= windows.FILE_WRITE_DATA
	}
	if s.SetMode || s.SetOwner {
		acc |= windows.FILE_WRITE_EA | windows.FILE_READ_EA
	}
	h, err := f.openAt(path, acc, windows.FILE_OPEN, 0, nil)
	if err != nil {
		return Attr{}, err
	}
	defer windows.CloseHandle(h)
	var iosb windows.IO_STATUS_BLOCK
	if s.SetSize {
		size := int64(s.Size)
		if err := windows.NtSetInformationFile(h, &iosb, (*byte)(unsafe.Pointer(&size)), 8, fileEndOfFileInformation); err != nil {
			return Attr{}, ntErr(err)
		}
	}
	if s.SetAtime || s.SetMtime {
		var bi [5]int64 // creation, access, write, change, attributes(+pad)
		now := toFiletime(time.Now().UnixNano())
		conv := func(v int64) int64 {
			if v < 0 {
				return now
			}
			return toFiletime(v)
		}
		if s.SetAtime {
			bi[1] = conv(s.Atime)
		}
		if s.SetMtime {
			bi[2] = conv(s.Mtime)
		}
		if err := windows.NtSetInformationFile(h, &iosb, (*byte)(unsafe.Pointer(&bi[0])), 40, fileBasicInformation); err != nil {
			return Attr{}, ntErr(err)
		}
	}
	if s.SetMode || s.SetOwner {
		cur, err := statHandle(h)
		if err != nil {
			return Attr{}, err
		}
		var ea []byte
		switch {
		case s.SetMode && s.SetOwner:
			ea = lxEA(cur.Mode&sIFMT|s.Mode&0o7777, &Owner{s.UID, s.GID})
		case s.SetMode:
			ea = lxEA(cur.Mode&sIFMT|s.Mode&0o7777, nil)
		default:
			ea = lxOwnerEA(s.UID, s.GID)
		}
		if err := setEA(h, ea); err != nil {
			return Attr{}, err
		}
	}
	return statHandle(h)
}

// Statfs reports the host volume.
func (f *NTFS) Statfs() (Statfs, error) {
	p, err := windows.UTF16PtrFromString(strings.TrimPrefix(f.rootPath, `\\?\`) + `\`)
	if err != nil {
		return Statfs{}, EINVAL
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return Statfs{}, ntErr(err)
	}
	const bs = 4096
	return Statfs{Blocks: total / bs, Bfree: free / bs, Bavail: avail / bs, Files: 1 << 32, Ffree: 1 << 31, Bsize: bs, Frsize: bs, NameLen: maxNameLen}, nil
}
