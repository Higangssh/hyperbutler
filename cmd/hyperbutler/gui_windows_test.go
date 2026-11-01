//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/desktopimport"
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

// TestGUIHostLock (L2, L3): while a start or stop holds the operations lock, a setting
// or a daemon.json save from the window is busy within a few seconds, not a minute;
// once free, the save reads the file again under the lock.
func TestGUIHostLock(t *testing.T) {
	testHome(t)
	in, err := openInstall()
	if err != nil {
		t.Fatal(err)
	}
	ops, err := in.opsLock()
	if err != nil {
		t.Fatal(err)
	}
	h := &guiHost{}
	t0 := time.Now()
	_, err = h.ConfigSet(context.Background(), "cpus", "3")
	var be *bridge.Error
	if !errors.As(err, &be) || be.Code != bridge.CodeEngineBusy || time.Since(t0) > guiLockWait+3*time.Second {
		t.Fatalf("config set while locked: %v after %v", err, time.Since(t0))
	}
	ok := func([]byte, bool) error { return nil }
	if err := h.DaemonWrite(context.Background(), []byte("{}"), ok); !errors.As(err, &be) || be.Code != bridge.CodeEngineBusy {
		t.Fatalf("daemon.json write while locked: %v", err)
	}
	_ = ops.Unlock()
	var seen []byte
	if err := h.DaemonWrite(context.Background(), []byte(`{"debug":true}`), func(cur []byte, exists bool) error { seen = cur; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := h.DaemonWrite(context.Background(), []byte(`{}`), func(cur []byte, exists bool) error {
		if !exists || string(cur) != `{"debug":true}` {
			t.Errorf("check saw %q (exists %v)", cur, exists)
		}
		return errors.New("refused")
	}); err == nil {
		t.Fatal("a refused check still wrote")
	}
	if _, b, _, _ := h.DaemonRead(context.Background()); string(b) != `{"debug":true}` || seen != nil {
		t.Fatalf("daemon.json %q, first check saw %q", b, seen)
	}
}

// TestInstanceKey (L1): one window per browser profile as it resolves, whatever the
// spelling; another profile (a --data of its own) is another instance.
func TestInstanceKey(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	a := instanceKey(filepath.Join(dir, "gui"))
	if b := instanceKey(`gui\.\`); a != b {
		t.Errorf("relative spelling: %q != %q", b, a)
	}
	if b := instanceKey(strings.ToUpper(filepath.Join(dir, "GUI") + `\`)); a != b {
		t.Errorf("case and trailing separator: %q != %q", b, a)
	}
	if b := instanceKey(filepath.Join(dir, "other")); a == b {
		t.Error("two profiles share an instance")
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

func TestImportStatus(t *testing.T) {
	for s, want := range map[desktopimport.Status]string{
		desktopimport.StatusNew: bridge.ImportNew, desktopimport.StatusAlready: bridge.ImportDone,
		desktopimport.StatusPartial: bridge.ImportPartial, desktopimport.StatusConflict: bridge.ImportConflict,
		desktopimport.StatusExists: bridge.ImportExists, desktopimport.StatusUnsupported: bridge.ImportUnsupported,
		desktopimport.StatusDifferentLabels: bridge.ImportLabels,
		"something new":                     bridge.ImportAttention,
	} {
		if got := importStatus(s); got != want {
			t.Errorf("%s: %s, want %s", s, got, want)
		}
	}
	// Every status the import can give, on an image and a volume (in use too), comes
	// out as a status and a detail the window's catalog has (TestPageKeys checks the
	// catalog has each of bridge.ImportStatuses and bridge.ImportDetails).
	statuses := []desktopimport.Status{desktopimport.StatusNew, desktopimport.StatusAlready, desktopimport.StatusPartial,
		desktopimport.StatusConflict, desktopimport.StatusDifferentLabels, desktopimport.StatusExists, desktopimport.StatusUnsupported, "something new"}
	var items []bridge.ImportItem
	for _, s := range statuses {
		items = append(items,
			imageItem(desktopimport.ImagePlan{ID: "sha256:1", Tags: []string{"a:1"}, Status: s, MissingTags: []string{"a:2"}, Conflicts: []string{"a:3"}, Reason: "English"}),
			volumeItem(desktopimport.VolumePlan{Name: "v", Driver: "local", Status: s, Reason: "English"}),
			volumeItem(desktopimport.VolumePlan{Name: "v", Driver: "nfs", Status: s}),
			volumeItem(desktopimport.VolumePlan{Name: "v", Driver: "local", Status: s, UsedBy: []string{"web", "db"}}))
	}
	for _, it := range items {
		if !slices.Contains(bridge.ImportStatuses, it.Status) {
			t.Errorf("%s: status %q is not one the window knows", it.ID, it.Status)
		}
		if it.Detail != "" && !slices.Contains(bridge.ImportDetails, it.Detail) {
			t.Errorf("%s (%s): detail %q is not in bridge.ImportDetails", it.ID, it.Status, it.Detail)
		}
		if it.Status != bridge.ImportNew && it.Status != bridge.ImportAttention && it.Detail == "" {
			t.Errorf("%s (%s): no detail", it.ID, it.Status)
		}
	}
	if it := volumeItem(desktopimport.VolumePlan{Name: "v", Status: desktopimport.StatusNew, UsedBy: []string{"web"}}); it.Status != bridge.ImportInUse ||
		it.Detail != bridge.DetailVolumeInUse || strings.Join(it.Args["containers"], ",") != "web" {
		t.Errorf("volume in use: %+v", it)
	}
	// Without a plan, nothing runs.
	g := &guiImporter{}
	if _, err := g.Run(context.Background(), []string{"image:x"}, func(bridge.ImportProgress) {}); err == nil {
		t.Fatal("ran without a plan")
	}
	// With the engine stopped, the plan says so with its code.
	testHome(t)
	if _, err := g.Plan(context.Background()); !errors.Is(err, bridge.ErrEngineStopped) {
		t.Fatalf("plan with no engine: %v", err)
	}
}
