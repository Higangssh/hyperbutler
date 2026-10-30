//go:build windows

// Command winbox is the winbox CLI: a Docker engine for Windows in a small Hyper-V VM,
// without WSL.
//
//	winbox start    boot the VM in a background engine process, select the docker context
//	winbox stop     shut the VM down (gracefully, then forced after --timeout)
//	winbox status   show whether it runs, and where its data is
//	winbox logs     print the engine log (or the guest console with --console)
//	winbox delete   remove the VM state and the data disk
//
// See docs/research/cli-engine.md.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
)

const usageText = `winbox: a Docker engine for Windows in a small Hyper-V VM, without WSL

Usage:
  winbox start   [--data DIR] [--image-dir DIR] [--memory SIZE] [--cpus N] [--disk-size SIZE]
  winbox stop    [--timeout DURATION]
  winbox status  [--json]
  winbox logs    [--console] [-n LINES] [-f]
  winbox delete  [--yes]
  winbox version

State lives in %LOCALAPPDATA%\winbox (override with WINBOX_HOME).
Run "winbox COMMAND -h" for a command's flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// exitNotRunning is the exit code of `winbox status` when winbox is not running.
const exitNotRunning = 3

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	var err error
	code := 0
	switch args[0] {
	case "start":
		err = cmdStart(args[1:], stdout, stderr)
	case "stop":
		err = cmdStop(args[1:], stdout, stderr)
	case "status":
		code, err = cmdStatus(args[1:], stdout, stderr)
	case "logs":
		err = cmdLogs(args[1:], stdout, stderr)
	case "delete":
		err = cmdDelete(args[1:], os.Stdin, stdout, stderr)
	case "engine":
		code, err = cmdEngine(args[1:], stderr)
	case "version", "--version":
		v := "(devel)"
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
			v = bi.Main.Version
		}
		fmt.Fprintln(stdout, "winbox", v)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
	default:
		fmt.Fprintf(stderr, "winbox: unknown command %q\n\n%s", args[0], usageText)
		return 2
	}
	if errors.Is(err, errUsage) {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	if err != nil {
		var se *shownError
		if !errors.As(err, &se) {
			fmt.Fprintln(stderr, "winbox:", err)
		}
		if code == 0 {
			code = 1
		}
	}
	return code
}

// shownError is an error whose explanation was already printed.
type shownError struct{ err error }

func (e *shownError) Error() string { return e.err.Error() }
func (e *shownError) Unwrap() error { return e.err }
