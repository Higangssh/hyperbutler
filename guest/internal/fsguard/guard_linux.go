//go:build linux

// Package fsguard is the guest half of the free-space guard (REQ-DISK-004): it keeps an
// fallocate'd reservation file on the guest's data file system at the size
// internal/spaceguard computes from the host's budget. It is shared by guest/cmd/hyperbutlerd
// (budget over the guestctl channel) and guest/cmd/spaceguard (budget over a serial
// line).
//
// It fails closed: until the first budget arrives, after the channel is lost, and when
// no budget has arrived for StaleTicks of its own ticks, the budget is treated as 0 and
// the guest gets no free space. Staleness counts ticks, not wall-clock time, so a paused
// VM or a sleeping host (whose guest clock jumps on resume) does not trip it.
package fsguard

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/Higangssh/hyperbutler/internal/spaceguard"
)

// Trimmer runs FITRIM on the guarded file system. The guard calls it before growing the
// reservation, so the blocks fallocate picks are not still backed on the host.
type Trimmer interface {
	// TrimIfDue trims unless a trim ran within the last minInterval.
	TrimIfDue(minInterval time.Duration)
}

// Guard owns the reservation file. It is not safe for concurrent use: one goroutine
// calls SetBudget, Lost, Tick and Step.
type Guard struct {
	F       *os.File
	Cfg     spaceguard.Config
	DevSize uint64
	// StaleTicks fails closed when no budget arrived for this many ticks (0: never).
	StaleTicks int
	// LostGrace is how many ticks the last budget is still applied after the channel
	// was lost, so that a reconnect within it does not swing the reservation to the
	// whole disk and back (0: fail closed at once). The budget is exact for the guest's
	// own writes, so a short grace only exposes the margin to other host writers for
	// that long.
	LostGrace int
	// TrimEvery rate-limits the pre-grow trim (0: never trim).
	TrimEvery time.Duration
	Trim      Trimmer
	// Out, if set, receives a "status ..." line on change and at most once a second.
	Out io.Writer

	budget     spaceguard.Budget
	have       bool
	ticksSince int
	lost       bool
	lostTicks  int

	// Steps counts completed Step calls, for liveness reports.
	Steps       uint64
	lastStatus  time.Time
	lastTarget  uint64
	staleLogged bool

	// Last result, for status reports.
	Last Result
}

// Result is what the last Step did.
type Result struct {
	State       State
	Measured    spaceguard.Guest
	Reservation spaceguard.Reservation
	Err         error
}

// State is the guard's view of its budget.
type State int

const (
	NoBudget State = iota // none yet, or the channel was lost
	Stale                 // the budget is older than StaleTicks
	OK
)

// Open opens (creating) the reservation file and reads the device size.
func Open(file, dev string, cfg spaceguard.Config) (*Guard, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	devSize, err := BlockDevSize(dev)
	if err != nil {
		return nil, fmt.Errorf("fsguard: size of %s: %w", dev, err)
	}
	f, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	return &Guard{F: f, Cfg: cfg, DevSize: devSize}, nil
}

// SetBudget records a new host budget.
func (g *Guard) SetBudget(b spaceguard.Budget) {
	g.budget, g.have, g.ticksSince, g.lost = b, true, 0, false
}

// Lost records that the budget channel is gone. After LostGrace ticks without a new
// budget, the guard fails closed.
func (g *Guard) Lost() {
	if !g.lost {
		g.lost, g.lostTicks = true, 0
	}
}

// Have reports whether a budget from the current channel is held.
func (g *Guard) Have() bool { return g.have && !g.lost }

// Tick counts one interval for staleness.
func (g *Guard) Tick() {
	g.ticksSince++
	if g.lost {
		g.lostTicks++
	}
}

