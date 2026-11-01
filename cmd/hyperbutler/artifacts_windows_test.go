//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// guestArtifacts prefers image.json (the A2b image, verified by the engine) and falls
// back to the initramfs-only names; an A2b directory without image.json is refused,
// because its stage-1 initramfs has a name the fallback does not know.
func TestGuestArtifacts(t *testing.T) {
	touch := func(dir, name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	legacy := t.TempDir()
	touch(legacy, kernelName)
	touch(legacy, initrdName)
	if k, i, m, err := guestArtifacts(legacy); err != nil || m != "" || k != filepath.Join(legacy, kernelName) || i != filepath.Join(legacy, initrdName) {
		t.Fatalf("legacy: %q %q %q %v", k, i, m, err)
	}

	a2b := t.TempDir()
	touch(a2b, kernelName)
	touch(a2b, "initramfs-stage1.gz")
	touch(a2b, "system.vhd")
	if _, _, _, err := guestArtifacts(a2b); err == nil {
		t.Fatal("an A2b directory without image.json was accepted as a legacy image")
	}
	touch(a2b, "image.json")
	if k, i, m, err := guestArtifacts(a2b); err != nil || k != "" || i != "" || m != filepath.Join(a2b, "image.json") {
		t.Fatalf("a2b: %q %q %q %v", k, i, m, err)
	}

	// A directory named image.json is not a manifest.
	odd := t.TempDir()
	if err := os.Mkdir(filepath.Join(odd, "image.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, m, err := guestArtifacts(odd); err == nil || m != "" {
		t.Fatalf("a directory was taken for the manifest: %q %v", m, err)
	}
}

// Without --image-dir, the image is the image folder next to the executable, and image
// files directly next to it (an older layout) are named in the error.
func TestMissingImageError(t *testing.T) {
	touch := func(dir, name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if d := defaultImageDir(); filepath.Base(d) != imageDirName || filepath.Dir(d) != exeDir() {
		t.Fatalf("defaultImageDir() = %q, exe in %q", d, exeDir())
	}

	root := t.TempDir()
	dir := filepath.Join(root, imageDirName)
	err := missingImageError(dir)
	if err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "image.json")) || !strings.Contains(err.Error(), "Extract the release zip again") ||
		strings.Contains(err.Error(), "where this hyperbutler does not look") {
		t.Fatalf("no image: %v", err)
	}

	touch(root, "system.vhd")
	touch(root, "image.json")
	err = missingImageError(dir)
	if err == nil || !strings.Contains(err.Error(), "image.json, system.vhd") && !strings.Contains(err.Error(), "system.vhd, image.json") ||
		!strings.Contains(err.Error(), "into an empty folder") {
		t.Fatalf("old layout: %v", err)
	}
	if got := looseImageFiles(root); len(got) != 2 {
		t.Fatalf("looseImageFiles = %q", got)
	}
	if got := looseImageFiles(dir); len(got) != 0 {
		t.Fatalf("looseImageFiles of a missing folder = %q", got)
	}
}
