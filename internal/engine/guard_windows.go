//go:build windows

package engine

import (
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/guestctl"
	"github.com/Higangssh/hyperbutler/internal/spaceguard"
	"github.com/Higangssh/hyperbutler/internal/vm"
)

// vhdxMaxOverhead is the VHDX metadata allowance added to the virtual size for the
// largest the file can become (spaceguard.Host.VHDXMax). With 1 MiB blocks the BAT is
// 8 bytes per block, 512 KiB for a 64 GiB disk; 64 MiB covers that, the log and the
// headers for any disk size hyperbutler allows.
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

	// freeBytes measures the host volume (vm.FreeBytes; replaced in tests).
	freeBytes func(dir string) (uint64, error)
	// onPause, if set, is told of every pause (REQ-DISK-010), again while paused when
	// the free space changes, and of the resume (nil). volume names the host volume.
	onPause    func(*GuardPauseState)
	volume     string
	reported   *GuardPauseState
	reportedAt time.Time
	pausedAt   time.Time
	// now is the clock of the pause reports (time.Now; replaced in tests).
	now func() time.Time
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
		freeBytes: vm.FreeBytes, volume: volumeOf(filepath.Dir(disk.path)), now: time.Now,
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
	g.report(0)
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
	free, ferr := g.freeBytes(g.dir)
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
	g.report(free)
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
		g.pausedAt = time.Now().UTC().Truncate(time.Millisecond)
		g.log.Warn("the host volume is almost full: VM paused until space is freed", "hostFree", HumanSize(free), "volume", g.dir)
	} else {
		g.log.Warn("host volume has space again: VM resumed", "hostFree", HumanSize(free))
	}
}

// pauseReportStep is how much the free space of a paused VM's volume must change, and
// pauseReportEvery how long after the last report, before the pause is reported again.
// Both must hold, so state.json is not rewritten on every tick (every 100 ms) while the
// free space of a busy volume swings by a MiB or more.
const (
	pauseReportStep  = 1 << 20
	pauseReportEvery = 2 * time.Second
)

// report tells onPause about a pause, a resume, or a paused VM's free space changing.
// A pause and a resume are reported at once; a change while paused at most every
// pauseReportEvery.
func (g *hostGuard) report(free uint64) {
	if g.onPause == nil {
		return
	}
	if !g.paused {
		if g.reported != nil {
			g.reported = nil
			g.onPause(nil)
		}
		return
	}
	now := g.now()
	if r := g.reported; r != nil && r.Since.Equal(g.pausedAt) &&
		(max(free, r.FreeBytes)-min(free, r.FreeBytes) < pauseReportStep || now.Sub(g.reportedAt) < pauseReportEvery) {
		return
	}
	g.reported = &GuardPauseState{Volume: g.volume, FreeBytes: free, ResumeAtBytes: g.cfg.ResumeAt(), Since: g.pausedAt}
	g.reportedAt = now
	p := *g.reported
	g.onPause(&p)
}

// volumeOf names the volume that holds dir: its mount point without the trailing
// backslash ("C:", or a folder for a volume mounted there), or dir itself if Windows
// cannot say.
func volumeOf(dir string) string {
	p, err := windows.UTF16PtrFromString(dir)
	if err == nil {
		var buf [windows.MAX_PATH + 1]uint16
		if err = windows.GetVolumePathName(p, &buf[0], uint32(len(buf))); err == nil {
			if v := strings.TrimSuffix(windows.UTF16ToString(buf[:]), `\`); v != "" {
				return v
			}
		}
	}
	if v := filepath.VolumeName(dir); v != "" {
		return v
	}
	return dir
}
