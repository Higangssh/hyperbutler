//go:build windows

package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio/pkg/guid"

	"github.com/Higangssh/winbox/internal/transport"
)

// ServeOptions configure the background engine process.
type ServeOptions struct {
	Home     string
	Settings *Settings
	// KernelPath and InitrdPath are the guest artifacts; Image, if set, is an image
	// manifest that replaces them (see Config.Image).
	KernelPath, InitrdPath string
	Image                  string
	// Pipe is where the Docker API is served (transport.DefaultPipeName if empty).
	Pipe    string
	Net     NetConfig
	Logger  *slog.Logger
	Backend Backend
}

// consoleLogMax bounds each console log file (there are two: current and previous).
const consoleLogMax = 8 << 20

// Serve is the background engine: `winbox start` spawns a process that calls it. It
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
		return codeErr(CodeAlreadyRunning, errors.New("another winbox engine is already running"))
	}
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()

	pub := &publisher{l: l, log: log, st: State{
		Phase: PhaseStarting, PID: os.Getpid(), PIDCreated: SelfCreated(),
		Pipe: opts.Pipe, DataDir: l.Data, Started: time.Now().UTC().Truncate(time.Millisecond),
	}}
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
	defer close(done)
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

	CleanupOrphans(opts.Backend, l.VMPrefix(), log)

	console, err := openCapped(l.ConsoleLogPath(), consoleLogMax)
	if err != nil {
		return fail(err)
	}
	defer console.Close()

	runErr := Run(ctx, Config{
		KernelPath: opts.KernelPath, InitrdPath: opts.InitrdPath, Image: opts.Image,
		MemoryMB: opts.Settings.MemoryMB, CPUs: opts.Settings.CPUs,
		Pipe: opts.Pipe, VMID: l.VMPrefix() + randomHex(6),
		Disk: l, DiskSize: opts.Settings.DiskSize,
		Net: opts.Net, ConsoleLog: console, Logger: log, Backend: opts.Backend,
		OnCreated: func(id string, rid guid.GUID) {
			pub.update(func(s *State) { s.VMID, s.RuntimeID = id, strings.ToLower(rid.String()) })
		},
		OnReady: func(context.Context, *Running) error {
			pub.update(func(s *State) {
				if s.Phase == PhaseStarting {
					s.Phase = PhaseRunning
				}
				s.ReadyMillis = time.Since(s.Started).Milliseconds()
			})
			return nil
		},
		OnStopping: func() { pub.update(func(s *State) { s.Phase = PhaseStopping }) },
	})
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

// Describe explains an engine error code to the user, or returns "".
func Describe(code string) string {
	switch code {
	case CodeHCSAccessDenied:
		return strings.TrimSpace(fmt.Sprintf(`
The Host Compute Service (HCS) refused access.

winbox starts its VM through HCS directly, which today needs your account in the
local "Hyper-V Administrators" group. An administrator adds you once with:

    net localgroup "Hyper-V Administrators" %s /add

Then sign out and back in (group changes apply at sign-in). No elevation is needed
after that, and winbox never asks for it.

A later release replaces this with a small HCS access broker service (ADR 0004), so
that standard users need no group membership.`, os.Getenv("USERNAME")))
	case CodePipeInUse:
		return `Another program already serves the Docker API pipe \\.\pipe\docker_engine_winbox.
Stop it, or check with: Get-ChildItem \\.\pipe\ | Where-Object Name -like 'docker*'`
	case CodeDataDisk:
		return "The data disk could not be used. `winbox delete` removes it and starts over (all images, containers and volumes are lost)."
	case CodeVMExited:
		return "The VM stopped on its own. `winbox logs --console` shows why."
	}
	return ""
}
