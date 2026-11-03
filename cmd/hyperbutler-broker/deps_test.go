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
		// Failing, not skipping: a guard that can go quiet guards nothing.
		t.Fatalf("go list: %v: %s", err, out)
	}
	pkgs := strings.Fields(string(out))
	if len(pkgs) == 0 {
		t.Fatal("go list listed no packages")
	}
	for _, p := range pkgs {
		if !strings.HasPrefix(p, "github.com/Higangssh/hyperbutler/") && !strings.HasPrefix(p, "golang.org/x/sys/") {
			t.Errorf("the broker links %s (ADR 0004 rule 10)", p)
		}
	}
}
