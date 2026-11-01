//go:build windows

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/gui/app"
	"github.com/Higangssh/hyperbutler/internal/gui/i18n"
)

// REQ-UX-016 (0.1, console): hyperbutler.exe started with no arguments in a console of
// its own (a double-click in Explorer, the Run box, a shortcut) shows the status and
// offers to start a stopped engine, instead of flashing the usage in a window that
// closes at once. From a shell it prints the usage, as before.

var (
	launcherKernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procGetConsoleProcessList = launcherKernel32.NewProc("GetConsoleProcessList")
	procGetConsoleWindow      = launcherKernel32.NewProc("GetConsoleWindow")
)

// consoleProcesses is how many processes share this process's console (0 without one).
func consoleProcesses() int {
	var pids [8]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return int(n)
}

// consoleWindowVisible reports whether this process's console has a window on screen.
// A console started hidden (Start-Process -WindowStyle Hidden, a scheduled task that
// runs whether or not the user is signed in, CREATE_NO_WINDOW) has none, or a hidden
// one.
func consoleWindowVisible() bool {
	hwnd, _, _ := procGetConsoleWindow.Call()
	return hwnd != 0 && windows.IsWindowVisible(windows.HWND(hwnd))
}

// parentIsExplorer reports whether the process that started this one is explorer.exe,
// as for a double-click, the Run box, the Start menu or a shortcut. Start-Process -Wait,
// start /wait, wt.exe, Task Scheduler and VS Code tasks have another parent, so a script
// in a visible window never meets the launcher's prompt. If the parent has exited, or a
// later process reuses its PID (it then starts after this one), the answer is no.
func parentIsExplorer() bool {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	self := windows.GetCurrentProcessId()
	var parent uint32
	names := map[uint32]string{}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		names[e.ProcessID] = windows.UTF16ToString(e.ExeFile[:])
		if e.ProcessID == self {
			parent = e.ParentProcessID
		}
	}
	name, ok := names[parent]
	if !ok || !strings.EqualFold(name, "explorer.exe") {
		return false
	}
	return startedBefore(parent, self)
}

// startedBefore reports whether process a was created before process b, so a reused
// parent PID is not taken for the real parent.
func startedBefore(a, b uint32) bool {
	created := func(pid uint32) (windows.Filetime, bool) {
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
		if err != nil {
			return windows.Filetime{}, false
		}
		defer func() { _ = windows.CloseHandle(h) }()
		var c, x, k, u windows.Filetime
		if windows.GetProcessTimes(h, &c, &x, &k, &u) != nil {
			return windows.Filetime{}, false
		}
		return c, true
	}
	ca, ok1 := created(a)
	cb, ok2 := created(b)
	return ok1 && ok2 && ca.Nanoseconds() <= cb.Nanoseconds()
}

// noLauncherEnv names the variable that turns the launcher off: with it set to 1,
// hyperbutler.exe with no arguments prints the usage wherever it runs.
const noLauncherEnv = "HYPERBUTLER_NO_LAUNCHER"

// ownsConsole reports whether hyperbutler.exe runs in a console window of its own, as
// Explorer and shortcuts make one: the console was created for this process alone (a
// shell sharing it makes the count 2 or more), its window is visible, and stdout is
// that console (not redirected to a file or a pipe), and explorer.exe started it.
// Start-Process -Wait, start /wait, a scheduled task the user sees, a VS Code "process"
// task and wt.exe hyperbutler make such a window too, but with another parent. The
// launcher's waits still end on their own (launcherCloseWait, launcherStartWait), and
// HYPERBUTLER_NO_LAUNCHER=1 turns it off. A variable, so tests
// can choose either path.
var ownsConsole = func() bool {
	return consoleProcesses() == 1 && consoleWindowVisible() && isConsole(os.Stdout) && parentIsExplorer()
}

// launcherDisabled reports whether HYPERBUTLER_NO_LAUNCHER=1 turns the launcher off.
func launcherDisabled() bool { return os.Getenv(noLauncherEnv) == "1" }

// launcherInput is the launcher's input (os.Stdin; replaced in tests).
var launcherInput = func() io.Reader { return os.Stdin }

const (
	// launcherCloseWait bounds "Press Enter to close.": a window nobody reads closes
	// on its own.
	launcherCloseWait = time.Minute
	// launcherStartWait bounds "Press Enter to start": no answer starts nothing.
	launcherStartWait = 5 * time.Minute
)

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
	// closeWait and startWait bound the waits for Enter (launcherCloseWait,
	// launcherStartWait).
	closeWait, startWait time.Duration
}

func newLauncher(stdout, stderr io.Writer) *launcher {
	return &launcher{
		stdin: launcherInput(), stdout: stdout, stderr: stderr, interactive: isConsole,
		status:    func(o, e io.Writer) (int, error) { return cmdStatus(nil, o, e) },
		start:     func(o, e io.Writer) error { return cmdStart(nil, o, e) },
		closeWait: launcherCloseWait, startWait: launcherStartWait,
	}
}

const launcherKeepsRunning = "hyperbutler keeps running after this window closes, until `hyperbutler stop` or sign-out.\n" +
	"In a terminal: docker run hello-world, hyperbutler status, hyperbutler stop."

// errNoAnswer is a wait for Enter that ran out of time.
var errNoAnswer = errors.New("no answer")

// readLine waits up to d for a line from in. When the time runs out it returns
// errNoAnswer and leaves the read running: the process exits right after.
func readLine(in *bufio.Reader, d time.Duration) error {
	got := make(chan error, 1)
	go func() {
		_, err := in.ReadString('\n')
		got <- err
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case err := <-got:
		return err
	case <-t.C:
		return errNoAnswer
	}
}

// waitText is d in words: "5 minutes", or d itself when it is not whole minutes.
func waitText(d time.Duration) string {
	switch {
	case d == time.Minute:
		return "1 minute"
	case d > time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%d minutes", d/time.Minute)
	}
	return d.String()
}

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
			if readLine(in, l.closeWait) != nil {
				_, _ = fmt.Fprintln(l.stdout)
			}
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
	if err := readLine(in, l.startWait); err != nil {
		_, _ = fmt.Fprintln(l.stdout)
		if errors.Is(err, errNoAnswer) {
			_, _ = fmt.Fprintf(l.stdout, "No answer in %s: hyperbutler was not started.\n", waitText(l.startWait))
		}
		return 0 // no answer (the time ran out, or the input closed): start nothing
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

var procFreeConsole = launcherKernel32.NewProc("FreeConsole")

// launcherGUI opens the window for a start with no arguments in a console of its own
// (REQ-UX-016 from 0.2). It returns ran false when the window cannot open, after a
// message box that names the WebView2 Runtime's download page: the console launcher
// then runs as in 0.1. A variable, so tests can choose either path.
var launcherGUI = func() (code int, ran bool) {
	if !guiRuntimeOK() {
		lang := ""
		if in, err := openInstall(); err == nil && in.settings != nil {
			lang = in.settings.GUILanguage
		}
		title, text := noRuntimeText(i18n.Messages(i18n.Resolve(lang, app.WindowsLanguages())))
		app.MessageBox(title, text)
		return 0, false
	}
	// The console window Explorer made for this process closes: the window replaces it.
	_, _, _ = procFreeConsole.Call()
	return runGUI(guiOptions{wait: true}, nil, nil), true
}
