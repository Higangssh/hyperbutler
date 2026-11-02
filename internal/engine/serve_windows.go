//go:build windows

package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio/pkg/guid"

	"github.com/Higangssh/hyperbutler/internal/dockerenv"
	"github.com/Higangssh/hyperbutler/internal/netstack"
	"github.com/Higangssh/hyperbutler/internal/transport"
)

// ServeOptions configure the background engine process.
type ServeOptions struct {
	Home     string
	Settings *Settings
	// KernelPath and InitrdPath are the guest artifacts; Image, if set, is an image
	// manifest that replaces them (see Config.Image).
	KernelPath, InitrdPath string
	Image                  string
	// Broker, if set, is the HCS access broker's pipe: it boots the VM (Config.Broker).
	Broker string
	// Pipe is where the Docker API is served (transport.DefaultPipeName if empty).
	Pipe    string
	Net     NetConfig
	Logger  *slog.Logger
	Backend Backend
	// Executable and Version are this engine's hyperbutler.exe and its version, recorded
	// in state.json (REQ-PLAT-008).
	Executable, Version string
}

// consoleLogMax bounds each console log file (there are two: current and previous).
const consoleLogMax = 8 << 20

// Serve is the background engine: `hyperbutler start` spawns a process that calls it. It
//
//  1. takes the single-instance lock (CodeAlreadyRunning if another engine holds it);
//  2. publishes State in state.json at every phase;
//  3. terminates compute systems a crashed engine of this installation left behind;
//  4. runs the VM until the stop event is set (RequestStop) or the process is told to
//     exit, then shuts it down.
//
// It returns the error that ended the run; State has it too, with its code.
func Serve(opts ServeOptions) error {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	if opts.Backend == nil {
		opts.Backend = HCSBackend{}
	}
	if opts.Pipe == "" {
		opts.Pipe = transport.DefaultPipeName
	}
	if err := opts.Settings.Validate(); err != nil {
		return err
	}
	l := Layout{Home: opts.Home, Data: opts.Settings.DataDir}
	if err := MkdirPrivate(l.Home); err != nil {
		return err
	}
	lock, err := Lock(l.LockPath(), 2*time.Second)
	if errors.Is(err, ErrLocked) {
		// Another engine runs: its state is not ours to touch.
		return codeErr(CodeAlreadyRunning, errors.New("another hyperbutler engine is already running"))
	}
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()

	imageDir := opts.Image
	if imageDir == "" {
		imageDir = opts.KernelPath
	}
	if imageDir != "" {
		if abs, err := filepath.Abs(imageDir); err == nil {
			imageDir = filepath.Dir(abs)
		}
	}
	pub := &publisher{l: l, log: log, st: State{
		Phase: PhaseStarting, PID: os.Getpid(), PIDCreated: SelfCreated(),
		Pipe: opts.Pipe, DataDir: l.Data, Started: time.Now().UTC().Truncate(time.Millisecond),
		ImageDir: imageDir, Executable: opts.Executable, ExecutableVersion: opts.Version,
	}}
	// The VMs of earlier runs, whose access entries uninstall may still find.
	if prev, err := LoadState(l); err == nil {
		pub.st.PastRuntimeIDs = prev.PastRuntimeIDs
		pub.st.RecordRuntimeID(prev.RuntimeID) // a state.json from before the history
	}
	if err := pub.save(); err != nil {
		return err
	}
	fail := func(err error) error {
		pub.update(func(s *State) {
			s.Phase, s.Error, s.ErrorCode = PhaseFailed, err.Error(), ErrorCode(err)
		})
		return err
	}

	ev, err := CreateStopEvent()
	if err != nil {
		return fail(err)
	}
	defer ev.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	done := make(chan struct{})
	var doneOnce sync.Once
	stopApply := func() { doneOnce.Do(func() { close(done) }) }
	defer stopApply()
	go func() {
		if ev.Wait(done) {
			log.Info("stop requested")
			pub.update(func(s *State) { s.Phase = PhaseStopping })
			cancel()
		}
	}()
	// Sign-out and Windows shutdown: the engine has no console, so it learns of them
	// through a hidden window, which then holds the session end until the guest is down.
	finished := make(chan struct{})
	var stopOnce sync.Once
	unwatch, err := WatchSessionEnd(func() {
		stopOnce.Do(func() {
			log.Info("the Windows session is ending; stopping")
			go pub.update(func(s *State) { s.Phase = PhaseStopping })
			cancel()
		})
	}, finished)
	if err != nil {
		log.Warn("cannot watch for sign-out and shutdown; they will kill the VM uncleanly", "err", err)
	} else {
		defer unwatch() // after finished is closed (defers run in reverse)
	}
	defer close(finished)

	if opts.Broker == "" {
		CleanupOrphans(opts.Backend, l.VMPrefix(), log)
	} // the broker sweeps its own VMs

	// daemon.json (REQ-UX-007): delivered on every control connection, and again on
	// `hyperbutler config daemon apply`, which sets the daemon event.
	subnet := opts.Net.Subnet
	if !subnet.IsValid() {
		subnet = netstack.DefaultSubnet
	}
	dc := &DaemonConfig{Layout: l, GuestSubnet: subnet, Log: log, OnState: func(d DaemonState) {
		pub.update(func(s *State) { s.Daemon = &d })
	}}
	// dockerd's environment (REQ-UX-009): proxies and CA certificates, built from
	// settings.json as it is at boot and at each apply.
	envNet := EnvNet{Subnet: subnet}
	if opts.Net.HostGateway != nil {
		envNet.HostGW = netstack.Config{Subnet: subnet}.HostGatewayAddr()
	}
	dc.Env = func() (*dockerenv.Doc, EnvState) {
		s, err := LoadSettings(l)
		if err != nil {
			s = opts.Settings
			log.Warn("dockerd environment: settings.json unreadable; using the settings of the start", "err", err)
		}
		dj, _ := ReadDaemonJSON(l, subnet) // only for NO_PROXY; an invalid file adds nothing
		t0 := time.Now()
		doc, st := BuildDockerdEnv(s, envNet, dj, l.CAExtraPath())
		log.Info("dockerd environment built", "caImport", st.CAImport, "caCerts", st.CACerts, "caSkipped", st.CASkipped,
			"caStats", fmt.Sprintf("%+v", st.CAStats), "took", time.Since(t0).Round(time.Millisecond))
		return doc, st
	}
	dev, err := CreateDaemonEvent()
	if err != nil {
		return fail(err)
	}
	var applyWG sync.WaitGroup
	applyWG.Add(1)
	go func() {
		defer applyWG.Done()
		for dev.Wait(done) {
			log.Info("daemon.json apply requested")
			actx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			if err := dc.Apply(actx); err != nil {
				log.Warn("daemon.json not applied", "err", err)
			}
			cancel()
		}
	}()
	defer func() {
		// done is closed by an earlier defer's turn only at return, so stop the loop
		// here first: it must not wait on a closed handle.
		stopApply()
		applyWG.Wait()
		dev.Close()
	}()

	// Live settings (hostGateway.*): applied again on `hyperbutler config set`, which
	// sets the settings event.
	live := &liveSettings{l: l, log: log, publish: func(h *HostGatewayState) {
		pub.update(func(s *State) { s.HostGateway = h })
	}}
	sev, err := CreateSettingsEvent()
	if err != nil {
		return fail(err)
	}
	var liveWG sync.WaitGroup
	liveWG.Add(1)
	go func() {
		defer liveWG.Done()
		for sev.Wait(done) {
			log.Info("settings reload requested")
			live.reload()
		}
	}()
	defer func() {
		stopApply()
		liveWG.Wait()
		sev.Close()
	}()

	console, err := openCapped(l.ConsoleLogPath(), consoleLogMax)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = console.Close() }()

	netCfg := opts.Net
	netCfg.OnUnpublished = func(u []UnpublishedPort) {
		pub.update(func(s *State) { s.UnpublishedPorts = u })
	}
	var predicted string // the runtime id OnGrant recorded
	runErr := Run(ctx, Config{
		KernelPath: opts.KernelPath, InitrdPath: opts.InitrdPath, Image: opts.Image, Broker: opts.Broker,
		MemoryMB: opts.Settings.Memory(), CPUs: opts.Settings.CPUCount(runtime.NumCPU()),
		Pipe: opts.Pipe, VMID: l.VMPrefix() + randomHex(6),
		Disk: l, DiskSize: opts.Settings.DiskSize,
		Net: netCfg, ConsoleLog: console, Logger: log, Backend: opts.Backend,
		DaemonConfig: dc,
		OnGrant: func(_ string, rid guid.GUID) {
			predicted = strings.ToLower(rid.String())
			pub.update(func(s *State) { s.RecordRuntimeID(predicted) })
		},
		OnCreated: func(id string, rid guid.GUID) {
			actual := strings.ToLower(rid.String())
			if predicted != "" && predicted != actual {
				log.Warn("the VM's runtime id differs from the one its access was granted for", "predicted", predicted, "actual", actual)
			}
			pub.update(func(s *State) {
				s.VMID, s.RuntimeID = id, actual
				s.RecordRuntimeID(actual)
			})
		},
		OnReady: func(_ context.Context, r *Running) error {
			if r.Stack != nil {
				live.attach(r.Stack)
			}
			pub.update(func(s *State) {
				if s.Phase == PhaseStarting {
					s.Phase = PhaseRunning
				}
				s.ReadyMillis = time.Since(s.Started).Milliseconds()
			})
			return nil
		},
		OnStopping: func() { pub.update(func(s *State) { s.Phase = PhaseStopping }) },
		OnGuardPause: func(p *GuardPauseState) {
			pub.update(func(s *State) { s.GuardPause = p })
		},
		OnGuestFS: func(g *GuestFSState) {
			pub.update(func(s *State) { s.GuestFS = g })
		},
	})
	live.attach(nil) // the stack is closed
	if runErr != nil {
		log.Error("engine stopped", "err", runErr)
		return fail(runErr)
	}
	pub.update(func(s *State) { s.Phase, s.Error, s.ErrorCode = PhaseStopped, "", "" })
	log.Info("engine stopped")
	return nil
}

