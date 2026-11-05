//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A plain folder, and one a junction of the user's own leads to, are not redirected:
// only a packaged app's LocalCache counts.
func TestRedirectedDirPlainAndJunction(t *testing.T) {
	dir := t.TempDir()
	if final, ok := redirectedDir(dir); ok {
		t.Fatalf("plain folder reported as redirected to %s", final)
	}
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J: %v: %s", err, out)
	}
	if final, ok := redirectedDir(link); ok {
		t.Fatalf("junction reported as redirected to %s", final)
	}
	if _, ok := redirectedDir(filepath.Join(dir, "missing")); ok {
		t.Fatal("a missing folder reported as redirected")
	}
}

// Under a packaged app's file system virtualization (as when the tests run from such
// an app's terminal), a new folder under %LOCALAPPDATA% is really in the app's
// LocalCache, and is reported with that path. Elsewhere the test is skipped.
func TestRedirectedDirUnderPackage(t *testing.T) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		t.Skip("no LOCALAPPDATA")
	}
	dir := filepath.Join(base, "hyperbutler-redirect-test-"+strings.ReplaceAll(t.Name(), "/", "-"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	final, ok := redirectedDir(dir)
	if !ok {
		t.Skip("this process's %LOCALAPPDATA% is not redirected")
	}
	if !strings.Contains(strings.ToLower(final), `\localcache\`) {
		t.Fatalf("redirected to %s", final)
	}
	if err := redirectError(dir, final); !strings.Contains(err.Error(), "packaged app") || !strings.Contains(err.Error(), final) {
		t.Fatalf("error %q", err)
	}
	t.Logf("redirected to %s", final)
}
