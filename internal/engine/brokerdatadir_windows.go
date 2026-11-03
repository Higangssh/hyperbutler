package engine

import (
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/broker"
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
	fixAccess = "hyperbutler doctor --fix limits the folder and its data disk to you, SYSTEM and Administrators (stop hyperbutler first)"
	fixOwner  = "an administrator makes you the owner of the folder (its Properties, Security, Advanced, Owner), then hyperbutler doctor --fix does the rest; or use a new folder: hyperbutler delete, then hyperbutler start --data <a folder that does not exist yet>"
	fixOther  = "use a folder on a local NTFS drive that is not a link: hyperbutler delete, then hyperbutler start --data <a folder that does not exist yet>"
	fixDisk   = "hyperbutler delete removes the data disk (and the images and containers in it); the next start makes a new one"
)

// openDirForFix opens dir as a directory handle, not through a reparse point, with what
// the check reads; with fix also WRITE_DAC, to change its DACL.
func openDirForFix(dir string, fix bool) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	const fileListDirectory, fileTraverse, fileReadAttributes = 0x0001, 0x0020, 0x0080
	access := uint32(fileListDirectory | fileTraverse | fileReadAttributes | windows.READ_CONTROL | windows.SYNCHRONIZE)
	if fix {
		access |= windows.WRITE_DAC
	}
	return windows.CreateFile(p, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
}

// CheckBrokerDataDir checks dir and its data disk as the broker service checks them, as
// this user, so start and doctor can say why and how to fix it before the service
// refuses it. It returns a *DataDirError for a refusal.
func CheckBrokerDataDir(dir string) error {
	return checkBrokerDataDir(dir, false)
}

// FixBrokerDataDir fixes what CheckBrokerDataDir finds, where this user may: a folder of
// theirs that lets others in gets a protected DACL for them, SYSTEM and Administrators,
// and a data disk of theirs gets the same (both set on handles, not through paths). It works through
// a handle to the folder opened without following a reparse point (a link put in its
// place is refused, not followed) and opens the disk relative to it, as the service
// does. It returns what CheckBrokerDataDir returns afterwards.
func FixBrokerDataDir(dir string) error {
	return checkBrokerDataDir(dir, true)
}

func checkBrokerDataDir(dir string, fix bool) error {
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

	if err := broker.ValidateDataDir(h, sid); err != nil {
		reason := strings.TrimPrefix(err.Error(), "broker: data location: ")
		switch {
		case !aclReason(reason):
			fx := fixOther
			if strings.HasPrefix(reason, "owner is not the caller") {
				fx = fixOwner
			}
			return &DataDirError{Dir: dir, Reason: reason, Fix: fx, Err: err}
		case !fix:
			return &DataDirError{Dir: dir, Reason: reason, Fix: fixAccess, Fixable: true, Err: err}
		}
		// The handle is the folder ValidateDataDir found ours and not a link; only its
		// DACL was wrong.
		sd, derr := windows.SecurityDescriptorFromString(privateSDDL(tu.User.Sid))
		if derr != nil {
			return derr
		}
		// SetKernelObjectSecurity, not SetSecurityInfo: the latter also rewrites the
		// inherited ACEs of everything in the folder, and an entry there may be a hard
		// link to a file outside it. The data disk is reset on its own below, after the
		// checks that it is no link.
		err := windows.SetKernelObjectSecurity(h, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, sd)
		runtime.KeepAlive(sd)
		if err != nil {
			return fmt.Errorf("engine: limit the access of %s: %w", dir, err)
		}
		if err := broker.ValidateDataDir(h, sid); err != nil {
			return &DataDirError{Dir: dir, Reason: strings.TrimPrefix(err.Error(), "broker: data location: "), Fix: fixOther, Err: err}
		}
	}

	err = broker.CheckDataDisk(h, sid)
	if err == nil || isNotFound(err) {
		return nil
	}
	reason := strings.TrimPrefix(err.Error(), broker.ErrDiskRejected.Error()+": ")
	reason = strings.TrimPrefix(reason, broker.DataDiskName+" ")
	if !diskRuleReason(reason) {
		// Not the disk rule (it could not be opened, as while the VM runs): the service
		// says that itself.
		return nil
	}
	if !aclReason(reason) {
		return &DataDirError{Dir: dir, Reason: "its data disk " + reason, Fix: fixDisk, Err: err}
	}
	if !fix {
		return &DataDirError{Dir: dir, Reason: "its data disk " + reason, Fix: fixAccess, Fixable: true, Err: err}
	}
	if err := broker.ResetDataDisk(h, sid); err != nil {
		return &DataDirError{Dir: dir, Reason: "its data disk: " + err.Error(), Fix: fixDisk, Err: err}
	}
	if err := broker.CheckDataDisk(h, sid); err != nil && !isNotFound(err) {
		return &DataDirError{Dir: dir, Reason: "its data disk: " + err.Error(), Fix: fixDisk, Err: err}
	}
	return nil
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

// privateSDDL is the DACL hyperbutler gives the data directory: full control for sid,
// SYSTEM and Administrators, inherited by what is in it.
func privateSDDL(sid *windows.SID) string {
	return "D:P(A;OICI;FA;;;" + sid.String() + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
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
