//go:build windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/bench/internal/hostmon"
	"github.com/Higangssh/hyperbutler/bench/internal/netbench"
)

type config struct {
	docker, host, image, guest, work string
	virtual                          string
	port, pubPort                    int
	rounds, urlRounds                int
	secs                             float64
	rrN                              int
	cpsPar                           int
	url                              string
	enginePID                        uint
	debugPipe                        string
	profileDir                       string
	jsonOut                          string
	only                             string
	network                          string
	trace                            bool
	snapshot                         string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "net-perf:", err)
		os.Exit(1)
	}
}

func run() error {
	var c config
	fs := flag.NewFlagSet("net-perf", flag.ExitOnError)
	fs.StringVar(&c.docker, "docker", "docker", "docker CLI")
	fs.StringVar(&c.host, "host", "npipe:////./pipe/docker_engine_hyperbutler", "docker -H endpoint (the user's docker context is never used or changed)")
	fs.StringVar(&c.image, "image", "alpine:3.20", "base image for the bench container")
	fs.StringVar(&c.guest, "guest", "", "prebuilt linux/amd64 bench/net-guest (default: go build it)")
	fs.StringVar(&c.work, "work", filepath.Join(os.TempDir(), "hyperbutler-netperf"), "working directory")
	fs.StringVar(&c.virtual, "virtual", "192.168.127.254", "guest-side address of the engine's -host-forward entries for -port..-port+2")
	fs.IntVar(&c.port, "port", 5301, "host servers on 127.0.0.1: source, sink, echo and ping at port..port+3")
	fs.IntVar(&c.pubPort, "pub-port", 5311, "host ports published with docker run -p for the inbound tests (pub-port..pub-port+3)")
	fs.IntVar(&c.rounds, "rounds", 5, "runs per measurement and side; medians are reported")
	fs.IntVar(&c.urlRounds, "url-rounds", 3, "runs of the internet download per side")
	fs.Float64Var(&c.secs, "secs", 5, "seconds per throughput and connection-rate run")
	fs.IntVar(&c.rrN, "rr-n", 20000, "round trips per latency run")
	fs.IntVar(&c.cpsPar, "cps-par", 8, "parallel connectors in the connection-rate test")
	fs.StringVar(&c.url, "url", "https://proof.ovh.net/files/100Mb.dat", "internet download (empty: skip)")
	fs.UintVar(&c.enginePID, "engine-pid", 0, "engine process to measure (default: the one hyperbutler-dev.exe or hyperbutler.exe engine)")
	fs.StringVar(&c.debugPipe, "debug-pipe", "", "the engine's internal/diag pipe, for GC counts and -profile")
	fs.StringVar(&c.profileDir, "profile", "", "with -debug-pipe: save a CPU profile of each container run of round 1 here")
	fs.StringVar(&c.jsonOut, "json", "", "write every run as JSON here (keep raw results out of git)")
	fs.BoolVar(&c.trace, "trace", false, "with -profile: save a 1 s execution trace from the middle of each run instead of a CPU profile")
	fs.StringVar(&c.snapshot, "snapshot", "", "with -profile: save this named profile (heap, goroutine, ...) from the middle of each run instead of a CPU profile")
	fs.StringVar(&c.network, "network", "bridge", "docker run --network of the client containers")
	fs.StringVar(&c.only, "only", "", "comma-separated tests to run (dl,ul,rr,rr-load,cps,pub-dl,pub-ul,pub-rr,pub-cps,url); default all")
	_ = fs.Parse(os.Args[1:])
	if c.rounds < 1 || c.secs <= 0 || c.port <= 0 || c.port > 65532 || c.pubPort <= 0 || c.pubPort > 65532 {
		return errors.New("bad -rounds, -secs, -port or -pub-port")
	}
	ctx := context.Background()
	if err := c.prepare(); err != nil {
		return err
	}
	pid, err := c.findEngine()
	if err != nil {
		return err
	}
	lns, err := netbench.Listen("127.0.0.1", c.port)
	if err != nil {
		return fmt.Errorf("host servers on 127.0.0.1:%d..%d: %w", c.port, c.port+netbench.Kinds-1, err)
	}
	defer func() {
		for _, l := range lns {
			_ = l.Close()
		}
	}()
	mon := &monitor{pid: pid, pipe: c.debugPipe, trace: c.trace, snapshot: c.snapshot}
	b := &bench{c: c, mon: mon}
	// The container must reach the host servers through the engine's host forwards.
	if _, err := b.container("download", "-addr", b.virt(netbench.Source), "-secs", "0.2"); err != nil {
		return fmt.Errorf("a container cannot reach the host servers; start the engine with -host-forward %s:%d=127.0.0.1:%d (and +1, +2, +3): %w",
			c.virtual, c.port, c.port, err)
	}
	if err := b.all(ctx); err != nil {
		return err
	}
	b.report(os.Stdout)
	if c.jsonOut != "" {
		data, _ := json.MarshalIndent(b.runs, "", "  ")
		if err := os.WriteFile(c.jsonOut, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (c *config) want(test string) bool {
	if c.only == "" {
		return true
	}
	for _, t := range strings.Split(c.only, ",") {
		if strings.TrimSpace(t) == test {
			return true
		}
	}
	return false
}

// prepare builds net-guest and an image that contains it.
func (c *config) prepare() error {
	if err := os.MkdirAll(filepath.Join(c.work, "bin"), 0o755); err != nil {
		return err
	}
	bin := filepath.Join(c.work, "bin", "net-guest")
	if c.guest != "" {
		data, err := os.ReadFile(c.guest)
		if err != nil {
			return err
		}
		if err := os.WriteFile(bin, data, 0o755); err != nil {
			return err
		}
	} else {
		_, self, _, _ := runtime.Caller(0)
		cmd := exec.Command("go", "build", "-trimpath", "-o", bin, ".")
		cmd.Dir = filepath.Join(filepath.Dir(self), "..", "net-guest")
		cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("build net-guest (or pass -guest): %v: %s", err, out)
		}
	}
	if _, err := c.dockerCmd("image", "inspect", c.image); err != nil {
		if _, err := c.dockerCmd("pull", c.image); err != nil {
			return err
		}
	}
	_, _ = c.dockerCmd("rm", "-f", "hyperbutler-netperf-build")
	if _, err := c.dockerCmd("create", "--name", "hyperbutler-netperf-build", c.image, "true"); err != nil {
		return err
	}
	defer func() { _, _ = c.dockerCmd("rm", "-f", "hyperbutler-netperf-build") }()
	if _, err := c.dockerCmd("cp", bin, "hyperbutler-netperf-build:/usr/local/bin/net-guest"); err != nil {
		return err
	}
	_, err := c.dockerCmd("commit", "hyperbutler-netperf-build", benchImage)
	return err
}

const benchImage = "hyperbutler-netperf:dev"

func (c *config) dockerCmd(args ...string) ([]byte, error) {
	cmd := exec.Command(c.docker, append([]string{"-H", c.host}, args...)...)
	cmd.Env = append(os.Environ(), "DOCKER_CONFIG="+filepath.Join(c.work, "docker-config"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("docker %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (c *config) findEngine() (uint32, error) {
	if c.enginePID != 0 {
		return uint32(c.enginePID), nil
	}
	ps, err := hostmon.Snapshot()
	if err != nil {
		return 0, err
	}
	var found []hostmon.Proc
	for _, p := range ps {
		if strings.EqualFold(p.Name, hostmon.DevName) || strings.EqualFold(p.Name, hostmon.EngineName) {
			found = append(found, p)
		}
	}
	// `hyperbutler start` leaves only the engine running; a CLI may be momentarily there.
	if len(found) != 1 {
		return 0, fmt.Errorf("found %d hyperbutler processes; pass -engine-pid", len(found))
	}
	return found[0].PID, nil
}

// --- measurement --------------------------------------------------------------------

type bench struct {
	c    config
	mon  *monitor
	runs []runRec
}

func (b *bench) virt(kind int) string {
	return net.JoinHostPort(b.c.virtual, strconv.Itoa(b.c.port+kind))
}
func (b *bench) local(kind int) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(b.c.port+kind))
}
func (b *bench) pub(kind int) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(b.c.pubPort+kind))
}

func (b *bench) secs() string { return strconv.FormatFloat(b.c.secs, 'f', -1, 64) }
func (b *bench) dur() time.Duration {
	return time.Duration(b.c.secs * float64(time.Second))
}

// container runs net-guest in a fresh container and parses its result.
func (b *bench) container(args ...string) (netbench.Result, error) {
	out, err := b.c.dockerCmd(append([]string{"run", "--rm", "--network", b.c.network, benchImage, "net-guest"}, args...)...)
	if err != nil {
		return netbench.Result{}, err
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) > 0 && line[0] == '{' {
			return netbench.ParseResult(line)
		}
	}
	return netbench.Result{}, fmt.Errorf("no result in %q", out)
}

