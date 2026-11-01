//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/bench/internal/hostmon"
	"github.com/Higangssh/hyperbutler/internal/diag"
	"github.com/Higangssh/hyperbutler/internal/engine"
)

type config struct {
	hb        hostmon.Hyperbutler
	docker    string
	dockerCfg string
	image     string
	debugPipe string
	startArgs []string
	idle      time.Duration
	every     time.Duration
	out       string
	keep      bool
	// noVMWP lets the check pass without this VM's vmwp identified: its sockets are
	// then not watched.
	noVMWP bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "no-telemetry:", err)
		os.Exit(1)
	}
}

func parseFlags() (*config, error) {
	c := &config{}
	fs := flag.NewFlagSet("no-telemetry", flag.ContinueOnError)
	fs.StringVar(&c.hb.Exe, "hyperbutler", "", "hyperbutler.exe to check (required)")
	fs.StringVar(&c.hb.Home, "home", "", "HYPERBUTLER_HOME of a bench installation (required; never the user's)")
	fs.StringVar(&c.hb.ImageDir, "image-dir", "", "guest image directory (--image-dir; default next to hyperbutler.exe)")
	fs.StringVar(&c.docker, "docker", "", "stock docker.exe (required)")
	fs.StringVar(&c.dockerCfg, "docker-config", "", "DOCKER_CONFIG for the docker CLI (default: a new temporary directory)")
	fs.StringVar(&c.image, "image", "busybox:1.37", "image of the container that runs in the window; pulled first (not counted) if missing")
	fs.StringVar(&c.debugPipe, "debug-pipe", "", `engine diag endpoint to start with (default \\.\pipe\hyperbutler-notel-<pid>)`)
	startArgs := fs.String("start-args", "", "extra `hyperbutler start` flags, space separated")
	fs.DurationVar(&c.idle, "idle", 5*time.Minute, "length of the idle window; the container runs in its middle")
	fs.DurationVar(&c.every, "interval", 250*time.Millisecond, "socket table polling interval")
	fs.StringVar(&c.out, "out", "", "write the report as JSON to this file")
	fs.BoolVar(&c.keep, "keep-running", false, "leave the engine running afterwards")
	fs.BoolVar(&c.noVMWP, "allow-no-vmwp", false, "go on when this VM's vmwp cannot be identified (its sockets are then not watched, and the result says so)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return nil, err
	}
	if c.hb.Exe == "" || c.hb.Home == "" || c.docker == "" {
		return nil, errors.New("-hyperbutler, -home and -docker are required")
	}
	if c.idle < 10*time.Second || c.every <= 0 || c.every > 10*time.Second {
		return nil, errors.New("need -idle >= 10s and 0 < -interval <= 10s")
	}
	if c.debugPipe == "" {
		c.debugPipe = fmt.Sprintf(`\\.\pipe\hyperbutler-notel-%d`, os.Getpid())
	}
	c.startArgs = strings.Fields(*startArgs)
	return c, nil
}

// poller polls the socket tables for the watched processes until stopped.
type poller struct {
	mu      sync.Mutex
	phase   string
	engine  uint32 // 0 until known: then the engine's descendants are watched
	vmwp    uint32
	rec     Recorder
	errs    []string
	stop    chan struct{}
	stopped chan struct{}
}

func startPoller(every time.Duration) *poller {
	p := &poller{phase: "boot", stop: make(chan struct{}), stopped: make(chan struct{})}
	self := uint32(os.Getpid()) //nolint:gosec // G115: Windows process IDs are DWORDs
	go func() {
		defer close(p.stopped)
		var t tables
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			ps, perr := hostmon.Snapshot()
			ss, serr := t.read()
			now := time.Now()
			p.mu.Lock()
			if err := errors.Join(perr, serr); err != nil {
				if len(p.errs) < 10 {
					p.errs = append(p.errs, err.Error())
				}
			} else {
				// Until the engine is known, everything below this bench (the hyperbutler
				// CLI and the engine it spawns, while the CLI lives) is watched.
				root := self
				if p.engine != 0 {
					root = p.engine
				}
				pids := hostmon.Descendants(ps, root)
				if p.vmwp != 0 {
					pids[p.vmwp] = true
				}
				p.rec.Add(p.phase, now, ss, pids)
			}
			p.mu.Unlock()
			select {
			case <-p.stop:
				return
			case <-tick.C:
			}
		}
	}()
	return p
}

func (p *poller) set(f func(p *poller)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(p)
}

func (p *poller) halt() {
	close(p.stop)
	<-p.stopped
}

func logf(format string, a ...any) {
	fmt.Printf("%s "+format+"\n", append([]any{time.Now().Format(time.TimeOnly)}, a...)...)
}

