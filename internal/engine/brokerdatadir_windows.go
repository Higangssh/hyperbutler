package engine

import (
	"errors"
	"fmt"
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
	Err              error
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
	err = broker.ValidateDataDir(h, tu.User.Sid.String())
	if err == nil {
		return nil
	}
	reason := strings.TrimPrefix(err.Error(), "broker: data location: ")
	return &DataDirError{Dir: dir, Reason: reason, Fix: dataDirFix(dir, reason, accountName(tu.User.Sid)), Err: err}
}

// dataDirFix says how to make dir pass, for the reason the check gave.
func dataDirFix(dir, reason, account string) string {
	grant := fmt.Sprintf(`icacls "%s" /inheritance:r /grant:r "%s:(OI)(CI)F" "*S-1-5-18:(OI)(CI)F" "*S-1-5-32-544:(OI)(CI)F"`, dir, account)
	switch {
	case strings.HasPrefix(reason, "owner is not the caller"):
		return fmt.Sprintf(`an administrator makes you its owner: icacls "%s" /setowner "%s" /T, then you limit its access: %s`, dir, account, grant)
	case strings.HasPrefix(reason, "DACL"), strings.HasPrefix(reason, "has a NULL DACL"), strings.HasPrefix(reason, "has no DACL"):
		return "limit its access to you, SYSTEM and Administrators: " + grant
	}
	return "use a folder on a local NTFS drive that is not a link (hyperbutler delete, then hyperbutler start --data <folder>)"
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
