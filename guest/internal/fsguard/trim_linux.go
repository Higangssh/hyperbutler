//go:build linux

package fsguard

import (
	"math"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// fitrim is FITRIM from linux/fs.h: _IOWR('X', 121, struct fstrim_range). x/sys/unix
// does not export it.
const fitrim = 0xC0185879

// fstrimRange is struct fstrim_range from linux/fs.h.
type fstrimRange struct {
	Start, Len, MinLen uint64
}

// FSTrimmer runs FITRIM on one file system. Calls are serialized: the guard, the trim
// policy and host requests share it, so two trims never run at once.
type FSTrimmer struct {
	F *os.File // any file or directory on the file system
	// OnTrim, if set, is called after every trim, outside the trimmer's lock, so the
	// trim policy sees trims made by the guard and the host too.
	OnTrim func()

	mu      sync.Mutex
	last    time.Time // when the last trim finished
	count   uint64
	trimmed uint64
	lastErr error
}

// Trim runs FITRIM over the whole file system and returns how many bytes the kernel
// reports as trimmed. ext4 skips block groups it trimmed before and has not freed blocks
// in since, so a repeated trim is cheap.
func (t *FSTrimmer) Trim() (uint64, time.Duration, error) {
	t.mu.Lock()
	n, d, err := t.trimLocked()
	t.mu.Unlock()
	if t.OnTrim != nil {
		t.OnTrim()
	}
	return n, d, err
}

func (t *FSTrimmer) trimLocked() (uint64, time.Duration, error) {
	r := fstrimRange{Len: math.MaxUint64}
	t0 := time.Now()
	_, _, e := unix.Syscall(unix.SYS_IOCTL, t.F.Fd(), fitrim, uintptr(unsafe.Pointer(&r)))
	d := time.Since(t0)
	t.last = time.Now()
	t.count++
	if e != 0 {
		t.lastErr = e
		return 0, d, e
	}
	t.lastErr = nil
	t.trimmed += r.Len
	return r.Len, d, nil
}

// TrimIfDue trims unless a trim finished within minInterval.
func (t *FSTrimmer) TrimIfDue(minInterval time.Duration) {
	t.mu.Lock()
	if !t.last.IsZero() && time.Since(t.last) < minInterval {
		t.mu.Unlock()
		return
	}
	_, _, _ = t.trimLocked()
	t.mu.Unlock()
	if t.OnTrim != nil {
		t.OnTrim()
	}
}

// Stats returns the trim count, total bytes trimmed, and when the last trim finished.
func (t *FSTrimmer) Stats() (count, trimmed uint64, last time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.count, t.trimmed, t.last
}

// Free returns the file system's free bytes (statfs f_bfree * f_frsize).
func Free(f *os.File) (uint64, error) {
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &fs); err != nil {
		return 0, err
	}
	bs := uint64(fs.Frsize)
	if bs == 0 {
		bs = uint64(fs.Bsize)
	}
	return fs.Bfree * bs, nil
}
