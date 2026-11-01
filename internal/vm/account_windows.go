//go:build windows

package vm

import (
	"fmt"
	"os"
	"strings"
	"unsafe"

	"github.com/Microsoft/go-winio/pkg/guid"
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

// ProbeRuntimeID returns the runtime id that compute system id has, or will have once
// created: HcsGrantVmAccess names the VM's account before the VM exists, because the
// runtime id is a name-based GUID. It grants id access to a new, empty file of its own
// and reads the account back, so no file in use is touched; the file is then deleted.
func ProbeRuntimeID(id string) (guid.GUID, error) {
	f, err := os.CreateTemp("", "hyperbutler-vmaccount-*")
	if err != nil {
		return guid.GUID{}, err
	}
	name := f.Name()
	_ = f.Close()
	defer func() { _ = os.Remove(name) }()
	if err := GrantVMAccess(id, name); err != nil {
		return guid.GUID{}, err
	}
	sids, err := AccountsOnFile(name)
	if err != nil {
		return guid.GUID{}, err
	}
	if len(sids) != 1 {
		return guid.GUID{}, fmt.Errorf("vm: HcsGrantVmAccess for %s added %d VM accounts, want 1", id, len(sids))
	}
	return RuntimeIDFromSID(sids[0])
}
