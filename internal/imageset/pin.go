// Package imageset opens and verifies a guest image set (ADR 0005): the kernel, the
// stage-1 initramfs and the read-only system disk that image.json names, each checked
// against the manifest, and image.json against the SHA-256 pin a release build carries.
// hyperbutler.exe (internal/engine) and the HCS access broker (ADR 0004 Implementation
// decision 3) both boot only what it verified.
package imageset

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// pinnedManifest is the SHA-256 (lowercase hex) of the image.json that this build
// boots, or empty for a development build. A release build sets it (ADR 0005, Decision
// 4): dist/package.ps1 links in the hash of the guest image job's image.json with
//
//	-ldflags "-X github.com/Higangssh/hyperbutler/internal/imageset.pinnedManifest=<sha256>"
//
// image.json carries the size and SHA-256 of every other image file, so pinning it pins
// the whole set. The same flag pins hyperbutler.exe and hyperbutler-broker.exe.
var pinnedManifest string

// Pin returns the image.json SHA-256 this build was linked with, and whether it has
// one. A build without a pin accepts any image whose files match its own manifest.
func Pin() (sum string, pinned bool) {
	return pinnedManifest, pinnedManifest != ""
}

// CheckPin checks the manifest bytes b, read from path, against pin. An empty pin
// accepts anything; a pin that is not a SHA-256 (a broken build) accepts nothing.
func CheckPin(pin, path string, b []byte) error {
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
			"Extract the release zip again into an empty folder, and keep hyperbutler.exe next to its image\\ folder; "+
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
