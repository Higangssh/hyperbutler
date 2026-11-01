//go:build windows

package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func setOwnsConsole(t *testing.T, own bool) {
	t.Helper()
	prev, prevGUI := ownsConsole, launcherGUI
	ownsConsole = func() bool { return own }
	// The console launcher's tests: as on a machine without the WebView2 Runtime.
	launcherGUI = func() (int, bool) { return 0, false }
	t.Cleanup(func() { ownsConsole, launcherGUI = prev, prevGUI })
}

// fakeLauncher is a launcher whose status is code (and then 0 once started), whose
// input is input, and that counts starts and reads.
type fakeLauncher struct {
	*launcher
	out, errb bytes.Buffer
	in        *countingReader
	starts    int
}

type countingReader struct {
	r     io.Reader
	reads int
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.reads++
	return c.r.Read(p)
}

func newFakeLauncher(code int, input string, interactive bool, startErr error) *fakeLauncher {
	f := &fakeLauncher{in: &countingReader{r: strings.NewReader(input)}}
	f.launcher = &launcher{
		stdin: f.in, stdout: &f.out, stderr: &f.errb,
		closeWait: 10 * time.Second, startWait: 10 * time.Second,
		interactive: func(io.Reader) bool { return interactive },
		status: func(o, _ io.Writer) (int, error) {
			if f.starts > 0 {
				code = 0
			}
			_, _ = io.WriteString(o, "hyperbutler: STATUS\n")
			return code, nil
		},
		start: func(o, _ io.Writer) error {
			f.starts++
			if startErr != nil {
				return startErr
			}
			_, _ = io.WriteString(o, "hyperbutler is running\n")
			return nil
		},
	}
	return f
}

