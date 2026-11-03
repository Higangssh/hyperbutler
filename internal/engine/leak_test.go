package engine

import (
	"testing"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

// TestMain fails the package's tests if any goroutine is still running after
// they finish (ADR 0003, guardrail 1). winipc's completion port goroutine, started by
// the first pipe or hvsock a test opens, lives for the process.
func TestMain(m *testing.M) {
	leaktest.VerifyTestMain(m, leaktest.IgnoreIOCP)
}
