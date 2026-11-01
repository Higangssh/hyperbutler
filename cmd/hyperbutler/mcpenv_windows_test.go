//go:build windows

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// A malformed MCPGODEBUG must not crash hyperbutler: the MCP SDK panics on it in a
// package init, before main, and internal/mcpenv has to run first. The test binary
// links the SDK like hyperbutler.exe, so it is started again with the variable set: if
// the SDK's init ran first, that process would panic before running any test.
func TestMalformedMCPGODEBUG(t *testing.T) {
	if os.Getenv("HB_MCPENV_CHILD") == "1" {
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMalformedMCPGODEBUG$")
	cmd.Env = append(os.Environ(), "MCPGODEBUG=x", "HB_MCPENV_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil || strings.Contains(string(out), "panic") {
		t.Fatalf("child with MCPGODEBUG=x: %v\n%s", err, out)
	}
}
