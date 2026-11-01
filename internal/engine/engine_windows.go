//go:build windows

package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/guestctl"
	"github.com/Higangssh/hyperbutler/internal/netstack"
	"github.com/Higangssh/hyperbutler/internal/spaceguard"
	"github.com/Higangssh/hyperbutler/internal/transport"
	"github.com/Higangssh/hyperbutler/internal/vm"
)

// GuardConfig is the free-space guard policy: guest writes get ENOSPC before the host
// volume drops below 5 GiB (REQ-DISK-005's default reserve), and the VM is paused
// below 1 GiB, the last resort if other host programs fill the volume.
var GuardConfig = spaceguard.Config{
	Margin:     5 << 30,
	Floor:      1 << 30,
	ResumeGap:  256 << 20,
	Hysteresis: 64 << 20,
	Quantum:    1 << 20,
}

// DefaultCmdLine is the base kernel command line.
//
// swiotlb=noforce: a guest with RAM above 4 GiB of physical address space (any VM of
// 4 GiB, because of the MMIO gap below 4 GiB) otherwise reserves and zeroes a 64 MiB
// bounce-buffer pool at boot, which then stays committed on the host. Nothing here
// needs it: the Hyper-V synthetic devices (vmbus storage, network, hvsock) are not
// DMA-limited, and this is not an isolated (confidential) VM, where Linux forces bounce
// buffers regardless. See docs/research/idle-memory.md.
//
// page_reporting.page_reporting_order=0: hv_balloon's free page reporting hands free
// memory back to the host (the cold discard hint), by default only in free blocks of
// 2 MiB (order 9) or more. After a workload has touched most of the guest's RAM, the
// slab and kernel pages it left behind split many 2 MiB blocks, and the free pieces
// around them stay committed on the host: 50 to 90 MiB after a 4 GiB + 4 GiB workload
// in a 12 GiB guest, growing with each workload. Reporting single pages returns them.
// Idle guest CPU did not change measurably. See docs/research/mem-reclaim.md
// (REQ-MEM-002).
//
// The dentry and inode hash table sizes are added per VM size (HashTableArgs).
const DefaultCmdLine = "console=ttyS0 quiet swiotlb=noforce page_reporting.page_reporting_order=0"

// DockerPort is the AF_VSOCK port of the guest agent's Docker API relay.
const DockerPort = 2375

// agentMaxConns is the guest agent's default -max-conns.
const agentMaxConns = 256

// Config is one VM run.
type Config struct {
	// KernelPath and InitrdPath are the guest artifacts of an initramfs-only image.
	KernelPath, InitrdPath string
	// Image, if set, is an image manifest (image.json, ADR 0005): the kernel, the
	// stage-1 initramfs and the read-only system disk it names are verified and used
	// instead of KernelPath and InitrdPath.
	Image string
	// CmdLine is the base kernel command line (DefaultCmdLine if empty).
	CmdLine string
	// MemoryMB and CPUs size the VM (4096 and 2 if zero).
	MemoryMB, CPUs int
	// Pipe is where the Docker API is served (transport.DefaultPipeName if empty).
	Pipe string
	// VMID names the compute system ("hyperbutler-" + random if empty). It must start with
	// "hyperbutler-".
	VMID string
	// Disk, if Data is set, is the persistent data disk: Disk.Data holds data.vhdx and
	// disk.json, created on the first start with DiskSize bytes. Without it the guest
	// keeps /var/lib/docker in RAM (a development mode).
	Disk     Layout
	DiskSize uint64
	// Guard is the free-space guard policy (GuardConfig if zero).
	Guard spaceguard.Config
	// Net is the guest network.
	Net NetConfig
	// Share is the bind-mount file server for Windows paths (ADR 0007).
	Share ShareConfig
	// DaemonConfig, if set, delivers the user's daemon.json to the guest on every
	// control connection; the guest's init waits for it before starting dockerd.
	DaemonConfig *DaemonConfig
	// ReadyTimeout bounds the wait for dockerd (90 s). ShutdownTimeout bounds the guest's
	// orderly power-off before the VM is terminated (25 s).
	ReadyTimeout, ShutdownTimeout time.Duration
	// ConsoleLog receives the guest serial console (discarded if nil).
	ConsoleLog io.Writer
	// Logger receives the engine's events (slog.Default() if nil).
	Logger *slog.Logger
	// Backend makes the privileged calls (HCSBackend if nil).
	Backend Backend
	// ExtraServices and ExtraCmdLine are added to the VM document and the kernel command
	// line (development harness features such as the network bench).
	ExtraServices []vm.Service
	ExtraCmdLine  string
	// OnCreated, if set, is called once the compute system exists.
	OnCreated func(vmID string, runtimeID guid.GUID)
	// OnReady, if set, is called once dockerd answers, before the pipe is served. If it
	// returns ErrDone, Run shuts the VM down and returns nil; any other error is returned.
	OnReady func(ctx context.Context, r *Running) error
	// OnStopping, if set, is called when the shutdown begins.
	OnStopping func()
}

