//go:build windows

package main

// hyperbutler setup broker install|uninstall: the HCS access broker service, for the MSI
// (ADR 0004, Installer (M4)), elevated and as SYSTEM like the other setup verbs.
//
// install registers hyperbutler-broker.exe next to this hyperbutler.exe as the service,
// through the same code as `hyperbutler-broker install` (internal/broker InstallService,
// which refuses an executable that anyone but SYSTEM, Administrators and TrustedInstaller
// could replace), and starts it. It takes no path: the MSI's install folder is the only
// place it installs from. It is idempotent, because the MSI runs it on a first install,
// an upgrade and a repair (which a standard user can start):
//
//   - a service registered as it would register it is left running (a repair must not
//     restart the broker, which would end every user's VM), or started if it is
//     stopped; its DACL is set again;
//   - a service registered otherwise (another executable, an older layout) is removed
//     and registered again;
//   - a service that is deleted but not gone yet (the uninstall of the product this one
//     upgrades deleted it while a handle to it was open) is waited for.
//
// uninstall stops and deletes the service and its event log source; a service that is
// not installed is not an error.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/Higangssh/hyperbutler/internal/broker"
)

// brokerExeName is the broker's executable, which the MSI installs next to
// hyperbutler.exe.
const brokerExeName = "hyperbutler-broker.exe"

// brokerService is what setup broker does to the service; tests replace it.
type brokerService struct {
	// query reads the service's configuration and state; installed is false when there
	// is no such service.
	query     func() (installed bool, cfg mgr.Config, state svc.State, err error)
	install   func(broker.InstallConfig) error
	uninstall func() error
	start     func() error
	secure    func() error
	// checkPath is broker.CheckServiceBinaryPath.
	checkPath func(exe string) error
	sleep     func(time.Duration)
	// wait bounds each wait: for a deleted service to go, and for a start.
	wait time.Duration
}

var machineBrokerService = brokerService{
	query: func() (bool, mgr.Config, svc.State, error) {
		m, err := mgr.Connect()
		if err != nil {
			return false, mgr.Config{}, 0, fmt.Errorf("connect to the service control manager: %w", err)
		}
		defer func() { _ = m.Disconnect() }()
		s, err := m.OpenService(broker.ServiceName)
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return false, mgr.Config{}, 0, nil
		}
		if err != nil {
			return false, mgr.Config{}, 0, fmt.Errorf("open service %s: %w", broker.ServiceName, err)
		}
		defer func() { _ = s.Close() }()
		c, err := s.Config()
		if err != nil {
			return true, mgr.Config{}, 0, fmt.Errorf("read the configuration of %s: %w", broker.ServiceName, err)
		}
		q, err := s.Query()
		if err != nil {
			return true, c, 0, fmt.Errorf("query %s: %w", broker.ServiceName, err)
		}
		return true, c, q.State, nil
	},
	install:   broker.InstallService,
	uninstall: func() error { return broker.UninstallService(broker.ServiceName) },
	start:     func() error { return broker.RequestServiceStart(broker.ServiceName) },
	secure:    func() error { return broker.SecureService(broker.ServiceName) },
	checkPath: broker.CheckServiceBinaryPath,
	sleep:     time.Sleep,
	wait:      90 * time.Second,
}

func cmdSetupBroker(verb string, stdout io.Writer) error {
	switch verb {
	case "install":
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("setup broker install: %w", err)
		}
		return setupBrokerInstall(machineBrokerService, filepath.Join(filepath.Dir(filepath.Clean(exe)), brokerExeName), stdout)
	case "uninstall":
		return setupBrokerUninstall(machineBrokerService, stdout)
	}
	return usagef("setup broker: install or uninstall")
}

// sameBrokerConfig reports whether the installed service's configuration have is what
// InstallService registers for want, in what decides what runs and as whom.
func sameBrokerConfig(have mgr.Config, want broker.InstallConfig) bool {
	return strings.EqualFold(have.BinaryPathName, want.CommandLine) &&
		strings.EqualFold(have.ServiceStartName, want.Account) &&
		have.ServiceType == windows.SERVICE_WIN32_OWN_PROCESS &&
		have.StartType == want.StartType && have.DelayedAutoStart == want.DelayedAutoStart &&
		have.SidType == want.SidType
}

