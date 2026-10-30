package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/guest/image"
	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

func fakeErofs(t *testing.T, dir string, n int) string {
	t.Helper()
	b := bytes.Repeat([]byte{0x5a}, n)
	binary.LittleEndian.PutUint32(b[1024:], erofsMagic)
	p := filepath.Join(dir, "system.erofs")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWrapAndManifest(t *testing.T) {
	dir := t.TempDir()
	in := fakeErofs(t, dir, 3<<20+4096)
	out := filepath.Join(dir, "system.vhd")
	if err := wrap(in, out, time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 4<<20+image.FooterSize {
		t.Fatalf("size %d, want 4 MiB + footer", len(b))
	}
	f, err := image.ParseFooter(b[len(b)-image.FooterSize:])
	if err != nil || f.Size != 4<<20 {
		t.Fatalf("footer: %+v, %v", f, err)
	}
	if !bytes.Equal(b[3<<20+4096:4<<20], make([]byte, 4<<20-(3<<20+4096))) {
		t.Fatal("padding is not zero")
	}
	// Deterministic.
	out2 := filepath.Join(dir, "again.vhd")
	if err := wrap(in, out2, time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	if b2, _ := os.ReadFile(out2); !bytes.Equal(b, b2) {
		t.Fatal("two wraps of the same image differ")
	}

	for _, n := range []string{"vmlinuz-virt", "initramfs-hyperbutler.gz"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	man := filepath.Join(dir, image.ManifestName)
	if err := writeManifest(man, filepath.Join(dir, "vmlinuz-virt"), filepath.Join(dir, "initramfs-hyperbutler.gz"), out); err != nil {
		t.Fatal(err)
	}
	mb, _ := os.ReadFile(man)
	m, err := image.ParseManifest(mb)
	if err != nil {
		t.Fatal(err)
	}
	if m.System.Size != uint64(len(b)) || m.Kernel.Name != "vmlinuz-virt" {
		t.Fatalf("manifest: %+v", m)
	}
	if err := writeManifest(man, filepath.Join(t.TempDir(), "k"), out, out); err == nil {
		t.Error("a kernel outside the image directory was accepted")
	}
}

func TestWrapRefusesNonErofs(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x")
	if err := os.WriteFile(p, make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := wrap(p, filepath.Join(dir, "y.vhd"), time.Time{}); err == nil {
		t.Fatal("wrapped a file without the erofs magic")
	}
}
