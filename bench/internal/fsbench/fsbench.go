// Package fsbench is the Windows side of the bind-mount benchmarks (bench/fs-seq,
// bench/fs-meta, bench/fs-notify; ADR 0007). It drives the stock docker CLI against a
// running hyperbutler engine and runs bench/fs-guest in containers, on a bind mount of a
// Windows directory and on a named volume (the guest's own ext4).
package fsbench

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Config is what every driver needs.
type Config struct {
	Docker string // docker.exe
	Host   string // docker -H endpoint
	Work   string // Windows working directory (bind sources live below it)
	Guest  string // a prebuilt linux/amd64 fs-guest, or "" to build it
	Image  string // container image for the measurements
	Volume string // named volume on the guest's ext4
	JSON   string // write the results here as JSON
	WSL    string // WSL distribution for the 9P baseline ("" = none)
}

// Flags registers the common flags.
func (c *Config) Flags(fs *flag.FlagSet) {
	fs.StringVar(&c.Docker, "docker", "docker", "docker CLI")
	fs.StringVar(&c.Host, "host", "npipe:////./pipe/docker_engine_hyperbutler", "docker -H endpoint (the user's docker context is never used or changed)")
	fs.StringVar(&c.Work, "work", filepath.Join(os.TempDir(), "hyperbutler-fsbench"), "Windows directory to work in; bind sources are made below it")
	fs.StringVar(&c.Guest, "guest", "", "prebuilt linux/amd64 bench/fs-guest (default: go build it)")
	fs.StringVar(&c.Image, "image", "alpine:3.20", "container image")
	fs.StringVar(&c.Volume, "volume", "hyperbutler-fsbench", "named volume used as the guest ext4 baseline (emptied first)")
	fs.StringVar(&c.JSON, "json", "", "also write the results to this JSON file (keep raw results out of git)")
	fs.StringVar(&c.WSL, "wsl", "", "WSL 2 distribution to measure the 9P baseline in (the same fs-guest on /mnt/<drive>/...); empty: no baseline")
}

// Prepare makes the work directory and the guest binary (Work/bin/fs-guest).
func (c *Config) Prepare() error {
	if err := os.MkdirAll(filepath.Join(c.Work, "bin"), 0o750); err != nil {
		return err
	}
	dst := filepath.Join(c.Work, "bin", "fs-guest")
	if c.Guest != "" {
		b, err := os.ReadFile(c.Guest)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o755) //nolint:gosec // G306: a Linux binary the container runs from the bind mount; it must stay executable
	}
	_, self, _, _ := runtime.Caller(0)
	pkg := filepath.Join(filepath.Dir(self), "..", "..", "fs-guest")
	cmd := exec.Command("go", "build", "-trimpath", "-o", dst, ".") //nolint:gosec // G204: command and arguments come from the operator's own flags or this tool's work directory; exec passes argv, no shell
	cmd.Dir = pkg
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("build fs-guest (or pass -guest): %w: %s", err, out)
	}
	return nil
}

// Docker runs one docker CLI command and returns its standard output.
func (c *Config) DockerCmd(args ...string) ([]byte, error) {
	cmd := exec.Command(c.Docker, append([]string{"-H", c.Host}, args...)...) //nolint:gosec // G204: command and arguments come from the operator's own flags or this tool's work directory; exec passes argv, no shell
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Result is one measurement line of fs-guest.
type Result map[string]any

// Name returns the measurement name.
func (r Result) Name() string { s, _ := r["name"].(string); return s }

// Num returns a numeric field (NaN if absent).
func (r Result) Num(k string) float64 {
	if v, ok := r[k].(float64); ok {
		return v
	}
	return math.NaN()
}

// Results are the lines of one run, by name.
type Results map[string]Result

// Parse reads fs-guest output.
func Parse(out []byte) (Results, error) {
	rs := Results{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var r Result
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("fs-guest output %q: %w", line, err)
		}
		rs[r.Name()] = r
	}
	if len(rs) == 0 {
		return nil, fmt.Errorf("no results in %q", out)
	}
	return rs, nil
}

// Mount is one -v or --mount argument pair for docker run.
type Mount []string

// Bind mounts a Windows directory.
func Bind(win, guest string) Mount { return Mount{"-v", win + ":" + guest} }

