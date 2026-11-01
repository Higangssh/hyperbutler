//go:build windows

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/guest/image"
	"github.com/Higangssh/hyperbutler/internal/config"
	"github.com/Higangssh/hyperbutler/internal/diag"
	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/engine/dockerctx"
	"github.com/Higangssh/hyperbutler/internal/vm"
)

const (
	contextName = "hyperbutler"
	kernelName  = "vmlinuz-virt"
	initrdName  = "initramfs-hyperbutler.gz"

	// engineLogMax is when engine.log is rotated, at the next start.
	engineLogMax = 4 << 20
)

const (
	// contextDescription is what `docker context ls` shows for hyperbutler's context
	// (REQ-UX-014).
	contextDescription = "hyperbutler engine; run `hyperbutler start` if docker cannot connect"
	// desktopContext is Docker Desktop's context, named in the stop note when it exists.
	desktopContext = "desktop-linux"
)

// install is one hyperbutler installation: its layout and settings (nil before the first
// start).
type install struct {
	l        engine.Layout
	settings *engine.Settings
	// lenient: an invalid settings.json is taken as none, and settingsErr says why
	// (uninstall only, which must work without it).
	lenient     bool
	settingsErr error
}

func openInstall() (*install, error) { return loadInstall(false) }

// openInstallLenient is openInstall for uninstall: an invalid settings.json is treated
// like a missing one, and the data directory is the last engine's (state.json).
func openInstallLenient() (*install, error) { return loadInstall(true) }

func loadInstall(lenient bool) (*install, error) {
	home, err := engine.DefaultHome()
	if err != nil {
		return nil, err
	}
	in := &install{l: engine.Layout{Home: home, Data: home}, lenient: lenient}
	s, err := engine.LoadSettings(in.l)
	switch {
	case err == nil:
		in.settings = s
		in.l.Data = s.DataDir
	case errors.Is(err, os.ErrNotExist):
	case lenient:
		in.settingsErr = err
		if st, serr := engine.LoadState(in.l); serr == nil && st.DataDir != "" {
			in.l.Data = st.DataDir
		}
	default:
		return nil, fmt.Errorf("%w (fix or delete %s)", err, in.l.SettingsPath())
	}
	return in, nil
}

// reload reads the settings again (after taking the ops lock).
func (in *install) reload() error {
	fresh, err := loadInstall(in.lenient)
	if err != nil {
		return err
	}
	*in = *fresh
	return nil
}

// createDataDir creates a data directory outside the state directory that does not
// exist yet, and records in settings.json that hyperbutler made it
// (Settings.CreatedDataDir): uninstall removes that one, never a directory the user
// named that was already there.
func (in *install) createDataDir() error {
	d := in.settings.DataDir
	if engine.SamePath(d, in.l.Home) {
		return nil
	}
	if _, err := os.Lstat(d); err == nil || !errors.Is(err, os.ErrNotExist) {
		return nil // there already (or not ours to judge): the engine reports a bad one
	}
	if err := engine.MkdirPrivate(d); err != nil {
		return fmt.Errorf("create the data directory: %w", err)
	}
	s := *in.settings
	s.CreatedDataDir = d
	if err := engine.SaveSettings(in.l, &s); err != nil {
		return err
	}
	in.settings = &s
	return nil
}

// opsLock serializes start, stop and delete.
func (in *install) opsLock() (*engine.FileLock, error) {
	if err := engine.MkdirPrivate(in.l.Home); err != nil {
		return nil, err
	}
	l, err := engine.Lock(in.l.OpsLockPath(), 60*time.Second)
	if errors.Is(err, engine.ErrLocked) {
		return nil, errors.New("another hyperbutler start, stop or delete is still running")
	}
	return l, err
}

func (in *install) running() (bool, error) { return engine.Held(in.l.LockPath()) }

