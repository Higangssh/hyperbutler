package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// Commands are the icacls commands of Fix, in order (none when Fix says it in
	// words): each one for cmd or PowerShell as it is.
	Commands []string
	Err      error
}

func (e *DataDirError) Error() string {
	return fmt.Sprintf("the broker service refuses the data directory %s: %s; %s", e.Dir, e.Reason, e.Fix)
}

func (e *DataDirError) Unwrap() error { return e.Err }

// CheckBrokerDataDir checks dir as the broker service checks it, as this user, so start
// and doctor can say why and how to fix it before the service refuses it. It returns a
// *DataDirError for a refusal.
func CheckBrokerDataDir(dir string) error {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	h, err := openDirHandle(dir)
	if err != nil {
		return fmt.Errorf("engine: open the data directory %s: %w", dir, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	sid := tu.User.Sid.String()
	if err := broker.ValidateDataDir(h, sid); err != nil {
		reason := strings.TrimPrefix(err.Error(), "broker: data location: ")
		fix, cmds := dataDirFix(dir, reason, accountName(tu.User.Sid))
		return &DataDirError{Dir: dir, Reason: reason, Fix: fix, Commands: cmds, Err: err}
	}
	// The disk in it, once there is one: its own ACEs (set on it directly, or kept from a
	// copy) are checked too, and the folder's fix resets them.
	if _, err := os.Lstat(filepath.Join(dir, broker.DataDiskName)); err != nil {
		return nil
	}
	if err := broker.CheckDataDisk(h, sid); err != nil {
		reason := strings.TrimPrefix(err.Error(), broker.ErrDiskRejected.Error()+": ")
		reason = strings.TrimPrefix(reason, broker.DataDiskName+" ")
		if !diskRuleReason(reason) {
			// Not the disk rule (it could not be opened, as while the VM runs): the
			// service says that itself.
			return nil
		}
		why := reason
		switch {
		case strings.HasPrefix(reason, "DACL"), strings.HasPrefix(reason, "has a NULL DACL"), strings.HasPrefix(reason, "has no DACL"):
		case strings.HasPrefix(reason, "is not owned by the caller"):
			why = "owner is not the caller"
		default:
			return &DataDirError{Dir: dir, Reason: "its data disk " + reason, Err: err,
				Fix: "hyperbutler delete removes the data disk (and the images and containers in it); the next start makes a new one"}
		}
		fix, cmds := dataDirFix(dir, why, accountName(tu.User.Sid))
		return &DataDirError{Dir: dir, Reason: "its data disk " + reason, Fix: fix, Commands: cmds, Err: err}
	}
	return nil
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

// dataDirFix says how to make dir pass, for the reason the check gave, and returns the
// commands it names. The access fix first resets the folder and what is in it to what
// they inherit (/reset /T drops ACEs set on them directly, the data disk's too), then
// cuts the inheritance and grants only the three accounts the service admits, which the
// disk then inherits. A path with % gets no command: cmd would expand it.
func dataDirFix(dir, reason, account string) (string, []string) {
	dir = strings.TrimRight(dir, `\`)
	access := strings.HasPrefix(reason, "DACL") || strings.HasPrefix(reason, "has a NULL DACL") || strings.HasPrefix(reason, "has no DACL")
	owner := strings.HasPrefix(reason, "owner is not the caller")
	if !access && !owner {
		return "use a folder on a local NTFS drive that is not a link (hyperbutler delete, then hyperbutler start --data <folder>)", nil
	}
	if strings.Contains(dir, "%") {
		if owner {
			return "an administrator makes you the owner of the folder and what is in it, then limit its access to you, SYSTEM and Administrators, without inheritance (the folder's name has a %, so no command is given for cmd)", nil
		}
		return "limit the access of the folder and what is in it to you, SYSTEM and Administrators, without inheritance (the folder's name has a %, so no command is given for cmd)", nil
	}
	cmds := []string{
		fmt.Sprintf(`icacls "%s" /reset /T /C`, dir),
		fmt.Sprintf(`icacls "%s" /inheritance:r /grant:r "%s:(OI)(CI)F" "*S-1-5-18:(OI)(CI)F" "*S-1-5-32-544:(OI)(CI)F"`, dir, account),
	}
	steps := fmt.Sprintf("limit its access to you, SYSTEM and Administrators: %s, then %s", cmds[0], cmds[1])
	if owner {
		own := fmt.Sprintf(`icacls "%s" /setowner "%s" /T /C`, dir, account)
		return fmt.Sprintf("an administrator makes you its owner: %s; then %s", own, steps), append([]string{own}, cmds...)
	}
	return steps, cmds
}

// accountName is sid's DOMAIN\name, or the SID in icacls's *S-1-... form.
func accountName(sid *windows.SID) string {
	if a, d, _, err := sid.LookupAccount(""); err == nil && a != "" {
		if d != "" {
			return d + `\` + a
		}
		return a
	}
	return "*" + sid.String()
}

// IsDataDirError reports whether err is a refusal CheckBrokerDataDir explains.
func IsDataDirError(err error) (*DataDirError, bool) {
	var de *DataDirError
	ok := errors.As(err, &de)
	return de, ok
}
