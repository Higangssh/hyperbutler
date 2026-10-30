package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"

	"github.com/Higangssh/hyperbutler/internal/guestctl"
	"github.com/Higangssh/hyperbutler/internal/spaceguard"
)

type guestctlSupervisor = guestctl.Supervisor

func guestctlServiceID() string { return guestctl.ServiceID.String() }

// The guard mode tests the free-space guard (REQ-DISK-004) against a filling host volume.
//
//	hcs -mode guard -guard ...       ext4, guest spaceguard + host budget loop, fill until
//	                                 writes fail; reports ENOSPC/EIO counts and ext4 errors
//	hcs -mode guardcheck -keep       e2fsck, re-read and checksum every file on the same disk
//	hcs -mode guard (no -guard)      the same fill without the guard (the old EIO baseline)
//	-hostfill                        a host process fills the volume during the guest fill,
//	                                 to test the last resort: pause below the floor, resume
//
// The budget goes to the guest as text lines on a second COM port (/dev/ttyS1); the guest
// answers with status lines on the same port. See internal/spaceguard and
// guest/cmd/spaceguard.

var (
	procHcsPauseSystem = modComputecore.NewProc("HcsPauseComputeSystem")

	// guardPipe, when set, is the named pipe for COM2 (hcsConfig adds it).
	guardPipe string
	// initrdExtraFiles are packed under /wbextra by base name (buildInitrdExtra).
	initrdExtraFiles []string
)

type guardOpts struct {
	on                 bool
	bin                string
	margin, floor      uint64
	resumeGap, hyst    uint64
	interval           time.Duration
	hostFill           bool
	hostFillAt         uint64
	hostFillHold       time.Duration
	pauseOnly          bool
	vhdxMaxOverhead    uint64
	reportStatusPeriod time.Duration
	deadGuard          time.Duration
	zeroMB             int
}

func guardFlags(fs *flag.FlagSet) *guardOpts {
	o := &guardOpts{}
	fs.BoolVar(&o.on, "guard", false, "guard mode: run the free-space guard (host budget loop + guest spaceguard)")
	fs.StringVar(&o.bin, "guard-bin", filepath.Join(os.TempDir(), "hb-dev", "hyperbutler-spike", "guard-bin", "spaceguard"), "linux spaceguard binary to pack into the initrd")
	fs.Uint64Var(&o.margin, "guard-margin", 64<<20, "host bytes the guest may never consume")
	fs.Uint64Var(&o.floor, "guard-floor", 24<<20, "pause the VM below this much host free space")
	fs.Uint64Var(&o.resumeGap, "guard-resume", 32<<20, "resume once host free space is this far above the floor")
	fs.Uint64Var(&o.hyst, "guard-hyst", 16<<20, "guest reservation hysteresis band")
	fs.DurationVar(&o.interval, "guard-interval", 100*time.Millisecond, "host measurement interval")
	fs.BoolVar(&o.pauseOnly, "guard-pause-only", false, "send an unlimited budget (no reservation); only the hard-floor pause protects the host")
	fs.BoolVar(&o.hostFill, "hostfill", false, "guard mode: a host process fills the volume to below the floor while the guest writes, holds, then frees it")
	fs.Uint64Var(&o.hostFillAt, "hostfill-at", 200<<20, "start the host fill when host free space drops below this")
	fs.DurationVar(&o.hostFillHold, "hostfill-hold", 10*time.Second, "how long the host fill holds the volume below the floor")
	fs.Uint64Var(&o.vhdxMaxOverhead, "guard-vhdx-overhead", 64<<20, "VHDX metadata allowance added to the virtual size for VHDXMax")
	fs.DurationVar(&o.reportStatusPeriod, "guard-status", 2*time.Second, "print a guest status line at most this often")
	fs.IntVar(&o.zeroMB, "guard-zero", 0, "guard mode: write a file of this many MiB of zeros before the fill and overwrite it with random data after")
	fs.DurationVar(&o.deadGuard, "guard-dead", 5*time.Second, "guest status silence after which the host pauses at the margin instead of the floor")
	return o
}

