//go:build windows

package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/bench/internal/hostmon"
)

type config struct {
	docker, host, imageTar, image string
	process                       string
	pid                           uint
	vmmemPID, vmwpPID             int
	diagPipe                      string
	duration                      time.Duration
	interval, opTimeout, pause    time.Duration
	maxSampleErrors               int
	out                           string
	profileDir                    string
	shareDir                      string
	port                          int
	// Launch mode: the soak starts and stops the engine itself.
	hb        hostmon.Hyperbutler
	startArgs []string
	waitQuiet time.Duration
	verdictConfig
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "soak:", err)
		os.Exit(1)
	}
}

func parseFlags() (*config, error) {
	c := &config{}
	fs := flag.NewFlagSet("soak", flag.ContinueOnError)
	fs.StringVar(&c.docker, "docker", "", "path to the stock docker.exe (required)")
	fs.StringVar(&c.host, "host", "", "docker -H endpoint (default: the installation's pipe in launch mode, else npipe:////./pipe/docker_engine_hyperbutler)")
	fs.StringVar(&c.imageTar, "image-tar", "", "`docker load` tar, e.g. from `hyperbutler-dev mkimage` (required)")
	fs.StringVar(&c.image, "image", "hyperbutler-test/busybox:latest", "image name:tag inside -image-tar; it holds /bin/busybox and its default command prints and exits 0")
	fs.StringVar(&c.hb.Exe, "hyperbutler", "", "launch mode: run `hyperbutler start` with this hyperbutler.exe, sample its engine and VM, and stop it at the end")
	fs.StringVar(&c.hb.Home, "home", "", "launch mode: HYPERBUTLER_HOME of the bench installation (required with -hyperbutler)")
	fs.StringVar(&c.hb.ImageDir, "image-dir", "", "launch mode: guest image directory (--image-dir)")
	startArgs := fs.String("start-args", "", "launch mode: extra `hyperbutler start` flags, space separated")
	fs.DurationVar(&c.waitQuiet, "wait-quiet", 30*time.Minute, "launch mode: how long to wait for other VMs and engines to go away first")
	fs.StringVar(&c.process, "process", "hyperbutler.exe", "attach mode: image name of the target process when -pid is 0")
	fs.UintVar(&c.pid, "pid", 0, "attach mode: PID of the target process (0: the single running -process)")
	fs.IntVar(&c.vmmemPID, "vmmem-pid", 0, "attach mode: PID of the VM's vmmem process (0: the single running vmmem, -1: don't sample)")
	fs.IntVar(&c.vmwpPID, "vmwp-pid", -1, "attach mode: PID of the VM's vmwp process (-1: don't sample)")
	fs.StringVar(&c.diagPipe, "diag-pipe", "", `target's internal/diag endpoint (launch mode: default \\.\pipe\hyperbutler-soak-<pid>, started with the engine; attach mode: required, "none" skips goroutine and heap samples)`)
	fs.StringVar(&c.shareDir, "share-dir", "", `Windows directory to bind-mount in the loop (docker run -v DIR:/mnt); "none" leaves bind mounts out (required)`)
	fs.IntVar(&c.port, "port", 18480, "first host port of the published-port step (-p PORT:8080), 0 leaves it out; it rotates through 8 ports")
	fs.StringVar(&c.profileDir, "profile-dir", "", "fetch heap, allocs and goroutine profiles from the diag endpoint into this directory at the end of the warm-up and of the run")
	fs.DurationVar(&c.duration, "duration", 24*time.Hour, "total run time (main gate: 1h with -warmup 10m)")
	fs.DurationVar(&c.warmup, "warmup", time.Hour, "samples before this are left out of the trend fit")
	fs.DurationVar(&c.minWindow, "min-window", 0, "fail unless the steady-state window is at least this long (0: 80% of duration-warmup)")
	fs.DurationVar(&c.bucket, "bucket", 5*time.Minute, "trend buckets; must exceed the slowest periodic swing (Go forces a GC every 2 min)")
	fs.DurationVar(&c.interval, "interval", 10*time.Second, "sampling interval")
	fs.DurationVar(&c.opTimeout, "op-timeout", 2*time.Minute, "timeout for one docker command")
	fs.DurationVar(&c.pause, "pause", time.Second, "pause between workload iterations")
	fs.IntVar(&c.maxErrors, "max-errors", 0, "docker command failures tolerated before the run fails")
	fs.IntVar(&c.maxSampleErrors, "max-sample-errors", 3, "failed samples tolerated before the run fails")
	fs.StringVar(&c.out, "out", "", "write every sample to this CSV file (keep raw dumps out of git, e.g. under bench/results/)")

	wsAbs := fs.Int64("ws-abs", 1<<20, "private working set: allowed growth in bytes over the window")
	wsRel := fs.Float64("ws-rel", 0.05, "private working set: allowed growth as a fraction of its median")
	hAbs := fs.Float64("handles-abs", 20, "handle count: allowed growth over the window")
	hRel := fs.Float64("handles-rel", 0.05, "handle count: allowed growth as a fraction of its median")
	thAbs := fs.Float64("threads-abs", 2, "thread count: allowed growth over the window")
	gAbs := fs.Float64("goroutines-abs", 5, "goroutines: allowed growth over the window")
	heapAbs := fs.Int64("heap-abs", 1<<20, "Go heap in use: allowed growth in bytes over the window")
	heapRel := fs.Float64("heap-rel", 0.10, "Go heap in use: allowed growth as a fraction of its median")
	vmAbs := fs.Int64("vmmem-abs", 64<<20, "vmmem private working set: allowed growth in bytes over the window")
	vmRel := fs.Float64("vmmem-rel", 0.10, "vmmem private working set: allowed growth as a fraction of its median")
	wpAbs := fs.Int64("vmwp-abs", 8<<20, "vmwp private working set: allowed growth in bytes over the window")
	wpRel := fs.Float64("vmwp-rel", 0.10, "vmwp private working set: allowed growth as a fraction of its median")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return nil, err
	}
	if c.docker == "" || c.imageTar == "" {
		return nil, errors.New("-docker and -image-tar are required")
	}
	if c.hb.Exe != "" && c.hb.Home == "" {
		return nil, errors.New("-hyperbutler needs -home (a bench installation, never the user's)")
	}
	switch c.shareDir {
	case "":
		return nil, errors.New(`-share-dir is required; pass -share-dir none to leave bind mounts and published ports out`)
	case "none":
		c.shareDir, c.port = "", 0
	default:
		abs, err := filepath.Abs(c.shareDir)
		if err != nil {
			return nil, err
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("-share-dir %s is not a directory", c.shareDir)
		}
		c.shareDir = abs
	}
	if c.port < 0 || c.port+PortSpan > 65536 {
		return nil, errors.New("bad -port")
	}
	if c.interval <= 0 || c.duration <= c.warmup || c.warmup < 0 {
		return nil, errors.New("need -interval > 0 and 0 <= -warmup < -duration")
	}
	if c.minWindow == 0 {
		c.minWindow = (c.duration - c.warmup) * 8 / 10
	}
	if c.bucket < 3*c.interval || c.duration-c.warmup < minBuckets*c.bucket {
		return nil, fmt.Errorf("need -bucket >= 3 x -interval and -duration - -warmup >= %d x -bucket", minBuckets)
	}
	c.optedOut = map[string]bool{}
	switch {
	case c.diagPipe == "none":
		c.diagPipe = ""
		c.optedOut["goroutines"], c.optedOut["heap_inuse"] = true, true
	case c.diagPipe == "" && c.hb.Exe != "":
		c.diagPipe = fmt.Sprintf(`\\.\pipe\hyperbutler-soak-%d`, os.Getpid())
	case c.diagPipe == "":
		return nil, errors.New(`-diag-pipe is required in attach mode; pass -diag-pipe none to run without goroutine and heap samples`)
	}
	if c.hb.Exe == "" {
		if c.vmmemPID < 0 {
			c.optedOut["vmmem_ws_private"] = true
		}
		if c.vmwpPID < 0 {
			c.optedOut["vmwp_ws_private"] = true
		}
		if c.host == "" {
			c.host = "npipe:////./pipe/docker_engine_hyperbutler"
		}
	}
	c.startArgs = strings.Fields(*startArgs)
	c.limits = map[string]Limit{
		"ws_private":       {Abs: float64(*wsAbs), Rel: *wsRel},
		"private_bytes":    {Abs: float64(*wsAbs), Rel: *wsRel},
		"handles":          {Abs: *hAbs, Rel: *hRel},
		"threads":          {Abs: *thAbs},
		"goroutines":       {Abs: *gAbs},
		"heap_inuse":       {Abs: float64(*heapAbs), Rel: *heapRel},
		"vmmem_ws_private": {Abs: float64(*vmAbs), Rel: *vmRel},
		"vmwp_ws_private":  {Abs: float64(*wpAbs), Rel: *wpRel},
	}
	return c, nil
}

