package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Higangssh/hyperbutler/guest/image"
)

// pinnedManifest is the SHA-256 (lowercase hex) of the image.json that this build
// boots, or empty for a development build. A release build sets it (ADR 0005, Decision
// 4): dist/package.ps1 links in the hash of the guest image job's image.json with
//
//	-ldflags "-X github.com/Higangssh/hyperbutler/internal/engine.pinnedManifest=<sha256>"
//
// image.json carries the size and SHA-256 of every other image file, so pinning it pins
// the whole set.
var pinnedManifest string

// ImagePin returns the image.json SHA-256 this build was linked with, and whether it
// has one. A build without a pin accepts any image whose files match its own manifest.
func ImagePin() (sum string, pinned bool) {
	return pinnedManifest, pinnedManifest != ""
}

// CheckImageDirPin reports whether this build would boot the image in dir as far as the
// pin goes: the image.json there matches it (nil for a development build). The engine
// checks the rest of the image when it boots.
func CheckImageDirPin(dir string) error {
	p := filepath.Join(dir, image.ManifestName)
	b, err := ReadFileBounded(p)
	if err != nil {
		return err
	}
	return checkPin(pinnedManifest, p, b)
}

// checkPin checks the manifest bytes b, read from path, against pin. An empty pin
// accepts anything; a pin that is not a SHA-256 (a broken build) accepts nothing.
func checkPin(pin, path string, b []byte) error {
	if pin == "" {
		return nil
	}
	if len(pin) != sha256.Size*2 || !isLowerHex(pin) {
		return fmt.Errorf("engine: this hyperbutler.exe carries a malformed guest image pin %q; download the release again", pin)
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != pin {
		exe := "hyperbutler.exe"
		if p, err := os.Executable(); err == nil {
			exe = p
		}
		where := "the guest image folder is " + filepath.Dir(path) + ", " + exe + " is in " + filepath.Dir(exe)
		if filepath.Base(exe) != "hyperbutler.exe" {
			where = "the guest image folder is " + filepath.Dir(path) // a test binary or another host
		}
		return fmt.Errorf("engine: %s is not the guest image this hyperbutler.exe was built with (sha256 %s, expected %s; %s). "+
			"Extract the release zip again into one folder, replacing every file, and keep hyperbutler.exe next to its image; "+
			"to boot another image, use a development build", path, got, pin, where)
	}
	return nil
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
