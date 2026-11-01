//go:build windows

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// REQ-UX-016 (0.1, console): hyperbutler.exe started with no arguments in a console of
// its own (a double-click in Explorer, the Run box, a shortcut) shows the status and
// offers to start a stopped engine, instead of flashing the usage in a window that
// closes at once. From a shell it prints the usage, as before.

var procGetConsoleProcessList = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList")

// consoleProcesses is how many processes share this process's console (0 without one).
func consoleProcesses() int {
	var pids [8]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return int(n)
}

// ownsConsole reports whether the console was created for this process alone, as
// Explorer and shortcuts do; a shell sharing it makes the count 2 or more. A variable,
// so tests can choose either path.
var ownsConsole = func() bool { return consoleProcesses() == 1 }

// launcherInput is the launcher's input (os.Stdin; replaced in tests).
var launcherInput = func() io.Reader { return os.Stdin }

// launcher is the interactive no-arguments mode. Its fields are the seams the tests
// replace.
type launcher struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	// interactive reports whether stdin is a console that can answer; nothing is read
	// from stdin otherwise.
	interactive func(io.Reader) bool
	status      func(stdout, stderr io.Writer) (int, error)
	start       func(stdout, stderr io.Writer) error
}

func newLauncher(stdout, stderr io.Writer) *launcher {
	return &launcher{
		stdin: launcherInput(), stdout: stdout, stderr: stderr, interactive: isConsole,
		status: func(o, e io.Writer) (int, error) { return cmdStatus(nil, o, e) },
		start:  func(o, e io.Writer) error { return cmdStart(nil, o, e) },
	}
}

const launcherKeepsRunning = "hyperbutler keeps running after this window closes, until `hyperbutler stop` or sign-out.\n" +
	"In a terminal: docker run hello-world, hyperbutler status, hyperbutler stop."

// run shows the status and acts on it; it returns the exit code.
func (l *launcher) run() int {
	_, _ = fmt.Fprintln(l.stdout, "hyperbutler: a Docker engine for Windows in a small Hyper-V VM, without WSL.")
	_, _ = fmt.Fprintln(l.stdout)
	code, err := l.status(l.stdout, l.stderr)
	interactive := l.interactive(l.stdin)
	var in *bufio.Reader
	if interactive {
		in = bufio.NewReader(l.stdin)
	}
	wait := func() {
		if in != nil {
			_, _ = fmt.Fprint(l.stdout, "Press Enter to close.")
			_, _ = in.ReadString('\n')
		}
	}
	_, _ = fmt.Fprintln(l.stdout)
	switch {
	case err != nil:
		l.printErr(err)
		wait()
		return 1
	case code != exitNotRunning:
		_, _ = fmt.Fprintln(l.stdout, launcherKeepsRunning)
		wait()
		return 0
	case in == nil:
		_, _ = fmt.Fprintln(l.stdout, "Run `hyperbutler start` to start it.")
		return 0
	}
	_, _ = fmt.Fprint(l.stdout, "Press Enter to start hyperbutler, or close this window.")
	if _, err := in.ReadString('\n'); err != nil {
		_, _ = fmt.Fprintln(l.stdout)
		return 0 // no answer (the input closed): start nothing
	}
	if err := l.start(l.stdout, l.stderr); err != nil {
		l.printErr(err)
		wait()
		return 1
	}
	_, _ = fmt.Fprintln(l.stdout)
	_, _ = fmt.Fprintln(l.stdout, launcherKeepsRunning)
	wait()
	return 0
}

// printErr prints err as run does, unless its explanation was already printed.
func (l *launcher) printErr(err error) {
	var se *shownError
	if !errors.As(err, &se) {
		_, _ = fmt.Fprintln(l.stderr, "hyperbutler:", err)
	}
}