// engineState waits briefly for state.json to describe the process holding the lock:
// between taking the lock and writing its state, an engine leaves the previous one.
func (in *install) liveState() (*engine.State, *engine.Process, error) {
	deadline := time.Now().Add(3 * time.Second)
	for {
		st, err := engine.LoadState(in.l)
		if err == nil && st.Phase.Live() {
			p, perr := engine.OpenProcess(st.PID, st.PIDCreated)
			if perr == nil {
				return st, p, nil
			}
			err = perr
		} else if err == nil {
			err = fmt.Errorf("state.json says %s", st.Phase)
		}
		if time.Now().After(deadline) {
			return nil, nil, fmt.Errorf("hyperbutler is running but its engine process cannot be identified: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// --- start ----------------------------------------------------------------------------

func cmdStart(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("start", stderr)
	dataDir := fs.String("data", "", "where the data disk lives; chosen on the first start (default: the state directory)")
	imageDir := fs.String("image-dir", "", "directory with "+kernelName+" and "+initrdName+" (default: next to hyperbutler.exe)")
	memory := fs.String("memory", "", "guest memory, such as 4GiB or 4096 (MiB); kept for later starts")
	cpus := fs.Int("cpus", 0, "virtual CPUs; kept for later starts")
	var diskSize sizeFlag
	fs.Var(&diskSize, "disk-size", "data disk size, such as 64GiB; first start only (the disk is sparse)")
	timeout := fs.Duration("timeout", 2*time.Minute, "how long to wait for the Docker API")
	noContext := fs.Bool("no-context", false, `do not create or select the "hyperbutler" docker context (this time; config set context.manage false for good)`)
	pipe := fs.String("pipe", "", `Docker API pipe (default \\.\pipe\docker_engine_hyperbutler); kept for later starts`)
	var dcfg diag.Config
	fs.StringVar(&dcfg.MemoryLimit, "memlimit", "", "Go soft memory limit of the engine process, GOMEMLIMIT syntax (default "+engine.DefaultEngineMemoryLimit+`, "off" for none); kept for later starts. The GOMEMLIMIT variable wins`)
	fs.StringVar(&dcfg.DebugPipe, "debug-pipe", "", `serve the engine's pprof and runtime stats on this pipe, current user only, e.g. \\.\pipe\hyperbutler-engine-debug (default: $`+debugPipeEnv+`; off when both are empty)`)
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if dcfg.DebugPipe == "" {
		dcfg.DebugPipe = os.Getenv(debugPipeEnv)
	}
	if err := checkDiagFlags(dcfg); err != nil {
		return err
	}
	if *cpus < 0 || *cpus > engine.MaxCPUs {
		return fmt.Errorf("--cpus %d is outside 1..%d", *cpus, engine.MaxCPUs)
	}
	t0 := time.Now()
	in, err := openInstall()
	if err != nil {
		return err
	}
	ops, err := in.opsLock()
	if err != nil {
		return err
	}
	defer func() { _ = ops.Unlock() }()
	// Settings read before the lock may be stale: another start, or a delete, may
	// have changed them while this command waited.
	if err := in.reload(); err != nil {
		return err
	}

	if err := in.applySettings(*dataDir, *memory, *cpus, diskSize, *pipe, dcfg.MemoryLimit); err != nil {
		return err
	}
	memLimitSet := dcfg.MemoryLimit != ""
	dcfg.MemoryLimit = in.settings.EngineMemoryLimit()
	kernel, initrd, manifest, err := guestArtifacts(*imageDir)
	if err != nil {
		return err
	}

	if up, err := in.running(); err != nil {
		return err
	} else if up {
		st, p, err := in.liveState()
		if err != nil {
			return err
		}
		p.Close()
		_, _ = fmt.Fprintf(stdout, "hyperbutler is already running (engine pid %d, %s).\n", st.PID, st.Phase)
		if exe, err := os.Executable(); err == nil {
			if s := otherEngine(st, exe, buildVersion()); s != "" {
				_, _ = fmt.Fprintln(stdout, s)
			}
		}
		if dcfg.DebugPipe != "" || memLimitSet {
			_, _ = fmt.Fprintln(stderr, "hyperbutler: --debug-pipe and --memlimit apply at the next start; the running engine is unchanged")
		}
		if !*noContext && in.settings.ContextManaged() {
			in.selectContext(st.Pipe, stdout, stderr)
		}
		in.startNotes(st, stdout, stderr)
		return nil
	}
	firstStart := !diskExists(in.l)
	in.noteCrash(stderr)
	if err := in.createDataDir(); err != nil {
		return err
	}

	cmd, err := spawnEngine(in, kernel, initrd, manifest, dcfg)
	if err != nil {
		return err
	}
	// cmd.Process holds a handle, so the PID cannot be reused while this runs.
	created, err := engine.CreatedTime(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	st, err := in.waitReady(cmd.Process, created, exited, *timeout)
	if err != nil {
		return in.explainFailure(err, st, stderr)
	}
	_, _ = fmt.Fprintf(stdout, "hyperbutler is running: the Docker API answered after %.1f s (engine pid %d).\n", time.Since(t0).Seconds(), st.PID)
	if st.Daemon != nil && st.Daemon.Error != "" {
		_, _ = fmt.Fprintf(stderr, "hyperbutler: daemon.json: %s\n", printable([]byte(st.Daemon.Error)))
	}
	if !*noContext && in.settings.ContextManaged() {
		in.selectContext(st.Pipe, stdout, stderr)
	}
	checkDockerCLI(findDockerCLI(), stdout, stderr)
	in.startNotes(st, stdout, stderr)
	if firstStart && !in.settings.Autostart {
		_, _ = fmt.Fprintln(stdout, autostartTip)
	}
	return nil
}

// otherEngine says so when the running engine st is another hyperbutler.exe or another
// version than this one, exe at version (REQ-PLAT-008), and points to restart. It is
// empty when they are the same, or the engine is from a release that did not record it.
func otherEngine(st *engine.State, exe, version string) string {
	if st.Executable == "" || sameExecutable(st.Executable, exe) && st.ExecutableVersion == version {
		return ""
	}
	running := st.ExecutableVersion
	if running == "" {
		running = "(unknown version)"
	}
	return fmt.Sprintf("hyperbutler %s from %s is running; this is %s from %s.\n"+
		"Run `hyperbutler restart` from this folder to switch to this one.",
		printable([]byte(running)), printable([]byte(filepath.Dir(st.Executable))), version, filepath.Dir(exe))
}

// sameExecutable reports whether paths a and b name one file. When both exist, the
// file system decides (os.SameFile), so two spellings of one file (an 8.3 short name,
// a relative path, a junction or a mapped drive) match; otherwise the cleaned paths
// are compared without case (engine.SamePath).
func sameExecutable(a, b string) bool {
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	if errA == nil && errB == nil {
		return os.SameFile(fa, fb)
	}
	return engine.SamePath(a, b)
}

// applySettings creates settings.json on the first start and records start's flags,
// through the same keys and validators as `hyperbutler config set`. The data location
// and the disk size change only while there is no data disk (moving it is REQ-DISK-007's
// `hyperbutler move`, not implemented yet).
func (in *install) applySettings(dataDir, memory string, cpus int, diskSize sizeFlag, pipe, memLimit string) error {
	env := in.env()
	s := in.settings
	if s == nil {
		s = config.NewSettings(env)
	}
	set := func(flag, key, v string) error {
		k, err := config.Lookup(key)
		if err != nil {
			return err
		}
		n, err := k.Set(s, v, env)
		if err != nil {
			return fmt.Errorf("--%s: %w", flag, err)
		}
		s = n
		return nil
	}
	for _, f := range []struct{ flag, key, v string }{
		{"data", "data.dir", dataDir},
		{"memory", "memory", memory},
		{"pipe", "pipe", pipe},
		{"memlimit", "engine.memoryLimit", memLimit},
	} {
		if f.v == "" {
			continue
		}
		if err := set(f.flag, f.key, f.v); err != nil {
			return err
		}
	}
	if diskSize.set {
		if err := set("disk-size", "disk.size", engine.FormatSize(diskSize.n)); err != nil {
			return err
		}
	}
	if cpus > 0 {
		if err := set("cpus", "cpus", fmt.Sprint(cpus)); err != nil {
			return err
		}
	}
	if in.settings != nil && config.Equal(s, in.settings) {
		return nil
	}
	if err := engine.SaveSettings(in.l, s); err != nil {
		return err
	}
	in.settings, in.l.Data = s, s.DataDir
	return nil
}

// guestArtifacts finds the guest image in dir. A directory with an image.json (ADR
// 0005: kernel, stage-1 initramfs and system disk) is used through that manifest, which
// the engine verifies; one without it is an initramfs-only image.
func guestArtifacts(dir string) (kernel, initrd, manifest string, err error) {
	if dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", "", "", err
		}
		dir = filepath.Dir(exe)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return "", "", "", err
	}
	if m := filepath.Join(dir, image.ManifestName); isRegular(m) {
		return "", "", m, nil
	}
	kernel, initrd = filepath.Join(dir, kernelName), filepath.Join(dir, initrdName)
	for _, p := range []string{kernel, initrd} {
		if !isRegular(p) {
			return "", "", "", fmt.Errorf("guest image file %s not found; pass --image-dir with %s, or with %s and %s (built by guest/prototype/build.ps1)", p, image.ManifestName, kernelName, initrdName)
		}
	}
	return kernel, initrd, "", nil
}

func isRegular(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// noteCrash reports and clears a state left by an engine that died without cleaning
// up, and terminates any VM it left (normally none: the VM dies with its engine).
func (in *install) noteCrash(stderr io.Writer) {
	st, err := engine.LoadState(in.l)
	if err != nil || !st.Phase.Live() {
		return
	}
	_, _ = fmt.Fprintf(stderr, "hyperbutler: the previous engine (pid %d) exited without shutting down; see `hyperbutler logs`.\n", st.PID)
	if lk, err := engine.TryLock(in.l.LockPath()); err == nil {
		engine.CleanupOrphans(engine.HCSBackend{}, in.l.VMPrefix(), slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
		// The dead engine could not revoke its VM account's access to the disk.
		if st.VMID != "" {
			_ = vm.RevokeVMAccess(st.VMID, in.l.DiskPath())
		}
		st.Phase, st.Error, st.ErrorCode = engine.PhaseFailed, "the engine exited without shutting down", ""
		_ = engine.SaveState(in.l, st)
		_ = lk.Unlock()
	}
}

// spawnEngine starts `hyperbutler engine` as a detached background process whose output
// goes to engine.log. It breaks away from the caller's job object where allowed, so
// closing the terminal (or a CI step) does not take the engine with it.
func spawnEngine(in *install, kernel, initrd, manifest string, dcfg diag.Config) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(in.l.LogDir(), 0o700); err != nil {
		return nil, err
	}
	logPath := in.l.EngineLogPath()
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > engineLogMax {
		_ = os.Rename(logPath, logPath+".1")
	}
	logf, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // G304: the engine log in the user's own data layout
	if err != nil {
		return nil, err
	}
	defer func() { _ = logf.Close() }()
	const (
		detachedProcess        = 0x00000008
		createNewProcessGroup  = 0x00000200
		createBreakawayFromJob = 0x01000000
	)
	for _, breakaway := range []bool{true, false} {
		cmd := exec.Command(exe, engineArgs(in.l.Home, kernel, initrd, manifest, dcfg)...) //nolint:gosec // G204: exe is this program (os.Executable); the arguments are built by engineArgs
		cmd.Dir = in.l.Home
		cmd.Stdout, cmd.Stderr = logf, logf
		flags := uint32(detachedProcess | createNewProcessGroup)
		if breakaway {
			flags |= createBreakawayFromJob
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
		err = cmd.Start()
		if err == nil {
			return cmd, nil
		}
		// A job that does not allow breakaway refuses the flag with access denied.
		if !breakaway || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			break
		}
	}
	return nil, fmt.Errorf("start the engine: %w", err)
}

// waitReady follows state.json until the engine process proc (created at created) is
// running or failed, or the process exits. A state.json of an earlier engine whose PID
// was reused does not match: the creation time differs. On timeout it stops the engine,
// so nothing is left half up.
func (in *install) waitReady(proc *os.Process, created int64, exited <-chan error, timeout time.Duration) (*engine.State, error) {
	deadline := time.Now().Add(timeout)
	pid := proc.Pid
	ours := func(st *engine.State) bool { return st.PID == pid && st.PIDCreated == created }
	var last *engine.State
	for {
		st, err := engine.LoadState(in.l)
		if err == nil && ours(st) {
			last = st
			switch st.Phase {
			case engine.PhaseRunning:
				return st, nil
			case engine.PhaseFailed, engine.PhaseStopped:
				return st, engineError(st)
			}
		}
		select {
		case err := <-exited:
			if st, serr := engine.LoadState(in.l); serr == nil && ours(st) {
				if st.Phase == engine.PhaseFailed {
					return st, engineError(st)
				}
				last = st
			}
			if err == nil {
				err = errors.New("exit status 0")
			}
			return last, fmt.Errorf("the engine exited before it was ready (%w); see `hyperbutler logs`", err)
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			stopped := false
			if last != nil {
				if p, err := engine.OpenProcess(last.PID, last.PIDCreated); err == nil {
					_ = engine.RequestStop(last.PID, last.PIDCreated)
					if ok, _ := p.Wait(30 * time.Second); !ok {
						_ = p.Kill()
					}
					p.Close()
					stopped = true
				}
			}
			if !stopped { // it never published a state: kill it through our own handle
				_ = proc.Kill()
			}
			return last, fmt.Errorf("the Docker API did not answer within %v; the engine was stopped. See `hyperbutler logs` and `hyperbutler logs --console`", timeout)
		}
	}
}

type engineErr struct {
	code string
	msg  string
}

func (e *engineErr) Error() string { return e.msg }

func engineError(st *engine.State) error {
	if st.Phase == engine.PhaseStopped {
		return errors.New("the engine stopped before it was ready")
	}
	return &engineErr{code: st.ErrorCode, msg: st.Error}
}

// pipeOf is the Docker API pipe the failed engine used: state.json's, else the
// configured one.
func (in *install) pipeOf(st *engine.State) string {
	if st != nil && st.Pipe != "" {
		return st.Pipe
	}
	if in.settings != nil {
		return in.settings.PipeName()
	}
	return ""
}

// explainFailure prints err and, for known causes, what to do about it.
func (in *install) explainFailure(err error, st *engine.State, stderr io.Writer) error {
	_, _ = fmt.Fprintln(stderr, "hyperbutler: start failed:", err)
	var ee *engineErr
	if errors.As(err, &ee) {
		if d := engine.Describe(ee.code, in.pipeOf(st)); d != "" {
			_, _ = fmt.Fprintln(stderr)
			_, _ = fmt.Fprintln(stderr, d)
		}
		if ee.code == engine.CodeHCSAccessDenied {
			_, _ = fmt.Fprintln(stderr)
			if member, merr := inHyperVAdmins(); merr == nil && member {
				_, _ = fmt.Fprintln(stderr, "Your sign-in token IS a member of Hyper-V Administrators, so the denial has another cause; see `hyperbutler logs`.")
			} else if merr == nil {
				_, _ = fmt.Fprintln(stderr, "Your current sign-in token is NOT a member of Hyper-V Administrators.")
			}
		}
	}
	_, _ = fmt.Fprintf(stderr, "Logs: %s\n", in.l.EngineLogPath())
	return &shownError{err: err}
}

// inHyperVAdmins reports whether this process's token has BUILTIN\Hyper-V
// Administrators (S-1-5-32-578) enabled.
func inHyperVAdmins() (bool, error) {
	sid, err := windows.StringToSid("S-1-5-32-578")
	if err != nil {
		return false, err
	}
	return windows.Token(0).IsMember(sid)
}

// selectContext creates (or refreshes) and selects the "hyperbutler" docker context. A
// context of that name that hyperbutler did not create is left alone.
func selectContext(pipe string, stdout, stderr io.Writer) {
	store, err := dockerctx.DefaultStore()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hyperbutler: docker context:", err)
		return
	}
	host := npipeHost(pipe)
	if err := store.Ensure(contextName, host, contextDescription); err != nil {
		if errors.Is(err, dockerctx.ErrNotOurs) {
			_, _ = fmt.Fprintf(stderr, "hyperbutler: a docker context named %q already exists and was not created by hyperbutler; it was left alone.\n"+
				"        Use DOCKER_HOST=%s, or remove that context (docker context rm %s) and run hyperbutler start again.\n", contextName, host, contextName)
			return
		}
		_, _ = fmt.Fprintln(stderr, "hyperbutler: docker context:", err)
		return
	}
	prev, err := store.Current()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hyperbutler: docker context %q is registered but not selected: %v\n", contextName, err)
		return
	}
	if prev != contextName {
		if err := store.Use(contextName); err != nil {
			_, _ = fmt.Fprintf(stderr, "hyperbutler: docker context %q is registered but not selected: %v\n", contextName, err)
			return
		}
		was := prev
		if was == "" {
			was = "default"
		}
		_, _ = fmt.Fprintf(stdout, "Docker context %q selected (was %q).\n", contextName, was)
	} else {
		_, _ = fmt.Fprintf(stdout, "Docker context %q is selected.\n", contextName)
	}
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		_, _ = fmt.Fprintf(stderr, "hyperbutler: note: DOCKER_HOST=%s overrides the docker context in this shell.\n", h)
	} else if c := os.Getenv("DOCKER_CONTEXT"); c != "" && c != contextName {
		_, _ = fmt.Fprintf(stderr, "hyperbutler: note: DOCKER_CONTEXT=%s overrides the selected context in this shell.\n", c)
	}
	syncCompose(store, stdout, stderr)
}

// --- stop -----------------------------------------------------------------------------

func cmdStop(args []string, stdout, stderr io.Writer) error {
	return stopCommand(args, stdout, stderr, "")
}

// stopCommand is `hyperbutler stop`, and the stop of a restart when restarting names it
// ("hyperbutler restart"): then the containers come back after the restart, and there
// is no note about docker commands failing until the next start.
func stopCommand(args []string, stdout, stderr io.Writer, restarting string) error {
	fs := newFlagSet("stop", stderr)
	timeout := fs.Duration("timeout", 45*time.Second, "how long to wait for the orderly shutdown before killing the engine")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	in, err := openInstall()
	if err != nil {
		return err
	}
	ops, err := in.opsLock()
	if err != nil {
		return err
	}
	defer func() { _ = ops.Unlock() }()
	// Settings read before the lock may be stale: another start, or a delete, may
	// have changed them while this command waited.
	if err := in.reload(); err != nil {
		return err
	}
	if up, _ := in.running(); up {
		if st, err := engine.LoadState(in.l); err == nil && st.Phase.Live() {
			if restarting == "" {
				noteContainers(st, stdout, "hyperbutler stop", "at the next start")
			} else {
				noteContainers(st, stdout, restarting, "after the restart")
			}
		}
	}
	forced, took, err := in.stop(*timeout, stderr)
	if errors.Is(err, errNotRunning) {
		_, _ = fmt.Fprintln(stdout, "hyperbutler is not running.")
		return nil
	}
	if err != nil {
		return err
	}
	how := "shut down"
	if forced {
		how = "killed after the orderly shutdown timed out"
	}
	_, _ = fmt.Fprintf(stdout, "hyperbutler stopped: %s in %.1f s.\n", how, took.Seconds())
	if restarting == "" {
		stoppedNote(stdout)
	}
	return nil
}

// stoppedNote says, after a stop, that docker commands fail until the next start and
// how to use another engine meanwhile (REQ-UX-014): the docker CLI's own error names
// only the pipe.
func stoppedNote(stdout io.Writer) {
	selected, desktop := false, false
	if store, err := dockerctx.DefaultStore(); err == nil {
		cur, _ := store.Current()
		selected = cur == contextName
		_, derr := store.Get(desktopContext)
		desktop = derr == nil
	}
	if !selected {
		_, _ = fmt.Fprintf(stdout, "docker commands that use the %q context fail until `hyperbutler start`.\n", contextName)
		return
	}
	other := "docker context use default"
	if desktop {
		other = "docker context use " + desktopContext + " (Docker Desktop), or " + other
	}
	_, _ = fmt.Fprintf(stdout, "docker commands fail until `hyperbutler start`: the %q docker context is still selected.\n"+
		"To use another engine meanwhile: %s.\n", contextName, other)
}

var errNotRunning = errors.New("not running")

// stop asks the engine to shut down, waits up to timeout, then kills it. Either way the
// VM is gone afterwards: the engine's death closes the VM's last handle. Leftover
// compute systems of this installation are terminated too.
func (in *install) stop(timeout time.Duration, stderr io.Writer) (forced bool, took time.Duration, err error) {
	t0 := time.Now()
	up, err := in.running()
	if err != nil {
		return false, 0, err
	}
	if !up {
		in.noteCrash(stderr)
		return false, 0, errNotRunning
	}
	st, p, err := in.liveState()
	if err != nil {
		return false, 0, err
	}
	defer p.Close()
	if err := engine.RequestStop(st.PID, st.PIDCreated); err != nil {
		_, _ = fmt.Fprintln(stderr, "hyperbutler: could not ask the engine to stop; killing it:", err)
		forced = true
	} else if ok, _ := p.Wait(timeout); !ok {
		forced = true
	}
	if forced {
		if err := p.Kill(); err != nil {
			return true, 0, fmt.Errorf("kill engine pid %d: %w", st.PID, err)
		}
		if ok, _ := p.Wait(15 * time.Second); !ok {
			return true, 0, fmt.Errorf("engine pid %d did not exit after being killed", st.PID)
		}
	}
	// The lock goes with the process; take it to clean up under it.
	lk, err := engine.Lock(in.l.LockPath(), 10*time.Second)
	if err != nil {
		return forced, time.Since(t0), fmt.Errorf("the engine exited but its lock is still held: %w", err)
	}
	defer func() { _ = lk.Unlock() }()
	if n := engine.CleanupOrphans(engine.HCSBackend{}, in.l.VMPrefix(), slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))); n > 0 {
		_, _ = fmt.Fprintf(stderr, "hyperbutler: terminated %d VM(s) the engine left behind.\n", n)
	}
	if forced {
		if st, err := engine.LoadState(in.l); err == nil && st.Phase.Live() {
			st.Phase, st.Error = engine.PhaseFailed, "killed by hyperbutler stop after the orderly shutdown timed out"
			_ = engine.SaveState(in.l, st)
		}
	}
	return forced, time.Since(t0), nil
}

