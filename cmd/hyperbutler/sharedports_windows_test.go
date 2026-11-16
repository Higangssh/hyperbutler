//go:build windows

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/engine"
)

// The child of TestSharedPortsLive: it listens where HB_TEST_SHARED_LISTEN says, prints
// "ready" (or why not) and holds the listener until its stdin closes, 30 s at most.
func TestSharedPortsHelper(t *testing.T) {
	addr := os.Getenv("HB_TEST_SHARED_LISTEN")
	if addr == "" {
		t.Skip("helper process only")
	}
	l, err := net.Listen("tcp", addr)
	switch {
	case errors.Is(err, windows.WSAEADDRINUSE):
		fmt.Println("in-use")
		os.Exit(0)
	case errors.Is(err, windows.WSAEAFNOSUPPORT), errors.Is(err, windows.WSAEADDRNOTAVAIL):
		fmt.Println("no-ipv6")
		os.Exit(0)
	case err != nil:
		fmt.Println("error:", err)
		os.Exit(1)
	}
	defer func() { _ = l.Close() }()
	fmt.Println("ready")
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
	}
}

// startHelper starts a helper listening on addr and returns its first line.
func startHelper(t *testing.T, addr string) (string, *exec.Cmd) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSharedPortsHelper$")
	cmd.Env = append(os.Environ(), "HB_TEST_SHARED_LISTEN="+addr)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	line, _ := bufio.NewReader(out).ReadString('\n')
	return strings.TrimSpace(line), cmd
}

// Issue #2: another process listening on a published port after "the engine" (this
// test process) did is named, with its executable. The other listener is on [::1], so
// the test opens no wildcard socket (no Windows Firewall prompt).
func TestSharedPortsLive(t *testing.T) {
	created, err := engine.CreatedTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; ; attempt++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := uint16(l.Addr().(*net.TCPAddr).Port)
		st := &engine.State{PID: os.Getpid(), PIDCreated: created, PublishedPorts: []uint16{port}}
		if got, _ := sharedPorts(st); len(got) != 0 {
			_ = l.Close()
			if attempt < 3 {
				continue // another process already shares this ephemeral port
			}
			t.Fatalf("shared before the other listener exists: %+v", got)
		}
		line, cmd := startHelper(t, fmt.Sprintf("[::1]:%d", port))
		switch {
		case line == "in-use" && attempt < 3:
			_ = l.Close()
			continue
		case line == "no-ipv6":
			_ = l.Close()
			t.Skip("no IPv6 loopback on this host")
		case line != "ready":
			_ = l.Close()
			t.Fatalf("helper: %q", line)
		}

		got, more := sharedPorts(st)
		want := SharedPortJSON{Port: port, PID: uint32(cmd.Process.Pid), Process: filepath.Base(os.Args[0]), Listen: []string{fmt.Sprintf("[::1]:%d", port)}}
		if len(got) != 1 || more != 0 || got[0].Port != want.Port || got[0].PID != want.PID ||
			!strings.EqualFold(got[0].Process, want.Process) || strings.Join(got[0].Listen, ",") != want.Listen[0] {
			t.Errorf("got %+v (%d more), want %+v", got, more, want)
		}

		// A PID that is no longer the engine recorded (PIDCreated differs) lists nothing.
		if got, _ := sharedPorts(&engine.State{PID: os.Getpid(), PIDCreated: created + 1, PublishedPorts: []uint16{port}}); got != nil {
			t.Errorf("stale engine pid: %+v", got)
		}
		// A loopback listener of the engine that is not in PublishedPorts is not reported.
		// The published port is one the engine (this process) holds itself: port+1 could
		// be another process's, which would rightly be reported (a CI run hit that).
		other, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		otherPort := uint16(other.Addr().(*net.TCPAddr).Port)
		if got, _ := sharedPorts(&engine.State{PID: os.Getpid(), PIDCreated: created, PublishedPorts: []uint16{otherPort}}); got != nil {
			t.Errorf("unpublished port reported: %+v", got)
		}
		_ = other.Close()
		_ = l.Close()
		return
	}
}

func TestSharedPortsNoState(t *testing.T) {
	for _, st := range []*engine.State{nil, {PID: 0, PublishedPorts: []uint16{80}}, {PID: os.Getpid()}} {
		if got, more := sharedPorts(st); got != nil || more != 0 {
			t.Errorf("%+v: %+v, %d", st, got, more)
		}
	}
}

func TestSharedPortText(t *testing.T) {
	for _, tt := range []struct {
		in   SharedPortJSON
		want string
	}{
		{SharedPortJSON{Port: 8080, PID: 7, Process: "com.docker.backend.exe", Listen: []string{"0.0.0.0:8080", "[::]:8080"}},
			"8080 is also listened on by com.docker.backend.exe (process 7) on 0.0.0.0:8080 and [::]:8080; localhost reaches hyperbutler's container, not that program"},
		{SharedPortJSON{Port: 80, PID: 4, Listen: []string{"[::]:80"}},
			`80 is also listened on by process 4 (tasklist /fi "PID eq 4" names it) on [::]:80; localhost reaches hyperbutler's container, not that program`},
	} {
		if got := sharedPortText(tt.in); got != tt.want {
			t.Errorf("got  %q\nwant %q", got, tt.want)
		}
	}
	got := sharedPortText(SharedPortJSON{Port: 1, PID: 2, Process: "a\x1b[2Jb" + strings.Repeat("x", 1000) + ".exe", Listen: []string{"0.0.0.0:1"}})
	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("control character printed: %q", got)
	}
	if len([]rune(got)) > 250 {
		t.Errorf("process name not capped: %d runes", len([]rune(got)))
	}
}

// An engine that exited without shutting down leaves its last ports in state.json;
// `status` reports it stopped and lists none of them, as the window does.
func TestPrintStatusStalePorts(t *testing.T) {
	dir := t.TempDir()
	in := &install{l: engine.Layout{Home: dir, Data: dir}}
	st := &engine.State{Phase: engine.PhaseRunning, PID: 42, Started: time.Now(),
		UnpublishedPorts: []engine.UnpublishedPort{{Container: "0123456789ab", Port: 8080, Reason: "Ports are not available"}}}
	var out strings.Builder
	if err := in.printStatus(&out, StatusJSON{State: st}, nil, dockerCLI{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "exited without shutting down") || strings.Contains(out.String(), "ports:") {
		t.Errorf("stale state:\n%s", out.String())
	}
}
