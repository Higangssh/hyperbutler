package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// runFix runs one of a fix's commands as a user pastes it into cmd.
func runFix(t *testing.T, c string) {
	t.Helper()
	cmd := exec.Command("cmd")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: "cmd /c " + c}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the fix %s: %v: %s", c, err, out)
	}
}

// TestCheckBrokerDataDir: a folder hyperbutler made passes; one that inherits access for
// Everyone, one with Everyone set on itself and its disk, and one whose disk alone has it
// are refused with a fix whose commands make it pass, and the folder is closed after the
// first one already. A folder with a junction in it gets no command, and nothing past
// the junction changes.
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
	mkdir := func(dir string) string {
		t.Helper()
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	writeDisk := func(dir string) string {
		t.Helper()
		p := filepath.Join(dir, "data.vhdx")
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// inherited: inherits Everyone from a parent that grants it; no disk yet.
	gp := mkdir(filepath.Join(parent, "gp"))
	icacls(gp, "/grant", "*S-1-1-0:(OI)(CI)R")
	inherited := mkdir(filepath.Join(gp, "data"))
	// explicit: Everyone set on the folder itself and on its disk.
	explicit := mkdir(filepath.Join(parent, "explicit"))
	icacls(explicit, "/grant", "*S-1-1-0:(OI)(CI)R")
	icacls(writeDisk(explicit), "/grant", "*S-1-1-0:F")
	// diskOnly: the folder passes, its disk has Everyone set on it.
	diskOnly := mkdir(filepath.Join(parent, "diskonly"))
	disk := writeDisk(diskOnly)
	if err := CheckBrokerDataDir(diskOnly); err != nil {
		t.Fatalf("a folder and disk of the user's: %v", err)
	}
	icacls(disk, "/grant", "*S-1-1-0:F")

	for _, c := range []struct {
		dir, reason string
		cmds        int
	}{
		{inherited, "DACL grants access to another account", 1},
		{explicit, "DACL grants access to another account", 3},
		{diskOnly, "its data disk DACL grants access to another account", 2},
	} {
		de, ok := IsDataDirError(CheckBrokerDataDir(c.dir))
		if !ok {
			t.Fatalf("%s: passed", c.dir)
		}
		if de.Dir != c.dir || !strings.HasPrefix(de.Reason, c.reason) || len(de.Commands) != c.cmds || !strings.Contains(de.Error(), c.dir) {
			t.Fatalf("%s: refusal %+v", c.dir, de)
		}
		for i, cmd := range de.Commands {
			if !strings.Contains(de.Fix, cmd) || strings.Contains(cmd, "/T") {
				t.Errorf("%s: command %q (Fix %q)", c.dir, cmd, de.Fix)
			}
			runFix(t, cmd)
			if i == 0 && c.dir == inherited {
				// The first command already closes what the folder inherited: no
				// window in which it is wider than before. (Grants set on the folder
				// itself, as explicit's, were there before and go with the second.)
				if de2, ok := IsDataDirError(CheckBrokerDataDir(c.dir)); ok && !strings.HasPrefix(de2.Reason, "its data disk") {
					t.Errorf("%s after the first command: %v", c.dir, de2)
				}
			}
		}
		if err := CheckBrokerDataDir(c.dir); err != nil {
			t.Fatalf("%s after the fix: %v", c.dir, err)
		}
	}

	// linked: a refused folder with a junction to a folder outside gets no command.
	linked := mkdir(filepath.Join(gp, "linked"))
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep")
	if err := os.WriteFile(keep, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	icacls(keep, "/grant", "*S-1-1-0:R")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(linked, "j"), outside).CombinedOutput(); err != nil {
		t.Fatalf("mklink: %v: %s", err, out)
	}
	de, ok := IsDataDirError(CheckBrokerDataDir(linked))
	if !ok || de.Commands != nil || !strings.Contains(de.Fix, "link") {
		t.Fatalf("a folder with a junction: %+v", de)
	}
}

// TestDataDirFix: no recursion; a trailing backslash dropped; no command for a drive's
// root, a path a shell would expand, or a folder with links; the owner's fix covers the
// folder and its disk.
func TestDataDirFix(t *testing.T) {
	const acl = "DACL grants access to another account"
	if _, cmds := dataDirFix(`D:\data\`, acl, `PC\me`, false, false, nil); len(cmds) != 1 || strings.Contains(cmds[0], `\"`) {
		t.Errorf("trailing backslash: %q", cmds)
	}
	for _, d := range []string{`D:\`, `D:`, `D:\100%done`, `D:\my$data`, "D:\\back`tick"} {
		if fix, cmds := dataDirFix(d, acl, `PC\me`, true, false, nil); cmds != nil || strings.Contains(fix, "icacls") {
			t.Errorf("%s: %q %q", d, fix, cmds)
		}
	}
	if _, cmds := dataDirFix(`D:\h`, acl, `PC\me`, true, true, nil); cmds != nil {
		t.Errorf("links: %q", cmds)
	}
	fix, cmds := dataDirFix(`D:\h`, "owner is not the caller", `PC\me`, true, false, nil)
	if len(cmds) != 4 || !strings.Contains(cmds[0], `/setowner`) || !strings.Contains(cmds[1], `data.vhdx" /setowner`) || !strings.Contains(fix, "administrator") {
		t.Errorf("owner: %q %q", fix, cmds)
	}
	for _, c := range cmds {
		if strings.Contains(c, "/T") {
			t.Errorf("recursive: %q", c)
		}
	}
	if _, cmds := dataDirFix(`D:\h`, "is a reparse point", `PC\me`, false, false, nil); cmds != nil {
		t.Errorf("reparse point: %q", cmds)
	}
}
