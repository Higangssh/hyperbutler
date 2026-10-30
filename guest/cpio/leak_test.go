package cpio

import (
	"testing"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

// TestMain fails the package's tests if any goroutine is still running after
// they finish (ADR 0003, guardrail 1).
func TestMain(m *testing.M) {
	leaktest.VerifyTestMain(m)
}
