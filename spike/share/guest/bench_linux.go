package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

func atoi(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func secs(d time.Duration) float64 { return d.Seconds() }

func dropCaches() {
	unix.Sync()
	os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0)
}

func runBench(a map[string]string) (any, error) {
	dir := a["dir"]
	switch a["name"] {
	case "seqwrite":
		return seqWrite(dir, atoi(a["mb"], 1024))
	case "seqread":
		return seqRead(dir, a["drop"] != "0")
	case "untar":
		return untar(dir, atoi(a["files"], 20000))
	case "walk":
		return walk(dir, a["drop"] == "1")
	case "stat":
		return statStorm(dir, atoi(a["n"], 100000), false)
	case "open":
		return statStorm(dir, atoi(a["n"], 20000), true)
	case "statmiss":
		return statMiss(dir, atoi(a["n"], 20000))
	case "smallfiles":
		return smallFiles(dir, atoi(a["files"], 10000))
	case "rm":
		t := time.Now()
		err := os.RemoveAll(dir)
		return map[string]float64{"rm_s": secs(time.Since(t))}, err
	case "compat":
		return compat(dir)
	}
	return nil, fmt.Errorf("unknown bench %q", a["name"])
}

func seqWrite(dir string, mb int) (any, error) {
	p := filepath.Join(dir, "seq.bin")
	f, err := os.Create(p)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 1<<20)
	rand.New(rand.NewSource(1)).Read(buf)
	t := time.Now()
	for i := 0; i < mb; i++ {
		if _, err := f.Write(buf); err != nil {
			f.Close()
			return nil, err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	d := time.Since(t)
	return map[string]float64{"write_MBps": float64(mb) / d.Seconds(), "write_s": secs(d)}, nil
}

func seqRead(dir string, drop bool) (any, error) {
	p := filepath.Join(dir, "seq.bin")
	if drop {
		dropCaches()
	}
	res := map[string]float64{}
	for i, k := range []string{"read_cold_MBps", "read_warm_MBps"} {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		buf := make([]byte, 1<<20)
		t := time.Now()
		n, err := io.CopyBuffer(io.Discard, f, buf)
		f.Close()
		if err != nil {
			return nil, err
		}
		res[k] = float64(n) / (1 << 20) / time.Since(t).Seconds()
		_ = i
	}
	return res, nil
}

// genTree builds an in-memory tar that looks like a node_modules tree: about 20 files
// per package directory, nested two levels, sizes mostly 1-8 KiB with a long tail.
func genTree(files int) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	r := rand.New(rand.NewSource(42))
	data := make([]byte, 256<<10)
	r.Read(data)
	n := 0
	for pkg := 0; n < files; pkg++ {
		base := fmt.Sprintf("node_modules/pkg-%04d", pkg)
		tw.WriteHeader(&tar.Header{Name: base + "/", Typeflag: tar.TypeDir, Mode: 0o755})
		for sub := 0; sub < 3 && n < files; sub++ {
			d := base
			if sub > 0 {
				d = fmt.Sprintf("%s/lib%d", base, sub)
				tw.WriteHeader(&tar.Header{Name: d + "/", Typeflag: tar.TypeDir, Mode: 0o755})
			}
			for f := 0; f < 7 && n < files; f++ {
				sz := 512 + r.Intn(8<<10)
				if r.Intn(20) == 0 {
					sz = r.Intn(200 << 10)
				}
				tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("%s/file%d.js", d, f), Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(sz)})
				tw.Write(data[:sz])
				n++
			}
		}
	}
	tw.Close()
	return b.Bytes()
}

func untar(dir string, files int) (any, error) {
	blob := genTree(files)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	t := time.Now()
	tr := tar.NewReader(bytes.NewReader(blob))
	n := 0
	var bytesW int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		p := filepath.Join(dir, h.Name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return nil, err
			}
		case tar.TypeReg:
			f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return nil, err
			}
			w, err := io.Copy(f, tr)
			if err != nil {
				f.Close()
				return nil, err
			}
			bytesW += w
			if err := f.Close(); err != nil {
				return nil, err
			}
			n++
		}
	}
	d := time.Since(t)
	return map[string]float64{"untar_s": secs(d), "files": float64(n), "MiB": float64(bytesW) / (1 << 20)}, nil
}

// walk is what `git status` costs without git: readdir every directory and lstat every
// entry. With drop it starts from a cold guest cache.
func walk(dir string, drop bool) (any, error) {
	if drop {
		dropCaches()
	}
	t := time.Now()
	files, dirs := 0, 0
	var rec func(string) error
	rec = func(d string) error {
		es, err := os.ReadDir(d)
		if err != nil {
			return err
		}
		dirs++
		for _, e := range es {
			p := filepath.Join(d, e.Name())
			var st unix.Stat_t
			if err := unix.Lstat(p, &st); err != nil {
				return err
			}
			if st.Mode&unix.S_IFMT == unix.S_IFDIR {
				if err := rec(p); err != nil {
					return err
				}
			} else {
				files++
			}
		}
		return nil
	}
	err := rec(dir)
	d := time.Since(t)
	return map[string]float64{"walk_s": secs(d), "files": float64(files), "dirs": float64(dirs)}, err
}

func listFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	return files, err
}

func percentiles(lat []time.Duration) (p50, p99 float64) {
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	us := func(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1000 }
	return us(lat[len(lat)/2]), us(lat[len(lat)*99/100])
}

