package vm

import (
	"testing"

	"github.com/Higangssh/hyperbutler/internal/guid"
)

func TestAccountSID(t *testing.T) {
	// Measured: a VM with this runtime id ran as this account (the DACL of its disks).
	rid, err := guid.FromString("e9059f6c-20aa-5c55-9333-25140a12719b")
	if err != nil {
		t.Fatal(err)
	}
	const sid = "S-1-5-83-1-3909459820-1549082794-337982355-2607878666"
	if s := AccountSID(rid); s != sid {
		t.Fatalf("AccountSID = %q", s)
	}
	if back, err := RuntimeIDFromSID(sid); err != nil || back != rid {
		t.Fatalf("RuntimeIDFromSID = %v, %v", back, err)
	}
	for _, bad := range []string{"", "S-1-5-83-1-1-2-3", "S-1-5-83-1-1-2-3-4-5", "S-1-5-83-1-1-2-3-x", "S-1-5-21-1-2-3-4", "S-1-5-83-1-1-2-3-4294967296"} {
		if _, err := RuntimeIDFromSID(bad); err == nil {
			t.Errorf("RuntimeIDFromSID(%q) accepted", bad)
		}
	}
}