// Run runs fs-guest (or, with shell, a sh -c script that may call /bench/fs-guest) in a
// fresh container with the guest binary at /bench and the given mounts.
func (c *Config) Run(image string, mounts []Mount, script string) (Results, []byte, error) {
	args := []string{"run", "--rm", "-v", filepath.Join(c.Work, "bin") + ":/bench:ro"}
	for _, m := range mounts {
		args = append(args, m...)
	}
	args = append(args, "--entrypoint", "sh", image, "-c", script)
	out, err := c.DockerCmd(args...)
	if err != nil {
		return nil, out, err
	}
	rs, err := Parse(out)
	return rs, out, err
}

// ResetVolume empties the named volume.
func (c *Config) ResetVolume() error {
	_, err := c.DockerCmd("run", "--rm", "-v", c.Volume+":/v", c.Image, "sh", "-c", "rm -rf /v/* /v/.[!.]* 2>/dev/null; true")
	return err
}

// WSLRun runs fs-guest in the WSL distribution (the 9P baseline) with args, where
// winDir is the Windows directory to work in.
func (c *Config) WSLRun(winDir string, args ...string) (Results, error) {
	if c.WSL == "" {
		return nil, errors.New("no -wsl distribution")
	}
	p, err := wslPath(winDir)
	if err != nil {
		return nil, err
	}
	bin, _ := wslPath(filepath.Join(c.Work, "bin", "fs-guest"))
	full := append([]string{"-d", c.WSL, "-e", bin}, args...)
	for i, a := range full {
		full[i] = strings.ReplaceAll(a, "{dir}", p)
	}
	out, err := exec.Command("wsl.exe", full...).Output() //nolint:gosec // G204: command and arguments come from the operator's own flags or this tool's work directory; exec passes argv, no shell
	if err != nil {
		return nil, fmt.Errorf("wsl %s: %w", strings.Join(full, " "), err)
	}
	return Parse(out)
}

// wslPath turns C:\x\y into /mnt/c/x/y.
func wslPath(p string) (string, error) {
	p, err := filepath.Abs(p)
	if err != nil || len(p) < 3 || p[1] != ':' {
		return "", fmt.Errorf("not a drive path: %s", p)
	}
	return "/mnt/" + strings.ToLower(p[:1]) + filepath.ToSlash(p[2:]), nil
}

// Row is one line of a report table.
type Row struct {
	What                string
	Bind, Base, Ratio   string
	Limit, Verdict, Req string
}

// ErrIncomplete is returned by Verdict when nothing failed but a requirement row was not
// measured: such a run says nothing about that requirement.
var ErrIncomplete = errors.New("not every requirement was measured")

// Verdict is nil when every requirement row passed, an error naming the failures when
// any failed, and ErrIncomplete when the rest passed but some were not measured.
func Verdict(rows []Row) error {
	var failed, missing []string
	for _, r := range rows {
		switch {
		case r.Req == "":
		case strings.HasPrefix(r.Verdict, "FAIL"):
			failed = append(failed, r.Req+" "+r.What)
		case r.Verdict != "pass":
			missing = append(missing, r.Req+" "+r.What)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("failed: %s", strings.Join(failed, "; "))
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s", ErrIncomplete, strings.Join(missing, "; "))
	}
	return nil
}

// Exit prints err and exits: 0 for nil, 2 for ErrIncomplete, 1 otherwise.
func Exit(cmd string, err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
	if errors.Is(err, ErrIncomplete) {
		os.Exit(2)
	}
	os.Exit(1)
}

// Report prints rows as a table and returns whether every verdict passed.
func Report(w io.Writer, title string, rows []Row) bool {
	_, _ = fmt.Fprintf(w, "\n%s\n\n", title)
	_, _ = fmt.Fprintln(w, "| Requirement | Measurement | Bind mount | Baseline | Ratio | Limit | Verdict |")
	_, _ = fmt.Fprintln(w, "|---|---|---|---|---|---|---|")
	ok := true
	for _, r := range rows {
		_, _ = fmt.Fprintf(w, "| %s | %s | %s | %s | %s | %s | %s |\n", r.Req, r.What, r.Bind, r.Base, r.Ratio, r.Limit, r.Verdict)
		if strings.HasPrefix(r.Verdict, "FAIL") {
			ok = false
		}
	}
	return ok
}

// Slower returns how many times slower bind is than base for a time (bigger is worse)
// and the verdict against limit (0: informational).
func Slower(bind, base, limit float64) (string, string) {
	if math.IsNaN(bind) || math.IsNaN(base) || base <= 0 {
		return "n/a", "not measured"
	}
	r := bind / base
	if limit == 0 {
		return fmt.Sprintf("%.2f×", r), "info"
	}
	if r <= limit {
		return fmt.Sprintf("%.2f×", r), "pass"
	}
	return fmt.Sprintf("%.2f×", r), "FAIL"
}

// Faster returns how many times faster bind is than base for a time (bigger is
// better) and the verdict against a minimum.
func Faster(bind, base, min float64) (string, string) {
	if math.IsNaN(bind) || math.IsNaN(base) || bind <= 0 {
		return "n/a", "not measured (no 9P baseline: pass -wsl)"
	}
	r := base / bind
	if r >= min {
		return fmt.Sprintf("%.2f×", r), "pass"
	}
	return fmt.Sprintf("%.2f×", r), "FAIL"
}

// Dur formats seconds.
func Dur(s float64) string {
	switch {
	case math.IsNaN(s):
		return "—"
	case s < 1e-3:
		return fmt.Sprintf("%.1f µs", s*1e6)
	case s < 1:
		return fmt.Sprintf("%.1f ms", s*1e3)
	}
	return fmt.Sprintf("%.2f s", s)
}

// Quantile returns the q quantile of xs (sorted in place).
func Quantile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	sort.Float64s(xs)
	return xs[min(len(xs)-1, int(q*float64(len(xs))))]
}

