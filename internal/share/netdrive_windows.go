//go:build windows

package share

import (
	"strings"

	"golang.org/x/sys/windows"
)

// remoteDrive reports whether the drive of the absolute drive path p ("Z:\x") is a
// network drive in this logon session (a mapped share, or a subst of one). It is a
// variable for the tests.
var remoteDrive = func(p string) bool {
	root, err := windows.UTF16PtrFromString(strings.ToUpper(p[:1]) + `:\`)
	if err != nil {
		return false
	}
	return windows.GetDriveType(root) == windows.DRIVE_REMOTE
}

// checkLocalDrive refuses a drive path on a network drive (REQ-FS-006): the share
// server would serve it over SMB, slowly and without change notifications it can rely
// on, and the engine's logon session may not even see the user's mapping.
func checkLocalDrive(winPath, p string) error {
	if remoteDrive(p) {
		return NetworkDriveError(winPath, strings.ToUpper(p[:2]))
	}
	return nil
}

// checkDriveOf is checkLocalDrive for a drive path that may be behind "\\?\".
func checkDriveOf(src string) error {
	return checkLocalDrive(src, strings.TrimPrefix(src, `\\?\`))
}
