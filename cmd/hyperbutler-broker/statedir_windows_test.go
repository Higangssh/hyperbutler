package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// TestRemoveStaleStateDirs: the state directories of earlier runs go; a directory of
// another name, and a junction with the name, stay (the junction's target too).
func TestRemoveStaleStateDirs(t *testing.T) {
	base := t.TempDir()
	me, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	stale, err := privateDir(base, me.User.Sid.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "probe"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(base, "hyperbutler-broker-keep")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "precious"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(base, "hyperbutler-broker-0123456789abcdef")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink: %v: %s", err, out)
	}

	removeStaleStateDirs(base, me.User.Sid, t.Logf)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale state directory is still there: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("a directory of another name went: %v", err)
	}
	if _, err := os.Lstat(junction); err != nil {
		t.Errorf("the junction went: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "precious")); err != nil {
		t.Errorf("the junction's target was emptied: %v", err)
	}
}