// --- status ---------------------------------------------------------------------------

func cmdStatus(args []string, stdout, stderr io.Writer) (int, error) {
	return statusCommand(args, stdout, stderr, true)
}

// statusCommand is `hyperbutler status`. With checkUpdate false it makes no update check
// (no network request): the update field is the last check's record, as the file has it.
// The MCP status tool runs it so (REQ-UX-022: a read-only MCP tool makes no request).
func statusCommand(args []string, stdout, stderr io.Writer, checkUpdate bool) (int, error) {
	fs := newFlagSet("status", stderr)
	asJSON := fs.Bool("json", false, fmt.Sprintf("print JSON (schema %d)", SchemaVersion))
	if err := parseArgs(fs, args); err != nil {
		return 2, err
	}
	in, err := openInstall()
	if err != nil {
		return 1, err
	}
	r := StatusJSON{Schema: SchemaVersion, Home: in.l.Home, SetUp: in.settings != nil, DaemonJSON: in.l.DaemonConfigPath()}
	if _, err := os.Lstat(r.DaemonJSON); err == nil {
		r.DaemonJSONExists = true
	}
	if r.Running, err = in.running(); err != nil {
		return 1, err
	}
	if st, err := engine.LoadState(in.l); err == nil {
		r.State = st
	}
	if in.settings != nil {
		r.DataDir, r.MemoryMB, r.CPUs, r.DiskMax = in.settings.DataDir, in.settings.Memory(), in.settings.CPUCount(runtime.NumCPU()), in.settings.DiskSize
		if u, err := vm.Usage(in.l.DiskPath()); err == nil {
			r.Disk, r.DiskUsed = in.l.DiskPath(), u.Allocated
		}
		if f, err := vm.FreeBytes(in.settings.DataDir); err == nil {
			r.HostFree = f
		}
	}
	if store, err := dockerctx.DefaultStore(); err == nil {
		cur, cerr := store.Current()
		if cerr == nil {
			r.ContextCurrent = cur
			if cur == "" {
				r.ContextCurrent = "default"
			}
		}
		if c, err := store.Get(contextName); err == nil && c.Managed {
			r.Context = contextName
			r.ContextSel = cerr == nil && cur == contextName
		}
	}
	r.GuardPause = guardPauseStatus(r.State, r.Running, r.HostFree)
	if r.Running && r.State != nil && r.State.Phase.Live() && r.State.GuestFS != nil {
		g := r.State.GuestFS
		r.GuestDisk = &GuestDiskJSON{SizeBytes: g.SizeBytes, UsedBytes: g.UsedBytes(), FreeBytes: g.FreeBytes, UsedPercent: g.UsedPercent(), Updated: g.Updated}
	}
	upd := in.lastUpdate()
	if checkUpdate {
		upd = in.checkUpdate(stderr)
	}
	r.Update = updateJSON(upd)
	cli := findDockerCLI()
	r.DockerCLI, r.DockerCLIBundled = cli.Path, cli.Bundled
	code := 0
	if !r.Running {
		code = exitNotRunning
	}
	if *asJSON {
		if err := writeJSON(stdout, r); err != nil {
			return 1, err
		}
		return code, nil
	}
	st := r.State
	switch {
	case r.Running && st != nil && st.Phase.Live():
		_, _ = fmt.Fprintf(stdout, "hyperbutler: %s (engine pid %d, up %s", st.Phase, st.PID, time.Since(st.Started).Round(time.Second))
		if st.ReadyMillis > 0 {
			_, _ = fmt.Fprintf(stdout, ", Docker API ready after %.1f s", float64(st.ReadyMillis)/1000)
		}
		_, _ = fmt.Fprintln(stdout, ")")
		_, _ = fmt.Fprintf(stdout, "  docker:  %s\n", npipeHost(st.Pipe))
		_, _ = fmt.Fprintf(stdout, "  vm:      %s\n", st.VMID)
		writeEngineStatus(stdout, r)
	case r.Running:
		_, _ = fmt.Fprintln(stdout, "hyperbutler: starting")
	case st != nil && st.Phase.Live():
		_, _ = fmt.Fprintf(stdout, "hyperbutler: stopped (the engine, pid %d, exited without shutting down; see `hyperbutler logs`)\n", st.PID)
	case st != nil && st.Phase == engine.PhaseFailed:
		_, _ = fmt.Fprintf(stdout, "hyperbutler: stopped (last run failed: %s)\n", string(printable([]byte(st.Error))))
	case in.settings == nil:
		_, _ = fmt.Fprintln(stdout, "hyperbutler: not set up yet (run hyperbutler start)")
		return code, nil
	default:
		_, _ = fmt.Fprintln(stdout, "hyperbutler: stopped")
	}
	if in.settings != nil {
		_, _ = fmt.Fprintf(stdout, "  guest:   %d MiB, %d CPUs\n", r.MemoryMB, r.CPUs)
		if r.Disk != "" {
			_, _ = fmt.Fprintf(stdout, "  data:    %s (%s on the host, %s maximum; %s free on the volume)\n", r.Disk, engine.HumanSize(r.DiskUsed), engine.FormatSize(r.DiskMax), engine.HumanSize(r.HostFree))
			if r.GuestDisk != nil {
				full := ""
				if r.GuestDisk.UsedPercent > guestFullPercent {
					full = "; nearly full: docker system prune frees space"
				}
				_, _ = fmt.Fprintf(stdout, "           %s%s\n", guestFSLine(r.State.GuestFS), full)
			}
		} else {
			_, _ = fmt.Fprintf(stdout, "  data:    %s (no data disk yet)\n", r.DataDir)
		}
	}
	if r.Context != "" {
		sel := "not selected"
		switch {
		case r.ContextSel:
			sel = "selected"
		case r.ContextCurrent != "":
			// REQ-UX-018: say which one is, so Docker Desktop's containers are not
			// mistaken for hyperbutler's.
			sel = fmt.Sprintf("not selected; docker uses %q", r.ContextCurrent)
		}
		_, _ = fmt.Fprintf(stdout, "  context: %s (%s)\n", r.Context, sel)
	}
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		_, _ = fmt.Fprintf(stdout, "           DOCKER_HOST=%s overrides the context in this shell\n", h)
	} else if c := os.Getenv("DOCKER_CONTEXT"); c != "" && c != contextName {
		_, _ = fmt.Fprintf(stdout, "           DOCKER_CONTEXT=%s overrides the selection in this shell\n", c)
	}
	switch {
	case cli.usesBundled():
		_, _ = fmt.Fprintf(stdout, "  cli:     %s (bundled)\n", cli.Path)
	case cli.Path != "":
		_, _ = fmt.Fprintf(stdout, "  cli:     %s (on PATH)\n", cli.Path)
	case cli.Bundled != "":
		_, _ = fmt.Fprintf(stdout, "  cli:     none on PATH; bundled: %s\n", cli.Bundled)
	}
	_, _ = fmt.Fprintf(stdout, "  daemon:  %s\n", daemonStatusLine(in.l, st))
	if st != nil && st.Phase.Live() && st.Daemon != nil && st.Daemon.Env != nil {
		env := describeEnv(st.Daemon.Env)
		if e := st.Daemon.Env.Error; e != "" {
			env += "; last problem: " + string(printable([]byte(e)))
		}
		_, _ = fmt.Fprintf(stdout, "  proxy:   %s\n", env)
	}
	if st != nil && st.Phase.Live() {
		for _, u := range st.UnpublishedPorts {
			_, _ = fmt.Fprintf(stdout, "  ports:   %d of container %s is not published: %s\n", u.Port, u.Container, printable([]byte(u.Reason)))
		}
	}
	_, _ = fmt.Fprintf(stdout, "  state:   %s\n", in.l.Home)
	noteUpdate(stdout, upd)
	return code, nil
}

