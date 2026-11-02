//go:build windows

package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/Higangssh/hyperbutler/internal/broker"
)

// fakeSCM is the broker's service as setup broker sees it.
type fakeSCM struct {
	installed bool
	cfg       mgr.Config
	state     svc.State
	// gone counts down the queries after which a deleted service is removed.
	deleted bool
	gone    int
	// startTo is the state a start leads to (Running when zero).
	startTo  svc.State
	startErr error

	installs, uninstalls, starts, secures int
	installErr                            error
}

func (f *fakeSCM) service() brokerService {
	return brokerService{
		query: func() (bool, mgr.Config, svc.State, error) {
			if f.deleted {
				if f.gone--; f.gone < 0 {
					f.installed, f.deleted = false, false
				}
			}
			return f.installed, f.cfg, f.state, nil
		},
		install: func(c broker.InstallConfig) error {
			f.installs++
			if f.installed {
				return broker.ErrServiceExists
			}
			if f.installErr != nil {
				return f.installErr
			}
			f.installed, f.state = true, svc.Running
			f.cfg = mgr.Config{BinaryPathName: c.CommandLine, ServiceStartName: c.Account, ServiceType: windows.SERVICE_WIN32_OWN_PROCESS,
				StartType: c.StartType, DelayedAutoStart: c.DelayedAutoStart, SidType: c.SidType}
			return nil
		},
		uninstall: func() error {
			f.uninstalls++
			f.deleted, f.gone, f.state = true, 2, svc.Stopped
			return broker.ErrServiceMarkedForDelete
		},
		start: func() error {
			f.starts++
			if f.deleted {
				return windows.ERROR_SERVICE_MARKED_FOR_DELETE
			}
			if f.startErr != nil {
				return f.startErr
			}
			f.state = svc.Running
			if f.startTo != 0 {
				f.state = f.startTo
			}
			return nil
		},
		secure: func() error {
			f.secures++
			if f.deleted {
				return windows.ERROR_SERVICE_MARKED_FOR_DELETE
			}
			return nil
		},
		checkPath: func(string) error { return nil },
		sleep:     func(time.Duration) {},
		wait:      time.Second,
	}
}

