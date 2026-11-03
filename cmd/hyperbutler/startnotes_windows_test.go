//go:build windows

package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/engine"
)

func TestLastGuestFS(t *testing.T) {
	dir := t.TempDir()
	in := &install{l: engine.Layout{Home: dir, Data: dir}}
	if g := in.lastGuestFS(); g != nil {
		t.Fatalf("no state.json: %+v", g)
	}
	fs := &engine.GuestFSState{SizeBytes: 100, FreeBytes: 5}
	for _, tc := range []struct {
		name    string
		dataDir string
		want    bool
	}{
		{"same folder", dir, true},
		{"same folder, other case and a trailing separator", strings.ToUpper(dir) + `\`, true},
		{"another data folder", filepath.Join(dir, "elsewhere"), false},
		{"no data folder recorded", "", false},
	} {
		if err := engine.SaveState(in.l, &engine.State{Phase: engine.PhaseStopped, DataDir: tc.dataDir, GuestFS: fs}); err != nil {
			t.Fatal(err)
		}
		if g := in.lastGuestFS(); (g != nil) != tc.want {
			t.Errorf("%s: got %+v, want a report: %v", tc.name, g, tc.want)
		}
	}
}

func TestStartNotesUsesTheLastReportWithoutWaiting(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir()) // warnCredHelpers reads the docker config
	dir := t.TempDir()
	in := &install{l: engine.Layout{Home: dir, Data: dir}}
	full := &engine.GuestFSState{SizeBytes: 100 << 30, FreeBytes: 1 << 30}
	old := guestFSWait
	guestFSWait = 2 * time.Second
	defer func() { guestFSWait = old }()

	// No current report: the previous run's says nearly full, and start does not wait.
	var out, errw bytes.Buffer
	t0 := time.Now()
	in.startNotes(&engine.State{Pipe: `\\.\pipe\hb-test-none`}, full, false, &out, &errw)
	if d := time.Since(t0); d > time.Second {
		t.Errorf("waited %s", d)
	}
	if !strings.Contains(errw.String(), "nearly full") {
		t.Errorf("no warning from the last report: %q", errw.String())
	}

	// A current report wins over the last one.
	errw.Reset()
	in.startNotes(&engine.State{Pipe: `\\.\pipe\hb-test-none`, GuestFS: &engine.GuestFSState{SizeBytes: 100 << 30, FreeBytes: 90 << 30}}, full, false, &out, &errw)
	if strings.Contains(errw.String(), "nearly full") {
		t.Errorf("warned from the last report over a current one: %q", errw.String())
	}

	// Neither, with wait (an existing disk whose last run left no report): the report
	// that arrives meanwhile is waited for and warns.
	errw.Reset()
	st := &engine.State{Pipe: `\\.\pipe\hb-test-none`, PID: 7, Phase: engine.PhaseRunning}
	if err := engine.SaveState(in.l, st); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		cur := *st
		cur.GuestFS = full
		_ = engine.SaveState(in.l, &cur)
	}()
	in.startNotes(st, nil, true, &out, &errw)
	if !strings.Contains(errw.String(), "nearly full") {
		t.Errorf("did not wait for the report: %q", errw.String())
	}

	// Neither, and no wait: no warning, at once.
	errw.Reset()
	t0 = time.Now()
	in.startNotes(&engine.State{Pipe: `\\.\pipe\hb-test-none`}, nil, false, &out, &errw)
	if d := time.Since(t0); d > time.Second || strings.Contains(errw.String(), "nearly full") {
		t.Errorf("took %s, said %q", d, errw.String())
	}
}
