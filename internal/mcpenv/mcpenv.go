// Package mcpenv keeps a malformed MCPGODEBUG from crashing every hyperbutler command.
//
// The MCP SDK parses MCPGODEBUG in a package init and panics on a malformed value, and
// it is linked into hyperbutler.exe for `hyperbutler mcp`, so a stray MCPGODEBUG=x in
// the environment would stop `start`, `status` and everything else. This package
// removes such a value before the SDK reads it. It imports only the standard library,
// and its import path sorts before the SDK's, so the Go spec's initialization order
// (since Go 1.21: of the packages whose imports are initialized, the first by import
// path goes next) runs this init first. A well-formed value is left alone.
//
// That order depends on the module path: github.com/Higangssh/hyperbutler sorts before
// github.com/modelcontextprotocol/go-sdk ('H' < 'm'). A rename of the module to a path
// that sorts after the SDK's would run the SDK's init first and bring the panic back.
// The subprocess tests catch that: TestMalformedMCPGODEBUG in cmd/hyperbutler (Windows)
// and TestMalformedMCPGODEBUGSubprocess in internal/mcpserver (every platform) start
// their test binary again with MCPGODEBUG=x and fail if it panics. See CONTRIBUTING.md.
package mcpenv

import (
	"os"
	"strings"
)

// Key is the variable the MCP SDK reads.
const Key = "MCPGODEBUG"

// Dropped is the malformed value that was removed, or "" if there was none.
var Dropped string

func init() {
	if v := os.Getenv(Key); !Valid(v) {
		Dropped = v
		_ = os.Unsetenv(Key)
	}
}

// Valid reports whether v parses the way the SDK expects: empty, or comma-separated
// key=value pairs.
func Valid(v string) bool {
	if v == "" {
		return true
	}
	for part := range strings.SplitSeq(v, ",") {
		if !strings.Contains(part, "=") {
			return false
		}
	}
	return true
}