// comPorts is the HCS ComPorts map: COM1 is the console, COM2 the guard channel.
func comPorts(console string) map[string]any {
	m := map[string]any{"0": map[string]any{"NamedPipe": console}}
	if guardPipe != "" {
		m["1"] = map[string]any{"NamedPipe": guardPipe}
	}
	return m
}

func (o *guardOpts) config() spaceguard.Config {
	return spaceguard.Config{Margin: o.margin, Floor: o.floor, ResumeGap: o.resumeGap, Hysteresis: o.hyst, Quantum: 1 << 20}
}

func (o *guardOpts) cmdline() string {
	if !o.on {
		return ""
	}
	s := fmt.Sprintf(" WB_GUARD=1 WB_HYST=%d", o.hyst)
	if o.zeroMB > 0 {
		s += fmt.Sprintf(" WB_ZERO=%d", o.zeroMB)
	}
	return s
}

// hostGuard is the host half: every interval it measures the volume and the VHDX, sends
// the budget to the guest, and pauses or resumes the VM at the hard floor.
type hostGuard struct {
	o      *guardOpts
	cfg    spaceguard.Config
	system uintptr
	vhdx   string
	vdir   string
	vmax   uint64
	vmeta  uint64 // VHDX metadata: its allocation before the guest wrote anything
	start  time.Time
	pipe   net.Conn
	// sup, when set, carries budgets over guestctl instead of the COM2 pipe.
	sup *guestctl.Supervisor
	// lastStatus is when the guest guard last reported (UnixNano). If it goes silent, the
	// host stops trusting the reservation and pauses at the margin instead of the floor.
	lastStatus atomic.Int64
	sendq      chan string
	mu         sync.Mutex
	paused     bool
	pauses     int
	minFree    uint64
	lastLog    time.Time
	lastB      spaceguard.Budget
	fill       *hostFiller
	stop       chan struct{}
	done       chan struct{}
}

