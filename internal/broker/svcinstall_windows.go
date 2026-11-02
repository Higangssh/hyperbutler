//go:build windows

package broker

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// InstallConfig is how the broker service is registered. BuildInstallConfig fills it,
// InstallService applies it.
type InstallConfig struct {
	Name, DisplayName, Description string
	// Exe is the broker executable; CommandLine is the service's command line: Exe in
	// quotes, always, then "run" and the flags.
	Exe         string
	CommandLine string
	// StartType is SERVICE_AUTO_START, delayed: the broker is needed only once a user
	// starts hyperbutler, so it need not hold up the boot.
	StartType        uint32
	DelayedAutoStart bool
	ErrorControl     uint32
	// Account is LocalSystem (ADR 0004, Implementation decision 1).
	Account string
	// SidType gives the service its own SID (NT SERVICE\hyperbutler-broker) in its token,
	// which option C of ADR 0004 builds on.
	SidType uint32
	// RequiredPrivileges is every privilege the service's token keeps; the SCM removes
	// all others of LocalSystem's (see servicePrivileges).
	RequiredPrivileges []string
	// Recovery and ResetPeriod (seconds) are the SCM's failure actions: restart twice,
	// then nothing until the failure count resets.
	Recovery    []mgr.RecoveryAction
	ResetPeriod uint32
	// RecoverOnNonCrashFailures applies Recovery also when the service stops with a
	// nonzero exit code (a start that failed, or the server stopping on its own), not
	// only when the process dies.
	RecoverOnNonCrashFailures bool
}

// servicePrivileges is what the broker's token keeps:
//
//   - SeImpersonatePrivilege: ImpersonateNamedPipeClient, to identify each caller and to
//     open its data directory and process as that caller (ADR 0004 rule 4);
//   - SeChangeNotifyPrivilege: traverse checking, which every path open relies on (the
//     SCM keeps it in any case).
//
// HCS access comes from the account (LocalSystem), not from a privilege. Nothing else
// LocalSystem holds (SeDebugPrivilege, SeTcbPrivilege, SeBackupPrivilege,
// SeRestorePrivilege, SeTakeOwnershipPrivilege, SeLoadDriverPrivilege, ...) is needed, so a
// broker bug cannot use it.
var servicePrivileges = []string{"SeChangeNotifyPrivilege", "SeImpersonatePrivilege"}

