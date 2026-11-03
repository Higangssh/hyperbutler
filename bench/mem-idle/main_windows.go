//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/bench/internal/hostmon"
)

type config struct {
	hb              hostmon.Hyperbutler
	docker          string
	debugPipe       string
	startArgs       []string
	from, to, every time.Duration
	targetMB        float64
	waitQuiet       time.Duration
	retries         int
	out             string
	keep            bool
	sample          hostmon.Sampler
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mem-idle:", err)
		os.Exit(1)
	}
}

func parseFlags() (*config, error) {
	c := &config{}
	fs := flag.NewFlagSet("mem-idle", flag.ContinueOnError)
	fs.StringVar(&c.hb.Exe, "hyperbutler", "", "hyperbutler.exe to measure (required)")
	fs.StringVar(&c.hb.Home, "home", "", "HYPERBUTLER_HOME of a bench installation (required; never the user's)")
	fs.StringVar(&c.hb.ImageDir, "image-dir", "", "guest image directory (--image-dir; default the image folder next to hyperbutler.exe)")
	fs.StringVar(&c.docker, "docker", "", "stock docker.exe, to check that no container runs (required)")
	fs.StringVar(&c.debugPipe, "debug-pipe", "", `engine diag endpoint to start with (default \\.\pipe\hyperbutler-memidle-<pid>)`)
	startArgs := fs.String("start-args", "", "extra `hyperbutler start` flags, space separated (e.g. \"--memory 4GiB\")")
	fs.DurationVar(&c.from, "from", 5*time.Minute, "first sample, after `hyperbutler start` is invoked")
	fs.DurationVar(&c.to, "to", 10*time.Minute, "last sample")
	fs.DurationVar(&c.every, "interval", 10*time.Second, "sampling interval")
	fs.Float64Var(&c.targetMB, "target-mb", 400, "REQ-MEM-001 limit on the median total, in MB (10^6 bytes): 400 for 0.1, 250 for 1.0")
	fs.DurationVar(&c.waitQuiet, "wait-quiet", 30*time.Minute, "how long to wait for other VMs and engines to go away before each attempt")
	fs.IntVar(&c.retries, "retries", 2, "re-runs of an attempt that overlapped another VM or engine")
	fs.StringVar(&c.out, "out", "", "write every sample to this CSV (keep raw dumps out of git)")
	fs.BoolVar(&c.keep, "keep-running", false, "leave the engine running afterwards")
	sampler := fs.String("sampler", "nt", "how counters are read: nt (in process, NtQuerySystemInformation) or cim (a PowerShell CIM query per sample, the method before 2026-10-31)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return nil, err
	}
	var err error
	if c.sample, err = hostmon.SamplerByName(*sampler); err != nil {
		return nil, err
	}
	if c.hb.Exe == "" || c.hb.Home == "" || c.docker == "" {
		return nil, errors.New("-hyperbutler, -home and -docker are required")
	}
	if len(Schedule(c.from, c.to, c.every)) == 0 {
		return nil, errors.New("need 0 <= -from <= -to and -interval > 0")
	}
	if c.targetMB <= 0 || math.IsInf(c.targetMB, 0) || c.targetMB > 1e7 {
		return nil, errors.New("bad -target-mb")
	}
	if c.debugPipe == "" {
		c.debugPipe = fmt.Sprintf(`\\.\pipe\hyperbutler-memidle-%d`, os.Getpid())
	}
	c.startArgs = strings.Fields(*startArgs)
	return c, nil
}

func run() error {
	c, err := parseFlags()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cfgDir, err := os.MkdirTemp("", "hyperbutler-memidle-docker-config")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(cfgDir) }()
	if up, err := c.hb.Running(); err != nil {
		return err
	} else if up {
		return fmt.Errorf("the installation in %s is already running; idle is measured from a fresh start", c.hb.Home)
	}

	for attempt := 0; ; attempt++ {
		ss, err := measure(ctx, c, cfgDir)
		if errors.Is(err, errAmbiguousVM) && attempt < c.retries && !c.keep {
			fmt.Printf("\nattempt %d: %v; measuring again\n", attempt+1, err)
			continue
		}
		if err != nil {
			return err
		}
		sum := Summarize(ss, uint64(c.targetMB*1e6))
		sum.Print(os.Stdout)
		if c.out != "" {
			p := c.out
			if attempt > 0 {
				p = strings.TrimSuffix(p, filepath.Ext(p)) + fmt.Sprintf("-%d", attempt) + filepath.Ext(p)
			}
			if f, err := os.Create(p); err == nil { //nolint:gosec // G304: the path comes from the operator's own flags or this tool's work directory
				err = errors.Join(WriteCSV(f, ss), f.Close())
				if err != nil {
					return err
				}
			} else {
				return err
			}
		}
		switch {
		case sum.Overlapped > 0 && attempt < c.retries && !c.keep:
			fmt.Printf("\nattempt %d overlapped another VM or engine; measuring again\n", attempt+1)
			continue
		case sum.Overlapped > 0:
			return fmt.Errorf("every attempt overlapped another VM or engine; no valid measurement")
		case !sum.Pass:
			return errors.New("FAIL: REQ-MEM-001 idle memory above target")
		}
		return nil
	}
}

func logf(format string, a ...any) {
	fmt.Printf("%s "+format+"\n", append([]any{time.Now().Format(time.TimeOnly)}, a...)...)
}

// errAmbiguousVM means this attempt's VM could not be told from another one started at
// the same time; the attempt is repeated.
var errAmbiguousVM = errors.New("cannot identify this VM's processes")