// startHostGuard starts the host loop. vmeta is the VHDX's allocation before the guest
// wrote to it; it stands in for the VHDX metadata size (overstating it is safe).
func startHostGuard(o *guardOpts, system uintptr, vhdx string, virtSize, vmeta uint64, start time.Time, sup *guestctl.Supervisor) (*hostGuard, error) {
	if sup != nil {
		g := &hostGuard{
			o: o, cfg: o.config(), system: system, vhdx: vhdx, vdir: filepath.Dir(vhdx),
			vmax: virtSize + o.vhdxMaxOverhead, vmeta: vmeta, start: start, sup: sup,
			minFree: ^uint64(0), stop: make(chan struct{}), done: make(chan struct{}),
		}
		g.lastStatus.Store(time.Now().UnixNano())
		if err := g.cfg.Validate(); err != nil {
			return nil, err
		}
		if o.hostFill {
			g.fill = &hostFiller{path: filepath.Join(g.vdir, "hostfill.bin"), target: o.floor / 2, hold: o.hostFillHold, at: o.hostFillAt, g: g}
		}
		go g.loop()
		return g, nil
	}
	// The pipe is opened for overlapped I/O (go-winio). With a synchronous handle, Windows
	// serializes the reader's pending ReadFile and the sender's WriteFile, so budgets
	// only got through when the guest happened to print a status line.
	var conn net.Conn
	var err error
	for deadline := time.Now().Add(10 * time.Second); ; {
		timeout := time.Second
		conn, err = winio.DialPipe(guardPipe, &timeout)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("open %s: %w", guardPipe, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	g := &hostGuard{
		o: o, cfg: o.config(), system: system, vhdx: vhdx, vdir: filepath.Dir(vhdx),
		vmax: virtSize + o.vhdxMaxOverhead, vmeta: vmeta, start: start,
		pipe: conn, sendq: make(chan string, 1),
		minFree: ^uint64(0), stop: make(chan struct{}), done: make(chan struct{}),
	}
	g.lastStatus.Store(time.Now().UnixNano())
	if err := g.cfg.Validate(); err != nil {
		g.pipe.Close()
		return nil, err
	}
	if o.hostFill {
		g.fill = &hostFiller{path: filepath.Join(g.vdir, "hostfill.bin"), target: o.floor / 2, hold: o.hostFillHold, at: o.hostFillAt, g: g}
	}
	go g.sender()
	go g.readStatus()
	go g.loop()
	return g, nil
}

// sender writes budget lines. The pipe write can block (the guest UART drains slowly, or
// the VM is paused), so it runs on its own goroutine with a one-slot queue: the loop
// never blocks and only the newest budget is sent.
func (g *hostGuard) sender() {
	for line := range g.sendq {
		if _, err := g.pipe.Write([]byte(line)); err != nil {
			if !errors.Is(err, net.ErrClosed) && !errors.Is(err, os.ErrClosed) { // close() closes the pipe under a pending write
				g.logf("guard: send: %v", err)
			}
			return
		}
	}
}

func (g *hostGuard) send(line string) {
	select {
	case <-g.sendq: // replace an unsent older budget
	default:
	}
	select {
	case g.sendq <- line:
	default:
	}
}

func (g *hostGuard) readStatus() {
	sc := bufio.NewScanner(g.pipe)
	var last time.Time
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.HasPrefix(line, "status ") {
			g.lastStatus.Store(time.Now().UnixNano())
		}
		important := !strings.Contains(line, " short=0 ") || !strings.HasSuffix(line, "err=<nil>")
		if important || time.Since(last) >= g.o.reportStatusPeriod {
			g.logf("guest %s", line)
			last = time.Now()
		}
	}
}

func (g *hostGuard) logf(format string, args ...any) {
	fmt.Printf("  [%6.1fs] %s\n", time.Since(g.start).Seconds(), fmt.Sprintf(format, args...))
}

func (g *hostGuard) loop() {
	defer close(g.done)
	t := time.NewTicker(g.o.interval)
	defer t.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-t.C:
		}
		// Every failure below fails closed: budget 0 makes the guest reserve everything,
		// and a free-space reading of 0 pauses the VM. The pause check always runs.
		free, ferr := freeBytesErr(g.vdir)
		if ferr != nil {
			g.logf("guard: free space: %v", ferr)
		}
		var b spaceguard.Budget
		u, err := measure(g.vhdx)
		if err == nil {
			b, err = spaceguard.HostBudget(spaceguard.Host{Free: free, VHDXAllocated: uint64(u.Allocated), VHDXMetadata: min(g.vmeta, uint64(u.Allocated)), VHDXMax: g.vmax}, g.cfg)
		}
		if err != nil {
			g.logf("guard: %v; sending budget 0", err)
			b = spaceguard.Budget{}
		}
		if g.o.pauseOnly {
			b = spaceguard.Budget{Unlimited: true}
		}
		if g.sup != nil {
			g.sup.SetBudget(b)
			if t := ctlStatusAt.Load(); t > g.lastStatus.Load() {
				g.lastStatus.Store(t)
			}
		} else {
			g.send(spaceguard.FormatBudget(b))
		}

		g.mu.Lock()
		if free < g.minFree {
			g.minFree = free
		}
		paused := g.paused
		g.mu.Unlock()
		pcfg := g.cfg
		if silent := time.Since(time.Unix(0, g.lastStatus.Load())); silent > g.o.deadGuard && !paused {
			// The guest guard is not reporting: dead, killed, or its channel is broken.
			// Its reservation may be gone, so pause at the margin rather than the floor.
			pcfg.Floor = pcfg.Margin - 1
			if time.Since(g.lastLog) >= time.Second {
				g.logf("guard: no guest status for %v; pausing below the margin", silent.Round(time.Second))
				g.lastLog = time.Now()
			}
		}
		want := spaceguard.PauseWanted(free, paused, pcfg)
		if want != paused {
			g.setPaused(want, free)
		}
		if b != g.lastB && time.Since(g.lastLog) >= time.Second {
			g.logf("host free=%.1f MiB vhdx=%.1f MiB budget=%s", float64(free)/mb, float64(u.Allocated)/mb, b)
			g.lastLog, g.lastB = time.Now(), b
		}
		if g.fill != nil {
			g.fill.tick(free)
		}
	}
}