// --- logs -----------------------------------------------------------------------------

func cmdLogs(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("logs", stderr)
	console := fs.Bool("console", false, "print the guest serial console instead of the engine log")
	n := fs.Int("n", 100, "print the last N lines (0: all)")
	follow := fs.Bool("f", false, "keep printing new lines")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	in, err := openInstall()
	if err != nil {
		return err
	}
	path := in.l.EngineLogPath()
	if *console {
		path = in.l.ConsoleLogPath()
	}
	f, err := os.Open(path) //nolint:gosec // G304: a log file in the user's own data layout
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no log yet at %s", path)
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// The tail: read at most the last 4 MiB.
	const window = 4 << 20
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	off := max(fi.Size()-window, 0)
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(f, window))
	if err != nil {
		return err
	}
	if _, err := stdout.Write(printable(lastLines(b, *n))); err != nil {
		return err
	}
	if !*follow {
		return nil
	}
	pos := off + int64(len(b))
	buf := make([]byte, 64<<10)
	for {
		time.Sleep(500 * time.Millisecond)
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		if fi.Size() < pos { // rotated or truncated: start over
			_ = f.Close()
			if f, err = os.Open(path); err != nil { //nolint:gosec // G304: a log file in the user's own data layout
				continue
			}
			pos = 0
		}
		for {
			k, err := f.ReadAt(buf, pos)
			if k > 0 {
				if _, werr := stdout.Write(printable(buf[:k])); werr != nil {
					return werr
				}
				pos += int64(k)
			}
			if err != nil || k == 0 {
				break
			}
		}
	}
}

