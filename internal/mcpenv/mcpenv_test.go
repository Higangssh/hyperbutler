package mcpenv

import (
	"testing"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

// TestMain fails the package's tests if any goroutine is still running after
// they finish (ADR 0003, guardrail 1).
func TestMain(m *testing.M) {
	leaktest.VerifyTestMain(m)
}

func TestValid(t *testing.T) {
	for v, want := range map[string]bool{
		"":                  true,
		"a=1":               true,
		"a=1,b=":            true,
		"x":                 false,
		"a=1,x":             false,
		",":                 false,
		"hintomitempty=1,x": false,
	} {
		if got := Valid(v); got != want {
			t.Errorf("Valid(%q) = %v, want %v", v, got, want)
		}
	}
}
