package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestCheckBrokerDataDir: a folder hyperbutler made passes; one that inherits access
// for Everyone (as a folder made before, under a drive root every user may change) is
// refused with a fix, and the fix's icacls command, run as a user pastes it into cmd,
// makes it pass.
func TestCheckBrokerDataDir(t *testing.T) {
	// A temp folder itself may inherit other groups' access, so the test's parent is
	// made as hyperbutler makes the data directory.
	parent := filepath.Join(t.TempDir(), "p")
	if err := MkdirPrivate(parent); err != nil {
		t.Fatal(err)
	}
	if err := CheckBrokerDataDir(parent); err != nil {
		t.Fatalf("a folder hyperbutler made: %v", err)
	}
	if out, err := exec.Command("icacls", parent, "/grant", "*S-1-1-0:(OI)(CI)R").CombinedOutput(); err != nil {
		t.Fatalf("icacls: %v: %s", err, out)
	}
	dir := filepath.Join(parent, "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	de, ok := IsDataDirError(CheckBrokerDataDir(dir))
	if !ok {
		t.Fatal("a folder that inherits Everyone's access passed")
	}
	if de.Dir != dir || !strings.HasPrefix(de.Reason, "DACL grants access to another account") || !strings.Contains(de.Error(), dir) {
		t.Fatalf("refusal %+v", de)
	}
	i := strings.Index(de.Fix, "icacls ")
	if i < 0 {
		t.Fatalf("fix without a command: %q", de.Fix)
	}
	cmd := exec.Command("cmd")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: "cmd /c " + de.Fix[i:]}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the fix %s: %v: %s", de.Fix[i:], err, out)
	}
	if err := CheckBrokerDataDir(dir); err != nil {
		t.Fatalf("after the fix: %v", err)
	}
}
