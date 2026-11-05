package apply

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	fileNameNormalized        = 0x0 // GetFinalPathNameByHandle flags
	volumeNameNone            = 0x4
	fileAttributeTagInfo      = 9  // FileAttributeTagInfo
	fileRenameInformationEx   = 65 // FILE_INFORMATION_CLASS
	fileDispositionInfoEx     = 64
	renameReplace             = 0x1
	renamePosix               = 0x2
	renameIgnoreReadonly      = 0x40
	dispositionDelete         = 0x1
	dispositionPosix          = 0x2
	dispositionIgnoreReadonly = 0x10
	tagSurrogate              = 0x20000000
	fileAddFile               = 0x2
	fileAddSubdir             = 0x4
)

func applyPlanned(root string, changes []Change, segs [][]string, errs []error) ([]Outcome, error) {
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|fileAddFile|fileAddSubdir|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", root, err)
	}
	defer windows.CloseHandle(h) //nolint:errcheck // a read handle
	out := make([]Outcome, len(changes))
	// Deletions first, so that a file removed and a directory of its name created in
	// the same set can both apply.
	for _, pass := range []bool{true, false} {
		for i, c := range changes {
			if c.Delete != pass {
				continue
			}
			out[i].Path = c.Path
			if errs[i] != nil {
				out[i].Err = errs[i]
				continue
			}
			out[i].Err = applyOne(h, segs[i], c)
		}
	}
	return out, nil
}

func applyOne(root windows.Handle, segs []string, c Change) error {
	dir, err := openDirs(root, segs[:len(segs)-1], !c.Delete)
	if err != nil {
		return err
	}
	if dir != root {
		defer windows.CloseHandle(dir) //nolint:errcheck // a directory handle
	}
	name := segs[len(segs)-1]
	access := uint32(windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES | windows.DELETE)
	// While the target is checked and replaced, nobody else may write it.
	cur, err := ntOpen(dir, name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE)
	switch {
	case errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND):
		cur = 0
	case errors.Is(err, windows.STATUS_FILE_IS_A_DIRECTORY):
		return ErrConflict
	case err != nil:
		return err
	}
	if cur != 0 {
		defer windows.CloseHandle(cur) //nolint:errcheck // a read handle
		if err := checkFound(cur, name); err != nil {
			return err
		}
		if c.Base == nil {
			return ErrConflict
		}
		sum, err := hashHandle(cur)
		if err != nil {
			return err
		}
		if sum != *c.Base {
			return ErrConflict
		}
	} else if c.Base != nil {
		return ErrConflict
	}
	if c.Delete {
		return deleteHandle(cur)
	}
	return replace(dir, name, c.Data, cur != 0)
}

// openDirs opens each directory of segs below root by one name, creating missing ones
// if create, refusing a link (a name surrogate reparse point) or a name the file system
// found under another spelling.
func openDirs(root windows.Handle, segs []string, create bool) (windows.Handle, error) {
	dir := root
	for _, s := range segs {
		disp := uint32(windows.FILE_OPEN)
		if create {
			disp = windows.FILE_OPEN_IF
		}
		h, err := ntOpen(dir, s, windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|fileAddFile|fileAddSubdir,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, disp, windows.FILE_DIRECTORY_FILE)
		if dir != root {
			windows.CloseHandle(dir) //nolint:errcheck // a directory handle
		}
		if errors.Is(err, windows.STATUS_NOT_A_DIRECTORY) {
			return 0, ErrConflict
		}
		if err != nil {
			return 0, err
		}
		if err := checkFound(h, s); err != nil {
			windows.CloseHandle(h) //nolint:errcheck // a directory handle
			return 0, err
		}
		dir = h
	}
	return dir, nil
}

// ntOpen opens name relative to dir, never following a reparse point.
func ntOpen(dir windows.Handle, name string, access, share, disp, opts uint32) (windows.Handle, error) {
	us, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: dir, ObjectName: us, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&h, access|windows.SYNCHRONIZE, &oa, &iosb, nil, windows.FILE_ATTRIBUTE_NORMAL, share, disp,
		opts|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	return h, err
}