// ErrDone ends Run cleanly from OnReady.
var ErrDone = errors.New("engine: done")

// Running describes a booted VM, for OnReady.
type Running struct {
	VMID      string
	RuntimeID guid.GUID
	// Docker is the hvsock address of the guest's Docker API.
	Docker transport.HvsockTarget
	// Stack is the user-mode network stack, nil when the network is off.
	Stack *netstack.Stack
	// Started is when Run began; Ready is when dockerd first answered.
	Started, Ready time.Time
}

// CodedError carries one of the Code* values with the error that caused it.
type CodedError struct {
	Code string
	Err  error
}

func (e *CodedError) Error() string { return e.Err.Error() }
func (e *CodedError) Unwrap() error { return e.Err }

func codeErr(code string, err error) error {
	var ce *CodedError
	if errors.As(err, &ce) {
		return err
	}
	return &CodedError{Code: code, Err: err}
}

// ErrorCode returns err's Code* value, or "" if it has none.
func ErrorCode(err error) string {
	var ce *CodedError
	if errors.As(err, &ce) {
		return ce.Code
	}
	if vm.IsAccessDenied(err) {
		return CodeHCSAccessDenied
	}
	return ""
}

func (c *Config) defaults() {
	if c.CmdLine == "" {
		c.CmdLine = DefaultCmdLine
	}
	if c.MemoryMB == 0 {
		c.MemoryMB = 4096
	}
	if c.CPUs == 0 {
		c.CPUs = 2
	}
	if c.Pipe == "" {
		c.Pipe = transport.DefaultPipeName
	}
	if c.VMID == "" {
		c.VMID = "hyperbutler-" + randomHex(8)
	}
	if c.Guard == (spaceguard.Config{}) {
		c.Guard = GuardConfig
	}
	if c.ReadyTimeout <= 0 {
		c.ReadyTimeout = 90 * time.Second
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = 25 * time.Second
	}
	if c.ConsoleLog == nil {
		c.ConsoleLog = io.Discard
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Backend == nil {
		c.Backend = HCSBackend{}
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// CurrentUserSDDL grants hvsock access to the user running this process only.
func CurrentUserSDDL() (string, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("query token user: %w", err)
	}
	return vm.UserOnlySDDL(tu.User.Sid.String())
}

// Run boots the VM and serves the Docker API until ctx ends, the VM stops on its own,
// or OnReady returns. Then it shuts the guest down (an orderly power-off through the
// Hyper-V shutdown integration component, bounded by ShutdownTimeout) and terminates
// the VM. A VM that stopped on its own gives a CodedError with CodeVMExited.
func Run(ctx context.Context, cfg Config) (err error) {
	cfg.defaults()
	log := cfg.Logger
	if !validToken(cfg.VMID, maxVMIDLen, true) || len(cfg.VMID) <= len("hyperbutler-") || !strings.HasPrefix(cfg.VMID, "hyperbutler-") {
		return fmt.Errorf("engine: bad VM id %q", cfg.VMID)
	}
	t0 := time.Now()
	step := func(format string, a ...any) {
		log.Info(fmt.Sprintf("[%7.1f ms] %s", float64(time.Since(t0).Microseconds())/1000, fmt.Sprintf(format, a...)))
	}
	var kpath, ipath string
	var img *guestImage
	pin, pinned := ImagePin()
	if cfg.Image != "" {
		// Held open (deny-write) until Run returns, after the VM is gone.
		if img, err = openImage(cfg.Image, pin); err != nil {
			return err
		}
		defer func() { _ = img.Close() }()
		kpath, ipath = img.kernel, img.initrd
		step("verified guest image %s (system disk %s)", cfg.Image, HumanSize(img.man.System.Size))
		if pinned {
			step("guest image matches the image.json this build was released with (sha256 %s)", pin)
		} else {
			log.Info("development build: no guest image hash is embedded, so the image is checked against its own image.json only")
		}
	} else {
		if pinned {
			return fmt.Errorf("engine: this hyperbutler.exe boots only the guest image it was released with (an image.json next to it); " +
				"extract the release zip again, or use a development build for an initramfs-only image")
		}
		if kpath, err = filepath.Abs(cfg.KernelPath); err != nil {
			return err
		}
		if ipath, err = filepath.Abs(cfg.InitrdPath); err != nil {
			return err
		}
		for _, p := range []string{kpath, ipath} {
			if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
				return fmt.Errorf("engine: guest artifact %s is missing (use --image-dir)", p)
			}
		}
	}

	sddl, err := CurrentUserSDDL()
	if err != nil {
		return err
	}
	// Listen first: if the pipe name is taken (another hyperbutler, or a squatter), fail
	// before booting anything.
	ln, err := transport.ListenPipe(cfg.Pipe)
	if err != nil {
		return codeErr(CodePipeInUse, err)
	}
	defer func() { _ = ln.Close() }()

	gnet, netSvcs, netArgs, err := setupNet(cfg.Net, sddl, log)
	if err != nil {
		return err
	}
	if gnet != nil {
		defer gnet.close()
	}

	shares, shareSvcs, shareArgs := setupShares(cfg.Share, sddl, log)
	if shares != nil {
		// Deferred before the VM's own teardown, so it runs after it: the guest keeps
		// its shares through the orderly shutdown.
		defer shares.close()
	}

	var disk *dataDisk
	cmdline := cfg.CmdLine + HashTableArgs(cfg.MemoryMB) + netArgs + shareArgs + cfg.ExtraCmdLine
	if cfg.DaemonConfig != nil {
		cmdline += " hyperbutler.daemoncfg=1"
	}
	var disks []vm.Disk
	if cfg.Disk.Data != "" {
		if disk, err = prepareDisk(cfg.Disk, cfg.DiskSize); err != nil {
			return err
		}
		if disk.created {
			step("created data disk %s (%s, sparse, 1 MiB blocks)", disk.path, FormatSize(disk.meta.SizeBytes))
		}
		// The VM worker runs as a per-VM virtual account; it needs an ACE on the VHDX.
		if err := cfg.Backend.GrantAccess(cfg.VMID, disk.path); err != nil {
			return err
		}
		defer func() {
			if err := cfg.Backend.RevokeAccess(cfg.VMID, disk.path); err != nil {
				log.Warn("revoke VM access to the data disk", "err", err)
			}
		}()
		disks = []vm.Disk{{Path: disk.path, Sparse: true}}
		if img != nil {
			cmdline += " hyperbutler.data=lun:0"
		} else {
			cmdline += " hyperbutler.data=sda" // what the initramfs-only image understands
		}
		if disk.blank {
			cmdline += " hyperbutler.newdisk=1"
		}
	}
	if img != nil {
		// The system disk, read-only at the hypervisor: the guest cannot change it.
		if err := cfg.Backend.GrantAccess(cfg.VMID, img.system); err != nil {
			return err
		}
		defer func() {
			if err := cfg.Backend.RevokeAccess(cfg.VMID, img.system); err != nil {
				log.Warn("revoke VM access to the system disk", "err", err)
			}
		}()
		cmdline += fmt.Sprintf(" hyperbutler.system=lun:%d", len(disks))
		disks = append(disks, vm.Disk{Path: img.system, ReadOnly: true})
	}

	dockerSvc := transport.VsockServiceID(DockerPort)
	consolePipe := `\\.\pipe\` + cfg.VMID + "-com1"
	svcs := []vm.Service{{ID: dockerSvc, ConnectSDDL: sddl}, {ID: guestctl.ServiceID, ConnectSDDL: sddl}}
	svcs = append(append(append(svcs, netSvcs...), shareSvcs...), cfg.ExtraServices...)
	m, err := cfg.Backend.Create(cfg.VMID, &vm.Config{
		Owner:          VMOwner,
		KernelPath:     kpath,
		InitrdPath:     ipath,
		KernelCmdLine:  cmdline,
		MemoryMB:       cfg.MemoryMB,
		ProcessorCount: cfg.CPUs,
		MemoryHints:    true,
		Disks:          disks,
		ConsolePipe:    consolePipe,
		Services:       svcs,
	})
	if err != nil {
		return err
	}
	r := &runner{cfg: &cfg, m: m, log: log, step: step, vmGone: make(chan struct{})}
	defer func() { r.stopVM() }()
	step("created %s", cfg.VMID)
	if err := m.Start(); err != nil {
		return err
	}
	step("started")

	console, err := openConsole(ctx, consolePipe)
	if err != nil {
		return err
	}
	// Closed by stopVM after the VM is gone: an early close would look like a VM exit.
	r.console = console
	consoleDone := make(chan struct{})
	go func() {
		streamConsole(console, cfg.ConsoleLog, step)
		close(consoleDone)
	}()
	go r.watch(consoleDone)

	vmid, err := m.RuntimeID()
	if err != nil {
		return err
	}
	if cfg.OnCreated != nil {
		cfg.OnCreated(cfg.VMID, vmid)
	}
	target := transport.HvsockTarget{VMID: vmid, ServiceID: dockerSvc}
	step("runtime id %s", vmid)
	var rewrite transport.CreateRewriter
	if shares != nil {
		if err := shares.listen(vmid); err != nil {
			return err
		}
		rewrite = shares.rewriteCreate
	}

	// runCtx ends with ctx or when the VM stops on its own.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	go func() {
		select {
		case <-r.vmGone:
			cancelRun()
		case <-runCtx.Done():
		}
	}()

	if gnet != nil {
		if err := gnet.start(runCtx, vmid, step); err != nil {
			return err
		}
	}

	// The guest control channel and, with a data disk, the free-space guard.
	sup := &guestctl.Supervisor{
		Dial: guestctl.HvsockDialer(vmid),
		// An hvsock connect to a port nobody listens on yet waits for the whole timeout,
		// and dockerd waits for hyperbutlerd's first budget: a short timeout lets the first
		// connect land soon after hyperbutlerd listens (about 0.3 s into a system-disk boot).
		DialTimeout: 400 * time.Millisecond,
		MaxBackoff:  2 * time.Second,
		Logf:        func(f string, a ...any) { log.Debug(fmt.Sprintf(f, a...)) },
	}
	if dc := cfg.DaemonConfig; dc != nil {
		sup.OnConnect = func(ctx context.Context, c *guestctl.Client) { dc.Connected(ctx, c) }
	}
	var guard *hostGuard
	gs := &guestStats{log: log}
	sup.Config.OnStatus = func(s guestctl.Status) {
		if guard != nil {
			guard.onStatus(s)
		}
		gs.onStatus(s)
	}
	if disk != nil {
		if guard, err = newHostGuard(cfg.Guard, m, sup, disk, log); err != nil {
			return err
		}
	} else {
		// No disk, no guard: hyperbutlerd runs with the cache policy only.
		sup.SetBudget(spaceguard.Budget{Unlimited: true})
	}
	supCtx, cancelSup := context.WithCancel(context.Background())
	supDone := make(chan struct{})
	go func() { _ = sup.Run(supCtx); close(supDone) }()
	defer func() { cancelSup(); <-supDone }()
	// Deferred after the supervisor, so it runs while the control channel is still up.
	defer r.shutdownGuest(sup)
	if guard != nil {
		guard.start()
		defer guard.close() // runs first: no pause decisions during the shutdown
	}

	if err := waitPing(runCtx, target, cfg.ReadyTimeout); err != nil {
		if r.exited() {
			return r.exitErr()
		}
		if ctx.Err() != nil {
			return nil // stopped while booting
		}
		return err
	}
	if dc := cfg.DaemonConfig; dc != nil {
		// At boot the guest may still be settling daemon.json (a file dockerd did not
		// start with is rolled back, and a previous one applied with a dockerd restart):
		// report ready only for the dockerd that stays.
		wctx, cancel := context.WithTimeout(runCtx, cfg.ReadyTimeout)
		err := dc.WaitDelivered(wctx)
		cancel()
		if err != nil {
			log.Warn("daemon.json delivery did not finish; continuing", "err", err)
		} else if st := dc.State(); st.Restarted {
			if err := waitPing(runCtx, target, cfg.ReadyTimeout); err != nil {
				if r.exited() {
					return r.exitErr()
				}
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
	ready := time.Now()
	step("dockerd answered /_ping over hvsock")
	if cfg.OnReady != nil {
		var st *netstack.Stack
		if gnet != nil {
			st = gnet.st
		}
		err := cfg.OnReady(runCtx, &Running{VMID: cfg.VMID, RuntimeID: vmid, Docker: target, Stack: st, Started: t0, Ready: ready})
		if errors.Is(err, ErrDone) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	if gnet != nil && cfg.Net.AutoPublish {
		gnet.startAutoPublish(runCtx, target, step)
	}
	if shares != nil {
		shares.startLifetime(runCtx, target)
	}

	p := &transport.Proxy{
		Upstream: &transport.HvsockDialer{Target: target, Retries: 2, RetryWait: 100 * time.Millisecond},
		// Match the guest agent's -max-conns, so excess clients wait for a pipe instance
		// instead of timing out on an hvsock dial the agent will not accept.
		MaxConns: agentMaxConns,
		Logger:   slog.New(levelFilter{h: log.Handler(), min: slog.LevelWarn}),
		// Windows bind sources in container creates become shares (ADR 0007).
		RewriteCreate: rewrite,
	}
	step("serving %s", cfg.Pipe)
	if err := p.Serve(runCtx, ln); err != nil && runCtx.Err() == nil {
		return err
	}
	if r.exited() && ctx.Err() == nil {
		return r.exitErr()
	}
	return nil
}

// runner owns the compute system's shutdown.
type runner struct {
	cfg  *Config
	m    Machine
	log  *slog.Logger
	step func(string, ...any)

	console  io.Closer
	vmGone   chan struct{}
	goneOnce sync.Once
	// shutdownTried is set once an orderly shutdown was attempted.
	shutdownTried bool
}

func (r *runner) markGone() { r.goneOnce.Do(func() { close(r.vmGone) }) }

func (r *runner) exited() bool {
	select {
	case <-r.vmGone:
		return true
	default:
		return false
	}
}

func (r *runner) exitErr() error {
	return codeErr(CodeVMExited, errors.New("the VM stopped on its own; see the console log"))
}

// watch marks the VM gone when its console closes or HCS reports it stopped.
func (r *runner) watch(consoleDone <-chan struct{}) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-consoleDone:
			r.markGone()
			return
		case <-r.vmGone:
			return
		case <-t.C:
			if s, err := r.m.State(); err == nil && s == "Stopped" {
				r.markGone()
				return
			}
		}
	}
}

// shutdownGuest asks hyperbutlerd for an orderly power-off (dockerd stops the containers,
// the data disk is unmounted) and waits up to ShutdownTimeout for the VM to stop.
func (r *runner) shutdownGuest(sup *guestctl.Supervisor) {
	if r.exited() || r.shutdownTried {
		return
	}
	r.shutdownTried = true
	if r.cfg.OnStopping != nil {
		r.cfg.OnStopping()
	}
	t := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := sup.Shutdown(ctx)
	cancel()
	if err != nil {
		// HCS's own shutdown needs an integration service this guest does not run
		// (HcsShutDownComputeSystem fails with ERROR_NOT_SUPPORTED); try it anyway.
		r.log.Warn("the guest did not accept an orderly shutdown", "err", err)
		if err := r.m.ShutDown(); err != nil {
			r.log.Warn("HCS shutdown refused too; terminating the VM", "err", err)
			return
		}
	}
	r.wait(t)
}

func (r *runner) wait(t time.Time) {
	select {
	case <-r.vmGone:
		r.step("guest powered off in %v", time.Since(t).Round(time.Millisecond))
	case <-time.After(r.cfg.ShutdownTimeout):
		r.log.Warn("the guest did not power off in time; terminating the VM", "timeout", r.cfg.ShutdownTimeout)
	}
}

// stopVM terminates and closes the compute system, after an orderly shutdown if none
// was tried yet (Run failed before the control channel existed).
func (r *runner) stopVM() {
	defer func() { _ = r.m.Close() }()
	if !r.exited() && !r.shutdownTried {
		r.shutdownTried = true
		if r.cfg.OnStopping != nil {
			r.cfg.OnStopping()
		}
		if err := r.m.ShutDown(); err == nil {
			r.wait(time.Now())
		}
	}
	if err := r.m.Terminate(); err != nil {
		r.log.Warn("terminate", "err", err)
	}
	if r.console != nil {
		_ = r.console.Close()
	}
	r.step("VM stopped")
}

// guestStats logs what hyperbutlerd reports that is worth a line: trims and cache
// reclaims. The values are guest-reported, for display only.
type guestStats struct {
	log                 *slog.Logger
	trims, cacheTrims   uint64
	trimmed, cacheFreed uint64
	seen                bool
	lastMem             time.Time
}

func (g *guestStats) onStatus(s guestctl.Status) {
	if g.seen && s.Trims > g.trims {
		g.log.Info("guest fstrim", "trims", s.Trims, "trimmed", HumanSize(s.Trimmed-min(s.Trimmed, g.trimmed)), "fsFree", HumanSize(s.FSFree))
	}
	if g.seen && s.CacheTrims > g.cacheTrims {
		g.log.Info("guest page-cache reclaim", "reclaims", s.CacheTrims, "freed", HumanSize(s.CacheFreed-min(s.CacheFreed, g.cacheFreed)), "memFree", HumanSize(s.MemFree))
	}
	g.trims, g.trimmed, g.cacheTrims, g.cacheFreed, g.seen = s.Trims, s.Trimmed, s.CacheTrims, s.CacheFreed, true
	// The guest's own view of its memory, once a minute: with the host's vmmem private
	// working set it splits the idle footprint (REQ-MEM-001) into guest use and the rest.
	if now := time.Now(); now.Sub(g.lastMem) >= time.Minute {
		g.lastMem = now
		used := s.MemTotal - min(s.MemTotal, s.MemFree+s.FileCache+s.Shmem)
		g.log.Info("guest memory", "totalMiB", s.MemTotal>>20, "freeMiB", s.MemFree>>20,
			"usedMiB", used>>20, "fileCacheMiB", s.FileCache>>20, "shmemMiB", s.Shmem>>20)
	}
}
