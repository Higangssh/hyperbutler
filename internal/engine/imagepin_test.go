package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckImageDirPin(t *testing.T) {
	dir := t.TempDir()
	b := []byte(`{"version":1}`)
	if err := os.WriteFile(filepath.Join(dir, "image.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	pin := hex.EncodeToString(s[:])
	if err := checkImageDirPin(dir, pin); err != nil {
		t.Errorf("matching image: %v", err)
	}
	if err := checkImageDirPin(t.TempDir(), pin); err == nil {
		t.Error("a folder without image.json passed")
	}
	if err := checkImageDirPin(dir, strings.Repeat("0", 64)); err == nil {
		t.Error("another image passed")
	}
	if err := CheckImageDirPin(dir); err != nil {
		t.Errorf("a development build refused an image: %v", err)
	}
}
