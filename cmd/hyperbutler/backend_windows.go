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
}

var machineService = serviceProbe{
	query: func() (broker.ServiceState, error) { return broker.QueryService(broker.ServiceName) },
	start: func() error { return broker.RequestServiceStart(broker.ServiceName) },
	wait:  90 * time.Second, // the service's own start hint is 60 s
	tick:  250 * time.Millisecond,
	sleep: time.Sleep,
	now:   time.Now,
}

// backendChoice is how this start boots the VM: through the broker on pipe, or, with
// pipe empty, in the engine process. note says why the broker service is not used,
// when backend auto fell back from it.
type backendChoice struct {
	pipe string
	note string
}

// errNoBrokerService is the --backend broker error when the service cannot be used.
var errNoBrokerService = errors.New("--backend broker")

// chooseBackend picks how start boots the VM from --backend, --broker and whether
// --image-dir names an image of the caller's (customImage: the broker boots only its
// own). For auto and broker it uses the broker service when it runs, starts it when it
// is stopped (or waits for it while it starts or stops), and gives it up to p.wait; auto
// then falls back to the engine process and says why, broker fails. progress is told
// when it starts the service.
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
	why, err := serviceUsable(p, progress)
	if err == nil {
		return backendChoice{pipe: broker.ServicePipe}, nil
	}
	if backend == backendBroker {
		return backendChoice{}, fmt.Errorf("%w: %w; see hyperbutler doctor", errNoBrokerService, err)
	}
	return backendChoice{note: why}, nil
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
// stopped. Otherwise it says why not: why for the log, err for an error.
func serviceUsable(p serviceProbe, progress io.Writer) (why string, err error) {
	st, err := p.query()
	if err != nil {
		return "cannot read the broker service's state: " + err.Error(), err
	}
	if !st.Installed {
		return "the broker service (" + broker.ServiceName + ") is not installed", errors.New("the broker service is not installed")
	}
	deadline := p.now().Add(p.wait)
	started := false
	for {
		switch {
		case st.State == svc.Running:
			return "", nil
		case st.State == svc.Stopped && !started:
			started = true
			_, _ = fmt.Fprintln(progress, "hyperbutler: starting the broker service...")
			if err := p.start(); err != nil {
				return "the broker service is installed but stopped, and could not be started: " + err.Error(), err
			}
		case st.State == svc.Stopped:
			// It started and stopped again: its event log says why.
			err := fmt.Errorf("the broker service stopped while it was starting (the Application event log, source %s, says why)", broker.EventSource)
			return err.Error(), err
		}
		if !p.now().Before(deadline) {
			err := fmt.Errorf("the broker service is still %s after %v", broker.StateName(st.State), p.wait)
			return err.Error(), err
		}
		p.sleep(p.tick)
		if st, err = p.query(); err != nil {
			return "cannot read the broker service's state: " + err.Error(), err
		}
		if !st.Installed {
			return "the broker service was uninstalled", errors.New("the broker service was uninstalled")
		}
	}
}
