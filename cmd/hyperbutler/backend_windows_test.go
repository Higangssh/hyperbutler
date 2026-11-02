//go:build windows

package main

import (
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"

	"github.com/Higangssh/hyperbutler/internal/broker"
)

// fakeService is a broker service whose state follows a script: each query returns the
// next state (the last one repeats), and a start moves it on to states.
type fakeService struct {
	states   []broker.ServiceState
	afterRun []broker.ServiceState // the states after a start
	queryErr error
	startErr error
	starts   int
	clock    time.Time
}

func (f *fakeService) probe() serviceProbe {
	return serviceProbe{
		query: func() (broker.ServiceState, error) {
			if f.queryErr != nil {
				return broker.ServiceState{}, f.queryErr
			}
			st := f.states[0]
			if len(f.states) > 1 {
				f.states = f.states[1:]
			}
			return st, nil
		},
		start: func() error {
			f.starts++
			if f.startErr != nil {
				return f.startErr
			}
			if f.afterRun != nil {
				f.states = f.afterRun
			}
			return nil
		},
		wait:  10 * time.Second,
		tick:  time.Second,
		sleep: func(d time.Duration) { f.clock = f.clock.Add(d) },
		now:   func() time.Time { return f.clock },
	}
}

func svcState(s svc.State) broker.ServiceState { return broker.ServiceState{Installed: true, State: s} }

// TestChooseBackend: auto uses the service when it runs, starts it when it is stopped
// and waits while it is pending, and otherwise runs the VM in the engine and says why;
// broker fails instead; hcs, --broker and a developer's own image never ask the
// service.
func TestChooseBackend(t *testing.T) {
	running, stopped := svcState(svc.Running), svcState(svc.Stopped)
	for _, tc := range []struct {
		name     string
		backend  string
		pipe     string
		custom   bool
		svc      fakeService
		wantPipe string
		wantNote string // a substring; "" for none
		wantErr  bool
		starts   int
	}{
		{name: "auto, running", backend: backendAuto, svc: fakeService{states: []broker.ServiceState{running}}, wantPipe: broker.ServicePipe},
		{name: "auto, not installed", backend: backendAuto, svc: fakeService{states: []broker.ServiceState{{}}}, wantNote: "not installed"},
		{name: "auto, stopped, starts", backend: backendAuto,
			svc:      fakeService{states: []broker.ServiceState{stopped}, afterRun: []broker.ServiceState{svcState(svc.StartPending), svcState(svc.StartPending), running}},
			wantPipe: broker.ServicePipe, starts: 1},
		{name: "auto, stopped, start refused", backend: backendAuto,
			svc:      fakeService{states: []broker.ServiceState{stopped}, startErr: errors.New("access denied")},
			wantNote: "could not be started: access denied", starts: 1},
		{name: "auto, starts and stops again", backend: backendAuto,
			svc:      fakeService{states: []broker.ServiceState{stopped}, afterRun: []broker.ServiceState{svcState(svc.StartPending), stopped}},
			wantNote: "stopped while it was starting", starts: 1},
		{name: "auto, stopping, then starts", backend: backendAuto,
			svc:      fakeService{states: []broker.ServiceState{svcState(svc.StopPending), stopped}, afterRun: []broker.ServiceState{running}},
			wantPipe: broker.ServicePipe, starts: 1},
		{name: "auto, pending too long", backend: backendAuto,
			svc:      fakeService{states: []broker.ServiceState{svcState(svc.StartPending)}},
			wantNote: "still starting after 10s"},
		{name: "auto, uninstalled while waiting", backend: backendAuto,
			svc:      fakeService{states: []broker.ServiceState{svcState(svc.StartPending), {}}},
			wantNote: "uninstalled"},
		{name: "auto, query fails", backend: backendAuto, svc: fakeService{queryErr: errors.New("no SCM")}, wantNote: "no SCM"},
		{name: "broker, running", backend: backendBroker, svc: fakeService{states: []broker.ServiceState{running}}, wantPipe: broker.ServicePipe},
		{name: "broker, not installed", backend: backendBroker, svc: fakeService{states: []broker.ServiceState{{}}}, wantErr: true},
		{name: "broker, start refused", backend: backendBroker, svc: fakeService{states: []broker.ServiceState{stopped}, startErr: errors.New("access denied")}, wantErr: true, starts: 1},
		{name: "hcs", backend: backendHCS, svc: fakeService{queryErr: errors.New("not asked")}},
		{name: "--broker", backend: backendAuto, pipe: `\\.\pipe\b`, svc: fakeService{queryErr: errors.New("not asked")}, wantPipe: `\\.\pipe\b`},
		{name: "own image, auto", backend: backendAuto, custom: true, svc: fakeService{queryErr: errors.New("not asked")}},
		{name: "own image, broker", backend: backendBroker, custom: true, svc: fakeService{queryErr: errors.New("not asked")}, wantErr: true},
		{name: "own image, --broker", backend: backendAuto, pipe: `\\.\pipe\b`, custom: true, svc: fakeService{queryErr: errors.New("not asked")}, wantErr: true},
	} {
		f := tc.svc
		got, err := chooseBackend(tc.backend, tc.pipe, tc.custom, f.probe(), io.Discard)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: error %v", tc.name, err)
			continue
		}
		if tc.wantErr {
			if tc.backend == backendBroker && !tc.custom && !errors.Is(err, errNoBrokerService) {
				t.Errorf("%s: %v is not errNoBrokerService", tc.name, err)
			}
			continue
		}
		if got.pipe != tc.wantPipe {
			t.Errorf("%s: pipe %q, want %q", tc.name, got.pipe, tc.wantPipe)
		}
		if (tc.wantNote == "") != (got.note == "") || !strings.Contains(got.note, tc.wantNote) {
			t.Errorf("%s: note %q, want %q", tc.name, got.note, tc.wantNote)
		}
		if f.starts != tc.starts {
			t.Errorf("%s: %d starts, want %d", tc.name, f.starts, tc.starts)
		}
	}
}

// The image next to hyperbutler.exe, however it is spelled, is not a developer's own.
func TestCustomImageDir(t *testing.T) {
	def := defaultImageDir()
	if def == "" {
		t.Skip("no executable path")
	}
	if customImageDir("") || customImageDir(def) || customImageDir(strings.ToUpper(def)) || customImageDir(def+`\`) {
		t.Error("the default image folder counts as a custom one")
	}
	if !customImageDir(filepath.Join(t.TempDir(), "image")) {
		t.Error("another folder does not count as a custom one")
	}
}
