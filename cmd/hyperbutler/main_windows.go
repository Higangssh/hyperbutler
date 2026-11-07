//go:build windows

// Command hyperbutler is the hyperbutler CLI: a Docker engine for Windows in a small Hyper-V VM,
// without WSL.
//
//	hyperbutler start    boot the VM in a background engine process, select the docker context
//	hyperbutler stop     shut the VM down (gracefully, then forced after --timeout)
//	hyperbutler status   show whether it runs, and where its data is
//	hyperbutler logs     print the engine log (or the guest console with --console)
//	hyperbutler delete   remove the VM state and the data disk
//	hyperbutler config   read and change settings and daemon.json
//	hyperbutler restart  stop and start again
//	hyperbutler diagnose  write a local zip for bug reports, with secrets redacted
//	hyperbutler doctor    check what hyperbutler needs, read-only, with the fixes
//	hyperbutler uninstall remove everything hyperbutler created (--dry-run lists it)
//	hyperbutler import    copy images and volumes from Docker Desktop (--from-desktop)
//	hyperbutler mcp      serve the Model Context Protocol on stdio for AI clients
//	hyperbutler gui      open the window (also what a double-click on hyperbutler.exe does)
//
// See docs/research/cli-engine.md.
package main

// The icon and version resource; dist/package.ps1 regenerates it with the release version.
//go:generate go run github.com/tc-hib/go-winres@v0.3.3 make --in winres/winres.json --out rsrc --arch amd64,arm64

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

const usageText = `hyperbutler: a Docker engine for Windows in a small Hyper-V VM, without WSL

Usage:
  hyperbutler start   [--data DIR] [--image-dir DIR] [--memory SIZE] [--cpus N] [--disk-size SIZE]
                      [--memlimit LIMIT] [--debug-pipe PIPE]
  hyperbutler stop    [--timeout DURATION]
  hyperbutler restart [--image-dir DIR] [--timeout DURATION]
  hyperbutler status  [--json]
  hyperbutler logs    [--console] [-n LINES] [-f]
  hyperbutler config  list|get|set|unset|path|daemon ...   ("hyperbutler config help")
  hyperbutler delete  [--yes]
  hyperbutler diagnose [--out FILE] [--json]
  hyperbutler doctor  [--json]
  hyperbutler uninstall [--dry-run] [--keep-data] [--yes] [--json]
  hyperbutler version [--json]
  hyperbutler import  --from-desktop [--image NAME,...|--all-images] [--volume NAME,...|--all-volumes] [--dry-run] [--json]
  hyperbutler mcp     [--allow-write [--allow-delete]]   (MCP server on stdio, docs/mcp.md)
  hyperbutler gui     [--wait] [--lang LANG]   (the window, docs/gui.md)
  hyperbutler sandbox run|ls|attach|shell|diff|apply|commits|...   (a coding agent in its own VM, docs/sandbox.md)

State lives in %LOCALAPPDATA%\hyperbutler (override with HYPERBUTLER_HOME).
Run "hyperbutler COMMAND -h" for a command's flags.
Without arguments in a window of its own (Explorer, a shortcut) it opens the window (or,
without the WebView2 Runtime, shows the status and offers a start);
HYPERBUTLER_NO_LAUNCHER=1 prints this usage instead.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// exitNotRunning is the exit code of `hyperbutler status` when hyperbutler is not running.
const exitNotRunning = 3

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		// Launched on its own (Explorer, a shortcut): show the status and offer a start
		// (REQ-UX-016). From a shell, or with HYPERBUTLER_NO_LAUNCHER=1: the usage,
		// which is not an error.
		if !launcherDisabled() && ownsConsole() {
			// From 0.2 the window opens instead; the console launcher stays for a
			// machine without the WebView2 Runtime.
			if code, ran := launcherGUI(); ran {
				return code
			}
			return newLauncher(stdout, stderr).run()
		}
		_, _ = fmt.Fprint(stdout, usageText)
		return 0
	}
	var err error
	code := 0
	switch args[0] {
	case "start":
		err = cmdStart(args[1:], stdout, stderr)
	case "stop":
		err = cmdStop(args[1:], stdout, stderr)
	case "restart":
		err = cmdRestart(args[1:], stdout, stderr)
	case "config":
		err = cmdConfig(args[1:], stdout, stderr)
	case "status":
		code, err = cmdStatus(args[1:], stdout, stderr)
	case "logs":
		err = cmdLogs(args[1:], stdout, stderr)
	case "delete":
		err = cmdDelete(args[1:], os.Stdin, stdout, stderr)
	case "doctor":
		err = cmdDoctor(args[1:], stdout, stderr)
	case "diagnose":
		err = cmdDiagnose(args[1:], stdout, stderr)
	case "uninstall":
		err = cmdUninstall(args[1:], os.Stdin, stdout, stderr)
	case "import":
		err = cmdImport(args[1:], os.Stdin, stdout, stderr)
	case "gui":
		err = cmdGUI(args[1:], stdout, stderr)
	case "sandbox":
		code, err = cmdSandbox(args[1:], os.Stdin, stdout, stderr)
	case "mcp":
		err = cmdMCP(args[1:], os.Stdin, stdout, stderr)
	case "engine":
		code, err = cmdEngine(args[1:], stderr)
	case "setup":
		err = cmdSetup(args[1:], stdout, stderr)
	case "version", "--version":
		err = cmdVersion(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, usageText)
	default:
		_, _ = fmt.Fprintf(stderr, "hyperbutler: unknown command %q\n\n%s", args[0], usageText)
		return 2
	}
	if fe := (*flagError)(nil); errors.As(err, &fe) {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2 // the flag set printed the problem and the flags
	}
	if errors.Is(err, errUsage) {
		_, _ = fmt.Fprint(stderr, usageText)
		return 2
	}
	var se *shownError
	if ue := (*usageError)(nil); errors.As(err, &ue) {
		if !errors.As(err, &se) {
			_, _ = fmt.Fprintln(stderr, "hyperbutler:", err)
		}
		return 2
	}
	if err != nil {
		if !errors.As(err, &se) {
			_, _ = fmt.Fprintln(stderr, "hyperbutler:", err)
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