func (b *bench) record(test, side string, round int, profile bool, f func() (netbench.Result, error)) error {
	var stop func() monSample
	if side != "host" {
		var prof string
		if profile && round == 1 && b.c.profileDir != "" && b.c.debugPipe != "" {
			prof = filepath.Join(b.c.profileDir, test+".pprof")
		}
		stop = b.mon.start(prof, b.c.secs)
	}
	r, err := f()
	rec := runRec{Test: test, Side: side, Round: round, Result: r}
	if stop != nil {
		s := stop()
		rec.EngineCPU, rec.EngineWS, rec.EnginePriv, rec.EngineHeap, rec.GCs, rec.GCPauseMs = s.cores, s.maxWS, s.maxPriv, s.maxHeap, s.gcs, s.pauseMs
	}
	if err != nil {
		return fmt.Errorf("%s (%s, round %d): %w", test, side, round, err)
	}
	b.runs = append(b.runs, rec)
	fmt.Fprintf(os.Stderr, "%-8s %-9s round %d: %s cpu %.2f ws %.0f MB gc %d\n", test, side, round, summary(r), rec.EngineCPU, float64(rec.EngineWS)/1e6, rec.GCs)
	return nil
}

func summary(r netbench.Result) string {
	switch {
	case r.Gbit > 0:
		return fmt.Sprintf("%.2f Gbit/s", r.Gbit)
	case r.ConnsPS > 0:
		return fmt.Sprintf("%.0f conn/s (%d errors %s)", r.ConnsPS, r.Errors, r.FirstError)
	case r.N > 0:
		return fmt.Sprintf("p50 %.0f p99 %.0f p99.9 %.0f max %.0f us", r.P50us, r.P99us, r.P999us, r.MaxUs)
	}
	return "?"
}

