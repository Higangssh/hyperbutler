//go:build windows

package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/winbox/internal/vm"
)

// MkdirPrivate creates dir (and missing parents) if needed. A directory it creates gets
// a protected DACL for the current user, SYSTEM and Administrators only, so other local
// users cannot swap the data disk (a new directory on a non-system volume would
// otherwise inherit "Authenticated Users: Modify" from the volume root). An existing
// directory is used as is, but must not be a reparse point.
func MkdirPrivate(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("engine: %q is not an absolute path", dir)
	}
	if fi, err := os.Lstat(dir); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 || fi.Mode()&os.ModeIrregular != 0 || isReparse(dir) {
			return fmt.Errorf("engine: %s is a link; use the real directory", dir)
		}
		if !fi.IsDir() {
			return fmt.Errorf("engine: %s is not a directory", dir)
		}
		return nil
	}
	if parent := filepath.Dir(dir); parent != dir {
		if _, err := os.Stat(parent); errors.Is(err, os.ErrNotExist) {
			if err := os.MkdirAll(parent, 0o700); err != nil {
				return err
			}
		}
	}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	// OICI: files created inside inherit the same three entries.
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + tu.User.Sid.String() + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	if err := windows.CreateDirectory(p, sa); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return &os.PathError{Op: "mkdir", Path: dir, Err: err}
	}
	return nil
}

func isReparse(path string) bool {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	a, err := windows.GetFileAttributes(p)
	return err == nil && a&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

// dataDisk is the prepared data disk.
type dataDisk struct {
	path    string
	meta    *DiskMeta
	created bool
}

// prepareDisk returns the data disk of layout l, creating a blank one of size bytes on
// the first start. The steps are ordered so a crash at any point either leaves nothing
// or is completed on the next start:
//
//  1. CreateSparseVHDX writes data.vhdx.new (a stale one is removed first);
//  2. disk.json is written, with the new file's allocation as the metadata size;
//  3. data.vhdx.new is renamed to data.vhdx.
//
// A disk.json without data.vhdx or data.vhdx.new means the disk was removed behind
// winbox's back; that is an error, never a silent reformat.
func prepareDisk(l Layout, size uint64) (*dataDisk, error) {
	if err := MkdirPrivate(l.Data); err != nil {
		return nil, err
	}
	path, tmp := l.DiskPath(), l.DiskPath()+".new"
	meta, merr := LoadDiskMeta(l)
	_, derr := os.Lstat(path)
	switch {
	case merr == nil && derr == nil:
		return checkDisk(path, meta, false)
	case merr == nil && errors.Is(derr, os.ErrNotExist):
		if _, err := os.Lstat(tmp); err == nil {
			if err := os.Rename(tmp, path); err != nil {
				return nil, err
			}
			return checkDisk(path, meta, true)
		}
		return nil, codeErr(CodeDataDisk, fmt.Errorf("the data disk %s is missing but %s exists; `winbox delete` starts over", path, l.DiskMetaPath()))
	case merr != nil && !errors.Is(merr, os.ErrNotExist):
		return nil, codeErr(CodeDataDisk, merr)
	case derr == nil:
		return nil, codeErr(CodeDataDisk, fmt.Errorf("the data disk %s has no %s; `winbox delete` starts over", path, l.DiskMetaPath()))
	case !errors.Is(derr, os.ErrNotExist):
		return nil, derr
	}
	// First start.
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := vm.CreateSparseVHDX(tmp, size); err != nil {
		return nil, codeErr(CodeDataDisk, err)
	}
	u, err := vm.Usage(tmp)
	if err != nil {
		return nil, err
	}
	if !u.Sparse {
		_ = os.Remove(tmp)
		return nil, codeErr(CodeDataDisk, fmt.Errorf("%s did not become a sparse file; the volume must be NTFS", tmp))
	}
	meta = &DiskMeta{SizeBytes: size, BlockSize: vm.DataDiskBlockSize, MetadataBytes: u.Allocated, Created: time.Now().UTC().Truncate(time.Second)}
	if err := saveDiskMeta(l, meta); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}
	return checkDisk(path, meta, true)
}

func checkDisk(path string, meta *DiskMeta, created bool) (*dataDisk, error) {
	u, err := vm.Usage(path) // refuses reparse points
	if err != nil {
		return nil, codeErr(CodeDataDisk, err)
	}
	if !u.Sparse {
		return nil, codeErr(CodeDataDisk, fmt.Errorf("%s is not a sparse file, so freed space could not go back to the host", path))
	}
	return &dataDisk{path: path, meta: meta, created: created}, nil
}
