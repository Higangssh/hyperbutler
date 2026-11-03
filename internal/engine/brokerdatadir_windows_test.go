package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestCheckBrokerDataDir: a folder hyperbutler made passes; one that inherits access for
// Everyone (made before, under a drive root every user may change), and one with such an
// access set on itself and on a file in it, are refused with a fix whose commands, run
// as a user pastes them into cmd, make it pass.
func TestCheckBrokerDataDir(t *testing.T) {
	// A temp folder itself may inherit other groups' access, so the test's parent is
	// made as hyperbutler makes the data directory.
	parent := filepath.Join(t.TempDir(), "p q")
	if err := MkdirPrivate(parent); err != nil {
		t.Fatal(err)
	}
	if err := CheckBrokerDataDir(parent); err != nil {
		t.Fatalf("a folder hyperbutler made: %v", err)
	}
	icacls := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("icacls", args...).CombinedOutput(); err != nil {
			t.Fatalf("icacls %q: %v: %s", args, err, out)
		}
	}
	// explicit: Everyone set on the folder itself and on its disk.
	explicit := filepath.Join(parent, "explicit")
	if err := os.Mkdir(explicit, 0o700); err != nil {
		t.Fatal(err)
	}
	icacls(explicit, "/grant", "*S-1-1-0:(OI)(CI)R")
	disk := filepath.Join(explicit, "data.vhdx")
	if err := os.WriteFile(disk, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	icacls(disk, "/grant", "*S-1-1-0:F")

	// inherited: inherits Everyone from a parent that grants it.
	gp := filepath.Join(parent, "gp")
	if err := os.Mkdir(gp, 0o700); err != nil {
		t.Fatal(err)
	}
	icacls(gp, "/grant", "*S-1-1-0:(OI)(CI)R")
	inherited := filepath.Join(gp, "data")
	if err := os.Mkdir(inherited, 0o700); err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{inherited, explicit} {
		de, ok := IsDataDirError(CheckBrokerDataDir(dir))
		if !ok {
			t.Fatalf("%s: a folder others can read passed", dir)
		}
		if de.Dir != dir || !strings.HasPrefix(de.Reason, "DACL grants access to another account") || !strings.Contains(de.Error(), dir) || len(de.Commands) != 2 {
			t.Fatalf("refusal %+v", de)
		}
		for _, c := range de.Commands {
			if !strings.Contains(de.Fix, c) {
				t.Errorf("Fix %q does not name %q", de.Fix, c)
			}
			cmd := exec.Command("cmd")
			cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: "cmd /c " + c}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("the fix %s: %v: %s", c, err, out)
			}
		}
		if err := CheckBrokerDataDir(dir); err != nil {
			t.Fatalf("%s after the fix: %v", dir, err)
		}
	}
	// diskOnly: the folder passes, its disk has Everyone set on it.
	diskOnly := filepath.Join(parent, "diskonly")
	if err := os.Mkdir(diskOnly, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(diskOnly, "data.vhdx"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckBrokerDataDir(diskOnly); err != nil {
		t.Fatalf("a folder and disk of the user's: %v", err)
	}
	icacls(filepath.Join(diskOnly, "data.vhdx"), "/grant", "*S-1-1-0:F")
	de, ok := IsDataDirError(CheckBrokerDataDir(diskOnly))
	if !ok || !strings.HasPrefix(de.Reason, "its data disk DACL grants access to another account") || len(de.Commands) != 2 {
		t.Fatalf("a disk Everyone can change: %+v", de)
	}
	for _, c := range de.Commands {
		cmd := exec.Command("cmd")
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: "cmd /c " + c}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("the fix %s: %v: %s", c, err, out)
		}
	}
	if err := CheckBrokerDataDir(diskOnly); err != nil {
		t.Fatalf("the disk after the fix: %v", err)
	}

	out, err := exec.Command("icacls", disk).CombinedOutput()
	if err != nil || strings.Contains(string(out), "Everyone") {
		t.Errorf("the disk keeps its own grant after the fix: %v: %s", err, out)
	}
}

// TestDataDirFix: the fix never names a drive's root with \" (cmd reads it as an escaped
// quote), and gives no command for a path with %, which cmd would expand.
func TestDataDirFix(t *testing.T) {
	if _, cmds := dataDirFix(`D:\data\`, "DACL grants access to another account", `PC\me`); len(cmds) != 2 || strings.Contains(cmds[0], `\"`) {
		t.Errorf("trailing backslash: %q", cmds)
	}
	if fix, cmds := dataDirFix(`D:\100%done`, "DACL grants access to another account", `PC\me`); cmds != nil || strings.Contains(fix, "icacls") {
		t.Errorf("%% path: %q %q", fix, cmds)
	}
	if fix, cmds := dataDirFix(`D:\h`, "owner is not the caller", `PC\me`); len(cmds) != 3 || !strings.Contains(cmds[0], "/setowner") || !strings.Contains(fix, "administrator") {
		t.Errorf("owner: %q %q", fix, cmds)
	}
	if _, cmds := dataDirFix(`D:\h`, "is a reparse point", `PC\me`); cmds != nil {
		t.Errorf("reparse point: %q", cmds)
	}
}
