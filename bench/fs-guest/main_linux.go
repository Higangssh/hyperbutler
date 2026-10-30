// Command fs-guest is the in-container half of the bind-mount benchmarks (bench/fs-seq,
// bench/fs-meta, bench/fs-notify; REQ-FS-001, 002, 003, 005). The Windows drivers run it
// in a container against a bind mount and against a volume on the guest's ext4, and read
// one JSON object per measurement from its standard output.
//
//	fs-guest seq    -dir D [-mb 256] [-bs 1048576]
//	fs-guest files  -dir D [-n 10000] [-size 100]
//	fs-guest tree   -dir D [-rounds 3] [-git]
//	fs-guest notify -dir D
//	fs-guest exec   -dir D -name N -- command args...
//
// The output is an interface between two programs built from the same commit, not a
// stable format.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func main() {
	if len(os.Args) < 2 {
		fail(errors.New("usage: fs-guest seq|files|tree|notify -dir D"))
	}
	fset := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	dir := fset.String("dir", "", "directory to work in")
	mb := fset.Int("mb", 256, "seq: MiB to write")
	bs := fset.Int("bs", 1<<20, "seq: bytes per write(2)")
	n := fset.Int("n", 10000, "files: how many")
	size := fset.Int("size", 100, "files: bytes per file")
	rounds := fset.Int("rounds", 3, "tree: measured rounds after the warm-up")
	git := fset.Bool("git", false, "tree: also time git status")
	name := fset.String("name", "exec", "exec: the measurement name")
	fset.Parse(os.Args[2:])
	if *dir == "" {
		fail(errors.New("-dir is required"))
	}
	var err error
	switch os.Args[1] {
	case "seq":
		err = seq(*dir, *mb, *bs)
	case "files":
		err = files(*dir, *n, *size)
	case "tree":
		err = tree(*dir, *rounds, *git)
	case "notify":
		err = notify(*dir)
	case "exec":
		err = run(*dir, *name, fset.Args())
	default:
		err = fmt.Errorf("unknown mode %q", os.Args[1])
	}
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "fs-guest:", err)
	os.Exit(1)
}

// emit prints one measurement.
func emit(name string, v map[string]any) {
	v["name"] = name
	b, _ := json.Marshal(v)
	os.Stdout.Write(append(b, '\n'))
}

func mbps(bytes int64, d time.Duration) float64 {
	return float64(bytes) / (1 << 20) / d.Seconds()
}

// seq writes a file sequentially (with fsync), then reads it back from the guest's
// cache, then reads it again after dropping its cached pages.
func seq(dir string, mb, bs int) error {
	p := filepath.Join(dir, "fs-guest-seq.bin")
	defer os.Remove(p)
	buf := make([]byte, bs)
	for i := range buf {
		buf[i] = byte(i * 7)
	}
	total := int64(mb) << 20
	f, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	t0 := time.Now()
	for w := int64(0); w < total; w += int64(bs) {
		if _, err := f.Write(buf[:min(int64(bs), total-w)]); err != nil {
			f.Close()
			return err
		}
	}
	tw := time.Since(t0)
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	tws := time.Since(t0)
	f.Close()
	emit("seq-write", map[string]any{"bytes": total, "bs": bs, "s": tw.Seconds(), "mbps": mbps(total, tw)})
	emit("seq-write-fsync", map[string]any{"bytes": total, "bs": bs, "s": tws.Seconds(), "mbps": mbps(total, tws)})
	read := func(name string, drop bool) error {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		if drop {
			unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
		}
		rb := make([]byte, 1<<20)
		var got int64
		t0 := time.Now()
		for {
			n, err := f.Read(rb)
			got += int64(n)
			if err != nil {
				break
			}
		}
		d := time.Since(t0)
		if got != total {
			return fmt.Errorf("%s: read %d of %d bytes", name, got, total)
		}
		emit(name, map[string]any{"bytes": got, "s": d.Seconds(), "mbps": mbps(got, d)})
		return nil
	}
	// Warm the cache once, then measure the cached read; the cold read last.
	if err := read("seq-read-first", false); err != nil {
		return err
	}
	if err := read("seq-read-cached", false); err != nil {
		return err
	}
	return read("seq-read-cold", true)
}

