//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"

	"github.com/Higangssh/hyperbutler/internal/broker"
	"github.com/Higangssh/hyperbutler/internal/engine"
)

// serviceProbe is what chooseBackend reads and does to the broker service; tests
// replace it.
type serviceProbe struct {
	query func() (broker.ServiceState, error)
	// start asks the SCM to start the service, which the service lets interactive
	// users do (ADR 0004, Installer (M4)).
	start func() error
	// wait bounds how long a start waits for the service to run, and tick is how often
	// it looks.
	wait, tick time.Duration
	sleep      func(time.Duration)
	now        func() time.Time
	// hcsAccess reports whether this process may call HCS itself (canUseHCS).
	hcsAccess func() bool
}

var machineService = serviceProbe{
	query:     func() (broker.ServiceState, error) { return broker.QueryService(broker.ServiceName) },
	start:     func() error { return broker.RequestServiceStart(broker.ServiceName) },
	wait:      90 * time.Second, // the service's own start hint is 60 s
	tick:      250 * time.Millisecond,
	sleep:     time.Sleep,
	now:       time.Now,
	hcsAccess: canUseHCS,
}

// backendChoice is how this start boots the VM: through the broker on pipe, or, with
// pipe empty, in the engine process. note says why the broker service is not used,
// when backend auto fell back from it; warn says to show it on the terminal whatever
// this account's HCS access.
type backendChoice struct {
	pipe string
	note string
	warn bool
}

// errNotInteractive is serviceUsable's error when this sign-in may not even read the
// broker service: the service admits interactive sign-ins and services (IU, SU), and a
// network logon (ssh) or a batch task is neither.
var errNotInteractive = errors.New("this sign-in is not interactive, and the broker service serves interactive sign-ins only")

// errNoBrokerService is what the error of a start that needs the broker service and
// cannot use it wraps.
var errNoBrokerService = errors.New("the broker service cannot be used")

// brokerStartError is a start that did not spawn the engine because the broker
// service is installed but cannot be used: code is the engine's error code
// (CodeBrokerUnavailable), which start records in state.json for the window.
type brokerStartError struct {
	code string
	err  error
}

func (e *brokerStartError) Error() string { return e.err.Error() }
func (e *brokerStartError) Unwrap() error { return e.err }

// chooseBackend picks how start boots the VM from --backend, --broker and whether
// --image-dir names an image of the caller's (customImage: the broker boots only its
// own). For auto and broker it uses the broker service when it runs, starts it when it
// is stopped (or waits for it while it starts or stops), and gives it up to p.wait. A
// service that is installed but cannot be used is an error for both: an administrator
// installed it to be used, so running the VM in the engine process instead would only
// fail differently, or hide the problem (ADR 0004 rule 5). Only auto without the
// service at all falls back to the engine process, and says why. progress is told when
// it starts the service.
func chooseBackend(backend, brokerPipe string, customImage bool, p serviceProbe, progress io.Writer) (backendChoice, error) {
	switch {
	case brokerPipe != "":
		if customImage {
			return backendChoice{}, errors.New("--image-dir and --broker exclude each other: the broker boots its own guest image")
		}
		return backendChoice{pipe: brokerPipe}, nil
	case backend == backendHCS:
		return backendChoice{}, nil
	case customImage && backend == backendBroker:
		return backendChoice{}, errors.New("--image-dir and --backend broker exclude each other: the broker service boots the guest image installed with it")
	case customImage:
		return backendChoice{}, nil // a developer's own image boots in the engine process
	}
	installed, err := serviceUsable(p, progress)
	switch {
	case err == nil:
		return backendChoice{pipe: broker.ServicePipe}, nil
	case !installed && backend == backendAuto:
		return backendChoice{note: err.Error()}, nil
	case errors.Is(err, errNotInteractive) && backend == backendAuto && p.hcsAccess != nil && p.hcsAccess():
		// The service cannot serve this sign-in, but the account may call HCS itself
		// (Hyper-V Administrators, or elevated), as before the broker: say so and do that.
		return backendChoice{note: err.Error(), warn: true}, nil
	}
	return backendChoice{}, &brokerStartError{code: engine.CodeBrokerUnavailable,
		err: fmt.Errorf("%w: %w; hyperbutler doctor checks it (--backend hcs runs the VM without it, which needs Hyper-V Administrators or elevation)", errNoBrokerService, err)}
}

