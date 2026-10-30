//go:build windows

package broker

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/Higangssh/winbox/internal/broker/proto"
	"golang.org/x/sys/windows"
)

// DataDir is a data directory handle that passed the ADR 0004 rule 2 checks. The handle
// is the broker's own: it was reopened from the caller's handle, so no other process
// shares its file object. Everything below the directory must be opened relative to it
// with OBJ_DONT_REPARSE and FILE_OPEN_REPARSE_POINT, never by path.
type DataDir struct {
	mu sync.Mutex
	h  windows.Handle
}

// Handle returns the directory handle, or 0 after Close. It stays valid until Close.
func (d *DataDir) Handle() windows.Handle {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.h
}

// Close closes the handle. It is safe to call more than once.
func (d *DataDir) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.h == 0 {
		return nil
	}
	err := windows.CloseHandle(d.h)
	d.h = 0
	return err
}

func (d *DataDir) revalidate(sid string, validate func(windows.Handle, string) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.h == 0 {
		return reject("data location is closed", nil)
	}
	return validate(d.h, sid)
}

// dataDirAccess is the access the broker reopens the directory with: enough to create the
// VM's files below it and to read its security, and nothing that changes the directory's
// own security or deletes it.
const dataDirAccess = fileListDirectory | fileAddFile | fileAddSubdirectory | fileTraverse |
	fileReadAttributes | windows.READ_CONTROL | windows.SYNCHRONIZE

const (
	fileListDirectory   = 0x0001
	fileAddFile         = 0x0002
	fileAddSubdirectory = 0x0004
	fileTraverse        = 0x0020
	fileReadAttributes  = 0x0080
)

// openDataDir takes the directory handle with value v from the client and validates it.
//
//  1. The handle is duplicated out of the client process (Peer.DuplicateHandle).
//  2. Its object type must be File. NtQueryObject(ObjectTypeInformation) does not take
//     the file object lock, so a client cannot stall it with pending synchronous I/O.
//  3. The broker opens the same object again, relative to that handle with an empty name,
//     while impersonating the client, as a directory (FILE_DIRECTORY_FILE), without
//     following reparse points (FILE_OPEN_REPARSE_POINT, OBJ_DONT_REPARSE), and with an
//     anonymous security QoS. So the access check is the caller's, a pipe or device can
//     never be opened, and nothing could impersonate the broker even if it were. The new
//     handle has a private file object, so the client cannot block queries on it.
//  4. validate (ValidateDataDir outside tests) checks the reopened handle.
func openDataDir(p Peer, v uint64, sid string, validate func(windows.Handle, string) error) (*DataDir, error) {
	src, err := p.DuplicateHandle(v)
	if err != nil {
		return nil, &rejectError{code: proto.CodeInvalidHandle, reason: "cannot take the handle from the caller", err: err}
	}
	defer windows.CloseHandle(src)
	typ, err := objectTypeName(src)
	if err != nil || typ != "File" {
		return nil, reject("handle is not a directory", err)
	}
	var h windows.Handle
	err = p.Impersonate(func() error {
		var err error
		h, err = reopenDirectory(src)
		return err
	})
	if err != nil {
		return nil, reject("cannot open the handle as a directory", err)
	}
	if err := validate(h, sid); err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	return &DataDir{h: h}, nil
}

func reopenDirectory(root windows.Handle) (windows.Handle, error) {
	qos := windows.SECURITY_QUALITY_OF_SERVICE{ImpersonationLevel: windows.SecurityAnonymous}
	qos.Length = uint32(unsafe.Sizeof(qos))
	empty := &windows.NTUnicodeString{}
	oa := windows.OBJECT_ATTRIBUTES{
		RootDirectory: root,
		ObjectName:    empty,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
		SecurityQoS:   &qos,
	}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var (
		h    windows.Handle
		iosb windows.IO_STATUS_BLOCK
	)
	err := windows.NtCreateFile(&h, dataDirAccess, &oa, &iosb, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0, 0)
	runtime.KeepAlive(empty)
	runtime.KeepAlive(&qos)
	if err != nil {
		return 0, err
	}
	return h, nil
}

var (
	modntdll                         = windows.NewLazySystemDLL("ntdll.dll")
	procNtQueryObject                = modntdll.NewProc("NtQueryObject")
	procNtQueryVolumeInformationFile = modntdll.NewProc("NtQueryVolumeInformationFile")
)

