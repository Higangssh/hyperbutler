//go:build windows

package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/vm"
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
	// OICI: files created inside inherit the same three entries. O: makes the user the
	// owner even from an elevated shell, whose token defaults new objects to
	// Administrators: the broker accepts only a data directory the caller owns (ADR 0004).
	me := tu.User.Sid.String()
	sd, err := windows.SecurityDescriptorFromString("O:" + me + "D:P(A;OICI;FA;;;" + me + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
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

// ownByUser makes the current user the owner of path, which an elevated shell would
// otherwise leave to Administrators; the broker takes only a data disk the caller owns.
// Setting oneself as owner needs no privilege.
func ownByUser(path string) error {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, tu.User.Sid, nil, nil, nil)
}

// writeDiskMeta saves disk.json and makes it the user's alone: owned by the user (what
// an elevated shell writes belongs to Administrators) with a protected DACL for them,
// SYSTEM and Administrators. prepareDisk reads it only so (readPrivateFile), and the data
// folder may be one made before and open to others: without its own DACL the file would
// inherit that and the next start would refuse it.
func writeDiskMeta(l Layout, meta *DiskMeta) error {
	if err := saveDiskMeta(l, meta); err != nil {
		return err
	}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + tu.User.Sid.String() + ")(A;;FA;;;SY)(A;;FA;;;BA)")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	err = windows.SetNamedSecurityInfo(l.DiskMetaPath(), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		tu.User.Sid, nil, dacl, nil)
	runtime.KeepAlive(sd)
	return err
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
	// blank means the guest never wrote to the disk: its host allocation is still the
	// metadata allocation it had when it was created. Only then may the guest format it
	// (it checks for zeros and for a file system too).
	blank bool
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
// hyperbutler's back; that is an error, never a silent reformat.
func prepareDisk(l Layout, size uint64) (*dataDisk, error) {
	if err := MkdirPrivate(l.Data); err != nil {
		return nil, err
	}
	path, tmp := l.DiskPath(), l.DiskPath()+".new"
	// disk.json says whether the disk is blank, which has the guest format it: it is
	// read only if no other account could have changed it.
	var meta *DiskMeta
	b, merr := readPrivateFile(l.DiskMetaPath())
	switch {
	case merr == nil:
		meta, merr = ParseDiskMeta(b)
	case !errors.Is(merr, os.ErrNotExist):
		return nil, codeErr(CodeDataDisk, fmt.Errorf("%w; hyperbutler doctor --fix limits the data folder to you, or hyperbutler delete starts over", merr))
	}
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
		return nil, codeErr(CodeDataDisk, fmt.Errorf("the data disk %s is missing but %s exists; `hyperbutler delete` starts over", path, l.DiskMetaPath()))
	case merr != nil && !errors.Is(merr, os.ErrNotExist):
		return nil, codeErr(CodeDataDisk, merr)
	case derr == nil:
		return nil, codeErr(CodeDataDisk, fmt.Errorf("the data disk %s has no %s; `hyperbutler delete` starts over", path, l.DiskMetaPath()))
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
	if err := ownByUser(tmp); err != nil {
		_ = os.Remove(tmp)
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
	if err := writeDiskMeta(l, meta); err != nil {
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
	return &dataDisk{path: path, meta: meta, created: created, blank: u.Allocated <= meta.MetadataBytes}, nil
}