// The recovery actions: restart after 5 s, then after 30 s, then nothing; the failure
// count resets after a day without failures.
var serviceRecovery = []mgr.RecoveryAction{
	{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
	{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	{Type: mgr.NoAction},
}

const serviceResetPeriod = 24 * 60 * 60

// serviceArg is a flag the service command line may carry: a bare flag, so the command
// line needs no quoting beyond the executable's.
var serviceArg = regexp.MustCompile(`^-[a-z][a-z-]{0,39}$`)

// BuildInstallConfig returns the service configuration for the broker executable exe,
// run with the extra flags args after "run". It checks only the form of exe; see
// CheckServiceBinaryPath for where it lies.
func BuildInstallConfig(exe string, args []string) (InstallConfig, error) {
	if !filepath.IsAbs(exe) || filepath.Clean(exe) != exe {
		return InstallConfig{}, fmt.Errorf("broker: service executable %q is not a clean absolute path", exe)
	}
	// The SCM keeps the command line as REG_EXPAND_SZ, so a '%' would expand.
	if strings.ContainsAny(exe, "\"%\x00") || !strings.EqualFold(filepath.Ext(exe), ".exe") {
		return InstallConfig{}, fmt.Errorf("broker: service executable %q", exe)
	}
	cmd := `"` + exe + `" run`
	for _, a := range args {
		if !serviceArg.MatchString(a) {
			return InstallConfig{}, fmt.Errorf("broker: service argument %q", a)
		}
		cmd += " " + a
	}
	return InstallConfig{
		Name:                      ServiceName,
		DisplayName:               ServiceDisplayName,
		Description:               ServiceDescription,
		Exe:                       exe,
		CommandLine:               cmd,
		StartType:                 windows.SERVICE_AUTO_START,
		DelayedAutoStart:          true,
		ErrorControl:              windows.SERVICE_ERROR_NORMAL,
		Account:                   "LocalSystem",
		SidType:                   windows.SERVICE_SID_TYPE_UNRESTRICTED,
		RequiredPrivileges:        append([]string(nil), servicePrivileges...),
		Recovery:                  append([]mgr.RecoveryAction(nil), serviceRecovery...),
		ResetPeriod:               serviceResetPeriod,
		RecoverOnNonCrashFailures: true,
	}, nil
}

// Rights that let someone other than SYSTEM, Administrators and TrustedInstaller
// replace what the service runs. On the executable and its folder that is any write: a
// new file in the folder can be a DLL the executable loads. On every folder above, it
// is what renames or replaces a child (adding a new folder to C:\ is harmless).
const (
	genericAll      = 0x10000000
	genericWrite    = 0x40000000
	fileDeleteChild = 0x40

	leafWriteMask = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA |
		fileDeleteChild | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER |
		genericAll | genericWrite
	ancestorWriteMask = fileDeleteChild | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | genericAll
)

// trustedWriters may write where the service binary lies: SYSTEM, Administrators,
// TrustedInstaller (which owns Program Files) and OWNER RIGHTS (whose ACE applies to the
// owner, which must be one of the others).
var trustedWriters = map[string]bool{
	"S-1-5-18":     true,
	"S-1-5-32-544": true,
	"S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464": true,
	"S-1-3-4": true,
}

// CheckServiceBinaryPath refuses a service executable that someone other than SYSTEM,
// Administrators or TrustedInstaller could replace, which would let them run code as
// SYSTEM: it must be on a local fixed drive, and neither the file, its folder nor any
// folder above may be a reparse point, be owned by anyone else, or grant anyone else
// write access (leafWriteMask, ancestorWriteMask). Program Files passes; a user's
// folder does not.
func CheckServiceBinaryPath(exe string) error {
	vol := filepath.VolumeName(exe)
	if len(vol) != 2 || vol[1] != ':' || !filepath.IsAbs(exe) {
		return fmt.Errorf("%s is not on a drive letter", exe)
	}
	root, err := windows.UTF16PtrFromString(vol + `\`)
	if err != nil {
		return err
	}
	if t := windows.GetDriveType(root); t != windows.DRIVE_FIXED {
		return fmt.Errorf("%s is not on a local fixed drive (drive type %d)", exe, t)
	}
	p := filepath.Clean(exe)
	for i := 0; ; i++ {
		mask := uint32(ancestorWriteMask)
		if i < 2 {
			mask = leafWriteMask
		}
		if err := checkWriters(p, mask); err != nil {
			return err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return nil
		}
		p = parent
	}
}

// checkWriters checks one file or folder of CheckServiceBinaryPath.
func checkWriters(path string, mask uint32) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var tag struct{ FileAttributes, ReparseTag uint32 }
	if err := windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&tag)), uint32(unsafe.Sizeof(tag))); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if tag.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("%s is a reparse point (a link or junction)", path)
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("%s: read its security: %w", path, err)
	}
	if err := checkWritersSD(sd, mask); err != nil {
		return fmt.Errorf("%s %w", path, err)
	}
	return nil
}

// checkWritersSD is checkWriters' rule on a security descriptor: a trusted owner, and
// no allow ACE that applies to the object grants mask to anyone untrusted. Deny ACEs
// are not credited, and ACE types this check does not model are refused.
func checkWritersSD(sd *windows.SECURITY_DESCRIPTOR, mask uint32) error {
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return fmt.Errorf("has no owner: %w", err)
	}
	if !trustedWriters[owner.String()] || owner.String() == "S-1-3-4" {
		return fmt.Errorf("is owned by %s, not by SYSTEM or Administrators", owner)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("has no DACL: %w", err)
	}
	if dacl == nil {
		return errors.New("has a NULL DACL: everyone may write it")
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return fmt.Errorf("has an unreadable DACL: %w", err)
		}
		switch ace.Header.AceType {
		case accessDeniedACEType:
			continue
		case accessAllowedACEType:
		default:
			return fmt.Errorf("has an ACE of type %d", ace.Header.AceType)
		}
		if ace.Header.AceFlags&inheritOnlyACE != 0 {
			continue // applies to children only, each checked on its own
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if uint32(ace.Mask)&mask != 0 && !trustedWriters[sid.String()] {
			return fmt.Errorf("lets %s change it (access 0x%x)", sid, uint32(ace.Mask))
		}
	}
	return nil
}

// ErrServiceExists is returned by InstallService when the service is already installed.
var ErrServiceExists = errors.New("broker: the service is already installed; uninstall it first")

// ErrServiceMarkedForDelete is returned by InstallService and UninstallService when the
// service was deleted but its process has not ended: the SCM removes it only then, and
// until then it can be neither installed again nor deleted again.
var ErrServiceMarkedForDelete = errors.New("broker: the service is uninstalled but its process has not ended yet; " +
	"it goes away when hyperbutler-broker.exe exits (end it in Task Manager, or restart Windows), and then it can be installed again")

// markedForDelete reports whether the service s is marked for deletion: a no-op
// configuration change fails with ERROR_SERVICE_MARKED_FOR_DELETE then.
func markedForDelete(s *mgr.Service) bool {
	const noChange = windows.SERVICE_NO_CHANGE
	err := windows.ChangeServiceConfig(s.Handle, noChange, noChange, noChange, nil, nil, nil, nil, nil, nil, nil)
	return errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE)
}

// serviceRequiredPrivilegesInfo is SERVICE_REQUIRED_PRIVILEGES_INFO.
type serviceRequiredPrivilegesInfo struct {
	RequiredPrivileges *uint16 // MULTI_SZ
}

func multiSZ(ss []string) []uint16 {
	var out []uint16
	for _, s := range ss {
		out = append(out, windows.StringToUTF16(s)...)
	}
	return append(out, 0)
}

// InstallService checks cfg.Exe with CheckServiceBinaryPath, registers the event log
// source and the service as cfg says, and starts it. It needs an elevated caller. If a
// step fails, it undoes the ones before, and a failed undo is part of the error; a
// service that is installed but fails to start stays installed, so its event log says
// why.
func InstallService(cfg InstallConfig) (err error) {
	if err := CheckServiceBinaryPath(cfg.Exe); err != nil {
		return fmt.Errorf("broker: refusing to install: the service would run this file as SYSTEM, but %w", err)
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service control manager (run as administrator): %w", err)
	}
	defer func() { _ = m.Disconnect() }()
	if s, err := m.OpenService(cfg.Name); err == nil {
		marked := markedForDelete(s)
		_ = s.Close()
		if marked {
			return ErrServiceMarkedForDelete
		}
		return ErrServiceExists
	}
	// A source left by an earlier install is replaced, so its message file is ours.
	_ = eventlog.Remove(EventSource)
	if err := eventlog.InstallAsEventCreate(EventSource, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
		return fmt.Errorf("register the event log source: %w", err)
	}
	// installed is set once the service is complete: a failed start then keeps it.
	installed := false
	defer func() {
		if err != nil && !installed {
			if rerr := eventlog.Remove(EventSource); rerr != nil {
				err = errors.Join(err, fmt.Errorf("undo: remove the event log source: %w", rerr))
			}
		}
	}()
	str := func(s string) *uint16 {
		p, _ := windows.UTF16PtrFromString(s)
		return p
	}
	for _, s := range []string{cfg.Name, cfg.DisplayName, cfg.CommandLine, cfg.Account, cfg.Description} {
		if strings.ContainsRune(s, 0) {
			return errors.New("broker: NUL in the service configuration")
		}
	}
	h, err := windows.CreateService(m.Handle, str(cfg.Name), str(cfg.DisplayName), windows.SERVICE_ALL_ACCESS,
		windows.SERVICE_WIN32_OWN_PROCESS, cfg.StartType, cfg.ErrorControl, str(cfg.CommandLine),
		nil, nil, nil, str(cfg.Account), nil)
	if errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		return ErrServiceMarkedForDelete
	}
	if err != nil {
		return fmt.Errorf("create the service: %w", err)
	}
	s := &mgr.Service{Name: cfg.Name, Handle: h}
	defer func() {
		if err != nil && !installed {
			if derr := s.Delete(); derr != nil {
				err = errors.Join(err, fmt.Errorf("undo: delete the half-installed service: %w", derr))
			}
		}
		_ = s.Close()
	}()
	desc := windows.SERVICE_DESCRIPTION{Description: str(cfg.Description)}
	if err := windows.ChangeServiceConfig2(h, windows.SERVICE_CONFIG_DESCRIPTION, (*byte)(unsafe.Pointer(&desc))); err != nil {
		return fmt.Errorf("set the description: %w", err)
	}
	if cfg.DelayedAutoStart {
		d := windows.SERVICE_DELAYED_AUTO_START_INFO{IsDelayedAutoStartUp: 1}
		if err := windows.ChangeServiceConfig2(h, windows.SERVICE_CONFIG_DELAYED_AUTO_START_INFO, (*byte)(unsafe.Pointer(&d))); err != nil {
			return fmt.Errorf("set delayed start: %w", err)
		}
	}
	sidType := cfg.SidType
	if err := windows.ChangeServiceConfig2(h, windows.SERVICE_CONFIG_SERVICE_SID_INFO, (*byte)(unsafe.Pointer(&sidType))); err != nil {
		return fmt.Errorf("set the service SID type: %w", err)
	}
	privs := multiSZ(cfg.RequiredPrivileges)
	pi := serviceRequiredPrivilegesInfo{RequiredPrivileges: &privs[0]}
	if err := windows.ChangeServiceConfig2(h, windows.SERVICE_CONFIG_REQUIRED_PRIVILEGES_INFO, (*byte)(unsafe.Pointer(&pi))); err != nil {
		return fmt.Errorf("set the required privileges: %w", err)
	}
	if err := s.SetRecoveryActions(cfg.Recovery, cfg.ResetPeriod); err != nil {
		return fmt.Errorf("set the recovery actions: %w", err)
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(cfg.RecoverOnNonCrashFailures); err != nil {
		return fmt.Errorf("set the recovery actions: %w", err)
	}
	installed = true
	if err := s.Start(); err != nil {
		return &startError{fmt.Errorf("installed, but did not start: %w", err)}
	}
	if err := waitService(s, svc.Running, serviceStartHint); err != nil {
		return &startError{fmt.Errorf("installed, but did not start (the Application event log, source %s, says why): %w", EventSource, err)}
	}
	return nil
}

// startError is an InstallService failure after the service was installed; it stays
// installed.
type startError struct{ err error }

func (e *startError) Error() string { return e.err.Error() }
func (e *startError) Unwrap() error { return e.err }

// UninstallService stops the service, waits for it to stop, deletes it and removes its
// event log source. A service that is not installed is not an error.
func UninstallService(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service control manager (run as administrator): %w", err)
	}
	defer func() { _ = m.Disconnect() }()
	s, err := m.OpenService(name)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		_ = eventlog.Remove(EventSource)
		return nil
	}
	if err != nil {
		return fmt.Errorf("open the service: %w", err)
	}
	defer func() { _ = s.Close() }()
	var stopErr error
	if q, err := s.Query(); err != nil || q.State != svc.Stopped {
		if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			stopErr = fmt.Errorf("stop the service: %w", err)
		} else {
			// The service gives up on a stop after serviceStopLimit and exits.
			stopErr = waitService(s, svc.Stopped, serviceStopLimit+15*time.Second)
		}
	}
	marked := false
	if err := s.Delete(); errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		marked = true // an earlier uninstall deleted it already
	} else if err != nil {
		return fmt.Errorf("delete the service: %w", err)
	}
	if err := eventlog.Remove(EventSource); err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return fmt.Errorf("remove the event log source: %w", err)
	}
	if stopErr != nil || marked {
		// Deleted, but the SCM removes a service only once its process has ended.
		return errors.Join(ErrServiceMarkedForDelete, stopErr)
	}
	return nil
}

// waitService polls s until it is in state want, for at most timeout.
func waitService(s *mgr.Service, want svc.State, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		q, err := s.Query()
		if err != nil {
			return err
		}
		if q.State == want {
			return nil
		}
		if want == svc.Running && q.State == svc.Stopped {
			return fmt.Errorf("the service stopped (exit code %d, service exit code %d)", q.Win32ExitCode, q.ServiceSpecificExitCode)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the service is still %s after %v", StateName(q.State), timeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