// REQ-UX-016: a stopped engine is offered a start on Enter; afterwards the window says
// the engine outlives it and waits for Enter again.
func TestLauncherStartsOnEnter(t *testing.T) {
	f := newFakeLauncher(exitNotRunning, "\n\n", true, nil)
	if code := f.run(); code != 0 {
		t.Fatalf("exit %d: %s%s", code, f.out.String(), f.errb.String())
	}
	out := f.out.String()
	for _, want := range []string{"STATUS", "Press Enter to start hyperbutler, or close this window.", "hyperbutler is running",
		"keeps running after this window closes", "Press Enter to close."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if f.starts != 1 {
		t.Fatalf("%d starts", f.starts)
	}
	if strings.Index(out, "hyperbutler is running") > strings.Index(out, "Press Enter to close.") {
		t.Fatal("waited before the start")
	}
}

// Closing the input (no Enter) starts nothing.
func TestLauncherNoAnswerStartsNothing(t *testing.T) {
	f := newFakeLauncher(exitNotRunning, "", true, nil)
	if code := f.run(); code != 0 || f.starts != 0 {
		t.Fatalf("exit %d, %d starts", code, f.starts)
	}
}

// A running engine: the status, the note that it keeps running, and a wait; no start.
func TestLauncherRunningWaits(t *testing.T) {
	f := newFakeLauncher(0, "\n", true, nil)
	if code := f.run(); code != 0 || f.starts != 0 {
		t.Fatalf("exit %d, %d starts", code, f.starts)
	}
	out := f.out.String()
	if !strings.Contains(out, "STATUS") || !strings.Contains(out, "Press Enter to close.") || strings.Contains(out, "Press Enter to start") {
		t.Fatalf("output:\n%s", out)
	}
	if f.in.reads == 0 {
		t.Fatal("did not wait for Enter")
	}
}

// A failed start is shown, and the window waits so it can be read.
func TestLauncherStartFails(t *testing.T) {
	f := newFakeLauncher(exitNotRunning, "\n\n", true, errors.New("boom"))
	if code := f.run(); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(f.errb.String(), "hyperbutler: boom") || !strings.Contains(f.out.String(), "Press Enter to close.") {
		t.Fatalf("out %q err %q", f.out.String(), f.errb.String())
	}
}

// stdin that is not a console is never read, and nothing starts.
func TestLauncherNeverReadsNonConsole(t *testing.T) {
	for _, code := range []int{exitNotRunning, 0} {
		f := newFakeLauncher(code, "\n\n", false, nil)
		if got := f.run(); got != 0 || f.starts != 0 || f.in.reads != 0 {
			t.Fatalf("status %d: exit %d, %d starts, %d reads", code, got, f.starts, f.in.reads)
		}
		if strings.Contains(f.out.String(), "Press Enter") {
			t.Fatalf("asked for Enter without a console:\n%s", f.out.String())
		}
	}
}

// stuckReader is a console nobody types into: Read blocks until the test ends.
type stuckReader struct{ release chan struct{} }

func newStuckReader(t *testing.T) *stuckReader {
	r := &stuckReader{release: make(chan struct{})}
	t.Cleanup(func() { close(r.release) })
	return r
}

func (r *stuckReader) Read([]byte) (int, error) {
	<-r.release
	return 0, io.EOF
}

// Review M2: a window nobody answers (Start-Process -Wait, a scheduled task, a VS Code
// task: they make a console of its own too) does not keep the process forever. With a
// running engine, "Press Enter to close." gives up after closeWait and exits 0; with a
// stopped one, the start prompt gives up after startWait, says so, starts nothing and
// exits 0.
func TestLauncherWaitsEnd(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   int
		prompt string
		said   string
	}{
		{"running", 0, "Press Enter to close.", ""},
		{"stopped", exitNotRunning, "Press Enter to start hyperbutler, or close this window.", "No answer in 50ms: hyperbutler was not started."},
	} {
		f := newFakeLauncher(tc.code, "", true, nil)
		f.stdin = newStuckReader(t)
		f.closeWait, f.startWait = 50*time.Millisecond, 50*time.Millisecond
		done := make(chan int, 1)
		go func() { done <- f.run() }()
		select {
		case code := <-done:
			if code != 0 || f.starts != 0 {
				t.Fatalf("%s: exit %d, %d starts", tc.name, code, f.starts)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: still waiting for Enter", tc.name)
		}
		out := f.out.String()
		if !strings.Contains(out, tc.prompt) || !strings.Contains(out, tc.said) {
			t.Errorf("%s: output:\n%s", tc.name, out)
		}
	}
	if s := waitText(launcherStartWait); s != "5 minutes" {
		t.Errorf("waitText(launcherStartWait) = %q", s)
	}
	if launcherCloseWait != time.Minute || waitText(launcherCloseWait) != "1 minute" {
		t.Errorf("launcherCloseWait %s", launcherCloseWait)
	}
}

// Review M2: HYPERBUTLER_NO_LAUNCHER=1 prints the usage even in a console of its own.
func TestRunNoLauncherEnv(t *testing.T) {
	testHome(t)
	setOwnsConsole(t, true)
	prev := launcherInput
	launcherInput = func() io.Reader { return strings.NewReader("\n") }
	t.Cleanup(func() { launcherInput = prev })
	t.Setenv(noLauncherEnv, "1")
	if code, out, _ := runCLI(t); code != 0 || !strings.Contains(out, "Usage:") || strings.Contains(out, "not set up yet") ||
		!strings.Contains(out, noLauncherEnv) {
		t.Fatalf("%s=1: exit %d, %q", noLauncherEnv, code, out)
	}
	t.Setenv(noLauncherEnv, "0")
	if code, out, _ := runCLI(t); code != 0 || strings.Contains(out, "Usage:") {
		t.Fatalf("%s=0: exit %d, %q", noLauncherEnv, code, out)
	}
}

// A hidden console window, or stdout that is not the console, is not a window of its
// own; the test process has neither a console of its own nor a visible one here.
func TestOwnsConsoleChecks(t *testing.T) {
	if consoleProcesses() == 1 && consoleWindowVisible() && isConsole(os.Stdout) {
		t.Skip("the test runs in a visible console of its own")
	}
	if ownsConsole() {
		t.Fatal("ownsConsole without a visible console of its own")
	}
}

// go test is not started by Explorer, so the parent check says no; startedBefore
// orders a parent before its child.
func TestParentIsExplorer(t *testing.T) {
	if parentIsExplorer() {
		t.Fatal("go test's parent taken for explorer.exe")
	}
	self := windows.GetCurrentProcessId()
	parent := uint32(os.Getppid())
	if !startedBefore(parent, self) {
		t.Fatalf("parent %d not started before %d", parent, self)
	}
	if startedBefore(self, parent) {
		t.Fatal("this process taken as started before its parent")
	}
}

// The detection: in its own console, run with no arguments is the launcher; in a
// shared one, the usage with exit 0.
func TestRunNoArgsSeam(t *testing.T) {
	testHome(t)
	setOwnsConsole(t, true)
	prev := launcherInput
	launcherInput = func() io.Reader { return strings.NewReader("\n") } // not a console
	t.Cleanup(func() { launcherInput = prev })
	// The input is not a console, so the launcher shows the status of the empty
	// installation and returns without reading.
	code, out, _ := runCLI(t)
	if code != 0 || !strings.Contains(out, "not set up yet") || strings.Contains(out, "Usage:") {
		t.Fatalf("own console: exit %d, %q", code, out)
	}
	setOwnsConsole(t, false)
	if code, out, _ := runCLI(t); code != 0 || !strings.HasPrefix(out, "hyperbutler: a Docker engine") || !strings.Contains(out, "Usage:") {
		t.Fatalf("shared console: exit %d, %q", code, out)
	}
	if n := consoleProcesses(); n < 0 {
		t.Fatalf("console processes %d", n)
	}
}