// TestSetupBrokerInstall: a fresh install registers the service; a repair of a
// running, matching service changes nothing but its DACL (it must not end every
// user's VM); a stopped one is started; one registered for another executable is
// replaced; a deleted one that has not gone yet is waited for.
func TestSetupBrokerInstall(t *testing.T) {
	exe := filepath.Join(t.TempDir(), brokerExeName)
	if err := os.WriteFile(exe, []byte("MZ"), 0o600); err != nil {
		t.Fatal(err)
	}
	want, err := broker.BuildInstallConfig(exe, nil)
	if err != nil {
		t.Fatal(err)
	}
	ours := mgr.Config{BinaryPathName: want.CommandLine, ServiceStartName: "LocalSystem", ServiceType: windows.SERVICE_WIN32_OWN_PROCESS,
		StartType: want.StartType, DelayedAutoStart: true, SidType: want.SidType}
	other := ours
	other.BinaryPathName = `"C:\ProgramData\old\hyperbutler-broker.exe" run`

	for _, tc := range []struct {
		name                                      string
		f                                         fakeSCM
		ok                                        bool
		installs, uninstalls, starts, wantSecures int
	}{
		{name: "fresh", f: fakeSCM{}, ok: true, installs: 1},
		{name: "repair, running", f: fakeSCM{installed: true, cfg: ours, state: svc.Running}, ok: true, wantSecures: 1},
		{name: "repair, other case", f: fakeSCM{installed: true, cfg: func() mgr.Config { c := ours; c.BinaryPathName = strings.ToUpper(c.BinaryPathName); return c }(), state: svc.Running}, ok: true, wantSecures: 1},
		{name: "stopped", f: fakeSCM{installed: true, cfg: ours, state: svc.Stopped}, ok: true, starts: 1, wantSecures: 1},
		{name: "stopped, start fails", f: fakeSCM{installed: true, cfg: ours, state: svc.Stopped, startErr: errors.New("1053")}, starts: 1, wantSecures: 1},
		{name: "stopped, stops again", f: fakeSCM{installed: true, cfg: ours, state: svc.Stopped, startTo: svc.StartPending}, starts: 1, wantSecures: 1},
		{name: "other executable", f: fakeSCM{installed: true, cfg: other, state: svc.Running}, ok: true, uninstalls: 1, installs: 1},
		{name: "deleted, not gone", f: fakeSCM{installed: true, cfg: ours, state: svc.Stopped, deleted: true, gone: 3}, ok: true, installs: 1, wantSecures: 1},
		// A start that leaves it stopped is tried once, not until the wait runs out.
		{name: "stopped, start leaves it stopped", f: fakeSCM{installed: true, cfg: ours, state: svc.Stopped, startTo: svc.Stopped}, starts: 1, wantSecures: 1},
		{name: "install fails", f: fakeSCM{installErr: errors.New("create the service: access denied")}, installs: 1},
	} {
		f := tc.f
		m := f.service()
		if tc.name == "stopped, stops again" {
			// The start leads to StartPending, then the service stops on its own.
			q := m.query
			n := 0
			m.query = func() (bool, mgr.Config, svc.State, error) {
				i, c, s, err := q()
				if n++; n > 2 {
					s = svc.Stopped
				}
				return i, c, s, err
			}
		}
		err := setupBrokerInstall(m, exe, io.Discard)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
		if f.installs != tc.installs || f.uninstalls != tc.uninstalls || f.starts != tc.starts || f.secures != tc.wantSecures {
			t.Errorf("%s: installs %d uninstalls %d starts %d secures %d", tc.name, f.installs, f.uninstalls, f.starts, f.secures)
		}
		if tc.ok && (!f.installed || f.state != svc.Running || !sameBrokerConfig(f.cfg, want)) {
			t.Errorf("%s: ends %+v", tc.name, f)
		}
	}

	// A service an administrator disabled stays as it is (a repair a standard user starts
	// must not turn it on again), whatever it runs: the outcome is a note, not a failure.
	for _, c := range []mgr.Config{ours, other} {
		c.StartType = windows.SERVICE_DISABLED
		f := fakeSCM{installed: true, cfg: c, state: svc.Stopped}
		err := setupBrokerInstall(f.service(), exe, io.Discard)
		var note *setupNote
		if !errors.As(err, &note) || f.installs != 0 || f.uninstalls != 0 || f.starts != 0 || f.secures != 0 {
			t.Errorf("disabled %s: %v, %+v", c.BinaryPathName, err, f)
		}
	}

	// A missing executable, or one in a folder others can change, is refused before
	// the service is touched.
	f := fakeSCM{}
	if err := setupBrokerInstall(f.service(), filepath.Join(t.TempDir(), brokerExeName), io.Discard); err == nil || f.installs != 0 {
		t.Errorf("missing executable: %v, %d installs", err, f.installs)
	}
	m := f.service()
	m.checkPath = broker.CheckServiceBinaryPath
	if err := setupBrokerInstall(m, exe, io.Discard); err == nil || f.installs != 0 {
		t.Errorf("executable in a user's folder: %v, %d installs", err, f.installs)
	}
}

// TestSetupBrokerUninstall: the service is removed and waited for; one that is not
// installed is not an error.
func TestSetupBrokerUninstall(t *testing.T) {
	f := fakeSCM{installed: true, state: svc.Running}
	if err := setupBrokerUninstall(f.service(), io.Discard); err != nil || f.installed || f.uninstalls != 1 {
		t.Errorf("installed: %v, %+v", err, f)
	}
	f = fakeSCM{}
	m := f.service()
	m.uninstall = func() error { f.uninstalls++; return nil }
	if err := setupBrokerUninstall(m, io.Discard); err != nil || f.uninstalls != 1 {
		t.Errorf("not installed: %v, %+v", err, f)
	}
	// A service whose process never ends stays marked for deletion: an error.
	f = fakeSCM{installed: true, state: svc.Running}
	m = f.service()
	m.uninstall = func() error { f.uninstalls++; f.deleted, f.gone = true, 1<<30; return broker.ErrServiceMarkedForDelete }
	m.wait = 0
	if err := setupBrokerUninstall(m, io.Discard); !errors.Is(err, broker.ErrServiceMarkedForDelete) {
		t.Errorf("never gone: %v", err)
	}
}
