package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFixBrokerDataDir: a folder hyperbutler made passes; folders that inherit access for
// Everyone, have it set on themselves or on their disk, or hold a conditional ACE are
// refused as fixable, and FixBrokerDataDir makes them pass. A hard-linked disk is not
// fixed (the other name's file keeps its ACL), and a folder swapped for a junction after
// the check is refused, the junction's target untouched.
func TestFixBrokerDataDir(t *testing.T) {
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
	aclOf := func(p string) string {
		t.Helper()
		out, err := exec.Command("icacls", p).CombinedOutput()
		if err != nil {
			t.Fatalf("icacls %s: %v: %s", p, err, out)
		}
		return strings.ReplaceAll(string(out), p, "")
	}

	gp := mkdir(filepath.Join(parent, "gp"))
	icacls(gp, "/grant", "*S-1-1-0:(OI)(CI)R")
	inherited := mkdir(filepath.Join(gp, "data"))
	writeDisk(inherited)
	explicit := mkdir(filepath.Join(parent, "explicit"))
	icacls(explicit, "/grant", "*S-1-1-0:(OI)(CI)R")
	icacls(writeDisk(explicit), "/grant", "*S-1-1-0:F")
	diskOnly := mkdir(filepath.Join(parent, "diskonly"))
	disk := writeDisk(diskOnly)
	if err := CheckBrokerDataDir(diskOnly); err != nil {
		t.Fatalf("a folder and disk of the user's: %v", err)
	}
	icacls(disk, "/grant", "*S-1-1-0:F")
	conditional := mkdir(filepath.Join(parent, "conditional"))
	ps := `$a = Get-Acl -LiteralPath $env:HB_DIR; $a.SetSecurityDescriptorSddlForm($a.Sddl + '(XA;OICI;FA;;;BU;(Member_of {SID(BA)}))'); Set-Acl -LiteralPath $env:HB_DIR -AclObject $a`
	cmd := exec.Command("powershell", "-NoProfile", "-Command", ps)
	cmd.Env = append(os.Environ(), "HB_DIR="+conditional)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Set-Acl: %v: %s", err, out)
	}

	for _, c := range []struct{ dir, reason string }{
		{inherited, "DACL grants access to another account"},
		{explicit, "DACL grants access to another account"},
		{diskOnly, "its data disk DACL grants access to another account"},
		{conditional, "DACL has an ACE of type"},
	} {
		cerr := CheckBrokerDataDir(c.dir)
		de, ok := IsDataDirError(cerr)
		if !ok {
			t.Fatalf("%s: %v", c.dir, cerr)
		}
		if !de.Fixable || !strings.HasPrefix(de.Reason, c.reason) || !strings.Contains(de.Fix, "doctor --fix") || strings.Contains(de.Fix, "icacls") {
			t.Fatalf("%s: refusal %+v", c.dir, de)
		}
		if err := FixBrokerDataDir(c.dir); err != nil {
			t.Fatalf("%s: fix: %v", c.dir, err)
		}
		if err := CheckBrokerDataDir(c.dir); err != nil {
			t.Fatalf("%s after the fix: %v", c.dir, err)
		}
	}
	if acl := aclOf(filepath.Join(explicit, "data.vhdx")); strings.Contains(acl, "Everyone") {
		t.Errorf("the disk after the fix: %s", acl)
	}

	// hardlinked: the disk is a second name of a file outside; nothing acts on it.
	hardlinked := mkdir(filepath.Join(gp, "hardlinked"))
	other := filepath.Join(t.TempDir(), "other.bin")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	icacls(other, "/grant", "*S-1-1-0:R")
	before := aclOf(other)
	if out, err := exec.Command("cmd", "/c", "mklink", "/H", filepath.Join(hardlinked, "data.vhdx"), other).CombinedOutput(); err != nil {
		t.Fatalf("mklink /H: %v: %s", err, out)
	}
	if err := FixBrokerDataDir(hardlinked); err == nil {
		t.Fatal("a hard-linked disk was fixed")
	} else if de, ok := IsDataDirError(err); !ok || !strings.Contains(de.Fix, "hyperbutler delete") {
		t.Fatalf("a hard-linked disk: %v", err)
	}
	if after := aclOf(other); after != before {
		t.Errorf("the other name's file changed:\n%s\n%s", before, after)
	}

	// swapped: after the check, the folder is renamed and a junction of its name points
	// at another folder of the user's with a disk; the fix refuses it.
	swapped := mkdir(filepath.Join(gp, "swapped"))
	if _, ok := IsDataDirError(CheckBrokerDataDir(swapped)); !ok {
		t.Fatal("the folder to swap passed")
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := MkdirPrivate(victim); err != nil {
		t.Fatal(err)
	}
	vdisk := writeDisk(victim)
	icacls(vdisk, "/grant", "*S-1-1-0:R")
	vbefore, vdbefore := aclOf(victim), aclOf(vdisk)
	if err := os.Rename(swapped, swapped+".old"); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", swapped, victim).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v: %s", err, out)
	}
	if err := FixBrokerDataDir(swapped); err == nil {
		t.Fatal("a junction in the folder's place was fixed")
	}
	if aclOf(victim) != vbefore || aclOf(vdisk) != vdbefore {
		t.Error("the junction's target changed")
	}
}