// publisher serializes state.json updates.
type publisher struct {
	l   Layout
	log *slog.Logger
	mu  sync.Mutex
	st  State
}

func (p *publisher) save() error {
	s := p.st
	return SaveState(p.l, &s)
}

func (p *publisher) update(fn func(*State)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fn(&p.st)
	if err := p.save(); err != nil {
		p.log.Error("write state", "err", err)
	}
}

// CleanupOrphans terminates the compute systems whose name starts with prefix. Call it
// only while holding the engine lock: then no live engine owns them. Normally there are
// none, because every VM is created with ShouldTerminateOnLastHandleClosed and dies
// with its engine.
func CleanupOrphans(b Backend, prefix string, log *slog.Logger) int {
	list, err := b.List(VMOwner)
	if err != nil {
		log.Warn("list compute systems", "err", err)
		return 0
	}
	n := 0
	for _, s := range list {
		if !strings.HasPrefix(s.ID, prefix) {
			continue
		}
		m, err := b.Open(s.ID)
		if err != nil {
			log.Warn("open orphaned VM", "id", s.ID, "err", err)
			continue
		}
		if err := m.Terminate(); err != nil {
			log.Warn("terminate orphaned VM", "id", s.ID, "err", err)
		} else {
			log.Warn("terminated a VM left behind by an earlier engine", "id", s.ID, "state", s.State)
			n++
		}
		_ = m.Close()
	}
	return n
}

