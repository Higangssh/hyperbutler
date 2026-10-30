//go:build windows

package share

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Review finding 7: --mount must not create a missing source; -v does.
func TestRewriteMountMissingSource(t *testing.T) {
	srv := NewServer(Options{NoWatch: true})
	defer srv.Close()
	reg := &Registry{Server: srv, MountPoint: "/mnt/hyperbutler"}
	missing := filepath.Join(t.TempDir(), "nope")
	js := strings.ReplaceAll(missing, `\`, `\\`)
	_, err := RewriteCreateMounts([]byte(`{"HostConfig":{"Mounts":[{"Type":"bind","Source":"`+js+`","Target":"/x"}]}}`), reg.Resolve, reg.ResolveExisting)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("--mount of a missing source: %v", err)
	}
	if _, serr := os.Stat(missing); !os.IsNotExist(serr) {
		t.Fatal("--mount created the missing source")
	}
	out, err := RewriteCreateMounts([]byte(`{"HostConfig":{"Binds":["`+js+`:/x"]}}`), reg.Resolve, reg.ResolveExisting)
	if err != nil || !strings.Contains(string(out), "/mnt/hyperbutler/b-") {
		t.Fatalf("-v of a missing source: %s %v", out, err)
	}
	if fi, serr := os.Stat(missing); serr != nil || !fi.IsDir() {
		t.Fatal("-v did not create the source directory")
	}
}

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

// Integration step 5: shares no container uses are removed, and a source deleted and
// made again does not leave one share per incarnation behind.
func TestRegistryPrune(t *testing.T) {
	srv := NewServer(Options{NoWatch: true})
	defer srv.Close()
	reg := &Registry{Server: srv, MountPoint: "/mnt/hyperbutler"}
	base := t.TempDir()
	name := func(guest string) string {
		n, _, _ := strings.Cut(strings.TrimPrefix(guest, "/mnt/hyperbutler/"), "/")
		return n
	}
	ga, _ := reg.Resolve(filepath.Join(base, "a"))
	gb, _ := reg.Resolve(filepath.Join(base, "b"))
	os.WriteFile(filepath.Join(base, "c.txt"), nil, 0o644)
	gc, err := reg.Resolve(filepath.Join(base, "c.txt"))
	if err != nil || reg.Len() != 3 {
		t.Fatalf("%d shares, %v", reg.Len(), err)
	}
	used := map[string]bool{name(ga): true}
	// Within the grace period nothing goes, used or not.
	if gone := reg.Prune(used, time.Now().Add(-time.Minute)); len(gone) != 0 {
		t.Fatalf("pruned %v inside the grace period", gone)
	}
	gone := reg.Prune(used, time.Now().Add(time.Second))
	if len(gone) != 2 || reg.Len() != 1 {
		t.Fatalf("pruned %v, %d left", gone, reg.Len())
	}
	if got := srv.Shares(); len(got) != 1 || got[0] != name(ga) {
		t.Fatalf("server shares %v, want only %s", got, name(ga))
	}
	// A pruned source can be shared again.
	if g, err := reg.Resolve(filepath.Join(base, "b")); err != nil || g != gb {
		t.Fatalf("re-resolve: %q %v, want %q", g, err, gb)
	}
	_ = gc
	// Delete and make the source again many times: only the live incarnation stays.
	dir := filepath.Join(base, "re")
	var last string
	for i := 0; i < 20; i++ {
		os.RemoveAll(dir)
		os.Mkdir(dir, 0o755)
		if last, err = reg.Resolve(dir); err != nil {
			t.Fatal(err)
		}
	}
	reg.Prune(map[string]bool{name(last): true}, time.Now().Add(time.Second))
	if got := srv.Shares(); len(got) != 1 || got[0] != name(last) {
		t.Fatalf("after recreating 20 times: shares %v", got)
	}
}

// The number of live shares is bounded; pruning makes room again.
func TestRegistryMaxShares(t *testing.T) {
	srv := NewServer(Options{NoWatch: true})
	defer srv.Close()
	reg := &Registry{Server: srv, MountPoint: "/mnt/hyperbutler", MaxShares: 2}
	base := t.TempDir()
	for _, d := range []string{"a", "b"} {
		if _, err := reg.Resolve(filepath.Join(base, d)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reg.Resolve(filepath.Join(base, "a")); err != nil {
		t.Fatalf("an existing share past the limit: %v", err)
	}
	if _, err := reg.Resolve(filepath.Join(base, "c")); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("a third share: %v", err)
	}
	reg.Prune(nil, time.Now().Add(time.Second))
	if _, err := reg.Resolve(filepath.Join(base, "c")); err != nil {
		t.Fatalf("after pruning: %v", err)
	}
}
