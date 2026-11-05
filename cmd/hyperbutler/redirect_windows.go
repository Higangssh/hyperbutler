//go:build windows

package main

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/imageset"
)

// redirectFix is what to do about a redirected folder.
const redirectFix = "run hyperbutler from Windows Terminal, PowerShell or Command Prompt started from the Start menu, not from inside a packaged app"

// redirectedDir reports where Windows really keeps dir when the folder is not where it
// was asked for because this process runs under a packaged app's file system
// virtualization: an MSIX or Store app's processes, and the shells it starts, have
// their writes under %LOCALAPPDATA% redirected to the app's own
// Packages\<app>\LocalCache. The broker service runs outside that package and finds a
// reparse point there instead of the disk, so it refuses it. The process need not have
// a package identity itself for this to happen. A folder that cannot be opened, or that
// a junction or symbolic link of the user's own leads elsewhere, is not reported.
func redirectedDir(dir string) (string, bool) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return "", false
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(h) //nolint:errcheck // read-only handle
	final, err := imageset.FinalPathOf(h)
	if err != nil || engine.SamePath(final, dir) {
		return "", false
	}
	lower := strings.ToLower(final)
	if !strings.Contains(lower, `\appdata\local\packages\`) || !strings.Contains(lower, `\localcache\`) {
		return "", false
	}
	return final, true
}

// redirectError is start's error for a redirected data folder.
func redirectError(dir, final string) error {
	return fmt.Errorf("this terminal runs inside a packaged app, and Windows keeps its files under %%LOCALAPPDATA%% in that app's own folder: %s is really %s, where the broker service cannot use it; %s",
		dir, final, redirectFix)
}

// redirectCheck is doctor's check of the same, for each folder that exists: none of
// them may be redirected.
func redirectCheck(dirs ...string) (DoctorCheck, bool) {
	for _, d := range dirs {
		if final, ok := redirectedDir(d); ok {
			reason := "Windows keeps it at " + final + ", because this terminal runs inside a packaged app"
			return DoctorCheck{ID: "data-dir", Status: doctorFail,
				Summary: "the broker service cannot use the data directory " + d + ": " + reason,
				Fix:     redirectFix, Msg: "doctor.data-dir.refused", FixMsg: "doctor.fix.dataDir",
				Args: msgArgs("dir", d, "reason", reason, "fix", redirectFix)}, true
		}
	}
	return DoctorCheck{}, false
}
