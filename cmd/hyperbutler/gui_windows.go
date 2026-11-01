//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/daemoncfg"
	"github.com/Higangssh/hyperbutler/internal/dockerapi"
	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/gui/app"
	"github.com/Higangssh/hyperbutler/internal/gui/bridge"
	"github.com/Higangssh/hyperbutler/internal/gui/i18n"
	"github.com/Higangssh/hyperbutler/internal/gui/webview2"
	"github.com/Higangssh/hyperbutler/internal/mcpserver"
	"github.com/Higangssh/hyperbutler/internal/schema"
)

// The GUI (REQ-UX-011, ADR 0008) is `hyperbutler gui`, in this executable: see
// docs/gui.md for why it is not a second one. The engine never loads WebView2: only
// this command (and the no-argument launcher, which becomes it) loads
// WebView2Loader.dll, from next to hyperbutler.exe.

// guiOptions are `hyperbutler gui`'s flags.
type guiOptions struct {
	wait        bool
	lang        string
	imageDir    string
	data        string
	reportPaint bool
	checkLayout bool
	verbose     bool
	// importFrom stands in for Docker Desktop's pipe in the import (tests).
	importFrom string
}

func cmdGUI(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("gui", stderr)
	var o guiOptions
	fs.BoolVar(&o.wait, "wait", false, "run the window in this process and return when it closes (default: start it and return at once)")
	fs.StringVar(&o.lang, "lang", "", `the window's language this time: en, ko, or "pseudo" for layout checks (default: gui.language, then Windows)`)
	fs.StringVar(&o.imageDir, "image-dir", "", "the guest image for a start from the window (default: the one the engine ran last, then next to hyperbutler.exe)")
	fs.StringVar(&o.data, "data", "", `the window's browser profile (default: "gui" in the state directory)`)
	fs.BoolVar(&o.reportPaint, "report-paint", false, "print a line at the first paint (bench/footprint)")
	fs.BoolVar(&o.checkLayout, "check-layout", false, "with --report-paint: check every screen for text that overflows its box and print the result (the pseudo-locale layout check)")
	fs.BoolVar(&o.verbose, "verbose", false, "print diagnostic lines on stderr")
	// For tests: another engine's pipe stands in for Docker Desktop's, as import --from-pipe.
	fs.StringVar(&o.importFrom, "import-from-pipe", "", "")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if o.lang != "" && o.lang != i18n.Pseudo && !slices.Contains(i18n.Languages(), o.lang) {
		return usagef("gui: --lang %q: want %s or pseudo", o.lang, strings.Join(i18n.Languages(), ", "))
	}
	if !o.wait {
		return startGUIProcess(args, stdout)
	}
	var paint io.Writer
	if o.reportPaint {
		paint = stdout
	}
	var log func(string)
	if o.verbose {
		log = func(s string) { _, _ = fmt.Fprintln(stderr, "hyperbutler gui:", s) }
	}
	switch code := runGUI(o, paint, log); code {
	case app.ExitOK:
		return nil
	case app.ExitNoRuntime:
		return &shownError{err: webview2.ErrNoRuntime}
	default:
		return &shownError{err: errors.New("the window failed")}
	}
}

// startGUIProcess starts `hyperbutler gui --wait` on its own, so the shell gets its
// prompt back, and returns.
func startGUIProcess(args []string, stdout io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, append([]string{"gui", "--wait"}, args...)...) //nolint:gosec // G204: this program, with the flags the user gave
	const detachedProcess, createNewProcessGroup = 0x00000008, 0x00000200
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | createNewProcessGroup}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the window: %w", err)
	}
	_ = cmd.Process.Release()
	_, _ = fmt.Fprintln(stdout, "hyperbutler: the window is opening. Closing it never stops the engine.")
	return nil
}

// guiRuntimeOK reports whether the window can open: the WebView2 Runtime and its
// loader are there. Without them the no-argument launcher shows where to get the
// runtime and falls back to the console.
func guiRuntimeOK() bool {
	_, err := webview2.RuntimeVersion()
	return err == nil
}

// noRuntimeText is the message when the WebView2 Runtime is missing, in the user's
// language; it names the download page.
func noRuntimeText(msgs map[string]string) (title, text string) {
	return msgs["app.title"], strings.ReplaceAll(msgs["gui.noRuntime"], "{url}", webview2.RuntimeURL)
}

