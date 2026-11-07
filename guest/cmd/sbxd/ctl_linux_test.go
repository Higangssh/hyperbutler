//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/sandbox/ctl"
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

// TestQuietProgramStillEnds: a program that prints, says nothing for longer than a write
// may take, then exits, must still hand its exit code to the window that watched it, and
// the finished terminal must not count against the limit.
func TestQuietProgramStillEnds(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	oldWait, oldDir := viewerWait, terminalDir
	viewerWait, terminalDir = 200*time.Millisecond, t.TempDir()
	defer func() { viewerWait, terminalDir = oldWait, oldDir }()

	var ts terminals
	for i := range ctl.MaxTerminals + 2 {
		host, guest := net.Pipe()
		served := make(chan error, 1)
		req := &ctl.TermRequest{ID: fmt.Sprintf("sh-%d", i+2), Argv: []string{"/bin/sh", "-c", "echo a; sleep 0.6; exit 3"}}
		go func() { served <- ts.serve(guest, req) }()
		code, out, err := readTerminal(host, 10*time.Second)
		_ = host.Close()
		if err != nil {
			t.Fatalf("terminal %d: %v (printed %q)", i, err, out)
		}
		if code != 3 || !strings.Contains(out, "a") {
			t.Fatalf("terminal %d: code %d, printed %q", i, code, out)
		}
		if err := <-served; err != nil {
			t.Fatalf("terminal %d: serve: %v", i, err)
		}
	}
	ts.mu.Lock()
	left := len(ts.m)
	ts.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d finished terminals kept", left)
	}
}

func readTerminal(c net.Conn, limit time.Duration) (int32, string, error) {
	_ = c.SetReadDeadline(time.Now().Add(limit))
	var out strings.Builder
	for {
		ty, data, err := ctl.ReadFrame(c)
		if err != nil {
			return 0, out.String(), err
		}
		switch ty {
		case ctl.TypeOutput:
			out.Write(data)
		case ctl.TypeExit:
			code, err := ctl.ParseExit(data)
			return code, out.String(), err
		default:
			return 0, out.String(), fmt.Errorf("frame type %d", ty)
		}
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
