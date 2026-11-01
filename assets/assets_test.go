package assets

import (
	"bytes"
	"image/png"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

// TestMain fails the package's tests if any goroutine is still running after
// they finish (ADR 0003, guardrail 1).
func TestMain(m *testing.M) {
	leaktest.VerifyTestMain(m)
}

func TestImagesDecode(t *testing.T) {
	for name, b := range map[string][]byte{"mascot": Mascot, "icon": Icon} {
		cfg, err := png.DecodeConfig(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if cfg.Width < 128 || cfg.Width > 512 {
			t.Errorf("%s is %d px wide", name, cfg.Width)
		}
	}
}