// measure does one attempt: start, sample, stop.
func measure(ctx context.Context, c *config, cfgDir string) (ss []Sample, err error) {
	pipe := c.hb.DockerPipe()
	f, busy, err := hostmon.WaitQuiet(ctx, pipe, 15*time.Second, c.waitQuiet, logf)
	if err != nil {
		return nil, err
	}
	if busy {
		return nil, fmt.Errorf("the Docker API pipe %s is still in use after %s", pipe, c.waitQuiet)
	}
	if f.Any() {
		logf("host still not quiet after %s (%s); measuring anyway, the attempt will be marked overlapped", c.waitQuiet, f)
	}
	before, err := hostmon.Snapshot()
	if err != nil {
		return nil, err
	}
	// Watch the host for the whole attempt, minutes 0-5 included, not just at samples.
	watch := hostmon.Watch(time.Second)
	defer watch.Stop()
	// Registered before the start: a start cut short (Ctrl+C) leaves the detached
	// engine running, and only this installation's own stop can end it.
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
		return nil, err
	}
	logf("%s", out)
	// A --pipe in -start-args is only in settings.json once the start has run.
	pipe = c.hb.DockerPipe()
	st, err := c.hb.State()
	if err != nil {
		return nil, err
	}
	enginePID := uint32(st.PID) //nolint:gosec // G115: Windows process IDs are DWORDs
	eh, err := hostmon.Pin(enginePID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = windows.CloseHandle(eh) }()
	after, err := hostmon.Snapshot()
	if err != nil {
		return nil, err
	}
	vm, err := hostmon.FindNewVM(before, after)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errAmbiguousVM, err)
	}
	logf("engine PID %d, vmwp PID %d, vmmem PID %d, debug pipe %s", enginePID, vm.VMWP, vm.VMMem, c.debugPipe)
	vh, err := hostmon.Pin(vm.VMMem)
	if err != nil {
		vh = 0 // vmmem is protected; its disappearance from the counters still shows
	} else {
		defer func() { _ = windows.CloseHandle(vh) }()
	}

	d := &hostmon.Docker{Exe: c.docker, Host: hostmon.NpipeHost(pipe), ConfigDir: cfgDir}
	checkIdle := func(when string) error {
		out, err := d.Run(ctx, "ps", "-q")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) != "" {
			return fmt.Errorf("%s: containers are running; REQ-MEM-001 is measured with none", when)
		}
		return nil
	}
	if err := checkIdle("at start"); err != nil {
		return nil, err
	}
	dc := hostmon.NewDiagClient(c.debugPipe)
	defer dc.Close()
	if _, err := dc.Stats(ctx); err != nil {
		return nil, err
	}

	watch.SetOurs(enginePID, vm.VMMem, vm.VMWP)
	for _, off := range Schedule(c.from, c.to, c.every) {
		select {
		case <-ctx.Done():
			return ss, ctx.Err()
		case <-time.After(time.Until(t0.Add(off))):
		}
		if hostmon.Exited(eh) || vh != 0 && hostmon.Exited(vh) {
			return ss, errors.New("the engine or its VM exited during the idle window")
		}
		ps, err := hostmon.Snapshot()
		if err != nil {
			return ss, err
		}
		if !hostmon.StillOurs(ps, vm) {
			return ss, errors.New("this VM's vmmem/vmwp are gone (or their PIDs were reused)")
		}
		helpers := hostmon.ChildrenOf(ps, enginePID)
		pids := append([]uint32{enginePID, vm.VMMem, vm.VMWP}, helpers...)
		// The broker service runs as SYSTEM; the process list still gives its counters.
		brokers := hostmon.ByName(ps, hostmon.BrokerName)
		pids = append(pids, brokers...)
		sm, err := c.sample(ctx, pids)
		if err != nil {
			return ss, err
		}
		st, err := dc.Stats(ctx)
		if err != nil {
			return ss, err
		}
		s := Sample{T: time.Since(t0).Seconds(), Stats: st}
		for _, p := range []struct {
			pid uint32
			dst *uint64
		}{{enginePID, &s.EngineWS}, {vm.VMMem, &s.VMMemWS}, {vm.VMWP, &s.VMWPWS}} {
			v, ok := sm[p.pid]
			if !ok {
				return ss, fmt.Errorf("PID %d is gone from the counters", p.pid)
			}
			*p.dst = v.WorkingSetPriv
		}
		s.Threads = sm[enginePID].ThreadCount
		for _, h := range helpers {
			s.HelpersWS += sm[h].WorkingSetPriv
		}
		for _, p := range brokers {
			s.BrokerWS += sm[p].WorkingSetPriv
		}
		// Everything foreign seen since the previous sample (for the first sample: since
		// the start command), so a VM that came and went in between still counts.
		s.Foreign = watch.Take()
		logf("t=%3.0fs engine %s vmmem %s vmwp %s broker %s total %s heap %s goroutines %d%s", s.T, mib(float64(s.EngineWS)), mib(float64(s.VMMemWS)),
			mib(float64(s.VMWPWS)), mib(float64(s.BrokerWS)), mib(float64(s.Total())), mib(float64(st.MemStats.HeapAlloc)), st.Goroutines, foreignNote(s.Foreign))
		ss = append(ss, s)
	}
	if err := checkIdle("at the end"); err != nil {
		return ss, err
	}
	return ss, nil
}

func foreignNote(f string) string {
	if f == "" {
		return ""
	}
	return "  [foreign: " + f + "]"
}
