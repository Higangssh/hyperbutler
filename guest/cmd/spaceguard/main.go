//go:build linux

// Command spaceguard keeps an fallocate'd reservation file on the guest's data file system
// at the size internal/spaceguard computes, so that the guest gets ENOSPC before the host
// volume holding its sparse VHDX fills up (REQ-DISK-004).
//
// The host sends a budget ("budget <bytes> <backed>" or "budget unlimited", one per line)
// on -ctl. For the spike that is a serial port (/dev/ttyS1); the real guest agent will
// get it over hvsock. spaceguard re-measures the file system every -interval and whenever
// a budget arrives, and reports "status ..." lines back on -ctl.
//
// It fails closed: until the first budget arrives, after the control channel breaks, and
// when no budget has arrived for -stale worth of its own ticks, the budget is treated as
// 0 and the guest gets no free space. Staleness counts ticks, not wall-clock time, so a
// paused VM or a sleeping host (whose guest clock jumps on resume) does not trip it.
//
// Cross-compile on Windows with GOOS=linux CGO_ENABLED=0.
//
//	spaceguard -dev /dev/sda -file /mnt/.winbox-reserve -ctl /dev/ttyS1
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/Higangssh/winbox/internal/spaceguard"
)

func main() {
	file := flag.String("file", "", "reservation file; it must be on the file system to guard")
	ctl := flag.String("ctl", "", "budget input and status output (a tty, FIFO or socket path)")
	dev := flag.String("dev", "", "block device that holds the file system")
	interval := flag.Duration("interval", 100*time.Millisecond, "how often to re-measure the file system")
	stale := flag.Duration("stale", 30*time.Second, "fail closed when no budget arrived for this many intervals' worth of ticks (0 = never)")
	trimEvery := flag.Duration("trim-every", 10*time.Second, "at most this often, FITRIM the file system before growing the reservation (0 = never)")
	hyst := flag.Uint64("hysteresis", spaceguard.DefaultConfig.Hysteresis, "reservation hysteresis band in bytes")
	quantum := flag.Uint64("quantum", spaceguard.DefaultConfig.Quantum, "reservation granularity in bytes")
	ready := flag.String("ready", "", "file to create once the first host budget has been applied")
	flag.Parse()
	if *file == "" || *ctl == "" || *dev == "" || *interval <= 0 {
		flag.Usage()
		os.Exit(2)
	}
	// Only Hysteresis and Quantum matter in the guest; Margin and Floor are host-side and
	// only need to pass Validate.
	cfg := spaceguard.Config{Margin: 2, Floor: 1, Hysteresis: *hyst, Quantum: *quantum}
	if err := cfg.Validate(); err != nil {
		log.Fatal(err)
	}
	f, err := os.OpenFile(*file, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		log.Fatal(err)
	}
	devSize, err := blockDevSize(*dev)
	if err != nil {
		log.Fatalf("spaceguard: size of %s: %v", *dev, err)
	}

	g := &guard{f: f, cfg: cfg, devSize: devSize, trimEvery: *trimEvery}
	if *stale > 0 {
		g.staleTicks = max(1, int(*stale / *interval))
	}
	events := make(chan event, 1)
	outs := make(chan io.Writer, 1)
	go control(*ctl, events, outs)

	tick := time.NewTicker(*interval)
	defer tick.Stop()
	readyDone := *ready == ""
	for {
		select {
		case e := <-events:
			if e.lost {
				log.Printf("spaceguard: control channel lost; failing closed")
				g.have = false
			} else {
				g.budget, g.have, g.ticksSince = e.b, true, 0
			}
		case w := <-outs:
			g.out = w
		case <-tick.C:
			g.ticksSince++
		}
		g.step()
		if !readyDone && g.have {
			if err := os.WriteFile(*ready, nil, 0o644); err != nil {
				log.Printf("spaceguard: ready file: %v", err)
			}
			readyDone = true
		}
	}
}

// control opens the control channel, reads budget lines from it, and reopens it when it
// breaks. Only the newest event matters, so the reader never blocks on a slow main loop.
func control(path string, events chan event, outs chan io.Writer) {
	send := func(e event) {
		select {
		case <-events:
		default:
		}
		events <- e
	}
	for {
		c, err := os.OpenFile(path, os.O_RDWR|unix.O_NOCTTY, 0)
		if err != nil {
			log.Printf("spaceguard: open %s: %v", path, err)
			send(event{lost: true})
			time.Sleep(time.Second)
			continue
		}
		if err := rawTTY(c); err != nil && !errors.Is(err, unix.ENOTTY) {
			log.Printf("spaceguard: tty setup: %v", err)
		}
		outs <- c
		err = readBudgets(c, send)
		log.Printf("spaceguard: control channel: %v", err)
		send(event{lost: true})
		c.Close()
		time.Sleep(time.Second)
	}
}