const (
	objectTypeInformation   = 2
	fileFsDeviceInformation = 4
	fileDeviceDisk          = 0x00000007
	fileRemoteDevice        = 0x00000010
	inheritOnlyACE          = 0x08
	accessAllowedACEType    = 0
	accessDeniedACEType     = 1
)

func objectTypeName(h windows.Handle) (string, error) {
	// PUBLIC_OBJECT_TYPE_INFORMATION is a UNICODE_STRING followed by reserved fields,
	// with the name stored after it in the same buffer.
	buf := make([]byte, 1024)
	var n uint32
	r, _, _ := procNtQueryObject.Call(uintptr(h), objectTypeInformation,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&n)))
	if r != 0 {
		return "", windows.NTStatus(r)
	}
	us := (*windows.NTUnicodeString)(unsafe.Pointer(&buf[0]))
	if us.Length > us.MaximumLength || uintptr(us.MaximumLength) > uintptr(len(buf)) {
		return "", errors.New("broker: bad object type information")
	}
	return us.String(), nil
}

// ValidateDataDir checks an open directory handle against ADR 0004 rule 2. It reads
// everything from the handle and never resolves a path:
//
//   - the volume is local (device type disk, not FILE_REMOTE_DEVICE) and NTFS;
//   - the object is a directory and not a reparse point (junction, symlink, or any other
//     tag);
//   - the owner is sid;
//   - the DACL is present, contains only access-allowed and access-denied ACEs, and every
//     allow ACE is for sid, SYSTEM or Administrators (an inherit-only ACE may also be for
//     CREATOR OWNER).
func ValidateDataDir(h windows.Handle, sid string) error {
	caller, err := windows.StringToSid(sid)
	if err != nil {
		return reject("caller SID is invalid", err)
	}

	var dev struct{ DeviceType, Characteristics uint32 }
	var iosb windows.IO_STATUS_BLOCK
	r, _, _ := procNtQueryVolumeInformationFile.Call(uintptr(h), uintptr(unsafe.Pointer(&iosb)),
		uintptr(unsafe.Pointer(&dev)), unsafe.Sizeof(dev), fileFsDeviceInformation)
	if r != 0 {
		return reject("cannot query the volume device", windows.NTStatus(r))
	}
	if dev.DeviceType != fileDeviceDisk || dev.Characteristics&fileRemoteDevice != 0 {
		return reject("not on a local disk", nil)
	}

	var fsName [windows.MAX_PATH + 1]uint16
	var fsFlags uint32
	if err := windows.GetVolumeInformationByHandle(h, nil, 0, nil, nil, &fsFlags, &fsName[0], uint32(len(fsName))); err != nil {
		return reject("cannot query the file system", err)
	}
	if fs := windows.UTF16ToString(fsName[:]); fs != "NTFS" {
		return reject("file system is not NTFS", nil)
	}

	var tag struct{ FileAttributes, ReparseTag uint32 }
	if err := windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&tag)), uint32(unsafe.Sizeof(tag))); err != nil {
		return reject("cannot query attributes", err)
	}
	if tag.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return reject("is a reparse point", nil)
	}
	if tag.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return reject("is not a directory", nil)
	}

	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return reject("cannot read security", err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(caller) {
		return reject("owner is not the caller", err)
	}
	return checkDACL(sd, caller)
}

var (
	sidSystem, _       = windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	sidAdmins, _       = windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	sidCreatorOwner, _ = windows.CreateWellKnownSid(windows.WinCreatorOwnerSid)
)

func checkDACL(sd *windows.SECURITY_DESCRIPTOR, caller *windows.SID) error {
	dacl, _, err := sd.DACL()
	if err != nil {
		return reject("has no DACL", err)
	}
	if dacl == nil {
		// A NULL DACL grants everyone full access.
		return reject("has a NULL DACL", nil)
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return reject("cannot read the DACL", err)
		}
		switch ace.Header.AceType {
		case accessDeniedACEType:
			continue
		case accessAllowedACEType:
		default:
			// Object, callback and conditional ACEs can grant access in ways this check
			// does not model.
			return reject(fmt.Sprintf("DACL has an ACE of type %d", ace.Header.AceType), nil)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		switch {
		case sid.Equals(caller), sid.Equals(sidSystem), sid.Equals(sidAdmins):
		case ace.Header.AceFlags&inheritOnlyACE != 0 && sid.Equals(sidCreatorOwner):
		default:
			return reject("DACL grants access to another account", nil)
		}
	}
	return nil
}