// Save writes v as JSON to c.JSON if set.
func (c *Config) Save(v any) error {
	if c.JSON == "" {
		return nil
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.JSON, b, 0o600)
}

// Machine describes where the run happened, for the record.
func (c *Config) Machine() map[string]string {
	m := map[string]string{"time": time.Now().UTC().Format(time.RFC3339), "goos": runtime.GOOS}
	if out, err := c.DockerCmd("info", "--format", "{{.KernelVersion}} {{.NCPU}} CPUs {{.MemTotal}} bytes"); err == nil {
		m["guest"] = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("cmd", "/c", "ver").Output(); err == nil {
		m["windows"] = strings.TrimSpace(string(out))
	}
	return m
}

// Stream is a running container whose output is read line by line.
type Stream struct {
	Lines <-chan Line
	c     *Config
	name  string
	done  chan struct{}
}

// Line is one line of a container's output and when it arrived.
type Line struct {
	Text string
	At   time.Time
}

// Start runs script in a named container and streams its standard output and error.
func (c *Config) Start(name, image string, mounts []Mount, script string) (*Stream, error) {
	_, _ = c.DockerCmd("rm", "-f", name)
	args := []string{"-H", c.Host, "run", "--rm", "--name", name, "-v", filepath.Join(c.Work, "bin") + ":/bench:ro"}
	for _, m := range mounts {
		args = append(args, m...)
	}
	args = append(args, "--entrypoint", "sh", image, "-c", script)
	cmd := exec.Command(c.Docker, args...) //nolint:gosec // G204: command and arguments come from the operator's own flags or this tool's work directory; exec passes argv, no shell
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	lines := make(chan Line, 4096)
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			select {
			case lines <- Line{Text: sc.Text(), At: time.Now()}:
			default: // nobody is reading: drop
			}
		}
		close(lines)
	}()
	done := make(chan struct{})
	go func() {
		// The stream ends when the container does; its exit status is not the result.
		_ = cmd.Wait()
		_ = pw.Close()
		close(done)
	}()
	return &Stream{Lines: lines, c: c, name: name, done: done}, nil
}

// Wait returns the first line for which match is true, or an error after d.
func (s *Stream) Wait(d time.Duration, match func(string) bool) (Line, error) {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case l, ok := <-s.Lines:
			if !ok {
				return Line{}, errors.New("the container exited")
			}
			if match(l.Text) {
				return l, nil
			}
		case <-t.C:
			return Line{}, fmt.Errorf("nothing matched within %v", d)
		}
	}
}

// Drain discards the lines that arrived so far.
func (s *Stream) Drain() {
	for {
		select {
		case _, ok := <-s.Lines:
			if !ok {
				return
			}
		default:
			return
		}
	}
}

// Stop removes the container.
func (s *Stream) Stop() {
	_, _ = s.c.DockerCmd("rm", "-f", s.name)
	<-s.done
}
