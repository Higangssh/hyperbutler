//go:build windows

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"

	"github.com/Higangssh/hyperbutler/internal/broker"
	"github.com/Higangssh/hyperbutler/internal/diag"
	"github.com/Higangssh/hyperbutler/internal/imageset"
)

// subcommands are the service's commands; anything else runs in the foreground.
var subcommands = map[string]func(args []string) error{
	"install":   cmdInstall,
	"uninstall": cmdUninstall,
	"run":       cmdRun,
}

// devBuild reports whether this is a development build: version dev and no image pin.
// Only such a build accepts the service's -fake-backend and -allow-unpinned-image.
func devBuild() bool {
	_, pinned := imageset.Pin()
	return version == "dev" && !pinned
}

// serviceFlags are the flags install passes on to run, and run accepts. Both are for
// development builds only, so they are hidden from the usage text.
type serviceFlags struct {
	fake, allowUnpinned bool
}

func (f *serviceFlags) register(fs *flag.FlagSet) {
	fs.BoolVar(&f.fake, "fake-backend", false, "")
	fs.BoolVar(&f.allowUnpinned, "allow-unpinned-image", false, "")
}

func (f serviceFlags) check() error {
	if (f.fake || f.allowUnpinned) && !devBuild() {
		return errors.New("-fake-backend and -allow-unpinned-image are for development builds only")
	}
	if f.fake && f.allowUnpinned {
		return errors.New("-allow-unpinned-image is for the HCS backend, not -fake-backend")
	}
	return nil
}

// args returns the flags as the service command line carries them.
func (f serviceFlags) args() []string {
	var a []string
	if f.fake {
		a = append(a, "-fake-backend")
	}
	if f.allowUnpinned {
		a = append(a, "-allow-unpinned-image")
	}
	return a
}

// newServiceFlagSet returns a flag set whose usage text shows no flags: those it has are
// hidden.
func newServiceFlagSet(name string, f *serviceFlags) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	f.register(fs)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), "usage: hyperbutler-broker %s\n", name)
	}
	return fs
}

func parseNoArgs(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%s takes no arguments: %s", fs.Name(), strings.Join(fs.Args(), " "))
	}
	return nil
}

func cmdInstall(args []string) error {
	var f serviceFlags
	if err := parseNoArgs(newServiceFlagSet("install", &f), args); err != nil {
		return err
	}
	if err := f.check(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe = filepath.Clean(exe)
	cfg, err := broker.BuildInstallConfig(exe, f.args())
	if err != nil {
		return err
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("install needs an elevated (administrator) prompt")
	}
	if err := broker.CheckServiceBinaryPath(exe); err != nil {
		return fmt.Errorf("refusing to install: the service would run this file as SYSTEM, but %w. "+
			"Install it in a folder only administrators can change, such as Program Files", err)
	}
	if err := broker.InstallService(cfg); err != nil {
		return err
	}
	fmt.Printf("installed and started %s: %s\n", broker.ServiceName, cfg.CommandLine)
	return nil
}

func cmdUninstall(args []string) error {
	if err := parseNoArgs(newServiceFlagSet("uninstall", new(serviceFlags)), args); err != nil {
		return err
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("uninstall needs an elevated (administrator) prompt")
	}
	// Only to word the result: "is not installed" after a removal read as if there had
	// been nothing to remove.
	before, _ := broker.QueryService(broker.ServiceName)
	if err := broker.UninstallService(broker.ServiceName); err != nil {
		return err
	}
	if before.Installed {
		fmt.Printf("uninstalled %s\n", broker.ServiceName)
	} else {
		fmt.Printf("%s was not installed\n", broker.ServiceName)
	}
	return nil
}

func cmdRun(args []string) error {
	if isSvc, err := svc.IsWindowsService(); err != nil || !isSvc {
		return errors.New("run is for the service control manager; to run the broker in the foreground, use -fake-backend or -hcs-backend")
	}
	elog, err := broker.OpenEventLog()
	if err != nil {
		return err
	}
	defer func() { _ = elog.Close() }()
	return broker.RunService(serviceStarter(args, elog), elog)
}

// serviceLog is what the service's broker logs to (broker.EventLog).
type serviceLog interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
}

