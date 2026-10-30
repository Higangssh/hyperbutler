//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	hb          hostmon.Hyperbutler
	docker      string
	n, warmup   int
	maxAttempts int
	timeout     time.Duration
	waitQuiet   time.Duration
	lim         Limits
	startArgs   []string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		os.Exit(1)
	}
}

func parseFlags() (*config, error) {
	c := &config{}
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.StringVar(&c.hb.Exe, "hyperbutler", "", "hyperbutler.exe to measure (required)")
	fs.StringVar(&c.hb.Home, "home", "", "HYPERBUTLER_HOME of a bench installation (required; never the user's)")
	fs.StringVar(&c.hb.ImageDir, "image-dir", "", "guest image directory (--image-dir)")
	fs.StringVar(&c.docker, "docker", "", "stock docker.exe (required)")
	fs.IntVar(&c.n, "n", 20, "valid runs to collect")
	fs.IntVar(&c.warmup, "warmup", 1, "warm-up runs to discard first (REQ-START-001: one per boot)")
	fs.IntVar(&c.maxAttempts, "max-attempts", 0, "give up after this many runs (default 3 × (n + warmup))")
	fs.DurationVar(&c.timeout, "timeout", 2*time.Minute, "per start")
	fs.DurationVar(&c.waitQuiet, "wait-quiet", 10*time.Minute, "how long to wait for other VMs and engines to go away before each run")
	fs.DurationVar(&c.lim.Median, "median", 8*time.Second, "median limit (0.1: 8s, 1.0: 5s)")
	fs.DurationVar(&c.lim.P95, "p95", 12*time.Second, "p95 limit (0.1: 12s, 1.0: 8s)")
	startArgs := fs.String("start-args", "", "extra `hyperbutler start` flags, space separated")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return nil, err
	}
	if c.hb.Exe == "" || c.hb.Home == "" || c.docker == "" {
		return nil, errors.New("-hyperbutler, -home and -docker are required")
	}
	if c.n < 1 || c.warmup < 0 {
		return nil, errors.New("need -n >= 1 and -warmup >= 0")
	}
	if c.maxAttempts == 0 {
		c.maxAttempts = 3 * (c.n + c.warmup)
	}
	c.startArgs = strings.Fields(*startArgs)
	return c, nil
}

func logf(format string, a ...any) {
	fmt.Printf("%s "+format+"\n", append([]any{time.Now().Format(time.TimeOnly)}, a...)...)
}

func run() error {
	c, err := parseFlags()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cfgDir, err := os.MkdirTemp("", "hyperbutler-start-docker-config")
	if err != nil {
		return err
	}
	defer os.RemoveAll(cfgDir)
	if up, err := c.hb.Running(); err != nil {
		return err
	} else if up {
		logf("the bench installation is running; stopping it first")
		if _, err := c.hb.Stop(ctx); err != nil {
			return err
		}
	}
	firstBoot, uptime := bootMarker(c.hb.Home)
	logf("host uptime %s; first start since boot as far as this installation knows: %v", uptime.Round(time.Second), firstBoot)

	var runs []Run
	valid, warm := 0, 0
	for attempt := 0; valid < c.n && attempt < c.maxAttempts; attempt++ {
		r := once(ctx, c, cfgDir)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if firstBoot && attempt == 0 {
			r.FirstBoot = true
		}
		if warm < c.warmup && r.Err == "" && r.Foreign == "" {
			r.Warmup = true
			warm++
		}
		if r.Valid() {
			valid++
		}
		logf("run %d: %.2f s (ready %d ms, stop %.2f s) %s%s", attempt+1, r.Seconds, r.ReadyMs, r.StopSecs, r.Err, foreignNote(r.Foreign))
		runs = append(runs, r)
		if r.Err != "" {
			break
		}
	}
	res := Summarize(runs, c.n, c.lim)
	Print(os.Stdout, runs, res)
	if !res.Pass {
		return errors.New("FAIL")
	}
	return nil
}

func foreignNote(f string) string {
	if f == "" {
		return ""
	}
	return " [overlapped: " + f + "]"
}

