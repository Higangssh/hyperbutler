package apply

import (
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func sum(s string) *[sha256.Size]byte {
	h := sha256.Sum256([]byte(s))
	return &h
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

func TestApply(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "keep.txt"), "old")
	write(t, filepath.Join(root, "edit.txt"), "v1")
	write(t, filepath.Join(root, "gone.txt"), "bye")
	write(t, filepath.Join(root, "host-edited.txt"), "host v2")
	write(t, filepath.Join(root, "Mixed.txt"), "m")
	out, err := Apply(root, []Change{
		{Path: "edit.txt", Data: []byte("v2"), Base: sum("v1")},
		{Path: "new/dir/file.go", Data: []byte("package x")},
		{Path: "gone.txt", Delete: true, Base: sum("bye")},
		{Path: "host-edited.txt", Data: []byte("agent"), Base: sum("host v1")},
		{Path: "keep.txt", Data: []byte("agent")}, // the agent thinks it is new
		{Path: "mixed.txt", Data: []byte("agent"), Base: sum("m")},
		{Path: ".git/hooks/pre-commit", Data: []byte("evil")},
		{Path: "../escape.txt", Data: []byte("evil")},
		{Path: "a.txt", Data: []byte("1")},
		{Path: "A.TXT", Data: []byte("2")},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []error{nil, nil, nil, ErrConflict, ErrConflict, ErrName, ErrNotApplied, nil, ErrCollision, ErrCollision}
	for i, o := range out {
		if i == 7 {
			if o.Err == nil {
				t.Errorf("%s applied", o.Path)
			}
			continue
		}
		if !errors.Is(o.Err, want[i]) {
			t.Errorf("%s: %v, want %v", o.Path, o.Err, want[i])
		}
	}
	for p, w := range map[string]string{
		"edit.txt": "v2", "new/dir/file.go": "package x", "host-edited.txt": "host v2",
		"keep.txt": "old", "Mixed.txt": "m",
	} {
		if got := read(t, filepath.Join(root, p)); got != w {
			t.Errorf("%s = %q, want %q", p, got, w)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "gone.txt")); !os.IsNotExist(err) {
		t.Errorf("gone.txt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape.txt")); !os.IsNotExist(err) {
		t.Errorf("escape.txt written outside: %v", err)
	}
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("temporary file left: %s", e.Name())
		}
	}
}

func TestApplyRefusesLinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write(t, filepath.Join(outside, "target.txt"), "outside")
	// A junction needs no privilege.
	if err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(root, "j"), outside).Run(); err != nil {
		t.Skipf("mklink /J: %v", err)
	}
	out, err := Apply(root, []Change{
		{Path: "j/target.txt", Data: []byte("evil"), Base: sum("outside")},
		{Path: "j/new.txt", Data: []byte("evil")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range out {
		if o.Err == nil {
			t.Errorf("%s written through a junction", o.Path)
		}
	}
	if got := read(t, filepath.Join(outside, "target.txt")); got != "outside" {
		t.Errorf("outside file = %q", got)
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); !os.IsNotExist(err) {
		t.Error("a file was created through a junction")
	}
}

func TestApplyRefusesShortNames(t *testing.T) {
	root := t.TempDir()
	long := filepath.Join(root, "averylongfilename.txt")
	write(t, long, "x")
	p, _ := windows.UTF16PtrFromString(long)
	buf := make([]uint16, 260)
	n, err := windows.GetShortPathName(p, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 {
		t.Skipf("no short names on this volume: %v", err)
	}
	short := filepath.Base(windows.UTF16ToString(buf[:n]))
	if short == filepath.Base(long) {
		t.Skip("8.3 names are off on this volume")
	}
	out, err := Apply(root, []Change{{Path: short, Data: []byte("evil"), Base: sum("x")}})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(out[0].Err, ErrName) {
		t.Errorf("%s: %v, want ErrName", short, out[0].Err)
	}
	if got := read(t, long); got != "x" {
		t.Errorf("written through the short name: %q", got)
	}
}

func TestApplyHeldOpen(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "locked.txt")
	write(t, p, "v1")
	// An editor holding the file open for writing: the apply must not race it.
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out, err := Apply(root, []Change{{Path: "locked.txt", Data: []byte("v2"), Base: sum("v1")}})
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Err == nil {
		t.Error("replaced a file another program holds open for writing")
	}
}