// statStorm lstats (or opens and closes) n random files of the tree, after one warm-up
// pass, and reports throughput and per-call latency.
func statStorm(dir string, n int, open bool) (any, error) {
	files, err := listFiles(dir)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errors.New("no files")
	}
	r := rand.New(rand.NewSource(7))
	lat := make([]time.Duration, n)
	op := func(p string) error {
		if open {
			fd, err := unix.Open(p, unix.O_RDONLY|unix.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			return unix.Close(fd)
		}
		var st unix.Stat_t
		return unix.Lstat(p, &st)
	}
	t := time.Now()
	for i := 0; i < n; i++ {
		p := files[r.Intn(len(files))]
		t0 := time.Now()
		if err := op(p); err != nil {
			return nil, err
		}
		lat[i] = time.Since(t0)
	}
	d := time.Since(t)
	p50, p99 := percentiles(lat)
	k := "stat"
	if open {
		k = "open"
	}
	return map[string]float64{k + "_per_s": float64(n) / d.Seconds(), k + "_p50_us": p50, k + "_p99_us": p99}, nil
}

// statMiss looks up names that do not exist, as module resolution does.
func statMiss(dir string, n int) (any, error) {
	lat := make([]time.Duration, n)
	t := time.Now()
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, fmt.Sprintf("node_modules/pkg-%04d/missing%d.js", i%500, i%40))
		var st unix.Stat_t
		t0 := time.Now()
		err := unix.Lstat(p, &st)
		lat[i] = time.Since(t0)
		if err != unix.ENOENT {
			return nil, fmt.Errorf("lstat %s: %v", p, err)
		}
	}
	d := time.Since(t)
	p50, p99 := percentiles(lat)
	return map[string]float64{"miss_per_s": float64(n) / d.Seconds(), "miss_p50_us": p50, "miss_p99_us": p99}, nil
}

// smallFiles is REQ-FS-002's create, stat, delete of 10k files in one directory.
func smallFiles(dir string, n int) (any, error) {
	d := filepath.Join(dir, "small")
	if err := os.MkdirAll(d, 0o755); err != nil {
		return nil, err
	}
	res := map[string]float64{}
	buf := []byte("hello\n")
	t := time.Now()
	for i := 0; i < n; i++ {
		fd, err := unix.Open(filepath.Join(d, "f"+strconv.Itoa(i)), unix.O_CREAT|unix.O_WRONLY|unix.O_TRUNC|unix.O_CLOEXEC, 0o644)
		if err != nil {
			return nil, err
		}
		unix.Write(fd, buf)
		unix.Close(fd)
	}
	res["create_s"] = secs(time.Since(t))
	t = time.Now()
	for i := 0; i < n; i++ {
		var st unix.Stat_t
		if err := unix.Lstat(filepath.Join(d, "f"+strconv.Itoa(i)), &st); err != nil {
			return nil, err
		}
	}
	res["stat_s"] = secs(time.Since(t))
	t = time.Now()
	for i := 0; i < n; i++ {
		if err := unix.Unlink(filepath.Join(d, "f"+strconv.Itoa(i))); err != nil {
			return nil, err
		}
	}
	res["delete_s"] = secs(time.Since(t))
	os.Remove(d)
	return res, nil
}

// Inotify watcher. Events go to the host as they arrive.

var (
	watchMu   sync.Mutex
	watchFD   = -1
	watchSeq  int64
	watchDirs map[int]string
)

func startWatch(dir string, recursive bool) error {
	watchMu.Lock()
	defer watchMu.Unlock()
	if watchFD >= 0 {
		return errors.New("already watching")
	}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		return err
	}
	watchDirs = map[int]string{}
	const mask = unix.IN_MODIFY | unix.IN_CLOSE_WRITE | unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_FROM | unix.IN_MOVED_TO | unix.IN_ATTRIB
	add := func(p string) error {
		wd, err := unix.InotifyAddWatch(fd, p, mask)
		if err != nil {
			return err
		}
		watchDirs[wd] = p
		return nil
	}
	if recursive {
		err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return add(p)
			}
			return nil
		})
	} else {
		err = add(dir)
	}
	if err != nil {
		unix.Close(fd)
		return err
	}
	watchFD = fd
	dirs := watchDirs
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := unix.Read(fd, buf)
			if err != nil || n <= 0 {
				return
			}
			for off := 0; off+unix.SizeofInotifyEvent <= n; {
				ev := (*unix.InotifyEvent)(unsafePointer(&buf[off]))
				nameLen := int(ev.Len)
				name := ""
				if nameLen > 0 {
					raw := buf[off+unix.SizeofInotifyEvent : off+unix.SizeofInotifyEvent+nameLen]
					name = string(bytes.TrimRight(raw, "\x00"))
				}
				off += unix.SizeofInotifyEvent + nameLen
				watchMu.Lock()
				d := dirs[int(ev.Wd)]
				watchSeq++
				seq := watchSeq
				watchMu.Unlock()
				p := filepath.Join(d, name)
				send(event{Ev: "inotify", Path: p, Mask: ev.Mask, Seq: seq})
				if recursive && ev.Mask&unix.IN_ISDIR != 0 && ev.Mask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0 {
					watchMu.Lock()
					if wd, err := unix.InotifyAddWatch(fd, p, mask); err == nil {
						dirs[wd] = p
					}
					watchMu.Unlock()
				}
			}
		}
	}()
	return nil
}

func stopWatch() {
	watchMu.Lock()
	defer watchMu.Unlock()
	if watchFD >= 0 {
		unix.Close(watchFD)
		watchFD = -1
	}
}
