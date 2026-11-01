//go:build windows

package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/engine"
)

// TestImportLock: a second import in the same installation refuses at once, before it
// reaches either engine, and can go once the first is done.
func TestImportLock(t *testing.T) {
	home, _ := testHome(t)
	in := &install{l: engine.Layout{Home: home, Data: home}}
	first, err := in.importLock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.importLock(); !errors.Is(err, errImportRunning) {
		t.Fatalf("second lock: %v", err)
	}
	if code, _, e := runCLI(t, "import", "--from-desktop", "--all-images"); code != 1 || !strings.Contains(e, "another import is running") {
		t.Fatalf("second import: %d %q", code, e)
	}
	if err := first.Unlock(); err != nil {
		t.Fatal(err)
	}
	second, err := in.importLock()
	if err != nil {
		t.Fatalf("after the first: %v", err)
	}
	_ = second.Unlock()
	// With the lock free, the import goes on to find the engine stopped.
	if code, _, e := runCLI(t, "import", "--from-desktop", "--all-images"); code != 1 || !strings.Contains(e, "not running") {
		t.Fatalf("import after the lock: %d %q", code, e)
	}
}