// runGUI opens the window in this process and returns its exit code.
func runGUI(o guiOptions, paint io.Writer, log func(string)) int {
	h := &guiHost{imageDir: o.imageDir}
	lang, startOnOpen := "", false
	data := o.data
	if in, err := openInstall(); err == nil {
		if in.settings != nil {
			lang, startOnOpen = in.settings.GUILanguage, in.settings.GUIStartOnOpen
		}
		h.home = in.l.Home
		if data == "" {
			data = filepath.Join(in.l.Home, "gui")
		}
	} else if data == "" {
		data = filepath.Join(os.TempDir(), "hyperbutler-gui")
	}
	if o.lang != "" {
		lang = o.lang
	}
	lang = i18n.Resolve(lang, app.WindowsLanguages())
	msgs := i18n.Messages(lang)
	h.msgs = msgs
	title, text := noRuntimeText(msgs)
	browserLang := lang
	if lang == i18n.Pseudo {
		browserLang = "en"
	}
	imp := &guiImporter{sourcePipe: o.importFrom}
	defer imp.close()
	return app.Run(app.Options{
		Bridge: bridge.Options{
			Host: h, Lang: lang, Messages: msgs, Version: buildVersion(), HostCPUs: runtime.NumCPU(),
			StartOnOpen: startOnOpen, CheckLayout: o.checkLayout, Importer: imp,
		},
		Title: msgs["app.title"], DataDir: filepath.Join(data, "webview2"), Language: browserLang,
		NoRuntimeTitle: title, NoRuntimeText: text,
		CloseNote: h.closeNote, ReportPaint: paint, Log: log, InstanceKey: strings.ToLower(h.home),
	})
}

// guiHost runs the CLI's own code for the window (bridge.Host). The engine operations
// run the CLI as a child process, so one the user started finishes even if the window
// closes meanwhile (and the engine it starts outlives the window).
type guiHost struct {
	home     string
	imageDir string
	msgs     map[string]string

	mu       sync.Mutex
	lastZip  string
	statusMu sync.Mutex
}

func (h *guiHost) Status(context.Context) (schema.Status, error) {
	// One at a time: whether the engine runs is a probe of its lock file, and several
	// at once (a timer, the containers list, the storage screen) could read each other
	// as a running engine.
	h.statusMu.Lock()
	defer h.statusMu.Unlock()
	in, err := openInstall()
	if err != nil {
		return schema.Status{}, bridge.Coded(bridge.CodeConfigFailed, err)
	}
	// No update check: the window makes no network request (REQ-UX-005).
	r, _, _, err := in.collectStatus(false, io.Discard)
	return r, err
}

func (h *guiHost) Docker(ctx context.Context) (*dockerapi.Client, error) {
	h.statusMu.Lock()
	pipe, err := (&mcpHost{}).Pipe(ctx)
	h.statusMu.Unlock()
	switch {
	case errors.Is(err, mcpserver.ErrEngineStopped):
		return nil, bridge.ErrEngineStopped
	case err != nil:
		return nil, bridge.Coded(bridge.CodeEngineBusy, err)
	}
	return dockerapi.New(func(ctx context.Context) (net.Conn, error) { return winio.DialPipeContext(ctx, pipe) }), nil
}

// engineArgs are the CLI arguments of an engine operation.
func (h *guiHost) engineArgs(op bridge.EngineOp) ([]string, string, error) {
	switch op {
	case bridge.EngineStart:
		args := []string{"start"}
		if dir := h.startImageDir(); dir != "" {
			args = append(args, "--image-dir", dir)
		}
		return args, bridge.CodeStartFailed, nil
	case bridge.EngineStop:
		return []string{"stop"}, bridge.CodeStopFailed, nil
	case bridge.EngineRestart:
		return []string{"restart"}, bridge.CodeRestartFailed, nil
	case bridge.EngineRestartDockerd:
		return []string{"config", "daemon", "apply"}, bridge.CodeDaemonApply, nil
	}
	return nil, "", fmt.Errorf("unknown engine operation %q", op)
}

// startImageDir is the guest image a start uses: --image-dir, else the one the engine
// ran last (as restart picks it), else none (next to hyperbutler.exe).
func (h *guiHost) startImageDir() string {
	if h.imageDir != "" {
		return h.imageDir
	}
	in, err := openInstall()
	if err != nil {
		return ""
	}
	st, err := engine.LoadState(in.l)
	if err != nil || st.ImageDir == "" {
		return ""
	}
	_, pinned := engine.ImagePin()
	if dir, ignored := restartImageDir(st.ImageDir, exeDir(), pinned, engine.CheckImageDirPin); ignored == nil {
		return dir
	}
	return ""
}

// maxEngineOutput bounds what an engine operation's output keeps.
const maxEngineOutput = 1 << 20

type boundedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *boundedBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if room := maxEngineOutput - w.b.Len(); room > 0 {
		w.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (w *boundedBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func (h *guiHost) Engine(ctx context.Context, op bridge.EngineOp) (string, error) {
	args, code, err := h.engineArgs(op)
	if err != nil {
		return "", err
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	var out boundedBuffer
	flags := uint32(windows.CREATE_NO_WINDOW)
	if app.Breakaway() {
		// Out of the window's kill-on-close job: the operation finishes, and the
		// engine it starts keeps running, after the window closes.
		flags |= windows.CREATE_BREAKAWAY_FROM_JOB
	}
	cmd := exec.Command(exe, args...) //nolint:gosec // G204: this program with fixed arguments (the image directory is the user's flag or state.json's)
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
	if err := cmd.Start(); err != nil {
		return "", bridge.Coded(code, fmt.Errorf("run hyperbutler %s: %w", strings.Join(args, " "), err))
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		// The window closed or the time ran out. The CLI is left to finish: stopping
		// it halfway would leave a start or stop half done.
		_ = cmd.Process.Release()
		return out.String(), bridge.Coded(code, fmt.Errorf("hyperbutler %s did not finish in time; it goes on in the background", strings.Join(args, " ")))
	}
	text := out.String()
	if err != nil {
		if op == bridge.EngineStart || op == bridge.EngineRestart {
			if in, ierr := openInstall(); ierr == nil {
				if st, serr := engine.LoadState(in.l); serr == nil && st.Phase == engine.PhaseFailed && st.ErrorCode != "" {
					code = bridge.EngineCode(st.ErrorCode)
				}
			}
		}
		return text, bridge.Coded(code, fmt.Errorf("hyperbutler %s: %w\n%s", strings.Join(args, " "), err, strings.TrimSpace(text)))
	}
	return text, nil
}

func (h *guiHost) ConfigList(context.Context) (schema.ConfigList, error) {
	b, err := captured(func(stdout, stderr io.Writer) error { return configList([]string{"--json"}, stdout, stderr) })
	if err != nil {
		return schema.ConfigList{}, err
	}
	var l schema.ConfigList
	if err := json.Unmarshal(b, &l); err != nil {
		return schema.ConfigList{}, fmt.Errorf("config list: %w", err)
	}
	return l, nil
}

func (h *guiHost) ConfigSet(_ context.Context, key, value string) (string, error) {
	// "--" ends the flags: a value such as "--apply" is a value, never a flag, so the
	// window never restarts anything through a setting (the page asks first, then
	// runs the restart as its own operation).
	b, err := captured(func(stdout, stderr io.Writer) error {
		return configSet("set", []string{"--", key, value}, stdout, stdout)
	})
	return string(b), err
}

func (h *guiHost) daemonPath() (string, error) {
	home, err := engine.DefaultHome()
	if err != nil {
		return "", err
	}
	return engine.Layout{Home: home}.DaemonConfigPath(), nil
}

func (h *guiHost) DaemonRead(context.Context) (string, []byte, bool, error) {
	p, err := h.daemonPath()
	if err != nil {
		return "", nil, false, err
	}
	b, err := engine.ReadFileBounded(p)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil, false, nil
	}
	return p, b, err == nil, err
}

// DaemonCheck is `config daemon validate`'s check, on text instead of a file: exactly the
// check the engine makes when it reads the file (an empty file is refused there too).
func (h *guiHost) DaemonCheck(data []byte) error {
	return daemoncfg.Check(data, guestSubnet())
}

func (h *guiHost) DaemonWrite(_ context.Context, data []byte) error {
	p, err := h.daemonPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return engine.WriteFileAtomic(p, data)
}

func (h *guiHost) Doctor(context.Context) (schema.Doctor, error) {
	in, err := openInstallLenient()
	if err != nil {
		return schema.Doctor{}, err
	}
	return runDoctor(in), nil
}

func (h *guiHost) Diagnose(context.Context) (schema.Diagnose, error) {
	out := filepath.Join(os.TempDir(), "hyperbutler-diagnose-"+time.Now().Format("20060102-150405")+".zip")
	b, err := captured(func(stdout, stderr io.Writer) error {
		return cmdDiagnose([]string{"--json", "--out", out}, stdout, stderr)
	})
	if err != nil {
		return schema.Diagnose{}, err
	}
	var d schema.Diagnose
	if err := json.Unmarshal(b, &d); err != nil {
		return schema.Diagnose{}, fmt.Errorf("diagnose: %w", err)
	}
	h.mu.Lock()
	// One zip per window: the previous one (in the temporary directory) goes.
	if h.lastZip != "" && h.lastZip != d.File {
		_ = os.Remove(h.lastZip)
	}
	h.lastZip = d.File
	h.mu.Unlock()
	return d, nil
}

// Reveal opens Explorer on the diagnose zip this window wrote; any other path is
// refused (the bridge passes only that one, and this checks again).
func (h *guiHost) Reveal(path string) error {
	h.mu.Lock()
	ok := path != "" && path == h.lastZip
	h.mu.Unlock()
	if !ok {
		return errors.New("not the diagnose file this window wrote")
	}
	return app.Reveal(path)
}

func (h *guiHost) CopyText(s string) error { return app.CopyText(s) }

// closeNote is S0.3's note, shown once: the first time the window closes while the
// engine runs, it says the engine keeps running. A file in the state directory
// remembers that it was shown.
func (h *guiHost) closeNote() string {
	if h.home == "" {
		return ""
	}
	marker := filepath.Join(h.home, "gui", "close-note-shown")
	if _, err := os.Stat(marker); err == nil {
		return ""
	}
	in, err := openInstall()
	if err != nil {
		return ""
	}
	if up, err := in.running(); err != nil || !up {
		return ""
	}
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		return ""
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		return ""
	}
	return h.msgs["gui.closeNote"]
}
