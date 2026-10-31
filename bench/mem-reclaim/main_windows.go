//go:build windows

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/bench/internal/hostmon"
	"github.com/Higangssh/hyperbutler/internal/engine"
)

type config struct {
	hb                 hostmon.Hyperbutler
	docker             string
	guest              string
	work               string
	debugPipe          string
	pipe               string
	memory             string
	startArgs          []string
	settle, baseline   time.Duration
	within, window     time.Duration
	every              time.Duration
	anon, cache        int64
	targetMB, marginMB float64
	waitQuiet          time.Duration
	out                string
	keep               bool
	keepVolume         bool
	sample             hostmon.Sampler
	residency          bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mem-reclaim:", err)
		os.Exit(1)
	}
}

func parseFlags() (*config, error) {
	c := &config{}
	fs := flag.NewFlagSet("mem-reclaim", flag.ContinueOnError)
	fs.StringVar(&c.hb.Exe, "hyperbutler", "", "hyperbutler.exe to measure (required)")
	fs.StringVar(&c.hb.Home, "home", "", "HYPERBUTLER_HOME of a bench installation (required; never the user's)")
	fs.StringVar(&c.hb.ImageDir, "image-dir", "", "guest image directory (--image-dir; default next to hyperbutler.exe)")
	fs.StringVar(&c.docker, "docker", "", "stock docker.exe (required)")
	fs.StringVar(&c.guest, "guest", "", "prebuilt linux/amd64 bench/mem-guest (default: go build it)")
	fs.StringVar(&c.work, "work", "", "working directory for the guest binary and image tar (default: a new temp dir)")
	fs.StringVar(&c.debugPipe, "debug-pipe", "", `engine diag endpoint to start with (default \\.\pipe\hyperbutler-memreclaim-<pid>)`)
	fs.StringVar(&c.pipe, "pipe", "", `Docker API pipe for the bench installation (--pipe; default: its settings.json's, else \\.\pipe\docker_engine_hyperbutler)`)
	fs.StringVar(&c.memory, "memory", "12GiB", "guest memory (REQ-MEM-002: a 12 GB maximum)")
	startArgs := fs.String("start-args", "", "extra `hyperbutler start` flags, space separated")
	fs.DurationVar(&c.settle, "settle", 3*time.Minute, "wait after `hyperbutler start` before the baseline")
	fs.DurationVar(&c.baseline, "baseline", time.Minute, "idle baseline sampled before the workload")
	fs.DurationVar(&c.within, "within", 60*time.Second, "REQ-MEM-002: host memory must be at or below the limit this long after the container exits")
	fs.DurationVar(&c.window, "window", 3*time.Minute, "how long to sample after the container exits (??-within)")
	fs.DurationVar(&c.every, "interval", 2*time.Second, "sampling interval")
	anon := fs.String("anon", "4GiB", "anonymous memory the container touches")
	cache := fs.String("cache", "4GiB", "file the container writes and reads back on a volume (page cache)")
	fs.Float64Var(&c.targetMB, "target-mb", 400, "REQ-MEM-001 target in MB (10^6 bytes): 400 for 0.1, 250 for 1.0")
	fs.Float64Var(&c.marginMB, "margin-mb", 100, "REQ-MEM-002 margin over the REQ-MEM-001 target, MB")
	fs.DurationVar(&c.waitQuiet, "wait-quiet", 30*time.Minute, "how long to wait for other VMs and engines to go away")
	fs.StringVar(&c.out, "out", "", "write every sample to this CSV (keep raw dumps out of git)")
	fs.BoolVar(&c.keep, "keep-running", false, "leave the engine running afterwards")
	fs.BoolVar(&c.keepVolume, "keep-volume", false, "leave the volume with the cache file afterwards")
	sampler := fs.String("sampler", "nt", "how counters are read: nt (in process, NtQuerySystemInformation) or cim (a PowerShell CIM query per sample, the method before 2026-10-31)")
	fs.BoolVar(&c.residency, "residency", true, "log where the engine's resident pages are (QueryWorkingSetEx) at the end of the baseline, after a sample where its working set rose by 8 MiB or more, and at the end")
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
	if c.anon, err = parseBytes(*anon); err != nil {
		return nil, fmt.Errorf("-anon: %w", err)
	}
	if c.cache, err = parseBytes(*cache); err != nil {
		return nil, fmt.Errorf("-cache: %w", err)
	}
	if c.every <= 0 || c.within <= 0 || c.window < c.within || c.baseline < c.every || c.settle < 0 {
		return nil, errors.New("need -interval > 0, -window ??-within > 0, -baseline ??-interval, -settle ??0")
	}
	if !(c.targetMB > 0 && c.targetMB < 1e7) || !(c.marginMB >= 0 && c.marginMB < 1e7) {
		return nil, errors.New("bad -target-mb or -margin-mb")
	}
	if c.debugPipe == "" {
		c.debugPipe = fmt.Sprintf(`\\.\pipe\hyperbutler-memreclaim-%d`, os.Getpid())
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
	if c.work == "" {
		if c.work, err = os.MkdirTemp("", "hyperbutler-memreclaim"); err != nil {
			return err
		}
		defer os.RemoveAll(c.work)
	}
	if up, err := c.hb.Running(); err != nil {
		return err
	} else if up {
		return fmt.Errorf("the installation in %s is already running; the bench starts it itself", c.hb.Home)
	}
	tarPath, err := c.guestTar()
	if err != nil {
		return err
	}
	ss, gi, err := measure(ctx, c, tarPath)
	if len(ss) > 0 && c.out != "" {
		if f, ferr := os.Create(c.out); ferr == nil {
			ferr = WriteCSV(f, ss)
			f.Close()
			err = errors.Join(err, ferr)
		} else {
			err = errors.Join(err, ferr)
		}
	}
	if err != nil {
		return err
	}
	gi.print(os.Stdout)
	sum := Summarize(ss, Criteria{
		Limit:    uint64((c.targetMB + c.marginMB) * 1e6),
		Within:   c.within,
		Workload: uint64(c.anon + c.cache),
	})
	sum.Print(os.Stdout)
	if !sum.Pass {
		return errors.New("REQ-MEM-002 not met: " + sum.VerdictText)
	}
	return nil
}

func logf(format string, a ...any) {
	fmt.Printf("%s "+format+"\n", append([]any{time.Now().Format(time.TimeOnly)}, a...)...)
}

const (
	benchImage  = "hyperbutler-memreclaim:dev"
	benchVolume = "hyperbutler-memreclaim"
	fillName    = "hyperbutler-memreclaim-fill"
)

// guestTar builds (or copies) mem-guest and wraps it in a tar for `docker import`.
func (c *config) guestTar() (string, error) {
	var bin []byte
	if c.guest != "" {
		b, err := os.ReadFile(c.guest)
		if err != nil {
			return "", err
		}
		bin = b
	} else {
		_, self, _, _ := runtime.Caller(0)
		p := filepath.Join(c.work, "mem-guest")
		cmd := exec.Command("go", "build", "-trimpath", "-o", p, ".")
		cmd.Dir = filepath.Join(filepath.Dir(self), "..", "mem-guest")
		cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("build mem-guest (or pass -guest): %v: %s", err, out)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		bin = b
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	epoch := time.Unix(0, 0)
	for _, h := range []*tar.Header{
		{Name: "data/", Mode: 0o755, Typeflag: tar.TypeDir, ModTime: epoch},
		{Name: "mem-guest", Mode: 0o755, Typeflag: tar.TypeReg, Size: int64(len(bin)), ModTime: epoch},
	} {
		if err := tw.WriteHeader(h); err != nil {
			return "", err
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(bin); err != nil {
				return "", err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	p := filepath.Join(c.work, "mem-guest.tar")
	return p, os.WriteFile(p, buf.Bytes(), 0o644)
}

// guestInfo is what the guest itself reported.
type guestInfo struct {
	peak, end *guestResult
	logLines  []string
}

type guestResult struct {
	AnonBytes, CacheBytes int64
	AnonSecs, CacheSecs   float64
	Meminfo               map[string]uint64
}

func (g *guestInfo) print(w io.Writer) {
	show := func(name string, r *guestResult) {
		if r == nil {
			return
		}
		m := r.Meminfo
		fmt.Fprintf(w, "guest %-5s MemTotal %s MemFree %s Cached %s AnonPages %s Shmem %s file LRU %s Slab %s\n", name,
			mib(float64(m["MemTotal"])), mib(float64(m["MemFree"])), mib(float64(m["Cached"])), mib(float64(m["AnonPages"])),
			mib(float64(m["Shmem"])), mib(float64(m["Active(file)"]+m["Inactive(file)"])), mib(float64(m["Slab"])))
	}
	if g.peak != nil {
		fmt.Fprintf(w, "\nfill: %s anonymous in %.1f s, %s written+read in %.1f s\n", mib(float64(g.peak.AnonBytes)), g.peak.AnonSecs,
			mib(float64(g.peak.CacheBytes)), g.peak.CacheSecs)
	}
	show("peak", g.peak)
	show("end", g.end)
	if len(g.logLines) > 0 {
		fmt.Fprintln(w, "engine log during the run (guest page-cache reclaim, guest memory, working-set guard):")
		for _, l := range g.logLines {
			fmt.Fprintln(w, "  "+l)
		}
	}
}

func parseGuest(out string) (*guestResult, error) {
	var r guestResult
	line := strings.TrimSpace(out)
	if i := strings.LastIndexByte(line, '\n'); i >= 0 {
		line = line[i+1:]
	}
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		return nil, fmt.Errorf("mem-guest output %q: %w", line, err)
	}
	return &r, nil
}

// measure does the run: start, settle, baseline, workload, reclaim window, stop.
func measure(ctx context.Context, c *config, tarPath string) (ss []Sample, gi guestInfo, err error) {
	pipe := c.pipe
	if pipe == "" {
		pipe = c.hb.DockerPipe()
	}
	f, busy, err := hostmon.WaitQuiet(ctx, pipe, 15*time.Second, c.waitQuiet, logf)
	if err != nil {
		return nil, gi, err
	}
	if busy {
		return nil, gi, fmt.Errorf("the Docker API pipe %s is still in use after %s", pipe, c.waitQuiet)
	}
	if f.Any() {
		logf("host still not quiet after %s (%s); measuring anyway, the run will be marked overlapped", c.waitQuiet, f)
	}
	before, err := hostmon.Snapshot()
	if err != nil {
		return nil, gi, err
	}
	watch := hostmon.Watch(time.Second)
	defer watch.Stop()
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
	args := append([]string{"--debug-pipe", c.debugPipe, "--memory", c.memory, "--pipe", pipe}, c.startArgs...)
	out, err := c.hb.Start(ctx, args...)
	if err != nil {
		return nil, gi, err
	}
	logf("%s", out)
	st, err := c.hb.State()
	if err != nil {
		return nil, gi, err
	}
	enginePID := uint32(st.PID)
	eh, err := hostmon.Pin(enginePID)
	if err != nil {
		return nil, gi, err
	}
	defer windows.CloseHandle(eh)
	after, err := hostmon.Snapshot()
	if err != nil {
		return nil, gi, err
	}
	vm, err := hostmon.FindNewVM(before, after)
	if err != nil {
		return nil, gi, err
	}
	logf("engine PID %d, vmwp PID %d, vmmem PID %d", enginePID, vm.VMWP, vm.VMMem)
	engineExe, err := hostmon.ImagePath(enginePID)
	if err != nil {
		return nil, gi, err
	}
	residency := func(why string) {
		if !c.residency {
			return
		}
		r, err := hostmon.ResidencyOf(enginePID, engineExe)
		if err != nil {
			logf("engine residency (%s): %v", why, err)
			return
		}
		logf("engine residency (%s): %s", why, r)
	}
	watch.SetOurs(enginePID, vm.VMMem, vm.VMWP)

	cfgDir := filepath.Join(c.work, "docker-config")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		return nil, gi, err
	}
	d := &hostmon.Docker{Exe: c.docker, Host: hostmon.NpipeHost(pipe), ConfigDir: cfgDir}
	if out, err := d.Run(ctx, "ps", "-q"); err != nil {
		return nil, gi, err
	} else if strings.TrimSpace(out) != "" {
		return nil, gi, errors.New("containers are running in the bench installation")
	}
	if _, err := d.Run(ctx, "import", tarPath, benchImage); err != nil {
		return nil, gi, err
	}
	_, _ = d.Run(ctx, "volume", "rm", "-f", benchVolume)
	if _, err := d.Run(ctx, "volume", "create", benchVolume); err != nil {
		return nil, gi, err
	}
	if !c.keepVolume {
		defer func() {
			_, _ = d.Run(context.WithoutCancel(ctx), "volume", "rm", "-f", benchVolume)
		}()
	}

	logPath := c.hb.Layout().EngineLogPath()
	logOff := fileSize(logPath)

	type stamped struct {
		at time.Time
		s  Sample
	}
	var raw []stamped
	take := func(ph Phase) error {
		if hostmon.Exited(eh) {
			return errors.New("the engine exited")
		}
		ps, err := hostmon.Snapshot()
		if err != nil {
			return err
		}
		if !hostmon.StillOurs(ps, vm) {
			return errors.New("this VM's vmmem/vmwp are gone (or their PIDs were reused)")
		}
		helpers := hostmon.ChildrenOf(ps, enginePID)
		pids := append([]uint32{enginePID, vm.VMMem, vm.VMWP}, helpers...)
		at := time.Now()
		sm, err := c.sample(ctx, pids)
		if err != nil {
			return err
		}
		s := Sample{Phase: ph}
		for _, p := range []struct {
			pid uint32
			dst *uint64
		}{{enginePID, &s.EngineWS}, {vm.VMMem, &s.VMMemWS}, {vm.VMWP, &s.VMWPWS}} {
			v, ok := sm[p.pid]
			if !ok {
				return fmt.Errorf("PID %d is gone from the counters", p.pid)
			}
			*p.dst = v.WorkingSetPriv
		}
		s.VMMemPriv = sm[vm.VMMem].PrivateBytes
		s.EnginePriv, s.EngineFaults = sm[enginePID].PrivateBytes, sm[enginePID].PageFaults
		for _, h := range helpers {
			s.HelpersWS += sm[h].WorkingSetPriv
		}
		s.Foreign = watch.Take()
		var prev *Sample
		if len(raw) > 0 {
			prev = &raw[len(raw)-1].s
		}
		raw = append(raw, stamped{at, s})
		faults := ""
		if prev != nil && s.EngineFaults > 0 {
			faults = fmt.Sprintf(" faults +%d", s.EngineFaults-prev.EngineFaults)
		}
		logf("%-8s engine %s (commit %s%s) vmmem %s (private bytes %s) vmwp %s total %s%s", ph, mib(float64(s.EngineWS)), mib(float64(s.EnginePriv)), faults,
			mib(float64(s.VMMemWS)), mib(float64(s.VMMemPriv)), mib(float64(s.VMWPWS)), mib(float64(s.Total())), note(s.Foreign))
		if prev != nil && s.EngineWS >= prev.EngineWS+jumpBytes {
			residency(fmt.Sprintf("engine +%s since the previous sample", mib(float64(s.EngineWS-prev.EngineWS))))
		}
		return nil
	}
	// finish converts the timestamps to seconds since the exit.
	var exit time.Time
	finish := func() []Sample {
		out := make([]Sample, len(raw))
		for i, r := range raw {
			out[i] = r.s
			if !exit.IsZero() {
				out[i].T = r.at.Sub(exit).Seconds()
				// A workload sample whose query started after the exit belongs to the window.
				if out[i].Phase == PhaseWorkload && r.at.After(exit) {
					out[i].Phase = PhaseAfter
				}
			} else {
				out[i].T = math.NaN()
			}
		}
		return out
	}
	tick := func(until time.Time, ph Phase, done <-chan struct{}) error {
		for {
			next := time.Now().Add(c.every)
			if err := take(ph); err != nil {
				return err
			}
			if !time.Now().Before(until) {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-done:
				return nil
			case <-time.After(time.Until(next)):
			}
		}
	}

	logf("settling until %s after start", c.settle)
	select {
	case <-ctx.Done():
		return nil, gi, ctx.Err()
	case <-time.After(time.Until(t0.Add(c.settle))):
	}
	if err := tick(time.Now().Add(c.baseline), PhaseBaseline, nil); err != nil {
		return finish(), gi, err
	}
	residency("end of the baseline")

	logf("running the fill container: %s anonymous + %s page cache", mib(float64(c.anon)), mib(float64(c.cache)))
	type result struct {
		out string
		err error
		at  time.Time // when docker run returned: the container had exited
	}
	defer func() {
		_, _ = d.Run(context.WithoutCancel(ctx), "rm", "-f", fillName)
	}()
	done := make(chan struct{})
	res := make(chan result, 1)
	go func() {
		defer close(done)
		out, err := d.Run(ctx, "run", "--rm", "--name", fillName, "-v", benchVolume+":/data", "--entrypoint", "/mem-guest", benchImage, "fill",
			"-anon", fmt.Sprint(c.anon), "-cache", fmt.Sprint(c.cache), "-dir", "/data")
		res <- result{out, err, time.Now()}
	}()
	terr := tick(time.Now().Add(24*time.Hour), PhaseWorkload, done)
	<-done
	r := <-res
	exit = r.at
	if terr != nil {
		return finish(), gi, terr
	}
	if r.err != nil {
		return finish(), gi, r.err
	}
	if gi.peak, err = parseGuest(r.out); err != nil {
		return finish(), gi, err
	}
	logf("fill container exited")
	if err := tick(exit.Add(c.window), PhaseAfter, nil); err != nil {
		return finish(), gi, err
	}
	residency("end of the window")
	// The guest's own view after the window (a small container; not sampled).
	if out, err := d.Run(ctx, "run", "--rm", "--entrypoint", "/mem-guest", benchImage, "meminfo"); err == nil {
		gi.end, _ = parseGuest(out)
	} else {
		logf("guest meminfo: %v", err)
	}
	gi.logLines = engineLog(logPath, logOff)
	return finish(), gi, nil
}

// jumpBytes is the engine working-set rise between two samples that triggers a
// residency walk.
const jumpBytes = 8 << 20

func note(f string) string {
	if f == "" {
		return ""
	}
	return "  [foreign: " + f + "]"
}

func fileSize(p string) int64 {
	if fi, err := os.Stat(p); err == nil {
		return fi.Size()
	}
	return 0
}

// engineLog returns the engine log lines about guest memory written since off.
func engineLog(p string, off int64) []string {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(f, 4<<20))
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "guest page-cache reclaim") || strings.Contains(l, "guest memory") || strings.Contains(l, "working set emptied") {
			out = append(out, strings.TrimSpace(l))
		}
	}
	return out
}

// parseBytes reads a byte count with an optional KiB/MiB/GiB suffix.
func parseBytes(s string) (int64, error) {
	n, err := engine.ParseSize(s)
	if err != nil {
		return 0, err
	}
	if n > 1<<40 {
		return 0, fmt.Errorf("%s is more than 1 TiB", s)
	}
	return int64(n), nil
}
