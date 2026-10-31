// Command mem-guest is the in-container half of bench/mem-reclaim (REQ-MEM-002). The
// Windows driver imports it as the only file of a scratch image and runs it:
//
//	mem-guest fill -anon 4GiB -cache 4GiB -dir /data
//	mem-guest meminfo
//	mem-guest kmsg              (diagnosis: the kernel log; needs --privileged)
//	mem-guest cat FILE...       (diagnosis: /proc and /sys files)
//	mem-guest write FILE VALUE  (diagnosis: a sysctl; needs --privileged)
//
// fill maps and touches anonymous memory, then writes a file of the cache size under
// -dir and reads it back, so the guest holds both at once; it prints one JSON line with
// the guest's /proc/meminfo at the peak and exits, which frees the anonymous memory.
// The file stays (it is on a volume), so its page cache outlives the container.
// meminfo prints the same JSON line. The output is an interface between two programs
// built from the same commit, not a stable format.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fail(errors.New("usage: mem-guest fill|meminfo [flags]"))
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	anon := fs.Int64("anon", 4<<30, "fill: bytes of anonymous memory to touch")
	cache := fs.Int64("cache", 4<<30, "fill: bytes of file to write and read back (page cache)")
	dir := fs.String("dir", "/data", "fill: where the cache file goes (a volume, so it outlives the container)")
	_ = fs.Parse(os.Args[2:])
	switch os.Args[1] {
	case "fill":
		r, err := fill(*anon, *cache, *dir)
		if err != nil {
			fail(err)
		}
		emit(r)
	case "meminfo":
		m, err := readMeminfo()
		if err != nil {
			fail(err)
		}
		emit(Result{Meminfo: m})
	case "cat":
		// Diagnosis only: print /proc or /sys files.
		for _, p := range fs.Args() {
			b, err := os.ReadFile(p)
			if err != nil {
				fail(err)
			}
			fmt.Printf("== %s\n%s", p, b)
		}
	case "write":
		// Diagnosis only: write a value to a /proc/sys or /sys file (needs --privileged).
		if fs.NArg() != 2 {
			fail(errors.New("usage: mem-guest write FILE VALUE"))
		}
		if err := os.WriteFile(fs.Arg(0), []byte(fs.Arg(1)), 0); err != nil {
			fail(err)
		}
	case "kmsg":
		// Diagnosis only: the kernel log (needs docker run --privileged).
		if err := kmsg(os.Stdout); err != nil {
			fail(err)
		}
	default:
		fail(fmt.Errorf("unknown command %q", os.Args[1]))
	}
}

// Result is the JSON line mem-guest prints.
type Result struct {
	AnonBytes, CacheBytes int64
	AnonSecs, CacheSecs   float64
	Meminfo               map[string]uint64 // bytes, the fields of /proc/meminfo that matter here
}

func emit(r Result) {
	b, _ := json.Marshal(r)
	fmt.Println(string(b))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "mem-guest:", err)
	os.Exit(1)
}

func fill(anon, cache int64, dir string) (Result, error) {
	r := Result{AnonBytes: anon, CacheBytes: cache}
	page := int64(os.Getpagesize())
	t := time.Now()
	var mem []byte
	if anon > 0 {
		var err error
		mem, err = syscall.Mmap(-1, 0, int(anon), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
		if err != nil {
			return r, fmt.Errorf("mmap %d bytes: %w", anon, err)
		}
		// A non-zero byte in every page, so each is a real, private page.
		for off := int64(0); off < anon; off += page {
			mem[off] = byte(off/page) | 1
		}
	}
	r.AnonSecs = time.Since(t).Seconds()

	t = time.Now()
	if cache > 0 {
		if err := writeRead(filepath.Join(dir, "cache.bin"), cache); err != nil {
			return r, err
		}
	}
	r.CacheSecs = time.Since(t).Seconds()
	m, err := readMeminfo()
	if err != nil {
		return r, err
	}
	r.Meminfo = m
	// Keep the anonymous memory live until the peak was read.
	var sum byte
	for off := int64(0); off < anon; off += 1 << 20 {
		sum += mem[off]
	}
	_ = sum
	return r, nil
}

// writeRead writes n bytes to p, syncs them (so the cache is clean, as after any
// finished write), and reads them back.
func writeRead(p string, n int64) error {
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	for i := range buf {
		buf[i] = byte(i*7 + 1)
	}
	for done := int64(0); done < n; {
		k := min(int64(len(buf)), n-done)
		if _, err := f.Write(buf[:k]); err != nil {
			return err
		}
		done += k
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	got, err := io.CopyBuffer(io.Discard, f, buf)
	if err != nil {
		return err
	}
	if got != n {
		return fmt.Errorf("read back %d of %d bytes", got, n)
	}
	return nil
}

// kmsg copies the kernel log buffer to w, one record per line, without waiting for new
// records.
func kmsg(w io.Writer) error {
	fd, err := syscall.Open("/dev/kmsg", syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	buf := make([]byte, 8192)
	for {
		n, err := syscall.Read(fd, buf)
		if err == syscall.EAGAIN {
			return nil
		}
		if err == syscall.EPIPE {
			continue // overwritten records
		}
		if err != nil {
			return err
		}
		rec := string(buf[:n])
		if _, msg, ok := strings.Cut(rec, ";"); ok {
			rec = msg
		}
		if i := strings.IndexByte(rec, '\n'); i >= 0 {
			rec = rec[:i]
		}
		fmt.Fprintln(w, rec)
	}
}

var meminfoFields = []string{"MemTotal", "MemFree", "MemAvailable", "Cached", "AnonPages", "Shmem", "Active(file)", "Inactive(file)", "Slab", "Dirty", "Writeback"}

func readMeminfo() (map[string]uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	want := map[string]bool{}
	for _, k := range meminfoFields {
		want[k] = true
	}
	m := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok || !want[k] {
			continue
		}
		fs := strings.Fields(v)
		if len(fs) == 0 {
			continue
		}
		n, err := strconv.ParseUint(fs[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("meminfo %s: %w", k, err)
		}
		if len(fs) > 1 && fs[1] == "kB" {
			n <<= 10
		}
		m[k] = n
	}
	return m, sc.Err()
}