// files creates, stats and deletes n small files in a fresh directory.
func files(dir string, n, size int) error {
	d, err := os.MkdirTemp(dir, "fs-guest-files-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(d)
	data := make([]byte, size)
	names := make([]string, n)
	for i := range names {
		names[i] = filepath.Join(d, fmt.Sprintf("f%05d.txt", i))
	}
	t0 := time.Now()
	for _, p := range names {
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return err
		}
	}
	emit("create", map[string]any{"n": n, "s": time.Since(t0).Seconds()})
	t0 = time.Now()
	var st unix.Stat_t
	for _, p := range names {
		if err := unix.Lstat(p, &st); err != nil {
			return err
		}
	}
	emit("stat", map[string]any{"n": n, "s": time.Since(t0).Seconds()})
	t0 = time.Now()
	ents, err := os.ReadDir(d)
	if err != nil || len(ents) != n {
		return fmt.Errorf("readdir: %d entries, %v", len(ents), err)
	}
	emit("readdir", map[string]any{"n": n, "s": time.Since(t0).Seconds()})
	t0 = time.Now()
	for _, p := range names {
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	emit("delete", map[string]any{"n": n, "s": time.Since(t0).Seconds()})
	return nil
}

type stats struct{ d []time.Duration }

func (s *stats) add(d time.Duration) { s.d = append(s.d, d) }

func (s *stats) out(name string, extra map[string]any) {
	sort.Slice(s.d, func(i, j int) bool { return s.d[i] < s.d[j] })
	q := func(f float64) float64 {
		if len(s.d) == 0 {
			return 0
		}
		return float64(s.d[min(len(s.d)-1, int(f*float64(len(s.d))))].Nanoseconds()) / 1e3
	}
	var sum time.Duration
	for _, d := range s.d {
		sum += d
	}
	v := map[string]any{"n": len(s.d), "p50_us": q(0.5), "p99_us": q(0.99), "s": sum.Seconds()}
	for k, x := range extra {
		v[k] = x
	}
	emit(name, v)
}

// tree measures metadata reads on an existing tree: a walk (readdir and lstat of every
// entry), an lstat of every path, an lstat of a missing name next to every path, and
// optionally git status. A first round warms the guest's caches ("once the guest has
// seen the tree"); the next rounds are measured.
func tree(dir string, rounds int, git bool) error {
	var paths []string
	walk := func() (time.Duration, int, error) {
		paths = paths[:0]
		t0 := time.Now()
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			var st unix.Stat_t
			if err := unix.Lstat(p, &st); err != nil {
				return err
			}
			paths = append(paths, p)
			return nil
		})
		return time.Since(t0), len(paths), err
	}
	gitStatus := func() (time.Duration, error) {
		t0 := time.Now()
		cmd := exec.Command("git", "-c", "safe.directory=*", "-C", dir, "status", "--porcelain")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return 0, fmt.Errorf("git status: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return time.Since(t0), nil
	}
	d, n, err := walk()
	if err != nil {
		return err
	}
	emit("walk-cold", map[string]any{"entries": n, "s": d.Seconds()})
	// The rest of the warm-up: the missing names too (module resolution asks for the
	// same missing names again and again).
	var st unix.Stat_t
	for _, p := range paths {
		unix.Lstat(p+".fs-guest-missing", &st)
	}
	if git {
		g, err := gitStatus()
		if err != nil {
			return err
		}
		emit("git-status-first", map[string]any{"s": g.Seconds()})
	}
	var ws, ls, neg, gs stats
	for r := 0; r < rounds; r++ {
		d, _, err := walk()
		if err != nil {
			return err
		}
		ws.add(d)
		for _, p := range paths {
			t0 := time.Now()
			if err := unix.Lstat(p, &st); err != nil {
				return err
			}
			ls.add(time.Since(t0))
			t0 = time.Now()
			if err := unix.Lstat(p+".fs-guest-missing", &st); err != unix.ENOENT {
				return fmt.Errorf("lstat of a missing name: %v", err)
			}
			neg.add(time.Since(t0))
		}
		if git {
			g, err := gitStatus()
			if err != nil {
				return err
			}
			gs.add(g)
		}
	}
	ws.out("walk-warm", map[string]any{"entries": n})
	ls.out("lstat", nil)
	neg.out("lstat-missing", nil)
	if git {
		gs.out("git-status", nil)
	}
	return nil
}

// run times a command in dir.
func run(dir, name string, args []string) error {
	if len(args) == 0 {
		return errors.New("exec: no command")
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	t0 := time.Now()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(args, " "), err)
	}
	emit(name, map[string]any{"s": time.Since(t0).Seconds()})
	return nil
}