func (g *hostGuard) setPaused(pause bool, free uint64) {
	name, proc := "HcsResumeComputeSystem", procHcsResumeSystem
	if pause {
		name, proc = "HcsPauseComputeSystem", procHcsPauseSystem
	}
	if pause && g.sup != nil {
		g.sup.SetPaused(true) // before the guest goes silent
	}
	t0 := time.Now()
	_, err := hcsCall(name, 30*time.Second, func(op uintptr) uintptr {
		hr, _, _ := proc.Call(g.system, op, 0)
		return hr
	})
	if g.sup != nil && (!pause || err != nil) {
		// Resumed (or the pause failed): the guest could not report while paused, so
		// restart the silence clock instead of treating the pause as a dead guard.
		g.sup.SetPaused(false)
		g.lastStatus.Store(time.Now().UnixNano())
		ctlStatusAt.Store(time.Now().UnixNano())
	}
	g.logf("guard: host free %.1f MiB: %s: %s (%v)", float64(free)/mb, name, errString(err), time.Since(t0).Round(time.Millisecond))
	printState(g.system)
	if err != nil {
		return
	}
	g.mu.Lock()
	g.paused = pause
	if pause {
		g.pauses++
	}
	g.mu.Unlock()
}

func (g *hostGuard) close() {
	close(g.stop)
	<-g.done
	g.mu.Lock()
	paused := g.paused
	g.mu.Unlock()
	if paused {
		g.setPaused(false, freeBytes(g.vdir))
	}
	if g.fill != nil {
		g.fill.remove()
	}
	if g.pipe != nil {
		close(g.sendq)
		g.pipe.Close()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	fmt.Printf("  guard summary: min host free %.1f MiB, pauses %d\n", float64(g.minFree)/mb, g.pauses)
}

// hostFiller stands in for another host process that eats the volume while the guest
// writes. Once host free space drops below `at`, it writes a plain file until free space
// is below target (under the floor), holds it for `hold`, then deletes it.
type hostFiller struct {
	path     string
	target   uint64
	at       uint64
	hold     time.Duration
	g        *hostGuard
	state    int // 0 waiting, 1 filling, 2 holding, 3 done
	f        *os.File
	heldAt   time.Time
	filled   int64
	pausedOK bool
}

func (h *hostFiller) tick(free uint64) {
	switch h.state {
	case 0:
		if free >= h.at {
			return
		}
		f, err := os.OpenFile(h.path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			h.g.logf("hostfill: %v", err)
			h.state = 3
			return
		}
		h.f, h.state = f, 1
		h.g.logf("hostfill: host free %.1f MiB, filling to %.1f MiB", float64(free)/mb, float64(h.target)/mb)
		fallthrough
	case 1:
		buf := make([]byte, 4<<20)
		// Write at most 64 MiB per tick so the guard loop keeps measuring in between.
		for i := 0; i < 16; i++ {
			free = freeBytes(h.g.vdir)
			if free <= h.target {
				h.f.Sync()
				h.state, h.heldAt = 2, time.Now()
				h.g.logf("hostfill: wrote %.1f MiB, host free %.1f MiB; holding %v", float64(h.filled)/mb, float64(free)/mb, h.hold)
				return
			}
			n := min(uint64(len(buf)), free-h.target)
			w, err := h.f.Write(buf[:n])
			h.filled += int64(w)
			if err != nil {
				h.f.Sync()
				h.state, h.heldAt = 2, time.Now()
				h.g.logf("hostfill: write: %v after %.1f MiB; holding", err, float64(h.filled)/mb)
				return
			}
		}
	case 2:
		h.g.mu.Lock()
		if h.g.paused {
			h.pausedOK = true
		}
		h.g.mu.Unlock()
		if time.Since(h.heldAt) < h.hold {
			return
		}
		h.g.logf("hostfill: VM was paused while held: %v; removing %s", h.pausedOK, filepath.Base(h.path))
		h.remove()
		h.state = 3
	}
}

func (h *hostFiller) remove() {
	if h.f == nil {
		return
	}
	h.f.Close()
	h.f = nil
	if err := os.Remove(h.path); err != nil {
		h.g.logf("hostfill: remove: %v", err)
	}
}

// freeBytesErr is freeBytes with the error: GetDiskFreeSpaceExW's caller-available bytes.
func freeBytesErr(dir string) (uint64, error) {
	var free uint64
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	if r, _, e := procGetDiskFreeSpaceEx.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&free)), 0, 0); r == 0 {
		return 0, e
	}
	return free, nil
}