// Describe explains an engine error code to the user, or returns "". pipe is the
// Docker API pipe the engine was configured with (State.Pipe); empty means the default.
func Describe(code, pipe string) string {
	if pipe == "" {
		pipe = transport.DefaultPipeName
	}
	switch code {
	case CodeHCSAccessDenied:
		return strings.TrimSpace(fmt.Sprintf(`
The Host Compute Service (HCS) refused access.

hyperbutler starts its VM through HCS directly, which today needs your account in the
local "Hyper-V Administrators" group. An administrator adds you once with:

    net localgroup "Hyper-V Administrators" %s /add

Then sign out and back in (group changes apply at sign-in). No elevation is needed
after that, and hyperbutler never asks for it.

A later release replaces this with a small HCS access broker service (ADR 0004), so
that standard users need no group membership.`, os.Getenv("USERNAME")))
	case CodeHCSSignInPending:
		return strings.TrimSpace(`
The Host Compute Service (HCS) refused access, but your account IS in the
"Hyper-V Administrators" group already (the installer, or an administrator, added it).

Windows applies group changes when you sign in, so this sign-in does not have it yet.
Sign out of Windows and sign back in (or restart), then run hyperbutler start again.
No elevation is needed after that.`)
	case CodePipeInUse:
		return describePipeInUse(pipe)
	case CodeDataDisk:
		return "The data disk could not be used. `hyperbutler delete` removes it and starts over (all images, containers and volumes are lost)."
	case CodeVMExited:
		return "The VM stopped on its own. `hyperbutler logs --console` shows why."
	}
	return ""
}

// describePipeInUse is Describe(CodePipeInUse) for pipe (REQ-UX-020, proposed). Pipe
// names are machine-wide, so the likely holder is another hyperbutler engine: another
// Windows user's, or another installation's of this user. Both make the listen fail
// with "Access is denied" (measured for the second; the pipe's DACL admits only its
// owner, so the first fails the same way).
func describePipeInUse(pipe string) string {
	return `Another program already serves the Docker API pipe ` + pipe + `.
Pipe names are shared by everyone on this PC, so another Windows user's hyperbutler
engine, or another hyperbutler installation of yours, may hold it (Windows says
"Access is denied" for both). Stop that engine, or give this installation a pipe of
its own and start again:

    hyperbutler config set pipe \\.\pipe\docker_engine_<name>

To see the Docker pipes: Get-ChildItem \\.\pipe\ | Where-Object Name -like 'docker*'`
}
