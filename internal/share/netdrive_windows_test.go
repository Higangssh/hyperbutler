//go:build windows

package share

import (
	"errors"
	"strings"
	"testing"
)

// TestRegistryRefusesNetworkDrive: a drive path on a network drive is refused before
// it is looked at (REQ-FS-006); a local drive goes on.
func TestRegistryRefusesNetworkDrive(t *testing.T) {
	old := remoteDrive
	t.Cleanup(func() { remoteDrive = old })
	remoteDrive = func(p string) bool { return strings.EqualFold(p[:1], "Z") }

	var r Registry
	for _, src := range []string{`Z:\src`, `z:/src/app`, `\\?\Z:\src`} {
		_, err := r.Resolve(src)
		var ue *UnsharableError
		if !errors.As(err, &ue) || ue.Source != src || ue.Why != "is on Z:, a network drive" {
			t.Errorf("%s: %v", src, err)
		}
	}
	var viaExport *UnsharableError
	if err := CheckBindSource(`\\?\Z:\src`); !errors.As(err, &viaExport) {
		t.Errorf("CheckBindSource of a network drive: %v", err)
	}
	if err := checkLocalDrive(`C:\src`, `C:\src`); err != nil {
		t.Fatal(err)
	}
}
