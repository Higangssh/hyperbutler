//go:build windows

package engine

import (
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// HypervAdminsSID is BUILTIN\Hyper-V Administrators, the group that may use HCS (ADR 0004).
const HypervAdminsSID = "S-1-5-32-578"

var procNetUserGetLocalGroups = windows.NewLazySystemDLL("netapi32.dll").NewProc("NetUserGetLocalGroups")

// HyperVAdminsInToken reports whether this process's token has Hyper-V Administrators.
func HyperVAdminsInToken() (bool, error) {
	sid, err := windows.StringToSid(HypervAdminsSID)
	if err != nil {
		return false, err
	}
	return windows.Token(0).IsMember(sid)
}

// HyperVAdminsInAccount reports whether the account running this process is a member of
// Hyper-V Administrators now, as the local account database says, directly or through a
// group. Unlike the token, this sees a membership added after the user signed in.
func HyperVAdminsInAccount() (bool, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false, err
	}
	user, domain, _, err := tu.User.Sid.LookupAccount("")
	if err != nil {
		return false, err
	}
	gsid, err := windows.StringToSid(HypervAdminsSID)
	if err != nil {
		return false, err
	}
	group, _, _, err := gsid.LookupAccount("")
	if err != nil {
		return false, err
	}
	name, err := windows.UTF16PtrFromString(domain + `\` + user)
	if err != nil {
		return false, err
	}
	const lgIncludeIndirect = 1
	var buf *byte
	var read, total uint32
	r, _, _ := procNetUserGetLocalGroups.Call(0, uintptr(unsafe.Pointer(name)), 0, lgIncludeIndirect,
		uintptr(unsafe.Pointer(&buf)), 0xFFFFFFFF, uintptr(unsafe.Pointer(&read)), uintptr(unsafe.Pointer(&total)))
	if buf != nil {
		defer func() { _ = windows.NetApiBufferFree(buf) }()
	}
	if r != 0 {
		return false, windows.Errno(r)
	}
	// LOCALGROUP_USERS_INFO_0: one LPWSTR per group.
	names := unsafe.Slice((**uint16)(unsafe.Pointer(buf)), read)
	for _, n := range names {
		if strings.EqualFold(windows.UTF16PtrToString(n), group) {
			return true, nil
		}
	}
	return false, nil
}

// HyperVAdminsSignInPending reports whether the account is in Hyper-V Administrators but
// this sign-in's token is not yet: Windows applies group changes at sign-in, so the user
// signs out and back in (or restarts). False when either check fails.
func HyperVAdminsSignInPending() bool {
	inToken, err := HyperVAdminsInToken()
	if err != nil || inToken {
		return false
	}
	inAccount, err := HyperVAdminsInAccount()
	return err == nil && inAccount
}
