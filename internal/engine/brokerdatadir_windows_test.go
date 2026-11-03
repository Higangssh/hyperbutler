package engine

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
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
	ownTree(t, parent)
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
	ownTree(t, diskOnly)
	if err := CheckBrokerDataDir(diskOnly); err != nil {
		t.Fatalf("a folder and disk of the user's: %v", err)
	}
	icacls(disk, "/grant", "*S-1-1-0:F")
	conditional := mkdir(filepath.Join(parent, "conditional"))
	// A conditional (callback) ACE, which a standard user may set on a folder of theirs
	// and icacls cannot remove: appended to the folder's DACL as SDDL.
	cur, err := windows.GetNamedSecurityInfo(conditional, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	withXA, err := windows.SecurityDescriptorFromString(cur.String() + "(XA;OICI;FA;;;BU;(Member_of {SID(BA)}))")
	if err != nil {
		t.Fatal(err)
	}
	xa, _, err := withXA.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(conditional, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, xa, nil); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ dir, reason string }{
		{inherited, "DACL grants access to another account"},
		{explicit, "DACL grants access to another account"},
		{diskOnly, "its data disk DACL grants access to another account"},
		{conditional, "DACL has an ACE of type"},
	} {
		ownTree(t, c.dir)
		cerr := CheckBrokerDataDir(c.dir)
		de, ok := IsDataDirError(cerr)
		if !ok {
			t.Fatalf("%s: %v", c.dir, cerr)
		}
		if !de.Fixable || !strings.HasPrefix(de.Reason, c.reason) || !strings.Contains(de.Fix, "doctor --fix") || strings.Contains(de.Fix, "icacls") {
			t.Fatalf("%s: refusal %+v", c.dir, de)
		}
		ownTree(t, c.dir)
		if err := FixBrokerDataDir(c.dir); err != nil {
			t.Fatalf("%s: fix: %v", c.dir, err)
		}
		ownTree(t, c.dir)
		if err := CheckBrokerDataDir(c.dir); err != nil {
			t.Fatalf("%s after the fix: %v", c.dir, err)
		}
	}
	if acl := aclOf(filepath.Join(explicit, "data.vhdx")); strings.Contains(acl, "Everyone") {
		t.Errorf("the disk after the fix: %s", acl)
	}

	// linkedDisk: the disk is a second name of a file outside; nothing acts on it.
	linkedDisk := mkdir(filepath.Join(gp, "linkedDisk"))
	other := filepath.Join(t.TempDir(), "other.bin")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	icacls(other, "/grant", "*S-1-1-0:R")
	before := aclOf(other)
	if out, err := exec.Command("cmd", "/c", "mklink", "/H", filepath.Join(linkedDisk, "data.vhdx"), other).CombinedOutput(); err != nil {
		t.Fatalf("mklink /H: %v: %s", err, out)
	}
	ownTree(t, linkedDisk)
	if err := FixBrokerDataDir(linkedDisk); err == nil {
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
	ownTree(t, swapped)
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

// TestFixBrokerDataDirEntries: the fix covers every entry of the folder (disk.json and a
// logs subfolder too, which kept a grant to Everyone otherwise); a folder reached through
// a junction an ancestor was swapped for is refused, its target unchanged; a drive's
// root is never fixed.
func TestFixBrokerDataDirEntries(t *testing.T) {
	base := filepath.Join(t.TempDir(), "b")
	if err := MkdirPrivate(base); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%q: %v: %s", args, err, out)
		}
	}
	aclOf := func(p string) string {
		t.Helper()
		out, err := exec.Command("icacls", p).CombinedOutput()
		if err != nil {
			t.Fatalf("icacls %s: %v: %s", p, err, out)
		}
		return strings.ReplaceAll(string(out), p, "")
	}
	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	run("icacls", open, "/grant", "*S-1-1-0:(OI)(CI)F")
	dir := filepath.Join(open, "data")
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"data.vhdx", "disk.json", filepath.Join("logs", "engine.log")} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ownTree(t, dir)
	if err := FixBrokerDataDir(dir); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"", "data.vhdx", "disk.json", "logs", filepath.Join("logs", "engine.log")} {
		if acl := aclOf(filepath.Join(dir, f)); strings.Contains(acl, "Everyone") {
			t.Errorf("%q keeps Everyone: %s", f, acl)
		}
	}

	// ancestor: check gp\data, then gp is renamed and a junction of its name points at a
	// folder of the user's with its own data folder, shared with Users on purpose.
	gp := filepath.Join(base, "gp")
	if err := os.MkdirAll(filepath.Join(gp, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	run("icacls", gp, "/grant", "*S-1-1-0:(OI)(CI)R")
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.MkdirAll(filepath.Join(victim, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	run("icacls", filepath.Join(victim, "data"), "/grant", "*S-1-5-32-545:(OI)(CI)F")
	before := aclOf(filepath.Join(victim, "data"))
	if err := os.Rename(gp, gp+".old"); err != nil {
		t.Fatal(err)
	}
	run("cmd", "/c", "mklink", "/J", gp, victim)
	if err := FixBrokerDataDir(filepath.Join(gp, "data")); err == nil {
		t.Fatal("a folder reached through a swapped ancestor was fixed")
	}
	if aclOf(filepath.Join(victim, "data")) != before {
		t.Error("the junction's target changed")
	}

	for _, root := range []string{`C:\`, `C:`} {
		de, ok := IsDataDirError(FixBrokerDataDir(root))
		if !ok || de.Fixable || !strings.Contains(de.Fix, "drive's root") {
			t.Errorf("%s: %+v", root, de)
		}
	}
}

// TestFixBrokerDataDirHeldOpen: an entry another program holds open to write is refused
// (its handle would outlive the new DACL), and fixed once it is closed; a folder named by
// its short (8.3) path is the same folder.
func TestFixBrokerDataDirHeldOpen(t *testing.T) {
	base := filepath.Join(t.TempDir(), "b")
	if err := MkdirPrivate(base); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "a long folder name")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(dir, "disk.json")
	if err := os.WriteFile(meta, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("icacls", dir, "/grant", "*S-1-1-0:(OI)(CI)F", "/T").CombinedOutput(); err != nil {
		t.Fatalf("icacls: %v: %s", err, out)
	}
	w, err := os.OpenFile(meta, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	ownTree(t, dir)
	if de, ok := IsDataDirError(FixBrokerDataDir(dir)); !ok || !strings.Contains(de.Reason, "open to write") {
		t.Fatalf("an entry held open to write: %+v", de)
	}
	_ = w.Close()

	short := dir
	if u, err := windows.UTF16PtrFromString(dir); err == nil {
		buf := make([]uint16, windows.MAX_PATH)
		if n, err := windows.GetShortPathName(u, &buf[0], uint32(len(buf))); err == nil && n > 0 {
			short = windows.UTF16ToString(buf[:n])
		}
	}
	ownTree(t, short)
	if err := FixBrokerDataDir(short); err != nil {
		t.Fatalf("fix by %s: %v", short, err)
	}
	ownTree(t, dir)
	if err := CheckBrokerDataDir(dir); err != nil {
		t.Fatal(err)
	}
}

// TestReadPrivateFile: disk.json is trusted only while no other account may change it.
func TestReadPrivateFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d")
	if err := MkdirPrivate(dir); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "disk.json")
	if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateFile(p); err != nil {
		t.Fatalf("a file in a folder hyperbutler made: %v", err)
	}
	if out, err := exec.Command("icacls", p, "/grant", "*S-1-1-0:M").CombinedOutput(); err != nil {
		t.Fatalf("icacls: %v: %s", err, out)
	}
	if _, err := readPrivateFile(p); err == nil || !strings.Contains(err.Error(), "another account") {
		t.Fatalf("a disk.json Everyone may change: %v", err)
	}
}

// TestReadPrivateFileShapes: a missing file is os.ErrNotExist; a hard link or a symbolic
// link is refused; the content comes from the checked handle.
func TestReadPrivateFileShapes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d")
	if err := MkdirPrivate(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateFile(filepath.Join(dir, "none.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	p := filepath.Join(dir, "disk.json")
	if err := os.WriteFile(p, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := readPrivateFile(p); err != nil || string(b) != `{"v":1}` {
		t.Fatalf("read: %q %v", b, err)
	}
	if err := os.Link(p, filepath.Join(dir, "second")); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateFile(p); err == nil || !strings.Contains(err.Error(), "hard link") {
		t.Fatalf("hard link: %v", err)
	}
}

// ownTree makes this user the owner of root and everything below it: what hyperbutler
// itself does (MkdirPrivate, ownByUser), where an elevated shell (as on CI) would
// otherwise leave the entries the test makes owned by Administrators.
func ownTree(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return filepath.SkipDir
		}
		return ownByUser(p)
	})
	if err != nil {
		t.Fatal(err)
	}
}
