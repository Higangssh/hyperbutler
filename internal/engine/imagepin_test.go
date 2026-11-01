package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckPin(t *testing.T) {
	b := []byte(`{"version":1}`)
	s := sha256.Sum256(b)
	pin := hex.EncodeToString(s[:])
	if err := checkPin("", "image.json", b); err != nil {
		t.Errorf("no pin: %v", err)
	}
	if err := checkPin(pin, "image.json", b); err != nil {
		t.Errorf("matching pin: %v", err)
	}
	err := checkPin(pin, `C:\x\image.json`, append(b, '\n'))
	if err == nil || !strings.Contains(err.Error(), `C:\x\image.json`) || !strings.Contains(err.Error(), pin) {
		t.Errorf("mismatch: %v", err)
	}
	for _, bad := range []string{"0", strings.ToUpper(pin), pin + "0", strings.Repeat("g", 64)} {
		if err := checkPin(bad, "image.json", b); err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Errorf("pin %q: %v", bad, err)
		}
	}
}

func TestCheckImageDirPin(t *testing.T) {
	dir := t.TempDir()
	b := []byte(`{"version":1}`)
	if err := os.WriteFile(filepath.Join(dir, "image.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	old := pinnedManifest
	t.Cleanup(func() { pinnedManifest = old })
	pinnedManifest = hex.EncodeToString(s[:])
	if err := CheckImageDirPin(dir); err != nil {
		t.Errorf("matching image: %v", err)
	}
	if err := CheckImageDirPin(t.TempDir()); err == nil {
		t.Error("a folder without image.json passed")
	}
	pinnedManifest = strings.Repeat("0", 64)
	if err := CheckImageDirPin(dir); err == nil {
		t.Error("another image passed")
	}
}

// A plain build has no pin. dist/package.ps1 sets one with -ldflags -X, which works only
// while pinnedManifest stays an uninitialized package-level string variable.
func TestImagePinDefault(t *testing.T) {
	if sum, pinned := ImagePin(); pinned || sum != "" {
		t.Fatalf("test binary has a pin %q", sum)
	}
}