// --- delete ---------------------------------------------------------------------------

func cmdDelete(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("delete", stderr)
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	in, err := openInstall()
	if err != nil {
		return err
	}
	ops, err := in.opsLock()
	if err != nil {
		return err
	}
	defer func() { _ = ops.Unlock() }()
	// Settings read before the lock may be stale: another start, or a delete, may
	// have changed them while this command waited.
	if err := in.reload(); err != nil {
		return err
	}

	up, err := in.running()
	if err != nil {
		return err
	}
	disk := in.l.DiskPath()
	if !*yes {
		size := "no data disk"
		if u, err := vm.Usage(disk); err == nil {
			size = engine.HumanSize(u.Allocated) + " on the host"
		}
		_, _ = fmt.Fprintf(stdout, "This deletes hyperbutler's VM state and its data disk:\n  %s (%s)\n"+
			"All images, containers and volumes in it are lost.", disk, size)
		if up {
			_, _ = fmt.Fprint(stdout, " hyperbutler is running and will be stopped first.")
		}
		_, _ = fmt.Fprint(stdout, "\nType \"yes\" to continue: ")
		if !isConsole(stdin) {
			_, _ = fmt.Fprintln(stdout)
			return errors.New("delete: input is not a terminal; pass --yes to confirm")
		}
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "yes" && a != "y" {
			_, _ = fmt.Fprintln(stdout, "Nothing was deleted.")
			return nil
		}
	}
	if up {
		if _, _, err := in.stop(45*time.Second, stderr); err != nil && !errors.Is(err, errNotRunning) {
			return fmt.Errorf("stop before delete: %w", err)
		}
	}
	// Under the engine lock, so no engine starts while the files go.
	lk, err := engine.Lock(in.l.LockPath(), 10*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = lk.Unlock() }()

	// Only files hyperbutler created, by name: never a recursive delete of a directory the
	// user chose.
	var failed []string
	remove := func(p string) {
		if err := removeRetry(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			failed = append(failed, fmt.Sprintf("%s: %v", p, err))
		}
	}
	remove(disk)
	remove(disk + ".new")
	remove(in.l.DiskMetaPath())
	remove(in.l.StatePath())
	remove(in.l.SettingsPath())
	remove(in.l.DaemonAppliedPath()) // the engine's copy; the user's daemon.json is kept
	for _, p := range []string{in.l.EngineLogPath(), in.l.ConsoleLogPath()} {
		remove(p)
		remove(p + ".1")
	}
	_ = os.Remove(in.l.LogDir()) // only if empty
	if !engine.SamePath(in.l.Data, in.l.Home) {
		_ = os.Remove(in.l.Data) // only if empty
	}
	if store, err := dockerctx.DefaultStore(); err == nil {
		if cur, err := store.Current(); err == nil && cur == contextName {
			if err := store.Use(""); err != nil {
				failed = append(failed, "docker context selection: "+err.Error())
			}
		}
		if err := store.Remove(contextName); err != nil && !errors.Is(err, dockerctx.ErrNotOurs) {
			failed = append(failed, "docker context: "+err.Error())
		}
		if d := clientsDir(); d != "" {
			if err := store.RemovePluginDir(filepath.Join(d, pluginsDirName)); err != nil {
				failed = append(failed, "docker compose plugin registration: "+err.Error())
			}
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("delete: could not remove:\n  %s", strings.Join(failed, "\n  "))
	}
	_, _ = fmt.Fprintln(stdout, "hyperbutler deleted. `hyperbutler start` sets it up again.")
	return nil
}

// removeRetry removes a file, retrying for a few seconds while the VM worker that just
// exited still has it open.
func removeRetry(p string) error {
	var err error
	for i := 0; i < 50; i++ {
		if err = os.Remove(p); err == nil || errors.Is(err, os.ErrNotExist) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

func isConsole(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil
}

// --- engine (internal) ----------------------------------------------------------------

func cmdEngine(args []string, stderr io.Writer) (int, error) {
	fs := newFlagSet("engine", stderr)
	home := fs.String("home", "", "state directory")
	kernel := fs.String("kernel", "", "guest kernel")
	initrd := fs.String("initrd", "", "guest initramfs")
	manifest := fs.String("image", "", "guest image manifest (image.json), instead of --kernel and --initrd")
	debugLog := fs.Bool("debug", false, "debug logging")
	var dcfg diag.Config
	fs.StringVar(&dcfg.MemoryLimit, "memlimit", "", "Go soft memory limit, GOMEMLIMIT syntax (default: settings.json's, else "+engine.DefaultEngineMemoryLimit+"; the GOMEMLIMIT variable wins)")
	fs.StringVar(&dcfg.DebugPipe, "debug-pipe", "", "serve pprof and runtime stats on this pipe, current user only (off by default)")
	if err := parseArgs(fs, args); err != nil {
		return 2, err
	}
	legacy := *kernel != "" || *initrd != ""
	if *home == "" || (*manifest != "") == legacy || legacy && (*kernel == "" || *initrd == "") {
		return 2, errors.New("engine: --home and either --image or --kernel and --initrd are required (it is started by hyperbutler start)")
	}
	level := slog.LevelInfo
	if *debugLog {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
	l := engine.Layout{Home: *home, Data: *home}
	s, err := engine.LoadSettings(l)
	if err != nil {
		return 1, err
	}
	// The debug pipe's DACL names the user's SID, which an elevated token shares with
	// the same user's unelevated processes: refuse it when elevated (as the broker does).
	if dcfg.DebugPipe != "" && windows.GetCurrentProcessToken().IsElevated() {
		return 1, errors.New("--debug-pipe is refused in an elevated engine")
	}
	if dcfg.MemoryLimit == "" {
		dcfg.MemoryLimit = s.EngineMemoryLimit()
	}
	if err := checkDiagFlags(dcfg); err != nil {
		return 2, err
	}
	dbg, err := diag.Start(dcfg)
	if err != nil {
		return 1, err
	}
	defer func() { _ = dbg.Close() }()
	if dcfg.DebugPipe != "" {
		log.Info("debug endpoint", "pipe", dcfg.DebugPipe)
	}
	// REQ-MEM-002: undo an outside scan that makes the untouched commit resident.
	guard := diag.StartWorkingSetGuard(func(rule string, private, commit uint64) {
		log.Info("working set emptied after an unexplained rise (likely an outside memory read)", "rule", rule, "privateWorkingSet", private, "privateBytes", commit)
	})
	defer guard.Stop()
	log.Info("working-set guard started", "partialRule", guard.PartialRule())
	log.Info("go memory limit", "bytes", debug.SetMemoryLimit(-1), "configured", dcfg.MemoryLimit, "env", os.Getenv("GOMEMLIMIT"))
	log.Info("engine starting", "pid", os.Getpid(), "home", *home, "data", s.DataDir, "memoryMB", s.Memory(), "cpus", s.CPUCount(runtime.NumCPU()))
	hg := s.HostGateway() // REQ-NET-004; changed later through the settings event
	// This executable, recorded in state.json; empty if unknown (REQ-PLAT-008).
	exe, err := os.Executable()
	if err != nil || !filepath.IsAbs(exe) || len(exe) > 1024 {
		exe = ""
	}
	err = engine.Serve(engine.ServeOptions{
		Home: *home, Settings: s, KernelPath: *kernel, InitrdPath: *initrd, Image: *manifest,
		Pipe: s.PipeName(), Net: engine.NetConfig{AutoPublish: true, HostGateway: &hg, CRC: frameCheckMode(s)}, Logger: log,
		Executable: exe, Version: buildVersion(),
	})
	if engine.ErrorCode(err) == engine.CodeAlreadyRunning {
		return 3, err
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}
