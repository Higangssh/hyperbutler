//go:build windows

package main

// hyperbutler setup: the machine-wide steps the MSI installer runs (dist/msi), elevated and
// as SYSTEM. It is not in the usage text: a user never needs it, and every verb refuses to
// run without an elevated token.
//
//	hyperbutler setup hyperv-admins add --sid SID
//	hyperbutler setup hyperv-admins remove --sid SID
//	hyperbutler setup hyperv-admins remove --all
//	hyperbutler setup vm-platform
//
// add puts one user account into Hyper-V Administrators (ADR 0004, Installer) and records
// it under HKLM, but only when this call added it: a user who was a member before stays
// one after an uninstall. Every account an install or upgrade added is kept in the record,
// so an upgrade run by another user does not lose the first one. remove --sid (the
// installer's rollback) takes out that account only if the record has it; remove --all
// (uninstall) takes out the recorded accounts. Neither removes an account the record does
// not name. Each verb also leaves its outcome in LastResult, for `hyperbutler doctor` and
// for support, because the installer goes on when a step fails.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	hypervAdminsSID = "S-1-5-32-578"
	// setupKey holds what the installer changed, so the uninstaller undoes only that. It is
	// under HKLM, which only administrators can write.
	setupKey         = `SOFTWARE\hyperbutler\Setup`
	setupAddedValue  = "HyperVAdminsAdded" // REG_MULTI_SZ of SIDs
	setupResultValue = "LastResult"
)

var (
	modNetapi32                 = windows.NewLazySystemDLL("netapi32.dll")
	procNetLocalGroupAddMembers = modNetapi32.NewProc("NetLocalGroupAddMembers")
	procNetLocalGroupDelMembers = modNetapi32.NewProc("NetLocalGroupDelMembers")
)

// Errors of NetLocalGroupAddMembers and NetLocalGroupDelMembers.
const (
	errMemberInAlias    = windows.Errno(1378) // ERROR_MEMBER_IN_ALIAS
	errMemberNotInAlias = windows.Errno(1377) // ERROR_MEMBER_NOT_IN_ALIAS
	errNoSuchMember     = windows.Errno(1387) // ERROR_NO_SUCH_MEMBER
)

func cmdSetup(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usagef("setup: hyperv-admins add|remove, or vm-platform")
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("setup: needs an elevated process; the hyperbutler installer runs it")
	}
	var step string
	var err error
	switch args[0] {
	case "hyperv-admins":
		if len(args) < 2 {
			return usagef("setup hyperv-admins: add --sid SID, or remove --sid SID|--all")
		}
		step = "hyperv-admins " + args[1]
		fs := newFlagSet("setup "+step, stderr)
		sid := fs.String("sid", "", "the user account's SID")
		var all *bool
		if args[1] == "remove" {
			all = fs.Bool("all", false, "every account the installer added")
		}
		if err := parseArgs(fs, args[2:]); err != nil {
			return err
		}
		switch {
		case args[1] == "add":
			err = setupAddHyperVAdmin(setupRecord{registry.LOCAL_MACHINE, setupKey}, *sid, stdout)
		case args[1] == "remove" && *all == (*sid != ""):
			return usagef("setup hyperv-admins remove: give --sid SID or --all")
		case args[1] == "remove":
			err = setupRemoveHyperVAdmins(setupRecord{registry.LOCAL_MACHINE, setupKey}, *sid, stdout)
		default:
			return usagef("setup hyperv-admins: unknown verb %q", args[1])
		}
	case "vm-platform":
		if err := parseArgs(newFlagSet("setup vm-platform", stderr), args[1:]); err != nil {
			return err
		}
		step = "vm-platform"
		err = setupVMPlatform(stdout)
	default:
		return usagef("setup: unknown command %q", args[0])
	}
	setupRecord{registry.LOCAL_MACHINE, setupKey}.result(step, err)
	return err
}

// checkSetupUserSID accepts only one person's account: a user of a machine or an Active
// Directory domain (S-1-5-21-...) or of Microsoft Entra ID (S-1-12-1-...). Not a group, a
// built-in or service account, or SYSTEM, whose SID a deployment tool's install has.
func checkSetupUserSID(sid string, use uint32) error {
	if use != windows.SidTypeUser {
		return fmt.Errorf("%s is not a user account (type %d)", sid, use)
	}
	if !strings.HasPrefix(sid, "S-1-5-21-") && !strings.HasPrefix(sid, "S-1-12-1-") {
		return fmt.Errorf("%s is a built-in or service account", sid)
	}
	return nil
}