// target is what the soak samples.
type target struct {
	engine, vmmem, vmwp uint32
}

func logf(format string, a ...any) {
	fmt.Printf("%s "+format+"\n", append([]any{time.Now().Format(time.TimeOnly)}, a...)...)
}

// launch starts the bench installation's engine with the diag endpoint on and finds
// its VM. The returned stop function stops it again.
func launch(ctx context.Context, c *config) (target, func() error, error) {
	pipe := c.hb.DockerPipe()
	if up, err := c.hb.Running(); err != nil {
		return target{}, nil, err
	} else if up {
		return target{}, nil, fmt.Errorf("the installation in %s is already running; launch mode starts it itself", c.hb.Home)
	}
	f, busy, err := hostmon.WaitQuiet(ctx, pipe, 15*time.Second, c.waitQuiet, logf)
	if err != nil {
		return target{}, nil, err
	}
	if busy {
		return target{}, nil, fmt.Errorf("the Docker API pipe %s is still in use", pipe)
	}
	if f.Any() {
		logf("other VMs or engines still run (%s); they will be noted per sample", f)
	}
	before, err := hostmon.Snapshot()
	if err != nil {
		return target{}, nil, err
	}
	stop := func() error {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		out, err := c.hb.Stop(sctx)
		logf("%s", out)
		return err
	}
	out, err := c.hb.Start(ctx, append([]string{"--debug-pipe", c.diagPipe}, c.startArgs...)...)
	if err != nil {
		// A start cut short (Ctrl+C) can leave the detached engine running.
		return target{}, nil, errors.Join(err, stop())
	}
	logf("%s", out)
	st, err := c.hb.State()
	if err != nil {
		return target{}, nil, errors.Join(err, stop())
	}
	after, err := hostmon.Snapshot()
	if err != nil {
		return target{}, nil, errors.Join(err, stop())
	}
	vm, err := hostmon.FindNewVM(before, after)
	if err != nil {
		return target{}, nil, errors.Join(err, stop())
	}
	if c.host == "" {
		c.host = hostmon.NpipeHost(pipe)
	}
	return target{engine: uint32(st.PID), vmmem: vm.VMMem, vmwp: vm.VMWP}, stop, nil
}