func (b *bench) all(ctx context.Context) error {
	rr := strconv.Itoa(b.c.rrN)
	type test struct {
		name    string
		host    func() (netbench.Result, error)
		guest   []string
		profile bool
	}
	tests := []test{
		{"dl", func() (netbench.Result, error) { return netbench.Download(ctx, b.local(netbench.Source), b.dur()) },
			[]string{"download", "-addr", b.virt(netbench.Source), "-secs", b.secs()}, true},
		{"ul", func() (netbench.Result, error) { return netbench.Upload(ctx, b.local(netbench.Sink), b.dur()) },
			[]string{"upload", "-addr", b.virt(netbench.Sink), "-secs", b.secs()}, true},
		{"rr", func() (netbench.Result, error) {
			return netbench.RoundTrips(ctx, b.local(netbench.Echo), b.c.rrN, 64, "")
		}, []string{"rr", "-addr", b.virt(netbench.Echo), "-n", rr}, false},
		{"rr-load", func() (netbench.Result, error) {
			return netbench.RoundTrips(ctx, b.local(netbench.Echo), b.c.rrN, 64, b.local(netbench.Source))
		}, []string{"rr", "-addr", b.virt(netbench.Echo), "-n", rr, "-load", b.virt(netbench.Source)}, true},
		{"cps", func() (netbench.Result, error) {
			return netbench.ConnsPerSecond(ctx, b.local(netbench.Ping), b.dur(), b.c.cpsPar)
		}, []string{"cps", "-addr", b.virt(netbench.Ping), "-secs", b.secs(), "-par", strconv.Itoa(b.c.cpsPar)}, true},
	}
	for _, t := range tests {
		if !b.c.want(t.name) {
			continue
		}
		for i := 1; i <= b.c.rounds; i++ {
			if err := b.record(t.name, "host", i, false, t.host); err != nil {
				return err
			}
			g := t.guest
			if err := b.record(t.name, "container", i, t.profile, func() (netbench.Result, error) { return b.container(g...) }); err != nil {
				return err
			}
		}
	}
	if err := b.inbound(ctx); err != nil {
		return err
	}
	if b.c.url != "" && b.c.want("url") {
		for i := 1; i <= b.c.urlRounds; i++ {
			if err := b.record("url", "host", i, false, func() (netbench.Result, error) { return netbench.FetchURL(ctx, b.c.url) }); err != nil {
				return err
			}
			if err := b.record("url", "container", i, false, func() (netbench.Result, error) { return b.container("url", "-url", b.c.url) }); err != nil {
				return err
			}
		}
	}
	return nil
}

