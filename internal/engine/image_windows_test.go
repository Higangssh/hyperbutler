//go:build windows

package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/guest/image"
	"github.com/Higangssh/hyperbutler/internal/imageset"
)

// writeImage makes an image directory: kernel, initrd, a 1 MiB system disk with a
// valid footer, and the manifest.
func writeImage(t *testing.T) (dir, manifest string) {
	t.Helper()
	dir = t.TempDir()
	write := func(name string, b []byte) image.File {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		d, err := image.Describe(name, f)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	footer, err := image.EncodeFooter(1<<20, time.Time{}, [16]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	m := &image.Manifest{
		Version: image.ManifestVersion,
		Kernel:  write("vmlinuz-virt", []byte("kernel")),
		Initrd:  write("initramfs-hyperbutler.gz", []byte("initrd")),
		System:  write("system.vhd", append(make([]byte, 1<<20), footer[:]...)),
	}
	b, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	manifest = filepath.Join(dir, image.ManifestName)
	if err := os.WriteFile(manifest, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, manifest
}

func TestOpenImage(t *testing.T) {
	dir, man := writeImage(t)
	g, err := imageset.Open(man, "")
	if err != nil {
		t.Fatal(err)
	}
	for got, want := range map[string]string{g.System: "system.vhd", g.Kernel: "vmlinuz-virt", g.Initrd: "initramfs-hyperbutler.gz"} {
		a, err1 := os.Stat(got)
		b, err2 := os.Stat(filepath.Join(dir, want))
		if err1 != nil || err2 != nil || !os.SameFile(a, b) || !filepath.IsAbs(got) || strings.HasPrefix(got, `\\?\`) {
			t.Errorf("path %q for %s (%v, %v)", got, want, err1, err2)
		}
	}
	// While the image is held, nobody can write or delete its files, but reading works.
	for _, name := range []string{"vmlinuz-virt", "initramfs-hyperbutler.gz", "system.vhd", image.ManifestName} {
		p := filepath.Join(dir, name)
		if f, err := os.OpenFile(p, os.O_WRONLY, 0); err == nil {
			f.Close()
			t.Errorf("%s opened for writing while the image is held", name)
		} else if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			t.Errorf("%s: want a sharing violation, got %v", name, err)
		}
		if err := os.Remove(p); err == nil {
			t.Errorf("%s deleted while the image is held", name)
		}
		if b, err := os.ReadFile(p); err != nil || len(b) == 0 {
			t.Errorf("%s: read while held: %v", name, err)
		}
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	if f, err := os.OpenFile(filepath.Join(dir, "system.vhd"), os.O_WRONLY, 0); err != nil {
		t.Fatalf("still locked after Close: %v", err)
	} else {
		f.Close()
	}
}

func TestOpenImageRejects(t *testing.T) {
	for name, mutate := range map[string]func(t *testing.T, dir string){
		"changed kernel": func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "vmlinuz-virt"), []byte("kernex"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"missing initrd": func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "initramfs-hyperbutler.gz")); err != nil {
				t.Fatal(err)
			}
		},
		"truncated system": func(t *testing.T, dir string) {
			if err := os.Truncate(filepath.Join(dir, "system.vhd"), 1<<20); err != nil {
				t.Fatal(err)
			}
		},
		"bad manifest": func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, image.ManifestName), []byte(`{"version":1}`), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"directory as kernel": func(t *testing.T, dir string) {
			p := filepath.Join(dir, "vmlinuz-virt")
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, man := writeImage(t)
			mutate(t, dir)
			if g, err := imageset.Open(man, ""); err == nil {
				g.Close()
				t.Fatal("accepted")
			}
		})
	}
}

// A system disk whose footer is wrong is refused even when the manifest hash matches
// (a build that wrapped the image wrongly).
func TestOpenImageChecksFooter(t *testing.T) {
	dir, man := writeImage(t)
	p := filepath.Join(dir, "system.vhd")
	b, _ := os.ReadFile(p)
	b[len(b)-1] = 1 // reserved byte
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(p)
	d, _ := image.Describe("system.vhd", f)
	f.Close()
	mb, _ := os.ReadFile(man)
	m, err := image.ParseManifest(mb)
	if err != nil {
		t.Fatal(err)
	}
	m.System = d
	enc, _ := m.Encode()
	if err := os.WriteFile(man, enc, 0o600); err != nil {
		t.Fatal(err)
	}
	if g, err := imageset.Open(man, ""); err == nil || !strings.Contains(err.Error(), "vhd") {
		if g != nil {
			g.Close()
		}
		t.Fatalf("want a footer error, got %v", err)
	}
}

func manifestSum(t *testing.T, man string) string {
	t.Helper()
	b, err := os.ReadFile(man)
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// A release build boots only the image.json it was linked with (Decision 4), even when
// a replaced set is consistent with its own manifest.
func TestOpenImagePinned(t *testing.T) {
	dir, man := writeImage(t)
	pin := manifestSum(t, man)
	g, err := imageset.Open(man, pin)
	if err != nil {
		t.Fatal(err)
	}
	g.Close()

	// Another kernel and a manifest that matches it.
	k := filepath.Join(dir, "vmlinuz-virt")
	if err := os.WriteFile(k, []byte("other kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	mb, _ := os.ReadFile(man)
	m, err := image.ParseManifest(mb)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(k)
	m.Kernel, _ = image.Describe("vmlinuz-virt", f)
	f.Close()
	enc, _ := m.Encode()
	if err := os.WriteFile(man, enc, 0o600); err != nil {
		t.Fatal(err)
	}
	if g, err := imageset.Open(man, ""); err != nil {
		t.Fatalf("a development build must accept a consistent image: %v", err)
	} else {
		g.Close()
	}
	_, err = imageset.Open(man, pin)
	if err == nil {
		t.Fatal("a pinned build accepted a replaced image")
	}
	for _, s := range []string{man, pin, "Extract the release zip again"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q does not mention %q", err, s)
		}
	}

	// The old manifest back, with the new kernel: the pin passes, the file check fails.
	if err := os.WriteFile(man, mb, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := imageset.Open(man, pin); err == nil || !strings.Contains(err.Error(), "vmlinuz-virt") ||
		!strings.Contains(err.Error(), "extract the release zip again") {
		t.Fatalf("want a kernel mismatch naming the fix, got %v", err)
	}

	for _, bad := range []string{strings.ToUpper(pin), pin[:63], "x" + pin[1:]} {
		if _, err := imageset.Open(man, bad); err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Errorf("pin %q: want a malformed-pin error, got %v", bad, err)
		}
	}
}

// While the image is held, its directory cannot be renamed and replaced either: HCS
// opens the kernel, initrd and system disk by path after the check.
func TestOpenImageHoldsDirectory(t *testing.T) {
	dir, man := writeImage(t)
	g, err := imageset.Open(man, manifestSum(t, man))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	for _, d := range []string{dir, filepath.Dir(dir)} {
		if err := os.Rename(d, d+"-moved"); err == nil {
			_ = os.Rename(d+"-moved", d)
			t.Fatalf("%s was renamed while the image is held", d)
		}
	}
}

// A junction on the way to the image is resolved: HCS gets the real directory, so
// retargeting the junction after the check changes nothing.
func TestOpenImageResolvesJunction(t *testing.T) {
	dir, _ := writeImage(t)
	link := filepath.Join(t.TempDir(), "img")
	// mklink /J needs no privilege, unlike a symbolic link.
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, dir).CombinedOutput(); err != nil {
		t.Skipf("mklink /J: %v %s", err, out)
	}
	g, err := imageset.Open(filepath.Join(link, image.ManifestName), "")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if strings.HasPrefix(strings.ToLower(g.System), strings.ToLower(link)) {
		t.Fatalf("system disk path %q goes through the junction", g.System)
	}
	a, _ := os.Stat(g.System)
	b, _ := os.Stat(filepath.Join(dir, "system.vhd"))
	if !os.SameFile(a, b) {
		t.Fatalf("%q is not the checked file", g.System)
	}
}