func attach(c *config) (target, error) {
	var t target
	var err error
	if t.engine, err = pickPID(c.pid, c.process); err != nil {
		return t, err
	}
	switch {
	case c.vmmemPID > math.MaxUint32:
		return t, fmt.Errorf("bad -vmmem-pid %d", c.vmmemPID)
	case c.vmmemPID > 0:
		t.vmmem = uint32(c.vmmemPID)
	case c.vmmemPID == 0:
		if t.vmmem, err = pickPID(0, hostmon.VMMemName); err != nil {
			return t, fmt.Errorf("%w (pass -vmmem-pid, or -vmmem-pid -1 to skip)", err)
		}
	}
	switch {
	case c.vmwpPID > math.MaxUint32:
		return t, fmt.Errorf("bad -vmwp-pid %d", c.vmwpPID)
	case c.vmwpPID > 0:
		t.vmwp = uint32(c.vmwpPID)
	}
	return t, nil
}

func run() (err error) {
	c, err := parseFlags()
	if err != nil {
		return err
	}
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopSignals()

	var tg target
	if c.hb.Exe != "" {
		var stopEngine func() error
		tg, stopEngine, err = launch(ctx, c)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, stopEngine()) }()
	} else if tg, err = attach(c); err != nil {
		return err
	}

	// Pin the PIDs so they cannot be reused by another process mid-run. vmmem and vmwp
	// are protected; if they cannot be opened, their disappearance from the counters
	// still ends the run.
	targetH, err := hostmon.Pin(tg.engine)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(targetH)
	var vmmemH windows.Handle
	if tg.vmmem != 0 {
		if h, err := hostmon.Pin(tg.vmmem); err == nil {
			vmmemH = h
			defer windows.CloseHandle(vmmemH)
		}
	}

	cfgDir, err := os.MkdirTemp("", "hyperbutler-soak-docker-config")
	if err != nil {
		return err
	}
	defer os.RemoveAll(cfgDir)

	w := &Workload{Docker: c.docker, Host: c.host, ImageTar: c.imageTar, Image: c.image,
		OpTimeout: c.opTimeout, Pause: c.pause, ConfigDir: cfgDir, ShareDir: c.shareDir, Port: c.port}
	if _, ok := w.op(ctx, "version", nil, "version"); !ok {
		_, _, _, f := w.Totals()
		return fmt.Errorf("engine at %s does not answer: %v", c.host, f)
	}
	w = &Workload{Docker: w.Docker, Host: w.Host, ImageTar: w.ImageTar, Image: w.Image,
		OpTimeout: w.OpTimeout, Pause: w.Pause, ConfigDir: w.ConfigDir, ShareDir: w.ShareDir, Port: w.Port}

	var dc *hostmon.DiagClient
	if c.diagPipe != "" {
		dc = hostmon.NewDiagClient(c.diagPipe)
		defer dc.Close()
		if _, err := dc.Stats(ctx); err != nil {
			return err
		}
	}

	var csvw *csv.Writer
	if c.out != "" {
		f, err := os.Create(c.out)
		if err != nil {
			return err
		}
		defer f.Close()
		csvw = csv.NewWriter(f)
		defer csvw.Flush()
		_ = csvw.Write(append(append([]string{"t_seconds"}, metrics...), extraColumns...))
	}

	fmt.Printf("soak: engine PID %d, vmmem PID %d, vmwp PID %d, diag %q, bind mounts %q, port %d, %s (warm-up %s), sample every %s\n",
		tg.engine, tg.vmmem, tg.vmwp, c.diagPipe, c.shareDir, c.port, c.duration, c.warmup, c.interval)

	runCtx, cancel := context.WithTimeout(ctx, c.duration)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.Run(runCtx)
	}()

	series := map[string][]Point{}
	var sampleErrs []string
	var fatal error
	start := time.Now()
	pids := []uint32{tg.engine}
	for _, p := range []uint32{tg.vmmem, tg.vmwp} {
		if p != 0 {
			pids = append(pids, p)
		}
	}
	watch := hostmon.Watch(time.Second)
	defer watch.Stop()
	watch.SetOurs(tg.engine, tg.vmmem, tg.vmwp)
	overlapped := 0
	profiledWarmup := false
	tick := time.NewTicker(c.interval)
	defer tick.Stop()
