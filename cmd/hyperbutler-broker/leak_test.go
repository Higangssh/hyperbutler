package main

import (
	"testing"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

// TestMain fails the package's tests if any goroutine is still running after
// they finish (ADR 0003, guardrail 1). The service tests open go-winio pipes, whose
// completion-port goroutine lives as long as the process.
func TestMain(m *testing.M) {
	leaktest.VerifyTestMain(m, leaktest.IgnoreWinioIOCP)
}
