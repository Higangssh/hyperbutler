package bridge

import (
	"errors"
	"fmt"

	"github.com/Higangssh/hyperbutler/internal/dockerapi"
	"github.com/Higangssh/hyperbutler/internal/redact"
)

// Stable error codes (REQ-UX-013): the page shows the translated explanation the
// catalog has under "error.<code>", the code itself, and a button that copies the
// English original. A code never changes meaning; new ones are added.
const (
	CodeBadRequest     = "HB-GUI-001" // the bridge refused a message
	CodeBusy           = "HB-GUI-002" // another operation that changes state is running
	CodeNotEditable    = "HB-GUI-003" // a setting the GUI does not change
	CodeHidden         = "HB-GUI-004" // a timer's refresh while the window is hidden
	CodeStartFailed    = "HB-ENG-001"
	CodeStopFailed     = "HB-ENG-002"
	CodeRestartFailed  = "HB-ENG-003"
	CodeEngineStopped  = "HB-ENG-004"
	CodeOtherEngine    = "HB-ENG-005"
	CodeEngineBusy     = "HB-ENG-006" // starting or stopping: try again shortly
	CodeAccessDenied   = "HB-VM-001"
	CodeVMExited       = "HB-VM-002"
	CodeSignInPending  = "HB-VM-003" // in Hyper-V Administrators, but not this sign-in yet
	CodeBrokerLost     = "HB-VM-004" // the broker service went away, and the VM with it
	CodeBrokerDown     = "HB-VM-005" // the broker service does not answer
	CodeBrokerRefused  = "HB-VM-006" // the broker service answered with an error
	CodeBrokerSquat    = "HB-VM-007" // another program holds a broker pipe name
	CodeBrokerOld      = "HB-VM-008" // the broker service speaks an older protocol
	CodePipeInUse      = "HB-PIPE-001"
	CodeDataDisk       = "HB-DISK-001"
	CodeAPIUnreachable = "HB-API-001"
	CodeAPIError       = "HB-API-002"
	CodeNoContainer    = "HB-API-003"
	CodeConfigValue    = "HB-CFG-001"
	CodeConfigFailed   = "HB-CFG-002"
	CodeDaemonInvalid  = "HB-DMN-001"
	CodeDaemonApply    = "HB-DMN-002"
	CodeDaemonReadOnly = "HB-DMN-003"
	CodeDiagnose       = "HB-DIAG-001"
	CodeDoctor         = "HB-DOC-001"
	CodeOS             = "HB-OS-001"
)

// Codes lists every code, for the catalog completeness test.
var Codes = []string{
	CodeBadRequest, CodeBusy, CodeNotEditable, CodeHidden,
	CodeStartFailed, CodeStopFailed, CodeRestartFailed, CodeEngineStopped, CodeOtherEngine, CodeEngineBusy,
	CodeAccessDenied, CodeVMExited, CodeSignInPending, CodeBrokerLost, CodeBrokerDown, CodeBrokerRefused, CodeBrokerSquat, CodeBrokerOld, CodePipeInUse, CodeDataDisk,
	CodeAPIUnreachable, CodeAPIError, CodeNoContainer,
	CodeConfigValue, CodeConfigFailed, CodeDaemonInvalid, CodeDaemonApply, CodeDaemonReadOnly,
	CodeDiagnose, CodeDoctor, CodeOS, CodeImportUnavailable, CodeImportFailed, CodeDesktopNotRunning, CodeImportSpace, CodeImportBusy, CodeConfirm,
}

// Error is an error with its code: what the page shows. Err's text is the English
// original, which may hold text dockerd or the guest wrote: it is cleaned, redacted
// and bounded before it reaches the page.
type Error struct {
	Code string
	Err  error
}

func (e *Error) Error() string { return e.Code + ": " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// Coded wraps err with code (nil stays nil).
func Coded(code string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Code: code, Err: err}
}

// withCode is err with its own code if it has one (a host's "busy"), else code.
func withCode(code string, err error) error {
	var e *Error
	if err == nil || errors.As(err, &e) {
		return err
	}
	return &Error{Code: code, Err: err}
}

// Codedf is Coded(code, fmt.Errorf(format, a...)).
func Codedf(code, format string, a ...any) error {
	return &Error{Code: code, Err: fmt.Errorf(format, a...)}
}

// ErrEngineStopped is what Host.Docker returns when no engine runs.
var ErrEngineStopped = Codedf(CodeEngineStopped, "the hyperbutler engine is not running")

// codeOf is err's code: its own, or one by the kind of error, or fallback.
func codeOf(err error, fallback string) string {
	var e *Error
	var ae *dockerapi.Error
	var re *dockerapi.RequestError
	switch {
	case errors.As(err, &e):
		return e.Code
	case errors.Is(err, ErrBadRequest):
		return CodeBadRequest
	case dockerapi.IsNotFound(err):
		return CodeNoContainer
	case errors.As(err, &ae):
		return CodeAPIError
	case errors.As(err, &re):
		return CodeAPIUnreachable
	}
	return fallback
}

// maxDetail bounds the English original of one error.
const maxDetail = 8 << 10

// englishText is err as the page may show it: the English original (an API error's
// includes dockerd's message), redacted, cleaned and bounded.
func englishText(err error) string {
	s := err.Error()
	return dockerapi.Clip(dockerapi.CleanText(redact.String(s), true), maxDetail)
}
