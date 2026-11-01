//go:build windows

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/vm"
)

// --- VM access entries ----------------------------------------------------------------

// vmSIDPrefix starts the account of every VM worker process: NT VIRTUAL MACHINE\<id>.
const vmSIDPrefix = "S-1-5-83-1-"

// vmSID is the account of the VM worker process of the compute system whose runtime id
// is runtimeID: S-1-5-83-1- and the GUID's bytes as four little-endian 32-bit numbers.
func vmSID(runtimeID string) (string, error) {
	g, err := guid.FromString(runtimeID)
	if err != nil {
		return "", err
	}
	b := g.ToWindowsArray()
	return fmt.Sprintf("%s%d-%d-%d-%d", vmSIDPrefix,
		binary.LittleEndian.Uint32(b[0:]), binary.LittleEndian.Uint32(b[4:]), binary.LittleEndian.Uint32(b[8:]), binary.LittleEndian.Uint32(b[12:])), nil
}

// vmACEs returns the VM accounts (S-1-5-83-1-...) that path's DACL names.
func vmACEs(path string) ([]string, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return nil, err
		}
		// Allowed and denied entries share this layout; the SID starts at SidStart.
		s := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		if strings.HasPrefix(s, vmSIDPrefix) && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out, nil
}

// revokeSID removes every entry for sid from path's DACL, keeping the DACL's
// protection (inheritance) as it is.
func revokeSID(path, sid string) error {
	s, err := windows.StringToSid(sid)
	if err != nil {
		return err
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	ctl, _, err := sd.Control()
	if err != nil {
		return err
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessMode: windows.REVOKE_ACCESS,
		Trustee:    windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeValue: windows.TrusteeValueFromSID(s)},
	}}, dacl)
	if err != nil {
		return err
	}
	var info windows.SECURITY_INFORMATION = windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	if ctl&windows.SE_DACL_PROTECTED != 0 {
		info = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, info, nil, nil, acl, nil)
}

// liveVMSIDs returns the VM accounts of every compute system that exists now, of any
// owner: their access entries are in use.
func liveVMSIDs() (map[string]bool, error) {
	list, err := vm.List("")
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	for _, s := range list {
		m, err := vm.Open(s.ID)
		if err != nil {
			return nil, fmt.Errorf("open compute system %s: %w", s.ID, err)
		}
		rid, err := m.RuntimeID()
		_ = m.Close()
		if err != nil {
			return nil, fmt.Errorf("compute system %s: %w", s.ID, err)
		}
		if sid, err := vmSID(rid.String()); err == nil {
			live[sid] = true
		}
	}
	return live, nil
}

// --- removing files -------------------------------------------------------------------

// removeVisible removes a file, retrying for up to timeout while something still has it
// open: the VM worker that just exited, or an antivirus scan of the freed data disk
// (minutes for a large one). After a few seconds it says on w what it waits for. A file
// still in use at the end is reported with the processes that hold it.
func removeVisible(p string, timeout time.Duration, w io.Writer) error {
	deadline := time.Now().Add(timeout)
	said := false
	for start := time.Now(); ; {
		err := os.Remove(p)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return err
		}
		if time.Now().After(deadline) {
			if h := fileHolders(p); h != "" {
				return fmt.Errorf("%w; in use by %s", err, h)
			}
			return err
		}
		if !said && time.Since(start) > 3*time.Second {
			_, _ = fmt.Fprintf(w, "hyperbutler: waiting up to %v for %s to be released (an antivirus scan of a large file takes a while)...\n", timeout.Round(time.Second), p)
			said = true
		}
		time.Sleep(200 * time.Millisecond)
	}
}

var (
	modRstrtmgr             = windows.NewLazySystemDLL("rstrtmgr.dll")
	procRmStartSession      = modRstrtmgr.NewProc("RmStartSession")
	procRmRegisterResources = modRstrtmgr.NewProc("RmRegisterResources")
	procRmGetList           = modRstrtmgr.NewProc("RmGetList")
	procRmEndSession        = modRstrtmgr.NewProc("RmEndSession")
)

// rmProcessInfo is RM_PROCESS_INFO.
type rmProcessInfo struct {
	PID          uint32
	StartTime    windows.Filetime
	AppName      [256]uint16
	ServiceName  [64]uint16
	AppType      uint32
	AppStatus    uint32
	TSSessionID  uint32
	BRestartable int32
}

// fileHolders names the processes that have path open ("hyperbutler.exe (pid 42)"), as
// the Restart Manager reports them, or "" if it cannot tell.
func fileHolders(path string) string {
	if modRstrtmgr.Load() != nil {
		return ""
	}
	var session uint32
	var key [33]uint16
	if r, _, _ := procRmStartSession.Call(uintptr(unsafe.Pointer(&session)), 0, uintptr(unsafe.Pointer(&key[0]))); r != 0 {
		return ""
	}
	defer procRmEndSession.Call(uintptr(session))
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return ""
	}
	if r, _, _ := procRmRegisterResources.Call(uintptr(session), 1, uintptr(unsafe.Pointer(&p)), 0, 0, 0, 0); r != 0 {
		return ""
	}
	var infos [16]rmProcessInfo
	var needed, n uint32 = 0, uint32(len(infos))
	var reasons uint32
	r, _, _ := procRmGetList.Call(uintptr(session), uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(&infos[0])), uintptr(unsafe.Pointer(&reasons)))
	if r != 0 && r != uintptr(windows.ERROR_MORE_DATA) {
		return ""
	}
	var names []string
	for _, in := range infos[:min(n, uint32(len(infos)))] {
		names = append(names, fmt.Sprintf("%s (pid %d)", windows.UTF16ToString(in.AppName[:]), in.PID))
	}
	if needed > n {
		names = append(names, fmt.Sprintf("%d more", needed-n))
	}
	return strings.Join(names, ", ")
}
