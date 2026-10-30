//go:build windows

package share

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRegistryRecreatedDirectoryGetsNewShare is the e2e failure of 2026-10-31: a bind
// source deleted and made again under the same path kept resolving to the share of the
// deleted directory, so containers saw an empty mount and single-file binds failed.
func TestRegistryRecreatedDirectoryGetsNewShare(t *testing.T) {
	srv := NewServer(Options{NoWatch: true})
	defer srv.Close()
	reg := &Registry{Server: srv, MountPoint: "/mnt/hyperbutler"}
	dir := filepath.Join(t.TempDir(), "src")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	g1, err := reg.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := reg.Resolve(strings.ToUpper(dir[:1]) + dir[1:]); err != nil || again != g1 {
		t.Fatalf("same directory: %q %v, want %q", again, err, g1)
	}
	f1, err := reg.Resolve(filepath.Join(dir, "f.txt"))
	if err != nil || !strings.HasSuffix(f1, "/f.txt") {
		t.Fatalf("file: %q %v", f1, err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	g2, err := reg.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	if g2 == g1 {
		t.Fatalf("recreated directory still maps to %q", g1)
	}
	f2, err := reg.Resolve(filepath.Join(dir, "f.txt"))
	if err != nil || f2 == f1 {
		t.Fatalf("file in recreated directory: %q %v (old %q)", f2, err, f1)
	}
	if n := len(srv.Shares()); n != 4 {
		t.Fatalf("%d shares, want 4", n)
	}
}
