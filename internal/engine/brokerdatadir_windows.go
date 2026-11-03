package engine

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/broker"
	"github.com/Higangssh/hyperbutler/internal/imageset"
)

// DataDirError is a data directory the broker service would refuse (ADR 0004 rule 2),
// with the reason and a fix for it. A directory made by hyperbutler passes; one made
// before (developer mode, or --data on a drive whose root lets every user change it)
// may inherit other accounts' access, or be owned by Administrators when an elevated
// shell made it.
type DataDirError struct {
	Dir, Reason, Fix string
	// Fixable says FixBrokerDataDir can fix it (hyperbutler doctor --fix).
	Fixable bool
	Err     error
}

func (e *DataDirError) Error() string {
	return fmt.Sprintf("the broker service refuses the data directory %s: %s; %s", e.Dir, e.Reason, e.Fix)
}

func (e *DataDirError) Unwrap() error { return e.Err }

// The fixes are said in words and done by FixBrokerDataDir, not printed as commands: a
// folder the service refuses is one other users may write to, and a command naming a
// path in it (icacls "<dir>\data.vhdx") could act on whatever another user renames or
// links there between the check and the run. FixBrokerDataDir works through handles.
const (
	fixAccess = "hyperbutler doctor --fix limits the folder and everything in it to you, SYSTEM and Administrators (stop hyperbutler first); if other accounts could write to it, a new folder is the safe choice (hyperbutler delete, then hyperbutler start --data <a folder that does not exist yet>), as files they opened before stay open to them"
	fixOwner  = "an administrator makes you the owner of the folder (its Properties, Security, Advanced, Owner), then hyperbutler doctor --fix does the rest; or use a new folder: hyperbutler delete, then hyperbutler start --data <a folder that does not exist yet>"
	fixOther  = "use a folder on a local NTFS drive that is not a link: hyperbutler delete, then hyperbutler start --data <a folder that does not exist yet>"
	fixRoot   = "a drive's root cannot be the data folder: hyperbutler delete, then hyperbutler start --data <a folder on that drive>"
	fixDisk   = "hyperbutler delete removes the data disk (and the images and containers in it); the next start makes a new one"
	fixEntry  = "move or delete it yourself, then hyperbutler doctor --fix again; or use a new folder: hyperbutler delete, then hyperbutler start --data <a folder that does not exist yet>"
)

// maxFixEntries bounds the folder walk: a data folder holds a handful of files.
const maxFixEntries = 10000

const (
	fileListDirectory  = 0x0001
	fileTraverse       = 0x0020
	fileReadAttributes = 0x0080
)

// openDirForFix opens dir as a directory handle, not through a reparse point, with what
// the check reads; with fix also WRITE_DAC, to change its DACL.
func openDirForFix(dir string, fix bool) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	access := uint32(fileListDirectory | fileTraverse | fileReadAttributes | windows.READ_CONTROL | windows.SYNCHRONIZE)
	if fix {
		access |= windows.WRITE_DAC
	}
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)
	if fix {
		// Read only: a folder someone holds open to add files is refused, and while the
		// fix holds it, it cannot be renamed nor opened to add files.
		share = windows.FILE_SHARE_READ
	}
	return windows.CreateFile(p, access, share, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
}

// CheckBrokerDataDir checks dir and its data disk as the broker service checks them, as
// this user, so start and doctor can say why and how to fix it before the service
// refuses it. It returns a *DataDirError for a refusal.
func CheckBrokerDataDir(dir string) error {
	return checkBrokerDataDir(dir, false)
}

// FixBrokerDataDir fixes what CheckBrokerDataDir finds, where this user may: a folder of
// theirs that lets others in, and everything in it, get a protected DACL for them,
// SYSTEM and Administrators. It works only through handles: the folder is opened without
// following a reparse point and must be the folder dir names (no link on the way), and
// every entry below it is opened relative to its parent's handle, never through a link.
// It first checks every entry (a plain directory, or a plain file with one link, owned by
// the user, none a reparse point, none in use by a VM) and changes nothing unless all
// pass; then it sets the DACLs on those same handles, which rewrites nothing else. It
// returns what CheckBrokerDataDir returns afterwards.
func FixBrokerDataDir(dir string) error {
	return checkBrokerDataDir(dir, true)
}