// bootMarker reports whether this is the first time the tool runs since the host
// booted (by a marker file in home holding the boot time), and the uptime.
func bootMarker(home string) (bool, time.Duration) {
	up := time.Duration(windows.DurationSinceBoot())
	boot := time.Now().Add(-up).Truncate(time.Minute).Unix()
	p := filepath.Join(home, "bench-start-boot")
	old, _ := os.ReadFile(p)
	first := strings.TrimSpace(string(old)) != strconv.FormatInt(boot, 10)
	if first {
		_ = os.MkdirAll(home, 0o700)
		_ = os.WriteFile(p, []byte(strconv.FormatInt(boot, 10)), 0o600)
	}
	return first, up
}

// once measures one start, then stops the engine and waits for its VM to go away.
func once(ctx context.Context, c *config, cfgDir string) (r Run) {
	pipe := c.hb.DockerPipe()
	f, busy, err := hostmon.WaitQuiet(ctx, pipe, 5*time.Second, c.waitQuiet, logf)
	if err != nil {
		return Run{Err: err.Error()}
	}
	if busy {
		return Run{Err: "the Docker API pipe " + pipe + " stays in use"}
	}
	if f.Any() {
		r.Foreign = f.String()
	}
	before, err := hostmon.Snapshot()
	if err != nil {
		return Run{Err: err.Error()}
	}

	// Watch the host during the run. Our own processes are the bench's descendants
	// (the hyperbutler CLI, docker) and, once known, the engine and its VM.
	mctx, stopMon := context.WithCancel(ctx)
	var mu sync.Mutex
	seen := map[uint32]string{} // foreign candidates: PID -> image name
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		self := uint32(os.Getpid())
		for mctx.Err() == nil {
			if ps, err := hostmon.Snapshot(); err == nil {
				mine := hostmon.Descendants(ps, self)
				mu.Lock()
				for _, p := range ps {
					if !mine[p.PID] && (strings.EqualFold(p.Name, hostmon.VMMemName) || strings.EqualFold(p.Name, hostmon.VMWPName) ||
						strings.EqualFold(p.Name, hostmon.EngineName) || strings.EqualFold(p.Name, hostmon.DevName)) {
						seen[p.PID] = p.Name
					}
				}
				mu.Unlock()
			}
			select {
			case <-mctx.Done():
			case <-time.After(250 * time.Millisecond):
			}
		}
	}()

	d := &hostmon.Docker{Exe: c.docker, Host: hostmon.NpipeHost(pipe), ConfigDir: cfgDir}
	sctx, cancel := context.WithTimeout(ctx, c.timeout)
	t0 := time.Now()
	_, err = c.hb.Start(sctx, c.startArgs...)
	for err == nil {
		if _, err = d.Run(sctx, "info", "--format", "{{.ServerVersion}}"); err == nil {
			break
		}
		if sctx.Err() != nil {
			break
		}
		err = nil
		time.Sleep(50 * time.Millisecond)
	}
	r.Seconds = time.Since(t0).Seconds()
	cancel()
	if err != nil {
		r.Err = err.Error()
	}

	var ours = map[uint32]bool{}
	var vm hostmon.VMProcs
	if st, serr := c.hb.State(); serr == nil {
		ours[uint32(st.PID)] = true
		r.ReadyMs = st.ReadyMillis
	}
	if after, serr := hostmon.Snapshot(); serr == nil {
		if v, verr := hostmon.FindNewVM(before, after); verr == nil {
			vm = v
			ours[vm.VMMem], ours[vm.VMWP] = true, true
		} else if r.Err == "" {
			// Another VM started at the same moment, or ours could not be found.
			r.Foreign = joinNote(r.Foreign, verr.Error())
		}
	}

	t1 := time.Now()
	stopCtx, cancelStop := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	if _, err := c.hb.Stop(stopCtx); err != nil && r.Err == "" {
		r.Err = err.Error()
	}
	cancelStop()
	r.StopSecs = time.Since(t1).Seconds()
	// The next run must start with no VM of ours left.
	for deadline := time.Now().Add(30 * time.Second); vm.VMMem != 0 && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		ps, err := hostmon.Snapshot()
		if err != nil {
			break
		}
		gone := true
		for _, p := range ps {
			if p.PID == vm.VMMem || p.PID == vm.VMWP {
				gone = false
			}
		}
		if gone {
			break
		}
	}
	stopMon()
	wg.Wait()
	mu.Lock()
	var other []string
	for pid, name := range seen {
		if !ours[pid] {
			other = append(other, fmt.Sprintf("%s %d", name, pid))
		}
	}
	mu.Unlock()
	if len(other) > 0 {
		r.Foreign = joinNote(r.Foreign, strings.Join(other, ", "))
	}
	return r
}

func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}
