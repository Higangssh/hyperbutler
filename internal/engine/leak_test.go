package engine

import (
	"testing"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

// TestMain fails the package's tests if any goroutine is still running after
// they finish (ADR 0003, guardrail 1). go-winio's completion port goroutine, started by
// the first broker pipe a test dials, lives for the process.
func TestMain(m *testing.M) {
	leaktest.VerifyTestMain(m, leaktest.IgnoreWinioIOCP)
}