// serviceStarter parses run's flags and returns the service's start function. The
// event log is open by then, and a bad flag is the start's error, so the service
// handler logs it and ends with exitStartFailed: a service has no console.
func serviceStarter(args []string, log serviceLog) func() (broker.ServiceInstance, error) {
	var f serviceFlags
	fs := newServiceFlagSet("run", &f)
	fs.SetOutput(io.Discard)
	err := parseNoArgs(fs, args)
	if err == nil {
		err = f.check()
	}
	return func() (broker.ServiceInstance, error) {
		if err != nil {
			return nil, fmt.Errorf("run: %w", err)
		}
		return startService(f, log)
	}
}

// startService builds the service's broker: the backend, the server for every
// interactive user, and the shared pipe.
func startService(f serviceFlags, log serviceLog) (_ broker.ServiceInstance, err error) {
	var cleanup []func()
	defer func() {
		if err != nil {
			runAll(cleanup)
		}
	}()
	dbg, err := diag.Start(diag.Config{MemoryLimit: defaultMemLimit})
	if err != nil {
		return nil, err
	}
	cleanup = append(cleanup, func() { _ = dbg.Close() })

	var backend broker.Backend = broker.NewFakeBackend()
	var sweep func() int
	what := "fake backend"
	if !f.fake {
		self, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			return nil, err
		}
		// The state directory's DACL admits the service account only. Those of runs that
		// ended without a stop (a crash, the stop limit) go first.
		removeStaleStateDirs(serviceTempDir(), self.User.Sid, log.Infof)
		state, err := privateDir(serviceTempDir(), self.User.Sid.String())
		if err != nil {
			return nil, err
		}
		cleanup = append(cleanup, func() { _ = os.RemoveAll(state) })
		hb, err := broker.NewHCSBackend(broker.HCSConfig{StateDir: state, AllowUnpinned: f.allowUnpinned, Logf: log.Infof})
		if err != nil {
			return nil, err
		}
		// Runs before the state directory is removed (cleanup runs in reverse).
		cleanup = append(cleanup, func() { _ = hb.Close() })
		// The service is every user's broker, so it sweeps every user's broker VMs: a
		// crashed service left them, and no one else may run them as SYSTEM.
		sweep = func() int { return hb.Sweep("") }
		backend, what = hb, "HCS backend"
	}
	log.Infof("hyperbutler broker %s (%s) starting", version, what)
	parts := cleanup
	cleanup = nil // StartService runs them, now or when the broker stops
	s, err := broker.StartService(broker.ServiceConfig{
		Pipe: broker.ServicePipe, Instances: broker.ServicePipeInstances, Version: version,
		Backend: backend, Sweep: sweep, Infof: log.Infof, Warnf: log.Warnf,
		Cleanup: func() { runAll(parts) },
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// serviceTempDir is %SystemRoot%\SystemTemp, which only SYSTEM and Administrators may
// use, where Windows has it; otherwise the temporary directory.
func serviceTempDir() string {
	if win, err := windows.GetWindowsDirectory(); err == nil {
		d := filepath.Join(win, "SystemTemp")
		if fi, err := os.Lstat(d); err == nil && fi.IsDir() {
			return d
		}
	}
	return os.TempDir()
}

// stateDirName matches the names privateDir makes.
var stateDirName = regexp.MustCompile(`^hyperbutler-broker-[0-9a-f]{16}$`)

// removeStaleStateDirs removes the state directories under base that an earlier run of
// the service left behind: plain directories (no reparse point) with privateDir's name,
// owned by owner. The service control manager runs one instance of the service, so none
// of them is in use.
func removeStaleStateDirs(base string, owner *windows.SID, logf func(string, ...any)) {
	ents, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !stateDirName.MatchString(e.Name()) {
			continue
		}
		dir := filepath.Join(base, e.Name())
		fi, err := os.Lstat(dir)
		if err != nil || !fi.IsDir() || fi.Mode()&os.ModeIrregular != 0 || fi.Mode()&os.ModeSymlink != 0 {
			continue
		}
		sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
		if err != nil {
			continue
		}
		if o, _, err := sd.Owner(); err != nil || !o.Equals(owner) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			logf("remove the stale state directory %s: %v", dir, err)
		}
	}
}

// runAll runs fns last to first.
func runAll(fns []func()) {
	for i := len(fns) - 1; i >= 0; i-- {
		fns[i]()
	}
}
