package imageset

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestCheckPin(t *testing.T) {
	b := []byte(`{"version":1}`)
	s := sha256.Sum256(b)
	pin := hex.EncodeToString(s[:])
	if err := CheckPin("", "image.json", b); err != nil {
		t.Errorf("no pin: %v", err)
	}
	if err := CheckPin(pin, "image.json", b); err != nil {
		t.Errorf("matching pin: %v", err)
	}
	err := CheckPin(pin, `C:\x\image.json`, append(b, '\n'))
	if err == nil || !strings.Contains(err.Error(), `C:\x\image.json`) || !strings.Contains(err.Error(), pin) {
		t.Errorf("mismatch: %v", err)
	}
	for _, bad := range []string{"0", strings.ToUpper(pin), pin + "0", strings.Repeat("g", 64)} {
		if err := CheckPin(bad, "image.json", b); err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Errorf("pin %q: %v", bad, err)
		}
	}
}

// A plain build has no pin. dist/package.ps1 sets one with -ldflags -X, which works only
// while pinnedManifest stays an uninitialized package-level string variable.
func TestImagePinDefault(t *testing.T) {
	if sum, pinned := Pin(); pinned || sum != "" {
		t.Fatalf("test binary has a pin %q", sum)
	}
}
