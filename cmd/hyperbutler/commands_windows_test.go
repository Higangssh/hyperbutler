//go:build windows

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/engine/dockerctx"
)

// testHome points the CLI at an empty state directory and docker config.
func testHome(t *testing.T) (home, dockerCfg string) {
	t.Helper()
	d, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, dockerCfg = filepath.Join(d, "home"), filepath.Join(d, "docker")
	t.Setenv(engine.HomeEnv, home)
	t.Setenv("DOCKER_CONFIG", dockerCfg)
	return home, dockerCfg
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestCLIUsage(t *testing.T) {
	testHome(t)
	if code, _, _ := runCLI(t); code != 2 {
		t.Fatalf("no args: exit %d", code)
	}
	if code, _, e := runCLI(t, "frobnicate"); code != 2 || !strings.Contains(e, "unknown command") {
		t.Fatalf("unknown command: %d %q", code, e)
	}
	if code, out, _ := runCLI(t, "version"); code != 0 || !strings.HasPrefix(out, "hyperbutler ") {
		t.Fatalf("version: %d %q", code, out)
	}
	if code, _, _ := runCLI(t, "stop", "extra"); code != 1 {
		t.Fatalf("stop with an argument: exit %d", code)
	}
	if code, _, _ := runCLI(t, "engine"); code != 2 {
		t.Fatalf("engine without flags: exit %d", code)
	}
}

func TestCLIFreshInstall(t *testing.T) {
	home, _ := testHome(t)
	code, out, _ := runCLI(t, "status")
	if code != exitNotRunning || !strings.Contains(out, "not set up yet") {
		t.Fatalf("status: %d %q", code, out)
	}
	if code, out, _ := runCLI(t, "stop"); code != 0 || !strings.Contains(out, "not running") {
		t.Fatalf("stop: %d %q", code, out)
	}
	if code, _, e := runCLI(t, "logs"); code != 1 || !strings.Contains(e, "no log yet") {
		t.Fatalf("logs: %d %q", code, e)
	}
	if code, out, e := runCLI(t, "delete", "--yes"); code != 0 || !strings.Contains(out, "deleted") {
		t.Fatalf("delete: %d %q %q", code, out, e)
	}
	// Starting without guest artifacts fails before anything is spawned, but the first
	// start's choices are recorded.
	code, _, e := runCLI(t, "start", "--image-dir", filepath.Join(home, "nope"), "--memory", "2GiB", "--cpus", "3", "--disk-size", "8GiB")
	if code != 1 || !strings.Contains(e, "vmlinuz-virt") {
		t.Fatalf("start without images: %d %q", code, e)
	}
	s, err := engine.LoadSettings(engine.Layout{Home: home})
	if err != nil || s.MemoryMB != 2048 || s.CPUs != 3 || s.DiskSize != 8<<30 || !engine.SamePath(s.DataDir, home) {
		t.Fatalf("settings %+v %v", s, err)
	}
}

func TestCLIDataLocationIsFixed(t *testing.T) {
	home, _ := testHome(t)
	data := filepath.Join(filepath.Dir(home), "data")
	nope := filepath.Join(home, "nope")
	runCLI(t, "start", "--data", data, "--image-dir", nope)
	s, err := engine.LoadSettings(engine.Layout{Home: home})
	if err != nil || !engine.SamePath(s.DataDir, data) {
		t.Fatalf("settings %+v %v", s, err)
	}
	// The same place (any case, trailing separator) is fine; another is refused.
	if _, _, e := runCLI(t, "start", "--data", strings.ToUpper(data)+`\`, "--image-dir", nope); strings.Contains(e, "already") {
		t.Fatalf("same location refused: %q", e)
	}
	code, _, e := runCLI(t, "start", "--data", filepath.Join(home, "elsewhere"), "--image-dir", nope)
	if code != 1 || !strings.Contains(e, "hyperbutler move") {
		t.Fatalf("moved data: %d %q", code, e)
	}
	if code, _, e := runCLI(t, "start", "--data", `\\server\share\hyperbutler`, "--image-dir", nope); code != 1 || !strings.Contains(e, "already") && !strings.Contains(e, "local") {
		t.Fatalf("UNC data: %d %q", code, e)
	}
	// The disk size is fixed once the disk exists.
	l := engine.Layout{Home: home, Data: data}
	if err := engine.MkdirPrivate(data); err != nil {
		t.Fatal(err)
	}
	if err := engine.WriteFileAtomic(l.DiskMetaPath(), []byte(`{"version":1,"sizeBytes":68719476736,"blockSize":1048576,"metadataBytes":0,"created":"2026-10-30T00:00:00Z"}`)); err != nil {
		t.Fatal(err)
	}
	if code, _, e := runCLI(t, "start", "--disk-size", "128GiB", "--image-dir", nope); code != 1 || !strings.Contains(e, "fixed") {
		t.Fatalf("disk resize: %d %q", code, e)
	}
}

func TestCLIStatusAfterCrash(t *testing.T) {
	home, _ := testHome(t)
	l := engine.Layout{Home: home, Data: home}
	if err := engine.MkdirPrivate(home); err != nil {
		t.Fatal(err)
	}
	if err := engine.SaveSettings(l, &engine.Settings{DataDir: home, DiskSize: 64 << 30, MemoryMB: 4096, CPUs: 2}); err != nil {
		t.Fatal(err)
	}
	// A state that says running, from an engine that is gone (nobody holds the lock).
	st := &engine.State{Phase: engine.PhaseRunning, PID: 999999, PIDCreated: 1, Pipe: `\\.\pipe\docker_engine_hyperbutler`, DataDir: home}
	if err := engine.SaveState(l, st); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI(t, "status")
	if code != exitNotRunning || !strings.Contains(out, "exited without shutting down") {
		t.Fatalf("status: %d %q", code, out)
	}
	// stop notices and records it.
	if code, out, e := runCLI(t, "stop"); code != 0 || !strings.Contains(out, "not running") || !strings.Contains(e, "exited without shutting down") {
		t.Fatalf("stop: %d %q %q", code, out, e)
	}
	got, err := engine.LoadState(l)
	if err != nil || got.Phase != engine.PhaseFailed {
		t.Fatalf("state after stop: %+v %v", got, err)
	}
	if code, out, _ := runCLI(t, "status", "--json"); code != exitNotRunning || !strings.Contains(out, `"running": false`) {
		t.Fatalf("status --json: %d %q", code, out)
	}
}

func TestCLIDeleteRemovesOnlyItsFiles(t *testing.T) {
	home, dockerCfg := testHome(t)
	data := filepath.Join(filepath.Dir(home), "userdata")
	l := engine.Layout{Home: home, Data: data}
	for _, d := range []string{home, data, l.LogDir()} {
		if err := engine.MkdirPrivate(d); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.SaveSettings(l, &engine.Settings{DataDir: data, DiskSize: 64 << 30, MemoryMB: 4096, CPUs: 2}); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(data, "my-notes.txt")
	for _, p := range []string{l.DiskPath(), l.DiskMetaPath(), l.EngineLogPath(), keep} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store := dockerctx.Store{Dir: dockerCfg}
	if err := store.Ensure(contextName, "npipe:////./pipe/x", "hyperbutler"); err != nil {
		t.Fatal(err)
	}
	if err := store.Use(contextName); err != nil {
		t.Fatal(err)
	}
	// Without --yes and without a terminal, nothing happens.
	if code, _, _ := runCLI(t, "delete"); code != 1 {
		t.Fatalf("delete without --yes: exit %d", code)
	}
	if _, err := os.Stat(l.DiskPath()); err != nil {
		t.Fatal("deleted without confirmation")
	}
	if code, _, e := runCLI(t, "delete", "--yes"); code != 0 {
		t.Fatalf("delete: %d %q", code, e)
	}
	for _, p := range []string{l.DiskPath(), l.DiskMetaPath(), l.SettingsPath(), l.EngineLogPath()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived delete", p)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("delete removed a file it did not create: %v", err)
	}
	if cur, _ := store.Current(); cur != "" {
		t.Fatalf("context still selected: %q", cur)
	}
	if _, err := store.Get(contextName); !os.IsNotExist(err) {
		t.Fatalf("context not removed: %v", err)
	}
}

func TestSelectContextLeavesForeign(t *testing.T) {
	_, dockerCfg := testHome(t)
	store := dockerctx.Store{Dir: dockerCfg}
	meta := filepath.Join(dockerCfg, "contexts", "meta", "1f11812811c947c3b0809ebbae1ed8305fe8633d6b1c0ddee6357f1f07e572fe")
	if err := os.MkdirAll(meta, 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := `{"Name":"hyperbutler","Metadata":{},"Endpoints":{"docker":{"Host":"tcp://example:2376"}}}`
	if err := os.WriteFile(filepath.Join(meta, "meta.json"), []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	selectContext(`\\.\pipe\docker_engine_hyperbutler`, &out, &errb)
	if !strings.Contains(errb.String(), "was not created by hyperbutler") {
		t.Fatalf("no warning: %q", errb.String())
	}
	if cur, _ := store.Current(); cur != "" {
		t.Fatalf("a foreign context was selected: %q", cur)
	}
	if b, _ := os.ReadFile(filepath.Join(meta, "meta.json")); string(b) != foreign {
		t.Fatal("the foreign context was rewritten")
	}
}
