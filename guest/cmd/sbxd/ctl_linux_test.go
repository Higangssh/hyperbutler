//go:build linux

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"syscall"
	"testing"
)

// TestChildrenTakeCtrlC: a terminal's programs must not inherit sbxd's ignored signals, or
// Ctrl+C stops nothing in a session.
func TestChildrenTakeCtrlC(t *testing.T) {
	signal.Ignore(syscall.SIGINT, syscall.SIGQUIT)
	defer signal.Reset(syscall.SIGINT, syscall.SIGQUIT)
	if ignoredInChild(t)&(1<<(syscall.SIGINT-1)) == 0 {
		t.Skip("signal.Ignore did not reach the child; nothing to check")
	}
	catchInherited()
	if got := ignoredInChild(t); got&(1<<(syscall.SIGINT-1)|1<<(syscall.SIGQUIT-1)) != 0 {
		t.Fatalf("the child ignores %#x", got)
	}
}

func ignoredInChild(t *testing.T) uint64 {
	t.Helper()
	out, err := exec.Command("/bin/sh", "-c", "cat /proc/self/status").Output() //nolint:gosec // G204: fixed arguments
	if err != nil {
		t.Skipf("no /bin/sh: %v", err)
	}
	m := regexp.MustCompile(`SigIgn:\s*([0-9a-f]+)`).FindSubmatch(out)
	if m == nil {
		t.Fatalf("no SigIgn in %q", out)
	}
	v, err := strconv.ParseUint(string(m[1]), 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

var _ = os.Args
