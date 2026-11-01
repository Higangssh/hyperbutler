//go:build windows

package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func setOwnsConsole(t *testing.T, own bool) {
	t.Helper()
	prev := ownsConsole
	ownsConsole = func() bool { return own }
	t.Cleanup(func() { ownsConsole = prev })
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
