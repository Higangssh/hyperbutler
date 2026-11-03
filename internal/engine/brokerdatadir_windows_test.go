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

	// hardlinked: the disk is a second name of a file outside; the fix must not act on it.
	hardlinked := mkdir(filepath.Join(gp, "hardlinked"))
	other := filepath.Join(t.TempDir(), "other.bin")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	icacls(other, "/grant", "*S-1-1-0:R")
	if out, err := exec.Command("cmd", "/c", "mklink", "/H", filepath.Join(hardlinked, "data.vhdx"), other).CombinedOutput(); err != nil {
		t.Fatalf("mklink /H: %v: %s", err, out)
	}
	if de, ok := IsDataDirError(CheckBrokerDataDir(hardlinked)); !ok || de.Commands != nil || !strings.Contains(de.Fix, "hyperbutler delete") {
		t.Fatalf("a hard-linked disk: %+v", de)
	}
	// conditional: an ACE icacls cannot remove gets words only.
	conditional := mkdir(filepath.Join(parent, "conditional"))
	ps := `$a = Get-Acl -LiteralPath $env:HB_DIR; $a.SetSecurityDescriptorSddlForm($a.Sddl + '(XA;OICI;FA;;;BU;(Member_of {SID(BA)}))'); Set-Acl -LiteralPath $env:HB_DIR -AclObject $a`
	cmd := exec.Command("powershell", "-NoProfile", "-Command", ps)
	cmd.Env = append(os.Environ(), "HB_DIR="+conditional)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Set-Acl: %v: %s", err, out)
	}
	if de, ok := IsDataDirError(CheckBrokerDataDir(conditional)); !ok || de.Commands != nil || !strings.HasPrefix(de.Reason, "DACL has an ACE of type") {
		t.Fatalf("a conditional ACE: %+v", de)
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

// TestDataDirFix: no recursion and no following links (/L); accounts by SID; one &&
// chain; a trailing backslash dropped; no command for a drive's root, a path a shell
// would expand, a folder with links, a linked disk or a conditional ACE; the owner's
// fix covers the folder and its disk, in an elevated cmd.
func TestDataDirFix(t *testing.T) {
	const acl, me = "DACL grants access to another account", "*S-1-5-21-1-2-3-1001"
	fix, cmds := dataDirFix(`D:\data\`, acl, me, false, false, false, nil)
	if len(cmds) != 1 || strings.Contains(cmds[0], `\"`) || !strings.Contains(cmds[0], me+":") {
		t.Errorf("trailing backslash: %q", cmds)
	}
	if !strings.HasSuffix(fix, cmds[0]) {
		t.Errorf("fix %q", fix)
	}
	for _, d := range []string{`D:\`, `D:`, `D:\100%done`, `D:\my$data`, "D:\back`tick", `D:\a!b`} {
		if fix, cmds := dataDirFix(d, acl, me, true, false, false, nil); cmds != nil || strings.Contains(fix, "icacls") {
			t.Errorf("%s: %q %q", d, fix, cmds)
		}
	}
	for name, c := range map[string]struct {
		reason            string
		diskLinked, links bool
	}{
		"links":       {acl, false, true},
		"linked disk": {acl, true, false},
		"conditional": {"DACL has an ACE of type 9", false, false},
	} {
		if _, cmds := dataDirFix(`D:\h`, c.reason, me, true, c.diskLinked, c.links, nil); cmds != nil {
			t.Errorf("%s: %q", name, cmds)
		}
	}
	fix, cmds = dataDirFix(`D:\h`, "owner is not the caller", me, true, false, false, []string{"S-1-1-0"})
	if len(cmds) != 5 || !strings.Contains(cmds[0], `"D:\h" /setowner `+me) || !strings.Contains(cmds[1], `data.vhdx" /setowner`) ||
		!strings.Contains(cmds[3], "/remove:g *S-1-1-0") || !strings.Contains(fix, "Run as administrator") || !strings.Contains(fix, strings.Join(cmds, " && ")) {
		t.Errorf("owner: %q %q", fix, cmds)
	}
	for _, c := range cmds {
		if strings.Contains(c, "/T") || !strings.HasSuffix(c, " /L") {
			t.Errorf("%q: recursive, or without /L", c)
		}
	}
	if _, cmds := dataDirFix(`D:\h`, "is a reparse point", me, false, false, false, nil); cmds != nil {
		t.Errorf("reparse point: %q", cmds)
	}
}
