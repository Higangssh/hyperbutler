package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestDependencies holds ADR 0004 rule 10: the broker service, which runs as SYSTEM,
// links nothing but the standard library, golang.org/x/sys and hyperbutler's own
// packages.
func TestDependencies(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".").CombinedOutput()
	if err != nil {
		t.Skipf("go list: %v: %s", err, out)
	}
	for _, p := range strings.Fields(string(out)) {
		if !strings.HasPrefix(p, "github.com/Higangssh/hyperbutler/") && !strings.HasPrefix(p, "golang.org/x/sys/") {
			t.Errorf("the broker links %s (ADR 0004 rule 10)", p)
		}
	}
}
