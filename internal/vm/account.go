package vm

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	"github.com/Microsoft/go-winio/pkg/guid"
)

// AccountPrefix starts the account of every VM worker process: NT VIRTUAL MACHINE\<id>.
const AccountPrefix = "S-1-5-83-1-"

// AccountSID is the account of the VM worker process of the compute system whose
// runtime id is rid: AccountPrefix and the GUID's bytes as four little-endian 32-bit
// numbers. HcsGrantVmAccess gives this account access to a file.
func AccountSID(rid guid.GUID) string {
	b := rid.ToWindowsArray()
	return fmt.Sprintf("%s%d-%d-%d-%d", AccountPrefix,
		binary.LittleEndian.Uint32(b[0:]), binary.LittleEndian.Uint32(b[4:]), binary.LittleEndian.Uint32(b[8:]), binary.LittleEndian.Uint32(b[12:]))
}

// RuntimeIDFromSID inverts AccountSID.
func RuntimeIDFromSID(sid string) (guid.GUID, error) {
	rest, ok := strings.CutPrefix(sid, AccountPrefix)
	parts := strings.Split(rest, "-")
	if !ok || len(parts) != 4 {
		return guid.GUID{}, fmt.Errorf("vm: %q is not a VM account", sid)
	}
	var b [16]byte
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return guid.GUID{}, fmt.Errorf("vm: %q is not a VM account", sid)
		}
		binary.LittleEndian.PutUint32(b[4*i:], uint32(n))
	}
	return guid.FromWindowsArray(b), nil
}
