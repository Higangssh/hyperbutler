//go:build windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/gui/bridge"
)

// REQ-UX-016 from 0.2: a start with no arguments in a console of its own opens the
// window; without the WebView2 Runtime it falls back to the console launcher (the
// launcher tests run that way, see setOwnsConsole).
func TestRunNoArgsOpensGUI(t *testing.T) {
	testHome(t)
	setOwnsConsole(t, true)
	opened := false
	launcherGUI = func() (int, bool) { opened = true; return 0, true }
	code, out, _ := runCLI(t)
	if !opened || code != 0 || strings.Contains(out, "Press Enter") || strings.Contains(out, "Usage:") {
		t.Fatalf("exit %d, opened %v, %q", code, opened, out)
	}
	// From a shell: the usage, never the window.
	opened = false
	setOwnsConsole(t, false)
	launcherGUI = func() (int, bool) { opened = true; return 0, true }
	if code, out, _ := runCLI(t); opened || code != 0 || !strings.Contains(out, "Usage:") {
		t.Fatalf("shell: exit %d, opened %v", code, opened)
	}
}

func TestGUIFlags(t *testing.T) {
	testHome(t)
	if code, _, _ := runCLI(t, "gui", "--wait", "--lang", "fr"); code != 2 {
		t.Fatalf("--lang fr: exit %d", code)
	}
	if code, _, _ := runCLI(t, "gui", "extra"); code != 2 {
		t.Fatalf("extra argument: exit %d", code)
	}
}

func TestGUIHost(t *testing.T) {
	home, _ := testHome(t)
	h := &guiHost{home: home, msgs: map[string]string{"gui.closeNote": "note"}}
	if err := h.Reveal(`C:\Windows\System32\cmd.exe`); err == nil {
		t.Fatal("revealed a path that is not this window's diagnose zip")
	}
	// The same check as the engine's: an empty file is refused there, so here too.
	if err := h.DaemonCheck([]byte("  \n")); err == nil {
		t.Fatal("empty daemon.json accepted")
	}
	if err := h.DaemonCheck([]byte("{}")); err != nil {
		t.Fatalf("{}: %v", err)
	}
	if err := h.DaemonCheck([]byte(`{"hosts":["tcp://0.0.0.0:2375"]}`)); err == nil {
		t.Fatal("daemon.json with hosts accepted")
	}
	// The engine is not running: no close note, and none recorded.
	if n := h.closeNote(); n != "" {
		t.Fatalf("close note with the engine stopped: %q", n)
	}
	if _, err := os.Stat(filepath.Join(home, "gui", "close-note-shown")); err == nil {
		t.Fatal("close note recorded while not shown")
	}
	// A setting the window does not change never reaches config set.
	d := bridge.New(bridge.Options{Host: h})
	r := d.Handle(context.Background(), `{"id":1,"op":"config.set","args":{"key":"pipe","value":"\\\\.\\pipe\\x"}}`)
	if r.OK || r.Error.Code != bridge.CodeNotEditable {
		t.Fatalf("pipe set from the window: %+v", r)
	}
	// Status needs no engine.
	if _, err := h.Status(context.Background()); err != nil {
		t.Fatalf("status: %v", err)
	}
}

func TestGUIEngineArgs(t *testing.T) {
	testHome(t)
	h := &guiHost{imageDir: `D:\img`}
	for op, want := range map[bridge.EngineOp]string{
		bridge.EngineStart:          `start --image-dir D:\img`,
		bridge.EngineStop:           "stop",
		bridge.EngineRestart:        "restart",
		bridge.EngineRestartDockerd: "config daemon apply",
	} {
		args, code, err := h.engineArgs(op)
		if err != nil || strings.Join(args, " ") != want || code == "" {
			t.Errorf("%s: %v %q %v", op, args, code, err)
		}
	}
	if _, _, err := h.engineArgs("delete"); err == nil {
		t.Error("unknown operation accepted")
	}
}
