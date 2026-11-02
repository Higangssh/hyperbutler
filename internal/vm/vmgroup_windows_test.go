//go:build windows

package vm

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestVMGroupCanRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "system.vhd")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		sddl string
		want bool
	}{
		{"D:P(A;;FA;;;OW)(A;;0x1200a9;;;S-1-5-83-0)", true},                // the installer's read and execute
		{"D:P(A;;FA;;;OW)(A;;GR;;;S-1-5-83-0)", true},                      // generic read
		{"D:P(A;;FA;;;OW)", false},                                         // no entry for the group
		{"D:P(A;;FA;;;OW)(A;;0x100000;;;S-1-5-83-0)", false},               // SYNCHRONIZE only
		{"D:P(A;;FA;;;OW)(A;OIIO;GR;;;S-1-5-83-0)", false},                 // inherit-only: not for this file
		{"D:P(A;;FA;;;OW)(D;;FR;;;S-1-5-83-0)(A;;GR;;;S-1-5-83-0)", false}, // denied
		{"D:P(A;;FA;;;OW)(A;;GR;;;S-1-5-83-1-1-2-3-4)", false},             // one VM's account, not the group
	} {
		sd, err := windows.SecurityDescriptorFromString(c.sddl)
		if err != nil {
			t.Fatal(c.sddl, err)
		}
		dacl, _, err := sd.DACL()
		if err != nil {
			t.Fatal(err)
		}
		if err := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
			t.Fatal(c.sddl, err)
		}
		if got := VMGroupCanRead(p); got != c.want {
			t.Errorf("%s: %v, want %v", c.sddl, got, c.want)
		}
	}
	if VMGroupCanRead(filepath.Join(t.TempDir(), "missing")) {
		t.Error("a missing file is readable")
	}
}