sampling:
	for {
		t := time.Since(start).Seconds()
		row := map[string]float64{}
		extra := map[string]string{}
		if hostmon.Exited(targetH) {
			fatal = fmt.Errorf("engine PID %d exited after %s", tg.engine, time.Since(start).Round(time.Second))
			break
		}
		if vmmemH != 0 && hostmon.Exited(vmmemH) {
			fatal = fmt.Errorf("vmmem PID %d exited after %s: the VM stopped", tg.vmmem, time.Since(start).Round(time.Second))
			break
		}
		s, err := hostmon.SampleNT(runCtx, pids)
		switch {
		case runCtx.Err() != nil:
		case err != nil:
			sampleErrs = append(sampleErrs, err.Error())
		default:
			ts, ok := s[tg.engine]
			if !ok {
				fatal = fmt.Errorf("engine PID %d is gone after %s", tg.engine, time.Since(start).Round(time.Second))
				break sampling
			}
			row["ws_private"] = float64(ts.WorkingSetPriv)
			row["private_bytes"] = float64(ts.PrivateBytes)
			row["handles"] = float64(ts.HandleCount)
			row["threads"] = float64(ts.ThreadCount)
			for _, v := range []struct {
				pid  uint32
				name string
			}{{tg.vmmem, "vmmem_ws_private"}, {tg.vmwp, "vmwp_ws_private"}} {
				if v.pid == 0 {
					continue
				}
				vs, ok := s[v.pid]
				if !ok {
					fatal = fmt.Errorf("%s PID %d is gone: the VM stopped", v.name, v.pid)
					break sampling
				}
				row[v.name] = float64(vs.WorkingSetPriv)
			}
		}
		if dc != nil && runCtx.Err() == nil {
			if st, err := dc.Stats(runCtx); err != nil {
				sampleErrs = append(sampleErrs, err.Error())
			} else {
				row["goroutines"] = float64(st.Goroutines)
				row["heap_inuse"] = float64(st.MemStats.HeapInuse)
				m := st.MemStats
				u := func(v uint64) string { return strconv.FormatUint(v, 10) }
				extra["heap_alloc"], extra["heap_idle"], extra["heap_released"] = u(m.HeapAlloc), u(m.HeapIdle), u(m.HeapReleased)
				extra["stack_inuse"], extra["go_sys"], extra["num_gc"] = u(m.StackInuse), u(m.Sys), strconv.FormatUint(uint64(m.NumGC), 10)
				if o := st.OS; o != nil {
					extra["os_private"], extra["os_heap_committed"] = u(o.PrivateBytes), u(o.HeapCommitted)
				}
			}
		}
		// Other VMs do not change this engine's counters, but they compete for host
		// memory; note every one seen since the previous sample so a reader can judge.
		if f := watch.Take(); f != "" {
			extra["foreign"] = f
			overlapped++
		}
		if tg.vmmem != 0 && tg.vmwp != 0 {
			// vmmem cannot be pinned: notice a dead VM whose PID was reused.
			if ps, err := hostmon.Snapshot(); err == nil && !hostmon.StillOurs(ps, hostmon.VMProcs{VMWP: tg.vmwp, VMMem: tg.vmmem}) {
				fatal = fmt.Errorf("vmmem PID %d / vmwp PID %d are no longer this VM's: the VM stopped", tg.vmmem, tg.vmwp)
				break
			}
		}
		if runCtx.Err() != nil {
			break
		}
		if len(row) > 0 {
			rec := []string{strconv.FormatFloat(t, 'f', 1, 64)}
			for _, m := range metrics {
				v, ok := row[m]
				if ok {
					series[m] = append(series[m], Point{T: t, V: v})
					rec = append(rec, strconv.FormatFloat(v, 'f', -1, 64))
				} else {
					rec = append(rec, "")
				}
			}
			for _, k := range extraColumns {
				rec = append(rec, extra[k])
			}
			if csvw != nil {
				_ = csvw.Write(rec)
				csvw.Flush()
			}
		}
		if !profiledWarmup && dc != nil && c.profileDir != "" && time.Since(start) >= c.warmup {
			profiledWarmup = true
			fetchProfiles(runCtx, dc, c.profileDir, "warmup")
		}
		if len(sampleErrs) > c.maxSampleErrors {
			fatal = fmt.Errorf("%d failed samples, last: %s", len(sampleErrs), sampleErrs[len(sampleErrs)-1])
			break
		}
		select {
		case <-runCtx.Done():
			break sampling
		case <-tick.C:
		}
	}
	if dc != nil && c.profileDir != "" && fatal == nil && ctx.Err() == nil {
		// Still under load: the workload stops only after cancel below.
		fetchProfiles(context.WithoutCancel(ctx), dc, c.profileDir, "end")
	}
	cancel()
	wg.Wait()
	if overlapped > 0 {
		fmt.Printf("\nnote: other VMs or engines ran during %d samples (see the CSV's foreign column)\n", overlapped)
	}

	ops, errs, byKind, failures := w.Totals()
	return report(os.Stdout, c.verdictConfig, series, ops, errs, byKind, failures, sampleErrs, fatal, ctx.Err() != nil)
}