// startHint is what serviceUsable says when this user could not start the service: the
// service lets interactive sign-ins start it, and nothing else.
func startHint(err error) string {
	switch {
	case errors.Is(err, windows.ERROR_SERVICE_DISABLED):
		return "an administrator disabled it; an administrator enables it again (sc.exe config " + broker.ServiceName + " start= delayed-auto) or repairs hyperbutler"
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return "only an interactive sign-in may start it (not ssh, a service or a scheduled task without one): start hyperbutler from your desktop, or an administrator runs sc.exe start " + broker.ServiceName
	}
	return "an administrator runs sc.exe start " + broker.ServiceName + "; the Application event log (source " + broker.EventSource + ") says why it does not run"
}

// recordStartFailure records a start that failed before the engine was spawned, because
// the broker service cannot be used (a brokerStartError), as a failed state.json with
// its code, so the window explains it as it does an engine's failure (HB-VM-005). It
// runs under the operations lock, with no engine running; the past runtime ids stay.
func (in *install) recordStartFailure(err error) {
	var be *brokerStartError
	if !errors.As(err, &be) {
		return
	}
	st := &engine.State{}
	if prev, lerr := engine.LoadState(in.l); lerr == nil {
		st.PastRuntimeIDs, st.Pipe, st.DataDir = prev.PastRuntimeIDs, prev.Pipe, prev.DataDir
	}
	st.Phase, st.Error, st.ErrorCode = engine.PhaseFailed, err.Error(), be.code
	_ = engine.SaveState(in.l, st)
}

// customImageDir reports whether start's --image-dir names an image other than the one
// installed next to hyperbutler.exe (which is the broker's too).
func customImageDir(dir string) bool {
	if dir == "" {
		return false
	}
	abs, err := filepath.Abs(dir)
	return err != nil || !engine.SamePath(abs, defaultImageDir())
}

// canUseHCS reports whether this process may call HCS itself: elevated, or in Hyper-V
// Administrators.
func canUseHCS() bool {
	if windows.GetCurrentProcessToken().IsElevated() {
		return true
	}
	member, err := inHyperVAdmins()
	return err == nil && member
}

// serviceUsable reports whether the broker service runs, after starting it if it is
// stopped; otherwise err says why not, and installed whether the service exists (a
// failed query counts as installed: auto must not fall back on it).
func serviceUsable(p serviceProbe, progress io.Writer) (installed bool, err error) {
	st, err := p.query()
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return true, fmt.Errorf("%w: %s", errNotInteractive, startHint(windows.ERROR_ACCESS_DENIED))
	}
	if err != nil {
		return true, fmt.Errorf("cannot read the broker service's state: %w", err)
	}
	if !st.Installed {
		return false, errors.New("the broker service (" + broker.ServiceName + ") is not installed")
	}
	deadline := p.now().Add(p.wait)
	started := false
	for {
		switch {
		case st.State == svc.Running:
			return true, nil
		case st.State == svc.Stopped && st.Disabled:
			return true, fmt.Errorf("the broker service is disabled: %s", startHint(windows.ERROR_SERVICE_DISABLED))
		case st.State == svc.Stopped && !started:
			started = true
			_, _ = fmt.Fprintln(progress, "hyperbutler: starting the broker service...")
			if err := p.start(); err != nil {
				return true, fmt.Errorf("the broker service is stopped, and this account cannot start it (%w): %s", err, startHint(err))
			}
		case st.State == svc.Stopped:
			// It started and stopped again: its event log says why.
			return true, fmt.Errorf("the broker service stopped while it was starting (the Application event log, source %s, says why)", broker.EventSource)
		}
		if !p.now().Before(deadline) {
			return true, fmt.Errorf("the broker service is still %s after %v", broker.StateName(st.State), p.wait)
		}
		p.sleep(p.tick)
		if st, err = p.query(); err != nil {
			return true, fmt.Errorf("cannot read the broker service's state: %w", err)
		}
		if !st.Installed {
			return true, errors.New("the broker service was uninstalled while it was starting")
		}
	}
}
