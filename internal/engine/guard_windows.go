//go:build windows

package engine

import (
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/Higangssh/winbox/internal/guestctl"
	"github.com/Higangssh/winbox/internal/spaceguard"
	"github.com/Higangssh/winbox/internal/vm"
)

// vhdxMaxOverhead is the VHDX metadata allowance added to the virtual size for the
// largest the file can become (spaceguard.Host.VHDXMax). With 1 MiB blocks the BAT is
// 8 bytes per block, 512 KiB for a 64 GiB disk; 64 MiB covers that, the log and the
// headers for any disk size winbox allows.
const vhdxMaxOverhead = 64 << 20

// hostGuard is the host half of the free-space guard (REQ-DISK-004): every interval it
// measures the host volume and the VHDX, sends the budget to the guest over guestctl,
// and pauses the VM below the hard floor. Every measurement failure fails closed: a
// zero budget makes the guest reserve everything, and an unreadable free space reads as
// 0, which pauses the VM.
//
// No guest-reported value feeds the budget or the floor. Guest status only decides
// whether the host pauses at the margin (the guard looks dead) or at the floor.
type hostGuard struct {
	cfg      spaceguard.Config
	m        Machine
	sup      *guestctl.Supervisor
	vhdx     string
	dir      string
	vmax     uint64
	vmeta    uint64
	interval time.Duration
	dead     time.Duration
	log      *slog.Logger

	// live is when the guest guard last showed progress (UnixNano); 0 until its first
	// report. Before that (the guest is booting, and its guard fails closed by
	// reserving everything) only the floor applies.
	live      atomic.Int64
	lastSteps atomic.Uint64

	paused  bool
	pauses  int
	lastB   spaceguard.Budget
	lastLog time.Time
	stop    chan struct{}
	done    chan struct{}
}

func newHostGuard(cfg spaceguard.Config, m Machine, sup *guestctl.Supervisor, disk *dataDisk, log *slog.Logger) (*hostGuard, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	g := &hostGuard{
		cfg: cfg, m: m, sup: sup, vhdx: disk.path, dir: filepath.Dir(disk.path),
		vmax: disk.meta.SizeBytes + vhdxMaxOverhead, vmeta: disk.meta.MetadataBytes,
		interval: 100 * time.Millisecond, dead: 5 * time.Second, log: log,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	return g, nil
}

// onStatus is the Supervisor's status callback. Liveness counts only a guard that is
// applying the budget and whose step counter advanced: the status goroutine answers
// even when the guard loop is stuck.
func (g *hostGuard) onStatus(s guestctl.Status) {
	if s.Guard == guestctl.GuardOK && s.GuardSteps != g.lastSteps.Load() {
		g.live.Store(time.Now().UnixNano())
	}
	g.lastSteps.Store(s.GuardSteps)
}

func (g *hostGuard) start() { go g.loop() }

// close stops the loop and resumes a VM it paused, so the guest can still shut down in
// order (a paused guest cannot hear the request).
func (g *hostGuard) close() {
	close(g.stop)
	<-g.done
	if g.paused {
		if err := g.m.Resume(); err != nil {
			g.log.Warn("resume the paused VM for its shutdown", "err", err)
		}
		g.paused = false
		g.sup.SetPaused(false)
	}
}

func (g *hostGuard) loop() {
	defer close(g.done)
	t := time.NewTicker(g.interval)
	defer t.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-t.C:
		}
		g.tick()
	}
}

func (g *hostGuard) tick() {
	free, ferr := vm.FreeBytes(g.dir)
	if ferr != nil {
		g.logEvery("free-space guard: cannot read host free space; treating it as 0", "err", ferr)
		free = 0
	}
	var b spaceguard.Budget
	u, err := vm.Usage(g.vhdx)
	if err == nil {
		b, err = spaceguard.HostBudget(spaceguard.Host{
			Free: free, VHDXAllocated: u.Allocated, VHDXMetadata: min(g.vmeta, u.Allocated), VHDXMax: g.vmax,
		}, g.cfg)
	}
	if err != nil {
		g.logEvery("free-space guard: sending budget 0", "err", err)
		b = spaceguard.Budget{}
	}
	g.sup.SetBudget(b)

	pcfg := g.cfg
	if live := g.live.Load(); live != 0 && time.Since(time.Unix(0, live)) > g.dead && !g.paused {
		// The guest guard is not reporting progress: its reservation may be gone, so
		// pause at the margin rather than the floor.
		pcfg.Floor = pcfg.Margin - 1
	}
	if want := spaceguard.PauseWanted(free, g.paused, pcfg); want != g.paused {
		g.setPaused(want, free)
	}
	if b != g.lastB && time.Since(g.lastLog) >= 10*time.Second {
		g.log.Debug("free-space guard", "hostFree", HumanSize(free), "vhdx", HumanSize(u.Allocated), "budget", b.String())
		g.lastB, g.lastLog = b, time.Now()
	}
}

func (g *hostGuard) logEvery(msg string, args ...any) {
	if time.Since(g.lastLog) >= 10*time.Second {
		g.log.Warn(msg, args...)
		g.lastLog = time.Now()
	}
}

func (g *hostGuard) setPaused(pause bool, free uint64) {
	var err error
	if pause {
		g.sup.SetPaused(true) // before the guest goes silent
		err = g.m.Pause()
	} else {
		err = g.m.Resume()
	}
	if !pause || err != nil {
		// Resumed (or the pause failed): the guest could not report while paused, so
		// restart the silence clock instead of treating the pause as a dead guard.
		g.sup.SetPaused(false)
		g.live.Store(time.Now().UnixNano())
	}
	if err != nil {
		g.log.Error("free-space guard: pause/resume failed", "pause", pause, "hostFree", HumanSize(free), "err", err)
		return
	}
	g.paused = pause
	if pause {
		g.pauses++
		g.log.Warn("the host volume is almost full: VM paused until space is freed", "hostFree", HumanSize(free), "volume", g.dir)
	} else {
		g.log.Warn("host volume has space again: VM resumed", "hostFree", HumanSize(free))
	}
}