// extraColumns are CSV columns recorded for analysis but not judged.
var extraColumns = []string{"heap_alloc", "heap_idle", "heap_released", "stack_inuse", "go_sys", "num_gc", "os_private", "os_heap_committed", "foreign"}

// fetchProfiles saves the engine's heap, allocs and goroutine profiles, so growth or a
// large working set can be traced afterwards (`go tool pprof`).
func fetchProfiles(ctx context.Context, dc *hostmon.DiagClient, dir, tag string) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		logf("profiles: %v", err)
		return
	}
	for _, p := range []struct{ path, file string }{
		{"/debug/pprof/heap", "heap"}, {"/debug/pprof/allocs", "allocs"}, {"/debug/pprof/goroutine?debug=1", "goroutine.txt"},
		{"/debug/stats", "stats.json"},
	} {
		pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		b, err := dc.Get(pctx, p.path)
		cancel()
		if err != nil {
			logf("profile %s: %v", p.path, err)
			continue
		}
		name := filepath.Join(dir, tag+"-"+p.file)
		if err := os.WriteFile(name, b, 0o600); err != nil {
			logf("profile %s: %v", name, err)
		}
	}
	logf("profiles (%s) written to %s", tag, dir)
}

func pickPID(pid uint, name string) (uint32, error) {
	if pid != 0 {
		if pid > math.MaxUint32 {
			return 0, fmt.Errorf("bad PID %d", pid)
		}
		return uint32(pid), nil
	}
	ps, err := hostmon.Snapshot()
	if err != nil {
		return 0, err
	}
	var pids []uint32
	for _, p := range ps {
		if strings.EqualFold(p.Name, name) {
			pids = append(pids, p.PID)
		}
	}
	if len(pids) != 1 {
		return 0, fmt.Errorf("found %d processes named %s, want exactly 1: %v", len(pids), name, pids)
	}
	return pids[0], nil
}