func setupBrokerInstall(m brokerService, exe string, stdout io.Writer) error {
	fail := func(err error) error { return fmt.Errorf("setup broker install: %w", err) }
	if fi, err := os.Stat(exe); err != nil {
		return fail(fmt.Errorf("no broker executable: %w", err))
	} else if !fi.Mode().IsRegular() {
		return fail(fmt.Errorf("%s is not a regular file", exe))
	}
	cfg, err := broker.BuildInstallConfig(exe, nil)
	if err != nil {
		return fail(err)
	}
	// Also for a service that is registered already: it must not run a file someone
	// else could replace.
	if err := m.checkPath(exe); err != nil {
		return fail(fmt.Errorf("the service would run %s as SYSTEM, but %w", exe, err))
	}
	installed, have, state, err := m.query()
	if err != nil {
		return fail(err)
	}
	if installed && sameBrokerConfig(have, cfg) {
		err := m.secure()
		if err == nil && state != svc.Running {
			err = startBroker(m, state)
		}
		switch {
		case err == nil:
			_, _ = fmt.Fprintf(stdout, "%s is installed and running: %s\n", broker.ServiceName, cfg.CommandLine)
			return nil
		case !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE):
			return fail(err)
		}
		// Deleted, not gone yet: wait, then register it again below.
	} else if installed {
		_, _ = fmt.Fprintf(stdout, "%s runs %s; registering it again for %s\n", broker.ServiceName, have.BinaryPathName, cfg.CommandLine)
		if err := m.uninstall(); err != nil && !errors.Is(err, broker.ErrServiceMarkedForDelete) {
			return fail(err)
		}
	}
	if err := waitBrokerGone(m); err != nil {
		return fail(err)
	}
	if err := m.install(cfg); err != nil {
		return fail(err)
	}
	_, _ = fmt.Fprintf(stdout, "installed and started %s: %s\n", broker.ServiceName, cfg.CommandLine)
	return nil
}

// startBroker starts the service, which is in state, and waits until it runs.
func startBroker(m brokerService, state svc.State) error {
	deadline := time.Now().Add(m.wait)
	for state != svc.Running {
		if state == svc.Stopped {
			if err := m.start(); err != nil {
				return err
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s is still %s after %v", broker.ServiceName, broker.StateName(state), m.wait)
		}
		m.sleep(250 * time.Millisecond)
		installed, _, st, err := m.query()
		if err != nil {
			return err
		}
		if !installed {
			return windows.ERROR_SERVICE_MARKED_FOR_DELETE // it went away under us
		}
		if st == svc.Stopped && state != svc.Stopped {
			return fmt.Errorf("%s stopped again (the Application event log, source %s, says why)", broker.ServiceName, broker.EventSource)
		}
		state = st
	}
	return nil
}

// waitBrokerGone waits until no service of the broker's name exists: the SCM removes a
// deleted service once its process has ended and the last handle to it is closed.
func waitBrokerGone(m brokerService) error {
	deadline := time.Now().Add(m.wait)
	for {
		installed, _, _, err := m.query()
		if err != nil {
			return err
		}
		if !installed {
			return nil
		}
		if time.Now().After(deadline) {
			return broker.ErrServiceMarkedForDelete
		}
		m.sleep(500 * time.Millisecond)
	}
}

func setupBrokerUninstall(m brokerService, stdout io.Writer) error {
	installed, _, _, err := m.query()
	if err != nil {
		return fmt.Errorf("setup broker uninstall: %w", err)
	}
	if err := m.uninstall(); err != nil && !errors.Is(err, broker.ErrServiceMarkedForDelete) {
		return fmt.Errorf("setup broker uninstall: %w", err)
	}
	// The SCM removes it once its process has ended; until then its file is in use.
	if err := waitBrokerGone(m); err != nil {
		return fmt.Errorf("setup broker uninstall: %w", err)
	}
	if installed {
		_, _ = fmt.Fprintf(stdout, "uninstalled %s\n", broker.ServiceName)
	} else {
		_, _ = fmt.Fprintf(stdout, "%s was not installed\n", broker.ServiceName)
	}
	return nil
}
