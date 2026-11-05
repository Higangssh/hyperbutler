//go:build windows

package broker

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/imageset"
	"github.com/Higangssh/hyperbutler/internal/vm"
)

// DataDiskName is the data disk's name in the data location. The broker opens only
// this name, relative to the validated directory handle.
const DataDiskName = "data.vhdx"

// SessionDiskName is the data disk of sandbox session slot (1..proto.MaxSlot) in the
// data location (ADR 0009): a disk of its own, never the Docker VM's.
func SessionDiskName(slot uint8) string { return fmt.Sprintf("session-%d.vhdx", slot) }

// diskName is the data disk of slot: DataDiskName for the Docker VM (slot 0).
func diskName(slot uint8) string {
	if slot == 0 {
		return DataDiskName
	}
	return SessionDiskName(slot)
}

// diskFileAccess is what the broker holds the data disk with: its attributes and
// security, its DACL, and read access. A handle without data access takes no part in
// share-mode checks, so without FILE_READ_DATA the missing FILE_SHARE_DELETE would not
// stop a rename. HCS's own open (read and write, by path) must share read with it.
const diskFileAccess = windows.FILE_READ_DATA | fileReadAttributes | windows.READ_CONTROL | windows.WRITE_DAC | windows.SYNCHRONIZE

// diskFile is the data disk, opened by handle relative to the data location and held
// open without FILE_SHARE_DELETE while the VM lives: the file cannot be renamed, deleted
// or replaced under HCS, which opens it by the final path of this handle.
type diskFile struct {
	mu   sync.Mutex
	h    windows.Handle
	path string // GetFinalPathNameByHandle of h
}

// diskInfo is what checkDiskInfo judges, read from the handle.
type diskInfo struct {
	attrs      uint32
	links      uint32
	directory  bool
	fileType   uint32
	owner      *windows.SID
	sd         *windows.SECURITY_DESCRIPTOR // owner and DACL; nil skips the DACL check (tests)
	remote     bool
	deviceType uint32
}

// CheckDataDisk checks the data disk below the open data directory dir as the service
// does before it starts a VM (checkDiskInfo), for the caller sid, and closes it again.
// The engine runs it, as the user, to say why the service refused a disk. The error's
// reason is caller-safe text.
func CheckDataDisk(dir windows.Handle, sid string) error {
	d, err := openDataDisk(dir, DataDiskName, sid)
	if err != nil {
		return err
	}
	return d.Close()
}

// openDataDisk opens name below the directory handle dir with OBJ_DONT_REPARSE and
// FILE_OPEN_REPARSE_POINT, as a non-directory file, and checks it with checkDiskInfo
// for the caller sid. The handle shares read and write (HCS opens the disk for both)
// but not delete.
func openDataDisk(dir windows.Handle, name, sid string) (_ *diskFile, err error) {
	if dir == 0 {
		return nil, fmt.Errorf("%w: no data location", ErrDiskRejected)
	}
	caller, err := windows.StringToSid(sid)
	if err != nil {
		return nil, fmt.Errorf("%w: caller SID: %w", ErrDiskRejected, err)
	}
	h, err := openRelative(dir, name)
	if err != nil {
		return nil, fmt.Errorf("%w: open %s: %w", ErrDiskRejected, name, err)
	}
	defer func() {
		if err != nil {
			_ = windows.CloseHandle(h)
		}
	}()
	info, err := readDiskInfo(h)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrDiskRejected, name, err)
	}
	if err := checkDiskInfo(info, caller); err != nil {
		return nil, fmt.Errorf("%w: %s %w", ErrDiskRejected, name, err)
	}
	// Read through the handle just checked and held: the contents HCS will open.
	if err := checkVHDX(handleReader{h}); err != nil {
		return nil, fmt.Errorf("%w: %s %w", ErrDiskRejected, name, err)
	}
	p, err := imageset.FinalPathOf(h)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: final path: %w", ErrDiskRejected, name, err)
	}
	return &diskFile{h: h, path: p}, nil
}

func openRelative(dir windows.Handle, name string) (windows.Handle, error) {
	return openEntry(dir, name, diskFileAccess, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_NON_DIRECTORY_FILE)
}

// OpenEntry opens name, a plain name below the directory handle dir, as the broker
// opens the data disk: by the handle and the name only, never through a reparse point
// (OBJ_DONT_REPARSE, and the reparse point itself if name is one), a file or a
// directory, with access and share. The engine's data folder fix opens every entry of
// the folder with it, sharing read only: an entry someone holds open to write is
// refused, and none can be renamed or written while the fix holds it.
func OpenEntry(dir windows.Handle, name string, access, share uint32) (windows.Handle, error) {
	return openEntry(dir, name, access, share, 0)
}