// Step measures the file system and moves the reservation toward its target.
func (g *Guard) Step() {
	b := g.budget
	state := OK
	if g.lost && g.lostTicks >= g.LostGrace {
		g.have = false
	}
	switch {
	case !g.have:
		b, state = spaceguard.Budget{}, NoBudget // fail closed until the host speaks
	case g.StaleTicks > 0 && g.ticksSince > g.StaleTicks:
		if !g.staleLogged {
			log.Printf("fsguard: no budget for %d ticks; failing closed", g.ticksSince)
			g.staleLogged = true
		}
		b, state = spaceguard.Budget{}, Stale
	default:
		g.staleLogged = false
	}

	defer func() { g.Steps++ }()
	m, err := g.Measure()
	if err != nil {
		g.Last = Result{State: state, Err: err}
		log.Printf("fsguard: measure: %v", err)
		return
	}
	r, err := spaceguard.GuestReservation(m, b, g.Cfg)
	if err != nil {
		g.Last = Result{State: state, Measured: m, Err: err}
		log.Printf("fsguard: %v", err)
		return
	}
	var aerr error
	switch {
	case r.Target > m.Reserved:
		aerr = g.grow(m.Reserved, r.Target)
	case r.Target < m.Reserved:
		aerr = unix.Ftruncate(int(g.F.Fd()), int64(r.Target))
	}
	if aerr != nil && !errors.Is(aerr, unix.ENOSPC) {
		log.Printf("fsguard: set reservation to %d: %v", r.Target, aerr)
	}
	after, merr := g.Measure()
	if merr != nil {
		after = m
	}
	g.Last = Result{State: state, Measured: after, Reservation: r, Err: aerr}
	// ext4 keeps a few clusters (s_resv_clusters, up to 16 MiB) that statfs reports as
	// free but fallocate cannot take, so ENOSPC on grow is normal when the budget is
	// tight.
	if g.Out != nil && (r.Target != g.lastTarget || time.Since(g.lastStatus) >= time.Second) {
		fmt.Fprintf(g.Out, "status budget=%s device=%d free=%d reserved=%d target=%d need=%d short=%d err=%v\n",
			b, after.Device, after.Free, after.Reserved, r.Target, r.Need, r.Short, aerr)
		g.lastStatus, g.lastTarget = time.Now(), r.Target
	}
}

// grow extends the reservation to target bytes. fallocate mode 0 allocates unwritten
// extents, so ext4 takes the blocks without writing data and the VHDX does not grow.
//
// Before growing it trims free space (rate-limited), so that the blocks fallocate picks
// are not still backed on the host from deleted data: the policy assumes the
// reservation is unbacked.
//
// It allocates from offset 0 in chunks, so holes anywhere in the file (someone truncated
// it up) are filled too, and if guest writes race it to the last free blocks it keeps
// what it got instead of failing as a whole.
//
// A grow by more than the hysteresis band trims whatever the rate limit says: a delete
// since the last trim could otherwise leave the new reservation on still-backed blocks,
// and the exposure estimate would be optimistic by that overlap. Repeated FITRIM is
// cheap on ext4, which skips groups it trimmed and has not freed blocks in since.
func (g *Guard) grow(current, target uint64) error {
	fd := int(g.F.Fd())
	if g.Trim != nil && g.TrimEvery > 0 {
		every := g.TrimEvery
		if target-current > g.Cfg.Hysteresis {
			every = 0
		}
		g.Trim.TrimIfDue(every)
	}
	chunk := uint64(64 << 20)
	var off uint64
	for off < target && chunk >= g.Cfg.Quantum {
		n := min(chunk, target-off)
		err := unix.Fallocate(fd, 0, int64(off), int64(n))
		if err == nil {
			off += n
			continue
		}
		if !errors.Is(err, unix.ENOSPC) {
			return err
		}
		if chunk/2 < g.Cfg.Quantum {
			return err
		}
		chunk /= 2
	}
	return nil
}

// Measure reads the guard's inputs from the file system.
func (g *Guard) Measure() (spaceguard.Guest, error) {
	fd := int(g.F.Fd())
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return spaceguard.Guest{}, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return spaceguard.Guest{}, err
	}
	bs := uint64(fs.Frsize)
	if bs == 0 {
		bs = uint64(fs.Bsize)
	}
	size, free := fs.Blocks*bs, fs.Bfree*bs
	used := size - free
	reserved := uint64(st.Blocks) * 512
	if reserved > used {
		reserved = used // st_blocks and statfs are not read atomically
	}
	return spaceguard.Guest{Device: g.DevSize, Size: size, Used: used, Free: free, Reserved: reserved}, nil
}

// BlockDevSize returns the size of a block device in bytes.
func BlockDevSize(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var n uint64
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.BLKGETSIZE64, uintptr(unsafe.Pointer(&n))); e != 0 {
		return 0, e
	}
	return n, nil
}
