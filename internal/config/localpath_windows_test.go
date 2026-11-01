//go:build windows

package config

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeLinks is a file system of directories, files and links (path -> target), with
// network drives.
type fakeLinks struct {
	links  map[string]string
	remote map[string]bool
	read   []string // what Lstat and Readlink were asked
}

type fakeInfo struct{ mode fs.FileMode }

func (i fakeInfo) Name() string       { return "x" }
func (i fakeInfo) Size() int64        { return 0 }
func (i fakeInfo) Mode() fs.FileMode  { return i.mode }
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fakeInfo) Sys() any           { return nil }

func (f *fakeLinks) Lstat(p string) (fs.FileInfo, error) {
	f.read = append(f.read, p)
	if strings.HasPrefix(p, `\\`) {
		return nil, errors.New("network path touched")
	}
	if _, ok := f.links[strings.ToLower(p)]; ok {
		return fakeInfo{fs.ModeSymlink}, nil
	}
	return fakeInfo{0}, nil
}

func (f *fakeLinks) Readlink(p string) (string, error) {
	f.read = append(f.read, p)
	t, ok := f.links[strings.ToLower(p)]
	if !ok {
		return "", errors.New("not a link")
	}
	return t, nil
}

func (f *fakeLinks) Remote(root string) bool { return f.remote[strings.ToUpper(root)] }

// TestResolveLocal (review M-C): links and junctions are followed without opening their
// targets, and a path that leads to a network location is refused.
func TestResolveLocal(t *testing.T) {
	fsys := &fakeLinks{
		links: map[string]string{
			`c:\certs\unc-junction`: `\??\UNC\server\share`, // as FSCTL_SET_REPARSE_POINT can make it
			`c:\certs\unc-link.pem`: `\\server\share\ca.pem`,
			`c:\certs\nt-unc.pem`:   `\\?\UNC\server\share\ca.pem`,
			`c:\certs\device.pem`:   `\\.\pipe\x`,
			`c:\certs\volume`:       `\??\Volume{01234567-89ab-cdef-0123-456789abcdef}\`,
			`c:\certs\to-z`:         `Z:\ca`,
			`c:\certs\local`:        `\??\D:\real`,
			`d:\real\rel.pem`:       `..\sib\ca.pem`,
			`c:\certs\root`:         `\pki`,
			`c:\certs\drive-rel`:    `D:ca.pem`,
			`c:\loop\a`:             `C:\loop\b`,
			`c:\loop\b`:             `C:\loop\a`,
		},
		remote: map[string]bool{`Z:\`: true},
	}
	for _, c := range []struct{ in, want string }{
		{`C:\certs\plain.pem`, `C:\certs\plain.pem`},
		{`C:\certs\local\ca.pem`, `D:\real\ca.pem`},
		{`C:\certs\local\rel.pem`, `D:\sib\ca.pem`},
		{`C:\certs\root\ca.pem`, `C:\pki\ca.pem`},
	} {
		got, err := resolveLocal(c.in, fsys)
		if err != nil || got != c.want {
			t.Errorf("%s: %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{
		`C:\certs\unc-junction\ca.pem`, `C:\certs\unc-link.pem`, `C:\certs\nt-unc.pem`, `C:\certs\device.pem`,
		`C:\certs\volume\ca.pem`, `C:\certs\to-z\x.pem`, `Z:\ca.pem`, `C:\certs\drive-rel`, `C:\loop\a\x`,
		`\\server\share\ca.pem`, `\\?\C:\ca.pem`,
	} {
		fsys.read = nil
		if got, err := resolveLocal(in, fsys); err == nil {
			t.Errorf("%s: accepted as %q", in, got)
		}
		for _, p := range fsys.read {
			if strings.HasPrefix(p, `\\`) {
				t.Errorf("%s: touched %s", in, p)
			}
		}
	}
}

// TestResolveLocalReal follows a real junction (no privilege needed) and, where this
// account may create one, refuses a real symbolic link to a UNC path without reading it.
func TestResolveLocalReal(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "ca.pem"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	j := filepath.Join(dir, "junction")
	if out, err := exec.Command("cmd", "/d", "/c", "mklink", "/J", j, target).CombinedOutput(); err != nil {
		t.Logf("no junction (%v: %s)", err, out)
	} else {
		got, err := resolveLocal(filepath.Join(j, "ca.pem"), osLinkFS{})
		if err != nil || !strings.EqualFold(got, filepath.Join(target, "ca.pem")) {
			t.Errorf("junction: %q, %v", got, err)
		}
	}
	l := filepath.Join(dir, "unc.pem")
	if err := os.Symlink(`\\hb-test.invalid\share\ca.pem`, l); err != nil {
		t.Logf("no symbolic link (needs developer mode or the privilege): %v", err)
		return
	}
	t0 := time.Now()
	if got, err := resolveLocal(l, osLinkFS{}); err == nil {
		t.Errorf("symbolic link to UNC accepted as %q", got)
	}
	if time.Since(t0) > 2*time.Second {
		t.Error("the refusal took long: the network path was probably touched")
	}
}
