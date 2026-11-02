package redact

import (
	"strings"
	"testing"
)

// The kernel echoes its command line, with the VM secret (ADR 0004 rule 6), into the
// console log when it is not quiet; a diagnostics bundle must not carry it.
func TestKernelCmdlineSecret(t *testing.T) {
	const hex = "101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f"
	for _, line := range []string{
		"[    0.000000] Kernel command line: console=ttyS0 hyperbutler.secret=" + hex + " hyperbutler.net=1024",
		"Kernel command line: hyperbutler.secret=" + hex,
		"hyperbutler-t 0.01 init-start\n[    0.000000] Command line: quiet hyperbutler.secret=" + hex + "\nnext",
	} {
		got := String(line)
		if strings.Contains(got, hex[:16]) || !strings.Contains(got, "hyperbutler.secret="+Mark) {
			t.Errorf("String(%q) = %q", line, got)
		}
	}
}