func openEntry(dir windows.Handle, name string, access, share, opts uint32) (windows.Handle, error) {
	if name == "" || strings.ContainsAny(name, `\/:`) || name == "." || name == ".." {
		return 0, errors.New("not a plain file name")
	}
	us, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	qos := windows.SECURITY_QUALITY_OF_SERVICE{ImpersonationLevel: windows.SecurityAnonymous}
	qos.Length = uint32(unsafe.Sizeof(qos))
	oa := windows.OBJECT_ATTRIBUTES{
		RootDirectory: dir,
		ObjectName:    us,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
		SecurityQoS:   &qos,
	}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var (
		h    windows.Handle
		iosb windows.IO_STATUS_BLOCK
	)
	err = windows.NtCreateFile(&h, access, &oa, &iosb, nil, 0,
		share,
		windows.FILE_OPEN,
		opts|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0, 0)
	runtime.KeepAlive(us)
	runtime.KeepAlive(&qos)
	if err != nil {
		return 0, err
	}
	return h, nil
}

// fileStandardInfo is FILE_STANDARD_INFO.
type fileStandardInfo struct {
	AllocationSize, EndOfFile int64
	NumberOfLinks             uint32
	DeletePending, Directory  bool
}

func readDiskInfo(h windows.Handle) (*diskInfo, error) {
	var info diskInfo
	var tag struct{ FileAttributes, ReparseTag uint32 }
	if err := windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&tag)), uint32(unsafe.Sizeof(tag))); err != nil {
		return nil, fmt.Errorf("query attributes: %w", err)
	}
	info.attrs = tag.FileAttributes
	var std fileStandardInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileStandardInfo,
		(*byte)(unsafe.Pointer(&std)), uint32(unsafe.Sizeof(std))); err != nil {
		return nil, fmt.Errorf("query links: %w", err)
	}
	info.links, info.directory = std.NumberOfLinks, std.Directory
	ft, err := windows.GetFileType(h)
	if err != nil {
		return nil, fmt.Errorf("query file type: %w", err)
	}
	info.fileType = ft
	var dev struct{ DeviceType, Characteristics uint32 }
	var iosb windows.IO_STATUS_BLOCK
	if r, _, _ := procNtQueryVolumeInformationFile.Call(uintptr(h), uintptr(unsafe.Pointer(&iosb)),
		uintptr(unsafe.Pointer(&dev)), unsafe.Sizeof(dev), fileFsDeviceInformation); r != 0 {
		return nil, fmt.Errorf("query the volume device: %w", ntStatus(r))
	}
	info.deviceType, info.remote = dev.DeviceType, dev.Characteristics&fileRemoteDevice != 0
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, fmt.Errorf("read security: %w", err)
	}
	info.sd = sd
	if info.owner, _, err = sd.Owner(); err != nil {
		return nil, fmt.Errorf("read owner: %w", err)
	}
	return &info, nil
}

// checkDiskInfo is the data disk rule: a regular file on a local disk, not a reparse
// point, with exactly one link (a second link could be a name in a directory the caller
// does not own, made for another file), owned by the caller, and with a DACL that lets
// no one else in: the folder's check (ADR 0004 rule 2) keeps other users from opening
// it by name, but traverse checking is off for every user, so the file's own DACL must
// hold too. Its allow ACEs are for the caller, SYSTEM, Administrators and the virtual
// machine accounts HCS grants (S-1-5-83-...).
func checkDiskInfo(info *diskInfo, caller *windows.SID) error {
	switch {
	case info.attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0:
		return errors.New("is a reparse point")
	case info.directory || info.attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0:
		return errors.New("is a directory")
	case info.attrs&windows.FILE_ATTRIBUTE_DEVICE != 0 || info.fileType != windows.FILE_TYPE_DISK:
		return errors.New("is not a regular file")
	case info.deviceType != fileDeviceDisk || info.remote:
		return errors.New("is not on a local disk")
	case info.links != 1:
		return fmt.Errorf("has %d links, want 1", info.links)
	case info.owner == nil || !info.owner.Equals(caller):
		return errors.New("is not owned by the caller")
	}
	if info.sd != nil {
		if err := checkDACLWith(info.sd, caller, isVMAccount); err != nil {
			var re *rejectError
			if errors.As(err, &re) {
				return errors.New(re.reason)
			}
			return err
		}
	}
	return nil
}

