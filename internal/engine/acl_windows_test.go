//go:build windows

package engine

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// checkUserOnly fails unless path's DACL is protected and grants the current user only.
func checkUserOnly(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	want, err := CurrentUserSDDL()
	if err != nil {
		t.Fatal(err)
	}
	// AI (auto-inherited) is a flag for propagation to children, not a grant.
	if got := strings.Replace(sd.String(), "D:PAI(", "D:P(", 1); got != want {
		t.Fatalf("%s: DACL %s, want %s", path, got, want)
	}
}
