//go:build windows

package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/engine"
)

// REQ-PLAT-008: start says so when the running engine is another hyperbutler.exe or
// another version, and points to restart.
func TestOtherEngine(t *testing.T) {
	st := &engine.State{Executable: `C:\new\hyperbutler.exe`, ExecutableVersion: "v0.1.1"}
	if s := otherEngine(st, `c:\NEW\hyperbutler.exe`, "v0.1.1"); s != "" {
		t.Fatalf("same exe (case differs): %q", s)
	}
	s := otherEngine(st, `C:\newer\hyperbutler.exe`, "v0.1.2")
	for _, want := range []string{`hyperbutler v0.1.1 from C:\new is running; this is v0.1.2 from C:\newer.`, "`hyperbutler restart` from this folder"} {
		if !strings.Contains(s, want) {
			t.Errorf("other folder and version: missing %q in %q", want, s)
		}
	}
	if s := otherEngine(st, `C:\new\hyperbutler.exe`, "v0.1.2"); !strings.Contains(s, "v0.1.1 from C:\\new is running; this is v0.1.2 from C:\\new.") {
		t.Errorf("same folder, new version: %q", s)
	}
	if s := otherEngine(st, `C:\copy\hyperbutler.exe`, "v0.1.1"); !strings.Contains(s, `this is v0.1.1 from C:\copy.`) {
		t.Errorf("same version, other folder: %q", s)
	}
	// An engine from before state.json recorded it: nothing to compare.
	if s := otherEngine(&engine.State{}, `C:\x\hyperbutler.exe`, "v0.1.2"); s != "" {
		t.Errorf("unrecorded: %q", s)
	}
}

func TestBuildVersion(t *testing.T) {
	v := buildVersion()
	if v == "" || len(v) > 128 || strings.ContainsAny(v, "\r\n") {
		t.Fatalf("buildVersion %q", v)
	}
}

// REQ-DISK-010: status shows a pause with the volume, the free space now and the free
// space it resumes at.
func TestGuardPauseStatus(t *testing.T) {
	since := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	st := &engine.State{Phase: engine.PhaseRunning, PID: 1, GuardPause: &engine.GuardPauseState{Volume: "C:", FreeBytes: 900 << 20, ResumeAtBytes: 1280 << 20, Since: since}}
	if p := guardPauseStatus(st, false, 0); p != nil {
		t.Fatalf("not running: %+v", p)
	}
	p := guardPauseStatus(st, true, 0)
	if p == nil || p.Volume != "C:" || p.FreeBytes != 900<<20 || p.ResumeAtBytes != 1280<<20 || !p.Since.Equal(since) {
		t.Fatalf("pause %+v", p)
	}
	// The free space status measured wins over the engine's last figure.
	if p := guardPauseStatus(st, true, 1<<30); p.FreeBytes != 1<<30 {
		t.Fatalf("free now %d", p.FreeBytes)
	}
	var b bytes.Buffer
	st.Executable, st.ExecutableVersion = `C:\hb\hyperbutler.exe`, "v0.1.0"
	writeEngineStatus(&b, StatusJSON{State: st, GuardPause: p})
	out := b.String()
	for _, want := range []string{"  paused:  C: has 900.0 MiB free; it resumes at 1.2 GiB free (the free-space guard paused the VM at ",
		"docker commands wait until then)", `  engine:  C:\hb\hyperbutler.exe (v0.1.0)`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	st.GuardPause = nil
	if p := guardPauseStatus(st, true, 0); p != nil {
		t.Fatalf("not paused: %+v", p)
	}
}