// setupUserSID parses the SID the installer passes ([UserSID], set by Windows Installer
// for the user the install runs for) and checks what it names.
func setupUserSID(s string) (*windows.SID, string, error) {
	if s == "" {
		return nil, "", errors.New("no --sid")
	}
	sid, err := windows.StringToSid(s)
	if err != nil {
		return nil, "", fmt.Errorf("--sid %q: %w", s, err)
	}
	if !strings.EqualFold(sid.String(), s) {
		return nil, "", fmt.Errorf("--sid %q is not in canonical form", s)
	}
	user, domain, use, err := sid.LookupAccount("")
	if err != nil {
		return nil, "", fmt.Errorf("account %s: %w", s, err)
	}
	if err := checkSetupUserSID(sid.String(), use); err != nil {
		return nil, "", err
	}
	return sid, domain + `\` + user, nil
}

// setupRecord is where the installer keeps what it changed: HKLM\SOFTWARE\hyperbutler\Setup,
// or a test key.
type setupRecord struct {
	root registry.Key
	path string
}

func (r setupRecord) added() ([]string, error) {
	k, err := registry.OpenKey(r.root, r.path, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	defer k.Close()
	v, _, err := k.GetStringsValue(setupAddedValue)
	if errors.Is(err, registry.ErrNotExist) {
		return nil, nil
	}
	return v, err
}

// setAdded writes the list, deleting the value (and the key, once it is empty) for none.
func (r setupRecord) setAdded(sids []string) error {
	k, _, err := registry.CreateKey(r.root, r.path, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if len(sids) > 0 {
		return k.SetStringsValue(setupAddedValue, sids)
	}
	if err := k.DeleteValue(setupAddedValue); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// result records a verb's outcome; failures to record are not the verb's failure.
func (r setupRecord) result(step string, err error) {
	k, _, kerr := registry.CreateKey(r.root, r.path, registry.SET_VALUE)
	if kerr != nil {
		return
	}
	defer k.Close()
	s := "ok"
	if err != nil {
		s = err.Error()
		if len(s) > 1024 {
			s = s[:1024]
		}
	}
	_ = k.SetStringValue(setupResultValue, fmt.Sprintf("%s %s: %s", time.Now().UTC().Format(time.RFC3339), step, s))
}

// delete removes the record's key if nothing is left in it (the uninstall's last step).
func (r setupRecord) deleteIfEmpty() {
	if v, err := r.added(); err == nil && len(v) == 0 {
		_ = registry.DeleteKey(r.root, r.path)
	}
}

// localGroupMembersInfo0 is LOCALGROUP_MEMBERS_INFO_0.
type localGroupMembersInfo0 struct{ sid *windows.SID }

// hypervAdminsName is the group's name in this Windows' language (it is "Hyper-V
// Administratoren" in German); the Net API takes the name, not the SID.
func hypervAdminsName() (string, error) {
	sid, err := windows.StringToSid(hypervAdminsSID)
	if err != nil {
		return "", err
	}
	name, _, _, err := sid.LookupAccount("")
	if err != nil {
		return "", fmt.Errorf("the Hyper-V Administrators group (%s): %w", hypervAdminsSID, err)
	}
	return name, nil
}

// netLocalGroup is swapped out by tests.
var netLocalGroup = func(proc *windows.LazyProc, member *windows.SID) error {
	group, err := hypervAdminsName()
	if err != nil {
		return err
	}
	g, err := windows.UTF16PtrFromString(group)
	if err != nil {
		return err
	}
	info := localGroupMembersInfo0{sid: member}
	r, _, _ := proc.Call(0, uintptr(unsafe.Pointer(g)), 0, uintptr(unsafe.Pointer(&info)), 1)
	if r != 0 {
		return windows.Errno(r)
	}
	return nil
}

// lookupSetupSID is swapped out by tests.
var lookupSetupSID = setupUserSID

func setupAddHyperVAdmin(rec setupRecord, s string, stdout io.Writer) error {
	sid, name, err := lookupSetupSID(s)
	if err != nil {
		return fmt.Errorf("setup hyperv-admins add: %w", err)
	}
	switch err := netLocalGroup(procNetLocalGroupAddMembers, sid); {
	case errors.Is(err, errMemberInAlias):
		// Not recorded: the uninstaller leaves a membership it did not make.
		_, _ = fmt.Fprintf(stdout, "%s (%s) is already in Hyper-V Administrators\n", name, sid)
		return nil
	case err != nil:
		return fmt.Errorf("setup hyperv-admins add %s: %w", name, err)
	}
	have, err := rec.added()
	if err == nil && !slices.Contains(have, sid.String()) {
		err = rec.setAdded(append(have, sid.String()))
	}
	if err != nil {
		return fmt.Errorf("setup hyperv-admins add: added %s, but cannot record it: %w", name, err)
	}
	_, _ = fmt.Fprintf(stdout, "added %s (%s) to Hyper-V Administrators; it applies from the next sign-in\n", name, sid)
	return nil
}

// setupRemoveHyperVAdmins takes out the recorded accounts, or only the one given (if the
// record has it), and drops them from the record.
func setupRemoveHyperVAdmins(rec setupRecord, only string, stdout io.Writer) error {
	have, err := rec.added()
	if err != nil {
		return fmt.Errorf("setup hyperv-admins remove: %w", err)
	}
	var keep []string
	var errs []error
	for _, s := range have {
		if only != "" && !strings.EqualFold(s, only) {
			keep = append(keep, s)
			continue
		}
		sid, err := windows.StringToSid(s)
		if err != nil || checkSetupUserSID(s, windows.SidTypeUser) != nil {
			// Not ours to act on; drop it from the record.
			errs = append(errs, fmt.Errorf("the recorded account %q is not a user SID", s))
			continue
		}
		switch err := netLocalGroup(procNetLocalGroupDelMembers, sid); {
		case errors.Is(err, errMemberNotInAlias), errors.Is(err, errNoSuchMember):
			_, _ = fmt.Fprintf(stdout, "%s is no longer in Hyper-V Administrators\n", s)
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", s, err))
			keep = append(keep, s)
		default:
			_, _ = fmt.Fprintf(stdout, "removed %s from Hyper-V Administrators\n", s)
		}
	}
	if len(have) == 0 || (only != "" && len(keep) == len(have)) {
		_, _ = fmt.Fprintln(stdout, "the record names no such account; nothing to remove")
	}
	if err := rec.setAdded(keep); err != nil {
		errs = append(errs, err)
	}
	if only == "" {
		rec.deleteIfEmpty()
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("setup hyperv-admins remove: %w", err)
	}
	return nil
}

// setupVMPlatform turns Virtual Machine Platform on with DISM, unless it already is (or
// is waiting for a restart). It never restarts Windows; the installer asks for that.
func setupVMPlatform(stdout io.Writer) error {
	dism := filepath.Join(os.Getenv("SystemRoot"), "System32", "dism.exe")
	if !filepath.IsAbs(dism) {
		return errors.New("setup vm-platform: SystemRoot is not set")
	}
	var out bytes.Buffer
	c := exec.Command(dism, "/English", "/Online", "/Get-FeatureInfo", "/FeatureName:VirtualMachinePlatform")
	c.Stdout, c.Stderr = &out, &out
	if err := c.Run(); err != nil {
		return fmt.Errorf("setup vm-platform: dism /Get-FeatureInfo: %w\n%s", err, out.Bytes())
	}
	switch st := dismFeatureState(out.String()); st {
	case "Enabled", "Enable Pending":
		_, _ = fmt.Fprintf(stdout, "Virtual Machine Platform: %s\n", st)
		return nil
	case "":
		return fmt.Errorf("setup vm-platform: no State in dism's output:\n%s", out.Bytes())
	}
	out.Reset()
	c = exec.Command(dism, "/English", "/Online", "/Enable-Feature", "/FeatureName:VirtualMachinePlatform", "/All", "/NoRestart", "/Quiet")
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) && ee.ExitCode() == 3010 {
		err = nil // ERROR_SUCCESS_REBOOT_REQUIRED
	}
	if err != nil {
		return fmt.Errorf("setup vm-platform: dism /Enable-Feature: %w\n%s", err, out.Bytes())
	}
	_, _ = fmt.Fprintln(stdout, "Virtual Machine Platform: enabled; restart Windows to finish")
	return nil
}

// dismFeatureState is the "State : ..." line of dism /English /Get-FeatureInfo.
func dismFeatureState(out string) string {
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(l, ":")
		if ok && strings.TrimSpace(k) == "State" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