func run() (err error) {
	c, err := parseFlags()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if c.dockerCfg == "" {
		if c.dockerCfg, err = os.MkdirTemp("", "hyperbutler-notel-docker-config"); err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(c.dockerCfg) }()
	}
	if up, err := c.hb.Running(); err != nil {
		return err
	} else if up {
		return fmt.Errorf("the installation in %s is already running; the check starts it itself", c.hb.Home)
	}
	// The docker CLI is the user's and is not watched, but its plugin hooks (Docker
	// Desktop's scout after a pull) reach the network; keep them out of the run.
	if err := os.Setenv("DOCKER_CLI_HOOKS", "0"); err != nil {
		return err
	}

	before, err := hostmon.Snapshot()
	if err != nil {
		return err
	}
	pl := startPoller(c.every)
	polling := true
	defer func() {
		if polling {
			pl.halt()
		}
	}()
	if !c.keep {
		defer func() {
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer cancel()
			if out, serr := c.hb.Stop(sctx); serr != nil {
				err = errors.Join(err, serr)
			} else {
				logf("%s", out)
			}
		}()
	}
	t0 := time.Now()
	out, err := c.hb.Start(ctx, append([]string{"--debug-pipe", c.debugPipe}, c.startArgs...)...)
	if err != nil {
		return err
	}
	logf("%s", out)
	st, err := c.hb.State()
	if err != nil {
		return err
	}
	enginePID := uint32(st.PID) //nolint:gosec // G115: Windows process IDs are DWORDs
	eh, err := hostmon.Pin(enginePID)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(eh) }()
	var vm hostmon.VMProcs
	if after, err := hostmon.Snapshot(); err != nil {
		return err
	} else if vm, err = hostmon.FindNewVM(before, after); err != nil {
		if err := noVMWP(err, c.noVMWP); err != nil {
			return err
		}
		logf("this VM's vmwp is not watched (-allow-no-vmwp): %v", err)
	}
	pl.set(func(p *poller) { p.engine, p.vmwp = enginePID, vm.VMWP })
	logf("engine PID %d, vmwp PID %d, debug pipe %s", enginePID, vm.VMWP, c.debugPipe)

	dc := hostmon.NewDiagClient(c.debugPipe)
	defer dc.Close()
	netDoc := func() (NetVars, error) {
		b, err := dc.Get(ctx, diag.VarsPath+engine.NetVarName)
		if err != nil {
			return NetVars{}, err
		}
		return ParseNetVars(bytes.NewReader(b))
	}
	rep := &Report{EnginePID: enginePID, VMWP: vm.VMWP}
	var prev NetVars // the stack starts empty
	from := t0
	// mark ends the current phase at now and starts next.
	mark := func(name string, counted bool, next string) error {
		cur, err := netDoc()
		if err != nil {
			return err
		}
		now := time.Now()
		pl.set(func(p *poller) { p.phase = next })
		rep.Phases = append(rep.Phases, Phase{Name: name, Counted: counted, From: from, To: now,
			Guest:  FlowDelta(prev.Flows, cur.Flows),
			Frames: [2]uint64{cur.Stats.FramesIn - prev.Stats.FramesIn, cur.Stats.FramesOut - prev.Stats.FramesOut}})
		prev, from = cur, now
		return nil
	}
	// The boot phase: from `hyperbutler start` until it returned.
	if err := mark("boot", true, "idle-1"); err != nil {
		return err
	}

	d := &hostmon.Docker{Exe: c.docker, Host: hostmon.NpipeHost(c.hb.DockerPipe()), ConfigDir: c.dockerCfg}
	if out, err := d.Run(ctx, "ps", "-q"); err != nil {
		return err
	} else if strings.TrimSpace(out) != "" {
		return errors.New("containers are running in the bench installation; use a fresh -home")
	}
	if _, err := d.Run(ctx, "image", "inspect", c.image); err != nil {
		logf("pulling %s (a user pull: not counted)", c.image)
		if err := mark("idle-0", true, "pull"); err != nil {
			return err
		}
		if _, err := d.Run(ctx, "pull", "-q", c.image); err != nil {
			return err
		}
		if err := mark("pull", false, "idle-1"); err != nil {
			return err
		}
	}

	wait := func(dur time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dur):
		}
		if hostmon.Exited(eh) {
			return errors.New("the engine exited during the check")
		}
		return nil
	}
	logf("idle for %s, with one container run in the middle", c.idle)
	if err := wait(c.idle / 2); err != nil {
		return err
	}
	if err := mark("idle-1", true, "container"); err != nil {
		return err
	}
	name := fmt.Sprintf("hb-notel-%d", os.Getpid())
	if _, err := d.Run(ctx, "run", "--rm", "--name", name, c.image, "sleep", "5"); err != nil {
		return err
	}
	if err := mark("container", true, "idle-2"); err != nil {
		return err
	}
	if err := wait(c.idle - c.idle/2); err != nil {
		return err
	}
	if err := mark("idle-2", true, "stop"); err != nil {
		return err
	}
	pl.halt()
	polling = false
	rep.Sockets, rep.Polls = pl.rec.All(), pl.rec.Polls
	for _, e := range pl.errs {
		logf("poll error: %s", e)
	}
	rep.Print(os.Stdout)
	if c.out != "" {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(c.out, b, 0o600); err != nil {
			return err
		}
	}
	if len(pl.errs) > 0 {
		return errors.New("the socket tables could not always be read")
	}
	if f := rep.Failures(); len(f) > 0 {
		return fmt.Errorf("FAIL: REQ-UX-005: %d requests of hyperbutler's own", len(f))
	}
	return nil
}
