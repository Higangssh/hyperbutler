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
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

type config struct {
	docker, host, imageTar, image string
	process                       string
	pid                           uint
	vmmemPID                      int
	diagPipe                      string
	duration                      time.Duration
	interval, opTimeout, pause    time.Duration
	maxSampleErrors               int
	out                           string
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
	fs.StringVar(&c.host, "host", "npipe:////./pipe/docker_engine_hyperbutler", "docker -H endpoint")
	fs.StringVar(&c.imageTar, "image-tar", "", "`docker load` tar, e.g. from `hyperbutler-dev mkimage` (required)")
	fs.StringVar(&c.image, "image", "hyperbutler-test/busybox:latest", "image name:tag inside -image-tar; its default command must print and exit 0")
	fs.StringVar(&c.process, "process", "hyperbutler-dev.exe", "image name of the target process when -pid is 0")
	fs.UintVar(&c.pid, "pid", 0, "PID of the target process (0: the single running -process)")
	fs.IntVar(&c.vmmemPID, "vmmem-pid", 0, "PID of the VM's vmmem process (0: the single running vmmem, -1: don't sample)")
	fs.StringVar(&c.diagPipe, "diag-pipe", "", `target's internal/diag endpoint, e.g. \\.\pipe\hyperbutler-dev-debug (required; "none" skips goroutine and heap samples)`)
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
	if err := fs.Parse(os.Args[1:]); err != nil {
		return nil, err
	}
	if c.docker == "" || c.imageTar == "" {
		return nil, errors.New("-docker and -image-tar are required")
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
	switch c.diagPipe {
	case "":
		return nil, errors.New(`-diag-pipe is required; pass -diag-pipe none to run without goroutine and heap samples`)
	case "none":
		c.diagPipe = ""
		c.optedOut["goroutines"], c.optedOut["heap_inuse"] = true, true
	}
	if c.vmmemPID < 0 {
		c.optedOut["vmmem_ws_private"] = true
	}
	c.limits = map[string]Limit{
		"ws_private":       {Abs: float64(*wsAbs), Rel: *wsRel},
		"private_bytes":    {Abs: float64(*wsAbs), Rel: *wsRel},
		"handles":          {Abs: *hAbs, Rel: *hRel},
		"threads":          {Abs: *thAbs},
		"goroutines":       {Abs: *gAbs},
		"heap_inuse":       {Abs: float64(*heapAbs), Rel: *heapRel},
		"vmmem_ws_private": {Abs: float64(*vmAbs), Rel: *vmRel},
	}
	return c, nil
}

func run() error {
	c, err := parseFlags()
	if err != nil {
		return err
	}
	target, err := pickPID(c.pid, c.process)
	if err != nil {
		return err
	}
	var vmmem uint32
	switch {
	case c.vmmemPID > math.MaxUint32:
		return fmt.Errorf("bad -vmmem-pid %d", c.vmmemPID)
	case c.vmmemPID > 0:
		vmmem = uint32(c.vmmemPID)
	case c.vmmemPID == 0:
		if vmmem, err = pickPID(0, "vmmem"); err != nil {
			return fmt.Errorf("%w (pass -vmmem-pid, or -vmmem-pid -1 to skip)", err)
		}
	}
	// Pin both PIDs so they cannot be reused by another process mid-run. vmmem is a
	// protected process; if it cannot be opened, its disappearance from the counters
	// still ends the run.
	targetH, err := pinProcess(target)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(targetH)
	var vmmemH windows.Handle
	if vmmem != 0 {
		if h, err := pinProcess(vmmem); err == nil {
			vmmemH = h
			defer windows.CloseHandle(vmmemH)
		}
	}

	cfgDir, err := os.MkdirTemp("", "hyperbutler-soak-docker-config")
	if err != nil {
		return err
	}
	defer os.RemoveAll(cfgDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	w := &Workload{Docker: c.docker, Host: c.host, ImageTar: c.imageTar, Image: c.image,
		OpTimeout: c.opTimeout, Pause: c.pause, ConfigDir: cfgDir}
	if _, ok := w.op(ctx, "version", nil, "version"); !ok {
		_, _, _, f := w.Totals()
		return fmt.Errorf("engine at %s does not answer: %v", c.host, f)
	}
	w = &Workload{Docker: w.Docker, Host: w.Host, ImageTar: w.ImageTar, Image: w.Image,
		OpTimeout: w.OpTimeout, Pause: w.Pause, ConfigDir: w.ConfigDir}

	var dc *diagClient
	if c.diagPipe != "" {
		dc = newDiagClient(c.diagPipe)
		defer dc.close()
		if _, err := dc.stats(ctx); err != nil {
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
		_ = csvw.Write(append([]string{"t_seconds"}, metrics...))
	}

	fmt.Printf("soak: target PID %d, vmmem PID %d, diag %q, %s (warm-up %s), sample every %s\n",
		target, vmmem, c.diagPipe, c.duration, c.warmup, c.interval)

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
	pids := []uint32{target}
	if vmmem != 0 {
		pids = append(pids, vmmem)
	}
	tick := time.NewTicker(c.interval)
	defer tick.Stop()
sampling:
	for {
		t := time.Since(start).Seconds()
		row := map[string]float64{}
		if exited(targetH) {
			fatal = fmt.Errorf("target PID %d exited after %s", target, time.Since(start).Round(time.Second))
			break
		}
		if vmmemH != 0 && exited(vmmemH) {
			fatal = fmt.Errorf("vmmem PID %d exited after %s: the VM stopped", vmmem, time.Since(start).Round(time.Second))
			break
		}
		s, err := sampleCIM(runCtx, pids)
		switch {
		case runCtx.Err() != nil:
		case err != nil:
			sampleErrs = append(sampleErrs, err.Error())
		default:
			ts, ok := s[target]
			if !ok {
				fatal = fmt.Errorf("target PID %d is gone after %s", target, time.Since(start).Round(time.Second))
				break sampling
			}
			row["ws_private"] = float64(ts.WorkingSetPriv)
			row["private_bytes"] = float64(ts.PrivateBytes)
			row["handles"] = float64(ts.HandleCount)
			row["threads"] = float64(ts.ThreadCount)
			if vmmem != 0 {
				vs, ok := s[vmmem]
				if !ok {
					fatal = fmt.Errorf("vmmem PID %d is gone: the VM stopped", vmmem)
					break sampling
				}
				row["vmmem_ws_private"] = float64(vs.WorkingSetPriv)
			}
		}
		if dc != nil && runCtx.Err() == nil {
			if st, err := dc.stats(runCtx); err != nil {
				sampleErrs = append(sampleErrs, err.Error())
			} else {
				row["goroutines"] = float64(st.Goroutines)
				row["heap_inuse"] = float64(st.MemStats.HeapInuse)
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
			if csvw != nil {
				_ = csvw.Write(rec)
				csvw.Flush()
			}
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
	cancel()
	wg.Wait()

	ops, errs, byKind, failures := w.Totals()
	return report(os.Stdout, c.verdictConfig, series, ops, errs, byKind, failures, sampleErrs, fatal, ctx.Err() != nil)
}

func pickPID(pid uint, name string) (uint32, error) {
	if pid != 0 {
		if pid > math.MaxUint32 {
			return 0, fmt.Errorf("bad PID %d", pid)
		}
		return uint32(pid), nil
	}
	pids, err := findProcesses(name)
	if err != nil {
		return 0, err
	}
	if len(pids) != 1 {
		return 0, fmt.Errorf("found %d processes named %s, want exactly 1: %v", len(pids), name, pids)
	}
	return pids[0], nil
}
