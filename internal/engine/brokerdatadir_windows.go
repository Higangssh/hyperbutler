package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

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
	// Commands are the icacls commands of Fix, in order, for cmd (none when Fix says
	// it in words).
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
		fix, cmds := dataDirFix(dir, reason, accountName(tu.User.Sid), diskExists(dir), hasLinks(dir), foreignGrants(dir, tu.User.Sid))
		return &DataDirError{Dir: dir, Reason: reason, Fix: fix, Commands: cmds, Err: err}
	}
	// The disk in it, once there is one: its own ACEs (set on it directly, or kept from a
	// copy) are checked too, and the folder's fix resets them.
	if !diskExists(dir) {
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
		fix, cmds := dataDirFix(dir, why, accountName(tu.User.Sid), true, hasLinks(dir), foreignGrants(dir, tu.User.Sid))
		return &DataDirError{Dir: dir, Reason: "its data disk " + reason, Fix: fix, Commands: cmds, Err: err}
	}
	return nil
}

// diskExists reports whether dir holds the data disk, as a plain file.
func diskExists(dir string) bool {
	fi, err := os.Lstat(filepath.Join(dir, broker.DataDiskName))
	return err == nil && fi.Mode().IsRegular()
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
// commands it names, for cmd. They act on the folder and its data disk only, never
// recursively (icacls /T follows junctions out of the folder): the folder's inheritance
// is cut and only the three accounts the service admits are granted, which closes it at
// once; then the grants set on the folder itself for other accounts (foreign) are
// removed; then the disk, if there is one, is reset to inherit that (its own ACEs go). The
// owner's fix makes the user the owner of both first. No command is given for a path
// cmd or PowerShell would change (% $ `), a drive's root, or a folder with links in it
// (disk tells whether there is a data disk; links whether the folder holds a link).
func dataDirFix(dir, reason, account string, disk, links bool, foreign []string) (string, []string) {
	dir = strings.TrimRight(dir, `\`)
	access := strings.HasPrefix(reason, "DACL") || strings.HasPrefix(reason, "has a NULL DACL") || strings.HasPrefix(reason, "has no DACL")
	owner := strings.HasPrefix(reason, "owner is not the caller")
	if !access && !owner {
		return "use a folder on a local NTFS drive that is not a link (hyperbutler delete, then hyperbutler start --data <folder>)", nil
	}
	vol := filepath.VolumeName(dir)
	switch {
	case vol == "" || strings.EqualFold(filepath.Clean(dir+`\`), vol+`\`):
		return "a drive's root cannot be the data folder: hyperbutler delete, then hyperbutler start --data <a folder on that drive>", nil
	case strings.ContainsAny(dir, "%$`"):
		return words(owner) + " (the folder's name has % or $ or `, which a shell would expand, so no command is given)", nil
	case links:
		return words(owner) + " (the folder holds a link, a junction or a symbolic link, which a fix must not follow: remove it first; no command is given)", nil
	}
	var cmds []string
	targets := []string{dir}
	if disk {
		targets = append(targets, filepath.Join(dir, broker.DataDiskName))
	}
	if owner {
		for _, t := range targets {
			cmds = append(cmds, fmt.Sprintf(`icacls "%s" /setowner "%s"`, t, account))
		}
	}
	cmds = append(cmds, fmt.Sprintf(`icacls "%s" /inheritance:r /grant:r "%s:(OI)(CI)F" "*S-1-5-18:(OI)(CI)F" "*S-1-5-32-544:(OI)(CI)F"`, dir, account))
	if len(foreign) > 0 {
		// Grants set on the folder itself stay through the above; they were there
		// before, so taking them out second opens no window.
		r := make([]string, len(foreign))
		for i, f := range foreign {
			r[i] = "*" + f
		}
		cmds = append(cmds, fmt.Sprintf(`icacls "%s" /remove:g %s`, dir, strings.Join(r, " ")))
	}
	if disk {
		cmds = append(cmds, fmt.Sprintf(`icacls "%s" /reset`, targets[1]))
	}
	if owner {
		return fmt.Sprintf("an administrator runs, in cmd: %s", strings.Join(cmds, ", then ")), cmds
	}
	return fmt.Sprintf("limit its access to you, SYSTEM and Administrators, in cmd: %s", strings.Join(cmds, ", then ")), cmds
}

// words is the fix without commands.
func words(owner bool) string {
	if owner {
		return "an administrator makes you the owner of the folder and its data disk, then limits the folder's access to you, SYSTEM and Administrators, without inheritance, and resets the disk's to inherit that"
	}
	return "limit the folder's access to you, SYSTEM and Administrators, without inheritance, and reset its data disk's to inherit that"
}

// foreignGrants lists the SIDs of the allow ACEs set on dir itself (not inherited) for
// accounts other than caller, SYSTEM and Administrators.
func foreignGrants(dir string, caller *windows.SID) []string {
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return nil
	}
	const accessAllowed, inherited = 0, 0x10
	var out []string
	seen := map[string]bool{}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(dacl, i, &ace) != nil || ace.Header.AceType != accessAllowed || ace.Header.AceFlags&inherited != 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.Equals(caller) || sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
			continue
		}
		if k := sid.String(); !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// hasLinks reports whether dir holds a reparse point (a junction, a symbolic link) right
// in it. It reads the entries without following them.
func hasLinks(dir string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return true
	}
	for _, e := range ents {
		if e.Type()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return true
		}
	}
	return false
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
