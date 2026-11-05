package workspace

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/sandbox/wire"
)

func tarOf(t *testing.T, files map[string]string) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(body)), Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &b
}

func openRoot(t *testing.T) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestExtractRefusesBadNames(t *testing.T) {
	for _, name := range []string{"../x", "/etc/x", "a/../../x", "a//b", "./a"} {
		r := openRoot(t)
		if _, err := Extract(r, tarOf(t, map[string]string{name: "x"})); err == nil {
			t.Errorf("%q extracted", name)
		}
	}
}

func TestChanges(t *testing.T) {
	r := openRoot(t)
	base, err := Extract(r, tarOf(t, map[string]string{"a.txt": "a", "dir/b.txt": "b", "same.txt": "s"}))
	if err != nil {
		t.Fatal(err)
	}
	dir := r.Name()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "dir", "b.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("c"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Changes(r, base)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		p string
		k wire.Kind
	}{{"a.txt", wire.Modified}, {"c.txt", wire.Added}, {"dir/b.txt", wire.Deleted}}
	if len(got) != len(want) {
		t.Fatalf("changes %+v", got)
	}
	for i, w := range want {
		if got[i].Path != w.p || got[i].Kind != w.k {
			t.Errorf("change %d: %s %d, want %s %d", i, got[i].Path, got[i].Kind, w.p, w.k)
		}
		if _, err := wire.Encode(got[i]); err != nil {
			t.Errorf("%s does not encode: %v", got[i].Path, err)
		}
	}
	// The baseline survives a save and load.
	f := filepath.Join(t.TempDir(), "b.json")
	if err := base.Save(f); err != nil {
		t.Fatal(err)
	}
	again, err := Load(f)
	if err != nil || len(again) != len(base) || again["a.txt"] != base["a.txt"] {
		t.Fatalf("Load: %v %v", again, err)
	}
}
