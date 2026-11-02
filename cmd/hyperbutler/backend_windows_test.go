//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"

	"github.com/Higangssh/hyperbutler/internal/broker"
	"github.com/Higangssh/hyperbutler/internal/engine"
)

// fakeService is a broker service whose state follows a script: each query returns the
// next state (the last one repeats), and a start moves it on to states.
type fakeService struct {
	states   []broker.ServiceState
	afterRun []broker.ServiceState // the states after a start
	queryErr error
	startErr error
	// hcs: this account may call HCS itself.
	hcs    bool
	starts int
	clock  time.Time
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
		wait:      10 * time.Second,
		tick:      time.Second,
		sleep:     func(d time.Duration) { f.clock = f.clock.Add(d) },
		now:       func() time.Time { return f.clock },
		hcsAccess: func() bool { return f.hcs },
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
			svc:     fakeService{states: []broker.ServiceState{stopped}, startErr: errors.New("access denied")},
			wantErr: true, starts: 1},
		{name: "auto, starts and stops again", backend: backendAuto,
			svc:     fakeService{states: []broker.ServiceState{stopped}, afterRun: []broker.ServiceState{svcState(svc.StartPending), stopped}},
			wantErr: true, starts: 1},
		{name: "auto, stopping, then starts", backend: backendAuto,
			svc:      fakeService{states: []broker.ServiceState{svcState(svc.StopPending), stopped}, afterRun: []broker.ServiceState{running}},
			wantPipe: broker.ServicePipe, starts: 1},
		{name: "auto, pending too long", backend: backendAuto,
			svc:     fakeService{states: []broker.ServiceState{svcState(svc.StartPending)}},
			wantErr: true},
		{name: "auto, uninstalled while waiting", backend: backendAuto,
			svc:     fakeService{states: []broker.ServiceState{svcState(svc.StartPending), {}}},
			wantErr: true},
		{name: "auto, query fails", backend: backendAuto, svc: fakeService{queryErr: errors.New("no SCM")}, wantErr: true},
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
			// Every error about the service itself carries the code the window shows.
			var be *brokerStartError
			if !tc.custom && (!errors.Is(err, errNoBrokerService) || !errors.As(err, &be) || be.code != engine.CodeBrokerUnavailable) {
				t.Errorf("%s: %v is not a coded broker start error", tc.name, err)
			}
			if f.starts != tc.starts {
				t.Errorf("%s: %d starts, want %d", tc.name, f.starts, tc.starts)
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

// TestChooseBackendNotInteractive: a sign-in that may not even read the service (ssh, a
// batch task) runs the VM itself, with a warning, when the account has HCS access of its
// own, and otherwise fails with what to do; --backend broker always fails.
func TestChooseBackendNotInteractive(t *testing.T) {
	denied := fmt.Errorf("open service hyperbutler-broker: %w", windows.ERROR_ACCESS_DENIED)
	f := fakeService{queryErr: denied, hcs: true}
	got, err := chooseBackend(backendAuto, "", false, f.probe(), io.Discard)
	if err != nil || got.pipe != "" || !got.warn || !strings.Contains(got.note, "not interactive") {
		t.Errorf("with HCS access: %+v, %v", got, err)
	}
	f = fakeService{queryErr: denied}
	_, err = chooseBackend(backendAuto, "", false, f.probe(), io.Discard)
	var be *brokerStartError
	if !errors.As(err, &be) || !strings.Contains(err.Error(), "interactive sign-in") || !strings.Contains(err.Error(), "sc.exe start") {
		t.Errorf("without HCS access: %v", err)
	}
	f = fakeService{queryErr: denied, hcs: true}
	if _, err := chooseBackend(backendBroker, "", false, f.probe(), io.Discard); err == nil {
		t.Error("--backend broker fell back")
	}
}

// TestChooseBackendDisabled: a disabled service is not started (and start says nothing
// about starting it); the error says an administrator disabled it.
func TestChooseBackendDisabled(t *testing.T) {
	var out strings.Builder
	f := fakeService{states: []broker.ServiceState{{Installed: true, State: svc.Stopped, Disabled: true}}, hcs: true}
	_, err := chooseBackend(backendAuto, "", false, f.probe(), &out)
	if err == nil || f.starts != 0 || out.Len() != 0 || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("%v, %d starts, %q", err, f.starts, out.String())
	}
}

// TestRecordStartFailure: a start the broker service kept from spawning the engine
// leaves a failed state.json with the code the window maps to HB-VM-005, keeping the
// past runtime ids; any other error writes nothing.
func TestRecordStartFailure(t *testing.T) {
	dir := t.TempDir()
	in := &install{l: engine.Layout{Home: dir, Data: dir}}
	if err := engine.SaveState(in.l, &engine.State{Phase: engine.PhaseStopped, PastRuntimeIDs: []string{"a-b"}}); err != nil {
		t.Fatal(err)
	}
	in.recordStartFailure(errors.New("not the broker's"))
	if st, _ := engine.LoadState(in.l); st.Phase != engine.PhaseStopped {
		t.Fatalf("a plain error changed the state: %+v", st)
	}
	f := fakeService{states: []broker.ServiceState{svcState(svc.Stopped)}, startErr: windows.ERROR_ACCESS_DENIED}
	_, err := chooseBackend(backendAuto, "", false, f.probe(), io.Discard)
	in.recordStartFailure(err)
	st, lerr := engine.LoadState(in.l)
	if lerr != nil || st.Phase != engine.PhaseFailed || st.ErrorCode != engine.CodeBrokerUnavailable || st.PID != 0 ||
		len(st.PastRuntimeIDs) != 1 || !strings.Contains(st.Error, "interactive") {
		t.Fatalf("%+v, %v", st, lerr)
	}
}

// TestStartHint: a token that is not interactive, or a disabled service, is told what
// to do instead of being pointed at Hyper-V Administrators.
func TestStartHint(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{windows.ERROR_ACCESS_DENIED, "interactive sign-in"},
		{windows.ERROR_SERVICE_DISABLED, "disabled it"},
		{errors.New("something else"), "sc.exe start"},
	} {
		f := fakeService{states: []broker.ServiceState{svcState(svc.Stopped)}, startErr: tc.err}
		_, err := chooseBackend(backendAuto, "", false, f.probe(), io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v", tc.err, err)
		}
	}
}
