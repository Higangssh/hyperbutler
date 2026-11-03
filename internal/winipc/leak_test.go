package winipc

import (
	"testing"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

// TestMain fails the package's tests if any goroutine is still running after they
// finish (ADR 0003, guardrail 1), other than the package's own completion-port loop,
// which lives for the process by design.
func TestMain(m *testing.M) {
	leaktest.VerifyTestMain(m, leaktest.IgnoreIOCP)
}
