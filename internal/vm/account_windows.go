//go:build windows

package vm

import (
	"fmt"
	"os"
	"strings"
	"unsafe"

	"github.com/Higangssh/hyperbutler/internal/guid"
	"golang.org/x/sys/windows"
)

// AccountsOnFile returns the VM accounts (AccountPrefix...) that path's DACL names.
func AccountsOnFile(path string) ([]string, error) {
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
		if strings.HasPrefix(s, AccountPrefix) && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out, nil
}

// VMGroupSID is NT VIRTUAL MACHINE\Virtual Machines: every VM worker's account is in it.
const VMGroupSID = "S-1-5-83-0"

// VMGroupCanRead reports whether path's DACL lets every VM read it: an allow entry for
// VMGroupSID with read access, as the MSI puts on the guest image in Program Files, where
// the user cannot add a per-VM entry. A deny entry for the group makes it false.
func VMGroupCanRead(path string) bool {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	return VMGroupCanReadSD(sd)
}

// VMGroupCanReadSD is VMGroupCanRead for a security descriptor already read (from a
// handle, say).
func VMGroupCanReadSD(sd *windows.SECURITY_DESCRIPTOR) bool {
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return false // a NULL DACL would grant everyone, but the installer never sets one
	}
	const read = windows.FILE_READ_DATA | windows.GENERIC_READ | windows.GENERIC_ALL
	allowed := false
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		if (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String() != VMGroupSID {
			continue
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			if uint32(ace.Mask)&read != 0 {
				return false
			}
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			if uint32(ace.Mask)&read != 0 {
				allowed = true
			}
		}
	}
	return allowed
}

// ProbeRuntimeID returns the runtime id that compute system id has, or will have once
// created: HcsGrantVmAccess names the VM's account before the VM exists, because the
// runtime id is a name-based GUID. It grants id access to a new, empty file of its own
// and reads the account back, so no file in use is touched; the file is then deleted.
func ProbeRuntimeID(id string) (guid.GUID, error) {
	rid, _, err := ProbeVMAccount("", id)
	return rid, err
}

// ProbeVMAccount is ProbeRuntimeID with the probe file in dir (os.TempDir() if empty),
// and it also returns the access mask HcsGrantVmAccess gave the VM's account, so that a
// caller can grant the same access itself, on a handle, instead of by path.
func ProbeVMAccount(dir, id string) (guid.GUID, uint32, error) {
	f, err := os.CreateTemp(dir, "hyperbutler-vmaccount-*")
	if err != nil {
		return guid.GUID{}, 0, err
	}
	name := f.Name()
	_ = f.Close()
	defer func() { _ = os.Remove(name) }()
	if err := GrantVMAccess(id, name); err != nil {
		return guid.GUID{}, 0, err
	}
	sids, err := AccountsOnFile(name)
	if err != nil {
		return guid.GUID{}, 0, err
	}
	if len(sids) != 1 {
		return guid.GUID{}, 0, fmt.Errorf("vm: HcsGrantVmAccess for %s added %d VM accounts, want 1", id, len(sids))
	}
	rid, err := RuntimeIDFromSID(sids[0])
	if err != nil {
		return guid.GUID{}, 0, err
	}
	mask, err := allowedMask(name, sids[0])
	if err != nil {
		return guid.GUID{}, 0, err
	}
	return rid, mask, nil
}

// allowedMask is the union of the allow entries for sid in path's DACL.
func allowedMask(path, sid string) (uint32, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return 0, err
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return 0, fmt.Errorf("vm: %s has no DACL", path)
	}
	var mask uint32
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return 0, err
		}
		if ace.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE && (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String() == sid {
			mask |= uint32(ace.Mask)
		}
	}
	if mask == 0 {
		return 0, fmt.Errorf("vm: %s grants %s nothing", path, sid)
	}
	return mask, nil
}