func checkBrokerDataDir(dir string, fix bool) error {
	if v := filepath.VolumeName(dir); v == "" || strings.EqualFold(filepath.Clean(dir+`\`), v+`\`) {
		return &DataDirError{Dir: dir, Reason: "a drive's root, or not an absolute path", Fix: fixRoot}
	}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sid := tu.User.Sid.String()
	h, err := openDirForFix(dir, fix)
	if err != nil {
		return fmt.Errorf("engine: open the data directory %s: %w", dir, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if fix {
		// A junction put in place of a folder on the way leads elsewhere; the handle
		// must be the folder dir names.
		if final, err := imageset.FinalPathOf(h); err != nil || !samePathName(final, dir) {
			return &DataDirError{Dir: dir, Reason: fmt.Sprintf("it opens as %q, through a link", final), Fix: fixOther, Err: err}
		}
	}

	var problem *DataDirError
	if err := broker.ValidateDataDir(h, sid); err != nil {
		reason := strings.TrimPrefix(err.Error(), "broker: data location: ")
		if !aclReason(reason) {
			fx := fixOther
			if strings.HasPrefix(reason, "owner is not the caller") {
				fx = fixOwner
			}
			return &DataDirError{Dir: dir, Reason: reason, Fix: fx, Err: err}
		}
		problem = &DataDirError{Dir: dir, Reason: reason, Fix: fixAccess, Fixable: true, Err: err}
	} else if dp := checkDataDisk(h, sid); dp != nil {
		problem = &DataDirError{Dir: dir, Reason: "its data disk " + dp.reason, Fix: fixDisk, Err: dp.err}
		if aclReason(dp.reason) {
			problem.Fix, problem.Fixable = fixAccess, true
		}
	}
	if problem == nil || !fix || !problem.Fixable {
		if problem == nil {
			return nil
		}
		return problem
	}

	// Fix. The folder first: ValidateDataDir found it the user's, and narrowing it ends
	// other users' adding entries by path. Then every entry, checked and held open
	// before any is changed; then their DACLs, on those handles. An entry that appeared
	// in between is caught by listing the tree again.
	if err := setPrivate(h, tu.User.Sid, true); err != nil {
		return fmt.Errorf("engine: limit the access of %s: %w", dir, err)
	}
	entries, ferr := collectEntries(h, tu.User.Sid)
	defer func() {
		for _, e := range entries {
			_ = windows.CloseHandle(e.h)
		}
	}()
	if ferr != nil {
		return &DataDirError{Dir: dir, Reason: ferr.Error(), Fix: fixEntry, Err: ferr}
	}
	for _, e := range entries {
		if err := setPrivate(e.h, tu.User.Sid, e.dir); err != nil {
			return fmt.Errorf("engine: limit the access of %s: %w", filepath.Join(dir, e.name), err)
		}
	}
	if extra, err := newEntries(h, entries); err != nil || extra != "" {
		if err == nil {
			err = fmt.Errorf("%s appeared while the fix ran", extra)
		}
		return &DataDirError{Dir: dir, Reason: err.Error(), Fix: fixEntry + " (another account may be writing to it)", Err: err}
	}
	// A file may have got a second name (a hard link) since it was checked.
	for _, e := range entries {
		var bi windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(e.h, &bi); err != nil || (!e.dir && bi.NumberOfLinks != 1) {
			err = fmt.Errorf("%s got another name while the fix ran", e.name)
			return &DataDirError{Dir: dir, Reason: err.Error(), Fix: fixEntry + " (another account may be writing to it)", Err: err}
		}
	}
	// The final check on the held handle: the path is not resolved again.
	if err := broker.ValidateDataDir(h, sid); err != nil {
		return &DataDirError{Dir: dir, Reason: strings.TrimPrefix(err.Error(), "broker: data location: "), Fix: fixOther, Err: err}
	}
	if dp := checkDataDisk(h, sid); dp != nil {
		return &DataDirError{Dir: dir, Reason: "its data disk " + dp.reason, Fix: fixDisk, Err: dp.err}
	}
	return nil
}

// newEntries lists the tree below h again, through the held directory handles, and
// returns the first name not among entries.
func newEntries(h windows.Handle, entries []entry) (string, error) {
	known := make(map[string]bool, len(entries))
	for _, e := range entries {
		known[strings.ToLower(e.name)] = true
	}
	dirs := []entry{{h: h}}
	for _, e := range entries {
		if e.dir {
			dirs = append(dirs, e)
		}
	}
	for _, d := range dirs {
		names, err := dirNames(d.h)
		if err != nil {
			return "", fmt.Errorf("cannot list %s again: %w", relOrDot(d.name), err)
		}
		for _, n := range names {
			if name := filepath.Join(d.name, n); !known[strings.ToLower(name)] {
				return name, nil
			}
		}
	}
	return "", nil
}

// diskProblem is a data disk refusal: the rule's reason, and the error.
type diskProblem struct {
	reason string
	err    error
}

// checkDataDisk runs the service's data disk check below h; nil when it passes, there is
// no disk, or the disk cannot be opened (as while a VM runs: the service says that).
func checkDataDisk(h windows.Handle, sid string) *diskProblem {
	err := broker.CheckDataDisk(h, sid)
	if err == nil || isNotFound(err) {
		return nil
	}
	reason := strings.TrimPrefix(err.Error(), broker.ErrDiskRejected.Error()+": ")
	reason = strings.TrimPrefix(reason, broker.DataDiskName+" ")
	if !diskRuleReason(reason) {
		return nil
	}
	return &diskProblem{reason, err}
}

// entry is one checked file or directory below the data folder, held open.
type entry struct {
	h    windows.Handle
	name string // relative to the data folder
	dir  bool
}

// collectEntries opens every entry below the directory handle h, relative to its
// parent's handle, and checks each; on the first that fails it returns what it opened
// so far and an error naming the entry.
func collectEntries(h windows.Handle, me *windows.SID) ([]entry, error) {
	var out []entry
	var walk func(parent windows.Handle, rel string) error
	walk = func(parent windows.Handle, rel string) error {
		names, err := dirNames(parent)
		if err != nil {
			return fmt.Errorf("cannot list %s: %w", relOrDot(rel), err)
		}
		for _, n := range names {
			if len(out) >= maxFixEntries {
				return fmt.Errorf("more than %d entries", maxFixEntries)
			}
			name := filepath.Join(rel, n)
			// FILE_READ_DATA (FILE_LIST_DIRECTORY) makes the open take part in sharing;
			// sharing read only refuses an entry someone holds open to write.
			eh, err := broker.OpenEntry(parent, n, fileListDirectory|fileReadAttributes|windows.READ_CONTROL|windows.WRITE_DAC|windows.SYNCHRONIZE, windows.FILE_SHARE_READ)
			if err != nil {
				why := "cannot open it"
				switch {
				case errors.Is(err, windows.STATUS_SHARING_VIOLATION):
					why = "another program holds it open to write (stop hyperbutler, or the program)"
				case errors.Is(err, windows.STATUS_ACCESS_DENIED):
					why = "you may not read it or change its access"
				}
				return fmt.Errorf("%s: %s: %w", name, why, err)
			}
			var bi windows.ByHandleFileInformation
			if err := windows.GetFileInformationByHandle(eh, &bi); err != nil {
				_ = windows.CloseHandle(eh)
				return fmt.Errorf("cannot read %s: %w", name, err)
			}
			isDir := bi.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
			out = append(out, entry{h: eh, name: name, dir: isDir})
			switch {
			case bi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0:
				return fmt.Errorf("%s is a link (a junction or symbolic link)", name)
			case !isDir && bi.NumberOfLinks != 1:
				return fmt.Errorf("%s has %d names (a hard link)", name, bi.NumberOfLinks)
			}
			sd, err := windows.GetSecurityInfo(eh, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
			if err != nil {
				return fmt.Errorf("cannot read the security of %s: %w", name, err)
			}
			if o, _, err := sd.Owner(); err != nil || o == nil || !o.Equals(me) {
				return fmt.Errorf("%s is not yours", name)
			}
			if hasVMAccount(sd) {
				return fmt.Errorf("%s is in use by a VM (stop hyperbutler first)", name)
			}
			if isDir {
				if err := walk(eh, name); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return out, walk(h, "")
}

func relOrDot(rel string) string {
	if rel == "" {
		return "the folder"
	}
	return rel
}

// dirNames lists the names in the directory handle h (not . and ..).
func dirNames(h windows.Handle) ([]string, error) {
	var names []string
	// 8-byte aligned, as the kernel writes FILE_FULL_DIR_INFO with 64-bit fields.
	buf := unsafe.Slice((*byte)(unsafe.Pointer(&make([]uint64, 8<<10)[0])), 64<<10)
	class := uint32(windows.FileFullDirectoryRestartInfo)
	for {
		err := windows.GetFileInformationByHandleEx(h, class, &buf[0], uint32(len(buf)))
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
		class = windows.FileFullDirectoryInfo
		got, err := parseFullDirInfo(buf)
		if err != nil {
			return nil, err
		}
		names = append(names, got...)
	}
}

// parseFullDirInfo reads the names (not . and ..) from a buffer of FILE_FULL_DIR_INFO
// records: NextEntryOffset at 0, FileNameLength at 60, FileName at 68.
func parseFullDirInfo(buf []byte) ([]string, error) {
	var names []string
	for off := 0; ; {
		if off < 0 || off+68 > len(buf) {
			return nil, errors.New("bad directory information")
		}
		next := int(binary.LittleEndian.Uint32(buf[off:]))
		nlen := int(binary.LittleEndian.Uint32(buf[off+60:]))
		if nlen%2 != 0 || nlen > len(buf)-off-68 {
			return nil, errors.New("bad directory information")
		}
		u := make([]uint16, nlen/2)
		for i := range u {
			u[i] = binary.LittleEndian.Uint16(buf[off+68+2*i:])
		}
		if n := windows.UTF16ToString(u); n != "." && n != ".." {
			names = append(names, n)
		}
		if next == 0 {
			return names, nil
		}
		if next < 68 || next > len(buf)-off {
			return nil, errors.New("bad directory information")
		}
		off += next
	}
}

// hasVMAccount reports whether sd's DACL has an ACE for a virtual machine account
// (S-1-5-83-...), which HCS adds to a disk a VM uses.
func hasVMAccount(sd *windows.SECURITY_DESCRIPTOR) bool {
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return false
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(dacl, i, &ace) != nil || ace.Header.AceType > 1 {
			// Only allow and deny ACEs have the SID where ACCESS_ALLOWED_ACE says;
			// the fix replaces the DACL anyway.
			continue
		}
		if strings.HasPrefix((*windows.SID)(unsafe.Pointer(&ace.SidStart)).String(), "S-1-5-83-") {
			return true
		}
	}
	return false
}

// setPrivate gives the object h a protected DACL of full control for sid, SYSTEM and
// Administrators (inherited by what a directory holds), with SetKernelObjectSecurity:
// unlike SetSecurityInfo it rewrites nothing below h, where an entry may be a hard link
// to a file outside (each entry is set on its own handle).
func setPrivate(h windows.Handle, sid *windows.SID, dir bool) error {
	inherit := ""
	if dir {
		inherit = "OICI"
	}
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;%[1]s;FA;;;%[2]s)(A;%[1]s;FA;;;SY)(A;%[1]s;FA;;;BA)", inherit, sid))
	if err != nil {
		return err
	}
	err = windows.SetKernelObjectSecurity(h, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, sd)
	runtime.KeepAlive(sd)
	return err
}

// samePathName reports whether final, a handle's final path (\\?\C:\...), names dir,
// which may be written with a \\?\ prefix or with short (8.3) names.
func samePathName(final, dir string) bool {
	final = strings.TrimPrefix(final, `\\?\`)
	abs, err := filepath.Abs(strings.TrimPrefix(dir, `\\?\`))
	if err != nil {
		return false
	}
	if u, err := windows.UTF16PtrFromString(abs); err == nil {
		buf := make([]uint16, windows.MAX_LONG_PATH)
		if n, err := windows.GetLongPathName(u, &buf[0], uint32(len(buf))); err == nil && n > 0 && int(n) < len(buf) {
			abs = windows.UTF16ToString(buf[:n])
		}
	}
	return strings.EqualFold(filepath.Clean(final), filepath.Clean(abs))
}

// aclReason reports whether a check's reason is about the DACL alone, which the fix
// replaces (conditional and other unusual ACEs included).
func aclReason(reason string) bool {
	return strings.HasPrefix(reason, "DACL") || strings.HasPrefix(reason, "has a NULL DACL") ||
		strings.HasPrefix(reason, "has no DACL") || strings.HasPrefix(reason, "cannot read the DACL")
}

// isNotFound reports whether err is the data disk not being there yet.
func isNotFound(err error) bool {
	var st windows.NTStatus
	return errors.As(err, &st) && (st == windows.STATUS_OBJECT_NAME_NOT_FOUND || st == windows.STATUS_OBJECT_PATH_NOT_FOUND)
}

// diskRuleReason reports whether reason is one of checkDiskInfo's.
func diskRuleReason(reason string) bool {
	for _, p := range []string{"is a reparse point", "is a directory", "is not a regular file", "is not on a local disk",
		"has ", "is not owned by the caller", "DACL", "cannot read the DACL"} {
		if strings.HasPrefix(reason, p) {
			return true
		}
	}
	return false
}

// IsDataDirError reports whether err is a refusal CheckBrokerDataDir explains.
func IsDataDirError(err error) (*DataDirError, bool) {
	var de *DataDirError
	ok := errors.As(err, &de)
	return de, ok
}

// readPrivateFile reads path, at most maxFileSize bytes, if it is a plain file with one
// link, owned by this user (or by Administrators or SYSTEM, as a file an elevated shell
// made is), whose DACL lets in only them, SYSTEM and Administrators (deny ACEs aside):
// what the engine trusts. The checks and the read use one handle, so the file cannot
// be swapped in between. A missing file is os.ErrNotExist. disk.json decides whether the
// guest formats the data disk, so another account able to change it could have the
// disk wiped.
func readPrivateFile(path string) ([]byte, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	const fileReadData = 0x0001
	h, err := windows.CreateFile(p, fileReadData|fileReadAttributes|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return nil, fmt.Errorf("%s: %w", path, os.ErrNotExist)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var bi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &bi); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	switch {
	case bi.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0:
		return nil, fmt.Errorf("%s is not a plain file", path)
	case bi.NumberOfLinks != 1:
		return nil, fmt.Errorf("%s has %d names (a hard link)", path, bi.NumberOfLinks)
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	trusted := func(s *windows.SID) bool {
		return s.Equals(tu.User.Sid) || s.IsWellKnown(windows.WinLocalSystemSid) || s.IsWellKnown(windows.WinBuiltinAdministratorsSid)
	}
	if o, _, err := sd.Owner(); err != nil || o == nil || !trusted(o) {
		return nil, fmt.Errorf("%s belongs to another account", path)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return nil, fmt.Errorf("%s has no DACL (every account may change it)", path)
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		switch ace.Header.AceType {
		case 1: // access denied
			continue
		case 0: // access allowed
		default:
			return nil, fmt.Errorf("%s has an ACE of type %d", path, ace.Header.AceType)
		}
		if sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)); !trusted(sid) {
			name := sid.String()
			if a, d, _, err := sid.LookupAccount(""); err == nil {
				name = d + `\` + a
			}
			return nil, fmt.Errorf("%s can be opened by another account (%s)", path, name)
		}
	}
	buf := make([]byte, maxFileSize+1)
	n := 0
	for n < len(buf) {
		var got uint32
		if err := windows.ReadFile(h, buf[n:], &got, nil); err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		if got == 0 {
			break
		}
		n += int(got)
	}
	if n > maxFileSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxFileSize)
	}
	return buf[:n], nil
}
