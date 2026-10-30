//go:build windows

package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/guest/image"
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
	g, err := openImage(man)
	if err != nil {
		t.Fatal(err)
	}
	if g.system != filepath.Join(dir, "system.vhd") || g.kernel != filepath.Join(dir, "vmlinuz-virt") {
		t.Fatalf("paths: %+v", g)
	}
	// While the image is held, nobody can write or delete its files, but reading works.
	for _, name := range []string{"vmlinuz-virt", "initramfs-hyperbutler.gz", "system.vhd"} {
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
			if g, err := openImage(man); err == nil {
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
	if g, err := openImage(man); err == nil || !strings.Contains(err.Error(), "vhd") {
		if g != nil {
			g.Close()
		}
		t.Fatalf("want a footer error, got %v", err)
	}
}