// checkFound refuses a handle that is a link, or whose name differs from want: the open
// matched an 8.3 short name, or a name differing in case in a case-insensitive
// directory. Writing through either would change a file the review did not name.
func checkFound(h windows.Handle, want string) error {
	var tag [2]uint32
	if err := windows.GetFileInformationByHandleEx(h, fileAttributeTagInfo, (*byte)(unsafe.Pointer(&tag[0])), 8); err != nil {
		return err
	}
	if tag[0]&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 && tag[1]&tagSurrogate != 0 {
		return ErrName
	}
	// The normalized name, not FileNameInfo: that one returns the name as opened, short
	// name and all.
	buf := make([]uint16, 32768)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), fileNameNormalized|volumeNameNone)
	if err != nil {
		return err
	}
	if int(n) >= len(buf) {
		return ErrName
	}
	full := windows.UTF16ToString(buf[:n])
	if got := full[strings.LastIndexByte(full, '\\')+1:]; got != want {
		return fmt.Errorf("%w (%s)", ErrName, got)
	}
	return nil
}

func hashHandle(h windows.Handle) ([sha256.Size]byte, error) {
	f := handleReader{h}
	d := sha256.New()
	if _, err := io.Copy(d, f); err != nil {
		return [sha256.Size]byte{}, err
	}
	var s [sha256.Size]byte
	copy(s[:], d.Sum(nil))
	return s, nil
}

type handleReader struct{ h windows.Handle }

func (r handleReader) Read(p []byte) (int, error) {
	var n uint32
	err := windows.ReadFile(r.h, p, &n, nil)
	if err == nil && n == 0 && len(p) > 0 {
		return 0, io.EOF
	}
	return int(n), err
}

// replace writes data to a new temporary file in dir and renames it to name (over the
// existing file if over, else failing if one appeared since the check), so the
// target is either the old or the new content, and a hard link elsewhere to the old
// file keeps the old content.
func replace(dir windows.Handle, name string, data []byte, over bool) error {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := ".hyperbutler-apply-" + hex.EncodeToString(rnd[:]) + ".tmp"
	h, err := ntOpen(dir, tmp, windows.FILE_WRITE_DATA|windows.DELETE|windows.FILE_READ_ATTRIBUTES, 0, windows.FILE_CREATE, windows.FILE_NON_DIRECTORY_FILE)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h) //nolint:errcheck // checked by FlushFileBuffers and the rename
	if err := writeAll(h, data); err != nil {
		_ = deleteHandle(h)
		return err
	}
	if err := windows.FlushFileBuffers(h); err != nil {
		_ = deleteHandle(h)
		return err
	}
	if err := renameHandle(h, dir, name, over); err != nil {
		if errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
			err = ErrConflict // created on the host after the check
		}
		_ = deleteHandle(h)
		return err
	}
	return nil
}

func writeAll(h windows.Handle, data []byte) error {
	for len(data) > 0 {
		chunk := data[:min(len(data), 1<<20)]
		var n uint32
		if err := windows.WriteFile(h, chunk, &n, nil); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

func renameHandle(h, dir windows.Handle, name string, over bool) error {
	u := utf16.Encode([]rune(name))
	b := make([]byte, 20+2*len(u)+2)
	flags := uint32(renamePosix | renameIgnoreReadonly)
	if over {
		flags |= renameReplace
	}
	binary.LittleEndian.PutUint32(b[0:], flags)
	binary.LittleEndian.PutUint64(b[8:], uint64(dir))
	binary.LittleEndian.PutUint32(b[16:], uint32(2*len(u))) //nolint:gosec // G115: a name is at most 255 code units
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[20+2*i:], c)
	}
	var iosb windows.IO_STATUS_BLOCK
	return windows.NtSetInformationFile(h, &iosb, &b[0], uint32(len(b)), fileRenameInformationEx) //nolint:gosec // G115: as above
}

func deleteHandle(h windows.Handle) error {
	var iosb windows.IO_STATUS_BLOCK
	flags := uint32(dispositionDelete | dispositionPosix | dispositionIgnoreReadonly)
	return windows.NtSetInformationFile(h, &iosb, (*byte)(unsafe.Pointer(&flags)), 4, fileDispositionInfoEx)
}
