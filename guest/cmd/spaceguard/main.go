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
//	spaceguard -dev /dev/sda -file /mnt/.hyperbutler-reserve -ctl /dev/ttyS1
package main

import (
	"errors"
	"flag"
	"io"
	"log"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Higangssh/hyperbutler/guest/internal/fsguard"
	"github.com/Higangssh/hyperbutler/internal/spaceguard"
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
	g, err := fsguard.Open(*file, *dev, cfg)
	if err != nil {
		log.Fatal(err)
	}
	g.TrimEvery = *trimEvery
	g.Trim = &fsguard.FSTrimmer{F: g.F}
	if *stale > 0 {
		g.StaleTicks = max(1, int(*stale / *interval))
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
				g.Lost()
			} else {
				g.SetBudget(e.b)
			}
		case w := <-outs:
			g.Out = w
		case <-tick.C:
			g.Tick()
		}
		g.Step()
		if !readyDone && g.Have() {
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