// isVMAccount reports whether sid is NT VIRTUAL MACHINE\Virtual Machines or a VM's own
// account (S-1-5-83-...), which HcsGrantVmAccess adds to the disk.
func isVMAccount(sid *windows.SID) bool { return strings.HasPrefix(sid.String(), "S-1-5-83-") }

// Path is the final path of the open disk, for HCS.
func (d *diskFile) Path() string { return d.path }

// Close closes the handle. It is safe to call more than once.
func (d *diskFile) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.h == 0 {
		return nil
	}
	err := windows.CloseHandle(d.h)
	d.h = 0
	return err
}

// grant gives the VM account sid mask on the disk, by handle.
func (d *diskFile) grant(sid string, mask uint32) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return grantHandle(d.h, sid, mask)
}

// revoke removes every entry for sid from the disk's DACL, by handle.
func (d *diskFile) revoke(sid string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return revokeHandle(d.h, []string{sid})
}

// grantHandle adds an allow entry for sid with mask to h's DACL, after removing the
// entries of any VM account (vm.AccountPrefix) a crashed run left behind. h needs
// READ_CONTROL and WRITE_DAC.
func grantHandle(h windows.Handle, sid string, mask uint32) error {
	if !strings.HasPrefix(sid, vm.AccountPrefix) {
		return fmt.Errorf("broker: %s is not a VM account", sid)
	}
	stale, err := vmAccountsOn(h)
	if err != nil {
		return err
	}
	stale = append(stale, sid)
	return setDACLEntries(h, stale, sid, mask)
}

// revokeHandle removes every entry for each of sids from h's DACL.
func revokeHandle(h windows.Handle, sids []string) error {
	return setDACLEntries(h, sids, "", 0)
}

// setDACLEntries revokes every entry of revoke and then, if grant is set, adds an allow
// entry for it with mask, and writes the DACL back by handle.
func setDACLEntries(h windows.Handle, revoke []string, grant string, mask uint32) error {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("broker: read DACL: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("broker: read DACL: %w", err)
	}
	var entries []windows.EXPLICIT_ACCESS
	add := func(s string, mode windows.ACCESS_MODE, m uint32) error {
		sid, err := windows.StringToSid(s)
		if err != nil {
			return err
		}
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.ACCESS_MASK(m),
			AccessMode:        mode,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
		return nil
	}
	for _, s := range revoke {
		if err := add(s, windows.REVOKE_ACCESS, 0); err != nil {
			return err
		}
	}
	if grant != "" {
		if err := add(grant, windows.GRANT_ACCESS, mask); err != nil {
			return err
		}
	}
	if len(entries) == 0 {
		return nil
	}
	nd, err := windows.ACLFromEntries(entries, dacl)
	if err != nil {
		return fmt.Errorf("broker: build DACL: %w", err)
	}
	if err := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, nd, nil); err != nil {
		return fmt.Errorf("broker: write DACL: %w", err)
	}
	return nil
}

// vmAccountsOn lists the VM accounts h's DACL names.
func vmAccountsOn(h windows.Handle) ([]string, error) {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, fmt.Errorf("broker: read DACL: %w", err)
	}
	return vmAccountsIn(sd)
}

func vmAccountsIn(sd *windows.SECURITY_DESCRIPTOR) ([]string, error) {
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return nil, err
		}
		s := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		if strings.HasPrefix(s, vm.AccountPrefix) && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out, nil
}

var procReOpenFile = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile")

// reOpenFile opens the file of handle h again, with other access (ReOpenFile): by the
// handle's file object, so no path is resolved.
func reOpenFile(h windows.Handle, access, share, flags uint32) (windows.Handle, error) {
	r, _, err := procReOpenFile.Call(uintptr(h), uintptr(access), uintptr(share), uintptr(flags))
	if windows.Handle(r) == windows.InvalidHandle || r == 0 {
		return 0, err
	}
	return windows.Handle(r), nil
}

// handleReader reads a synchronous file handle at given offsets.
type handleReader struct{ h windows.Handle }

func (r handleReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	total := 0
	for total < len(p) {
		var o windows.Overlapped
		at := uint64(off) + uint64(total)
		o.Offset, o.OffsetHigh = uint32(at), uint32(at>>32) //nolint:gosec // G115: the low and high halves of a 64-bit offset
		var n uint32
		err := windows.ReadFile(r.h, p[total:], &n, &o)
		total += int(n)
		if errors.Is(err, windows.ERROR_HANDLE_EOF) || err == nil && n == 0 {
			return total, io.EOF
		}
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