// inbound measures `docker run -p`: a container serves, the host connects to the
// published loopback ports. The baseline is the same client against the host's own
// servers.
func (b *bench) inbound(ctx context.Context) error {
	var any bool
	for _, t := range []string{"pub-dl", "pub-ul", "pub-rr", "pub-cps"} {
		any = any || b.c.want(t)
	}
	if !any {
		return nil
	}
	const name = "hyperbutler-netperf-serve"
	_, _ = b.c.dockerCmd("rm", "-f", name)
	args := []string{"run", "-d", "--name", name}
	for k := 0; k < netbench.Kinds; k++ {
		// A wildcard binding in the guest; auto-publish mirrors it on the host's loopback.
		args = append(args, "-p", fmt.Sprintf("%d:%d", b.c.pubPort+k, 5301+k))
	}
	args = append(args, benchImage, "net-guest", "serve", "-port", "5301")
	if _, err := b.c.dockerCmd(args...); err != nil {
		return err
	}
	defer func() { _, _ = b.c.dockerCmd("rm", "-f", name) }()
	// Wait for auto-publish and the server.
	deadline := time.Now().Add(20 * time.Second)
	for {
		r, err := netbench.RoundTrips(ctx, b.pub(netbench.Echo), 10, 1, "")
		if err == nil && r.N > 0 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("published port %s never answered: %v", b.pub(netbench.Echo), err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	type test struct {
		name    string
		f       func(addr func(int) string) (netbench.Result, error)
		profile bool
	}
	tests := []test{
		{"pub-dl", func(a func(int) string) (netbench.Result, error) {
			return netbench.Download(ctx, a(netbench.Source), b.dur())
		}, true},
		{"pub-ul", func(a func(int) string) (netbench.Result, error) {
			return netbench.Upload(ctx, a(netbench.Sink), b.dur())
		}, true},
		{"pub-rr", func(a func(int) string) (netbench.Result, error) {
			return netbench.RoundTrips(ctx, a(netbench.Echo), b.c.rrN, 64, "")
		}, false},
		{"pub-cps", func(a func(int) string) (netbench.Result, error) {
			return netbench.ConnsPerSecond(ctx, a(netbench.Ping), b.dur(), b.c.cpsPar)
		}, true},
	}
	for _, t := range tests {
		if !b.c.want(t.name) {
			continue
		}
		for i := 1; i <= b.c.rounds; i++ {
			if err := b.record(t.name, "host", i, false, func() (netbench.Result, error) { return t.f(b.local) }); err != nil {
				return err
			}
			if err := b.record(t.name, "container", i, t.profile, func() (netbench.Result, error) { return t.f(b.pub) }); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- engine monitor -------------------------------------------------------------------

type monitor struct {
	trace    bool
	snapshot string
	pid      uint32
	pipe     string
}

type monSample struct {
	cores   float64
	maxWS   uint64
	maxPriv uint64
	maxHeap uint64
	gcs     uint32
	pauseMs float64
}

// start begins sampling the engine: CPU time over the run, the largest private working
// set seen, and (with the diag pipe) GC counts. With prof set it also saves a CPU
// profile of the first secs-1 seconds.
func (m *monitor) start(prof string, secs float64) func() monSample {
	cpu0, _ := processCPU(m.pid)
	t0 := time.Now()
	var dc *hostmon.DiagClient
	var gc0 uint32
	var pause0 uint64
	if m.pipe != "" {
		dc = hostmon.NewDiagClient(m.pipe)
		if st, err := dc.Stats(context.Background()); err == nil {
			gc0, pause0 = st.MemStats.NumGC, st.MemStats.PauseTotalNs
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var mu sync.Mutex
	var maxWS, maxPriv, maxHeap uint64
	// The private working set comes from a CIM query, which starts a PowerShell: once in
	// the middle of the run, so that it disturbs the measurement little. The diag
	// endpoint, when there is one, is polled for private bytes throughout.
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(secs * float64(time.Second) / 2)):
		}
		if s, err := hostmon.SampleNT(ctx, []uint32{m.pid}); err == nil {
			mu.Lock()
			maxWS = max(maxWS, s[m.pid].WorkingSetPriv)
			mu.Unlock()
		}
	}()
	if dc != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if st, err := dc.Stats(ctx); err == nil && st.OS != nil {
					mu.Lock()
					maxPriv = max(maxPriv, st.OS.PrivateBytes)
					maxHeap = max(maxHeap, st.MemStats.HeapInuse)
					mu.Unlock()
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(250 * time.Millisecond):
				}
			}
		}()
	}
	if prof != "" && dc != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path := "/debug/pprof/profile?seconds=" + strconv.Itoa(max(1, int(secs)-1))
			if m.trace {
				// One second of execution trace, from the middle of the run.
				time.Sleep(time.Duration(secs * float64(time.Second) / 3))
				path, prof = "/debug/pprof/trace?seconds=1", strings.TrimSuffix(prof, ".pprof")+".trace"
			}
			if m.snapshot != "" {
				// A snapshot profile from the middle of the run.
				time.Sleep(time.Duration(secs * float64(time.Second) / 2))
				path, prof = "/debug/pprof/"+url.PathEscape(m.snapshot), strings.TrimSuffix(prof, ".pprof")+"."+m.snapshot
			}
			data, err := dc.Get(context.Background(), path)
			if err == nil {
				_ = os.MkdirAll(filepath.Dir(prof), 0o755)
				err = os.WriteFile(prof, data, 0o644)
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, "profile:", err)
			}
		}()
	}
	return func() monSample {
		cpu1, _ := processCPU(m.pid)
		el := time.Since(t0)
		var out monSample
		out.cores = (cpu1 - cpu0).Seconds() / el.Seconds()
		if dc != nil {
			if st, err := dc.Stats(context.Background()); err == nil {
				out.gcs = st.MemStats.NumGC - gc0
				out.pauseMs = float64(st.MemStats.PauseTotalNs-pause0) / 1e6
			}
		}
		// One last working-set sample after the run, then stop.
		if s, err := hostmon.SampleNT(context.Background(), []uint32{m.pid}); err == nil {
			mu.Lock()
			maxWS = max(maxWS, s[m.pid].WorkingSetPriv)
			mu.Unlock()
		}
		cancel()
		wg.Wait()
		if dc != nil {
			dc.Close()
		}
		out.maxWS, out.maxPriv, out.maxHeap = maxWS, maxPriv, maxHeap
		return out
	}
}

func processCPU(pid uint32) (time.Duration, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	var c, e, k, u windows.Filetime
	if err := windows.GetProcessTimes(h, &c, &e, &k, &u); err != nil {
		return 0, err
	}
	ft := func(f windows.Filetime) time.Duration {
		return time.Duration((int64(f.HighDateTime)<<32 | int64(f.LowDateTime)) * 100)
	}
	return ft(k) + ft(u), nil
}

// --- report ---------------------------------------------------------------------------

func (b *bench) report(w io.Writer) {
	fmt.Fprint(w, Report(b.runs))
}