type guard struct {
	f          *os.File
	cfg        spaceguard.Config
	out        io.Writer // nil until the control channel is open
	devSize    uint64
	staleTicks int // 0: never stale
	trimEvery  time.Duration

	budget     spaceguard.Budget
	have       bool // a host budget has arrived on the current channel
	ticksSince int

	lastTrim    time.Time
	lastStatus  time.Time
	lastTarget  uint64
	staleLogged bool
}

func (g *guard) step() {
	b := g.budget
	switch {
	case !g.have:
		b = spaceguard.Budget{} // fail closed until the host speaks
	case g.staleTicks > 0 && g.ticksSince > g.staleTicks:
		if !g.staleLogged {
			log.Printf("spaceguard: no budget for %d ticks; failing closed", g.ticksSince)
			g.staleLogged = true
		}
		b = spaceguard.Budget{}
	default:
		g.staleLogged = false
	}

	m, err := g.measure()
	if err != nil {
		log.Printf("spaceguard: measure: %v", err)
		return
	}
	r, err := spaceguard.GuestReservation(m, b, g.cfg)
	if err != nil {
		log.Printf("spaceguard: %v", err)
		return
	}
	var aerr error
	switch {
	case r.Target > m.Reserved:
		aerr = g.grow(r.Target)
	case r.Target < m.Reserved:
		aerr = unix.Ftruncate(int(g.f.Fd()), int64(r.Target))
	}
	if aerr != nil && !errors.Is(aerr, unix.ENOSPC) {
		log.Printf("spaceguard: set reservation to %d: %v", r.Target, aerr)
	}
	// ext4 keeps a few clusters (s_resv_clusters, up to 16 MiB) that statfs reports as free
	// but fallocate cannot take, so ENOSPC on grow is normal when the budget is tight. It
	// is reported at most once a second, like the rest.
	if g.out != nil && (r.Target != g.lastTarget || time.Since(g.lastStatus) >= time.Second) {
		after, _ := g.measure()
		fmt.Fprintf(g.out, "status budget=%s device=%d free=%d reserved=%d target=%d need=%d short=%d err=%v\n",
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
func (g *guard) grow(target uint64) error {
	fd := int(g.f.Fd())
	if g.trimEvery > 0 && time.Since(g.lastTrim) >= g.trimEvery {
		g.lastTrim = time.Now()
		r := fstrimRange{Len: math.MaxUint64}
		if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), fitrim, uintptr(unsafe.Pointer(&r))); e != 0 {
			log.Printf("spaceguard: FITRIM: %v", e)
		}
	}
	chunk := uint64(64 << 20)
	var off uint64
	for off < target && chunk >= g.cfg.Quantum {
		n := min(chunk, target-off)
		err := unix.Fallocate(fd, 0, int64(off), int64(n))
		if err == nil {
			off += n
			continue
		}
		if !errors.Is(err, unix.ENOSPC) {
			return err
		}
		if chunk/2 < g.cfg.Quantum {
			return err
		}
		chunk /= 2
	}
	return nil
}

// fitrim is FITRIM from linux/fs.h: _IOWR('X', 121, struct fstrim_range). x/sys/unix
// does not export it.
const fitrim = 0xC0185879

// fstrimRange is struct fstrim_range from linux/fs.h.
type fstrimRange struct {
	Start, Len, MinLen uint64
}

func (g *guard) measure() (spaceguard.Guest, error) {
	fd := int(g.f.Fd())
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
	return spaceguard.Guest{Device: g.devSize, Size: size, Used: used, Free: free, Reserved: reserved}, nil
}

// blockDevSize returns the size of a block device in bytes.
func blockDevSize(path string) (uint64, error) {
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

// rawTTY turns off echo and output processing, so budget lines are not echoed back to
// the host and status lines are not rewritten. Line mode (ICANON) stays on.
func rawTTY(f *os.File) error {
	fd := int(f.Fd())
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return err
	}
	t.Lflag &^= unix.ECHO | unix.ECHOE | unix.ECHOK | unix.ECHONL | unix.ISIG | unix.IEXTEN
	t.Lflag |= unix.ICANON
	t.Iflag &^= unix.IXON | unix.IXOFF | unix.ICRNL | unix.INLCR | unix.IGNCR
	t.Oflag &^= unix.OPOST
	t.Cflag = t.Cflag&^unix.CBAUD | unix.B115200
	t.Ispeed, t.Ospeed = unix.B115200, unix.B115200
	return unix.IoctlSetTermios(fd, unix.TCSETS, t)
}
