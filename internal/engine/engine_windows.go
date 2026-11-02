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
	"time"

	"github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/guestctl"
	"github.com/Higangssh/hyperbutler/internal/imageset"
	"github.com/Higangssh/hyperbutler/internal/netstack"
	"github.com/Higangssh/hyperbutler/internal/spaceguard"
	"github.com/Higangssh/hyperbutler/internal/transport"
	"github.com/Higangssh/hyperbutler/internal/vm"
	"github.com/Higangssh/hyperbutler/internal/vmtemplate"
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

// DefaultCmdLine is the base kernel command line (vmtemplate.DefaultCmdLine, which
// explains each argument).
const DefaultCmdLine = vmtemplate.DefaultCmdLine

// DockerPort is the AF_VSOCK port of the guest agent's Docker API relay.
const DockerPort = vmtemplate.DockerPort

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
	// Broker, if set, is the pipe of the HCS access broker (ADR 0004) that boots the VM
	// instead of this process. The broker boots its own guest image, so Image,
	// KernelPath, InitrdPath, CmdLine, the Extra fields, Backend and OnGrant are not
	// used, and no serial console is opened; Disk.Data is required.
	Broker string
	// ExtraServices and ExtraCmdLine are added to the VM document and the kernel command
	// line (development harness features such as the network bench).
	ExtraServices []vm.Service
	ExtraCmdLine  string
	// OnGrant, if set, is called before the VM is first given access to a file, with
	// the runtime id it will have (Backend.RuntimeIDOf), so that the access entry is on
	// record before it exists: a crash between the grant and Create leaves it behind.
	OnGrant func(vmID string, runtimeID guid.GUID)
	// OnCreated, if set, is called once the compute system exists.
	OnCreated func(vmID string, runtimeID guid.GUID)
	// OnReady, if set, is called once dockerd answers, before the pipe is served. If it
	// returns ErrDone, Run shuts the VM down and returns nil; any other error is returned.
	OnReady func(ctx context.Context, r *Running) error
	// OnStopping, if set, is called when the shutdown begins.
	OnStopping func()
	// OnGuardPause, if set, is called when the free-space guard pauses the VM, while it
	// stays paused and the free space changes, and with nil when it resumes it.
	OnGuardPause func(*GuardPauseState)
	// OnGuestFS, if set, is called with the guest data file system's size and free
	// space when they changed enough to show (guestFSThrottle; REQ-DISK-009).
	OnGuestFS func(*GuestFSState)
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
		if HyperVAdminsSignInPending() {
			return CodeHCSSignInPending
		}
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

// granter returns cfg.Backend.GrantAccess for cfg.VMID that, before the first grant,
// learns the VM's runtime id and reports it to OnGrant. A runtime id that cannot be
// learned is logged and the grant goes ahead: OnCreated reports the id once the VM
// exists, and only a crash in between leaves an entry nobody recorded.
func (c *Config) granter() func(path string) error {
	announced := false
	return func(path string) error {
		if !announced {
			announced = true
			if rid, err := c.Backend.RuntimeIDOf(c.VMID); err != nil {
				c.Logger.Warn("cannot learn the VM's runtime id before granting it access", "vm", c.VMID, "err", err)
			} else if c.OnGrant != nil {
				c.OnGrant(c.VMID, rid)
			}
		}
		return c.Backend.GrantAccess(c.VMID, path)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// CurrentUserSDDL grants hvsock access to the user running this process only.
func CurrentUserSDDL() (string, error) {
	sid, err := currentUserSID()
	if err != nil {
		return "", err
	}
	return vm.UserOnlySDDL(sid)
}

// currentUserSID is the user SID of this process's token.
func currentUserSID() (string, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("query token user: %w", err)
	}
	return tu.User.Sid.String(), nil
}

// restrictToUser gives path a protected DACL (nothing inherited) for the current user
// only.
func restrictToUser(path string) error {
	s, err := CurrentUserSDDL()
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(s)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
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
	var provider VMProvider
	hp := &hcsProvider{cfg: &cfg, step: step}
	if cfg.Broker != "" {
		// The broker boots its own guest image (the image folder next to it, checked
		// against its pin) and grants the VM its disks itself.
		step("booting through the HCS access broker on %s", cfg.Broker)
		provider = &brokerProvider{pipe: cfg.Broker, dataDir: cfg.Disk.Data, log: log}
	} else {
		provider = hp
		pin, pinned := ImagePin()
		if cfg.Image != "" {
			// Held open (deny-write) until Run returns, after the VM is gone.
			if hp.img, err = imageset.Open(cfg.Image, pin); err != nil {
				return err
			}
			defer func() { _ = hp.img.Close() }()
			hp.kernel, hp.initrd = hp.img.Kernel, hp.img.Initrd
			step("verified guest image %s (system disk %s)", cfg.Image, HumanSize(hp.img.Manifest.System.Size))
			if pinned {
				step("guest image matches the image.json this build was released with (sha256 %s)", pin)
			} else {
				log.Info("development build: no guest image hash is embedded, so the image is checked against its own image.json only")
			}
		} else {
			if pinned {
				return fmt.Errorf("engine: this hyperbutler.exe boots only the guest image it was released with (image\\image.json next to it); " +
					"extract the release zip again, or use a development build for an initramfs-only image")
			}
			if hp.kernel, err = filepath.Abs(cfg.KernelPath); err != nil {
				return err
			}
			if hp.initrd, err = filepath.Abs(cfg.InitrdPath); err != nil {
				return err
			}
			for _, p := range []string{hp.kernel, hp.initrd} {
				if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
					return fmt.Errorf("engine: guest artifact %s is missing (use --image-dir)", p)
				}
			}
		}
	}

	if hp.sid, err = currentUserSID(); err != nil {
		return err
	}
	// Listen first: if the pipe name is taken (another hyperbutler, or a squatter), fail
	// before booting anything.
	ln, err := transport.ListenPipe(cfg.Pipe)
	if err != nil {
		return codeErr(CodePipeInUse, err)
	}
	defer func() { _ = ln.Close() }()

	gnet, netSpec, err := setupNet(cfg.Net, log)
	if err != nil {
		return err
	}
	if gnet != nil {
		defer gnet.close()
	}

	shares := setupShares(cfg.Share, log)
	if shares != nil {
		// Deferred before the VM's own teardown, so it runs after it: the guest keeps
		// its shares through the orderly shutdown.
		defer shares.close()
	}

	var disk *dataDisk
	dockerSvc := transport.VsockServiceID(DockerPort)
	bs := BootSpec{CPUs: cfg.CPUs, MemoryMB: cfg.MemoryMB, Net: netSpec, Share: shares != nil, DaemonConfig: cfg.DaemonConfig != nil}
	if cfg.Disk.Data != "" {
		if disk, err = prepareDisk(cfg.Disk, cfg.DiskSize); err != nil {
			return err
		}
		if disk.created {
			step("created data disk %s (%s, sparse, 1 MiB blocks)", disk.path, FormatSize(disk.meta.SizeBytes))
		}
		hp.disk, bs.BlankDisk = disk, disk.blank
	}

	lease, info, err := provider.Boot(ctx, bs)
	if err != nil {
		return err
	}
	r := &runner{cfg: &cfg, l: lease, log: log, step: step}
	defer func() { r.stopVM() }()
	vmid := info.RuntimeID
	if cfg.OnCreated != nil {
		cfg.OnCreated(info.VMID, vmid)
	}
	// Every connection to the guest begins with the VM secret (ADR 0004 rule 6), and
	// every dial of the guest's Docker API goes through target.
	target := transport.HvsockTarget{VMID: vmid, ServiceID: dockerSvc, Secret: info.Secret}
	step("runtime id %s", vmid)
	var rewrite transport.CreateRewriter
	if shares != nil {
		if err := shares.listen(vmid, info.Secret); err != nil {
			return err
		}
		rewrite = shares.rewriteCreate
	}

	// runCtx ends with ctx or when the VM stops on its own.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	go func() {
		select {
		case <-lease.Gone():
			cancelRun()
		case <-runCtx.Done():
		}
	}()

	if gnet != nil {
		if err := gnet.start(runCtx, vmid, info.Secret, step); err != nil {
			return err
		}
	}

	// The guest control channel and, with a data disk, the free-space guard.
	sup := &guestctl.Supervisor{
		Dial: guestctl.HvsockDialer(vmid, info.Secret),
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
	var fsThrottle guestFSThrottle
	sup.Config.OnStatus = func(s guestctl.Status) {
		if guard != nil {
			guard.onStatus(s)
		}
		gs.onStatus(s)
		if cfg.OnGuestFS != nil && disk != nil {
			if st, ok := fsThrottle.next(s.Guard, s.FSSize, s.FSFree, time.Now()); ok {
				cfg.OnGuestFS(st)
			}
		}
	}
	if disk != nil {
		if guard, err = newHostGuard(cfg.Guard, lease, sup, disk, log); err != nil {
			return err
		}
		guard.onPause = cfg.OnGuardPause
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
		err := cfg.OnReady(runCtx, &Running{VMID: info.VMID, RuntimeID: vmid, Docker: target, Stack: st, Started: t0, Ready: ready})
		if errors.Is(err, ErrDone) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	var checkStart transport.StartChecker
	if gnet != nil && cfg.Net.AutoPublish {
		gnet.startAutoPublish(runCtx, target, step)
		// A start whose ports a host program holds fails as on Docker Desktop.
		checkStart = gnet.checkStart
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
		CheckStart:    checkStart,
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

// runner owns the VM's shutdown.
type runner struct {
	cfg  *Config
	l    Lease
	log  *slog.Logger
	step func(string, ...any)

	// shutdownTried is set once an orderly shutdown was attempted.
	shutdownTried bool
}

func (r *runner) exited() bool {
	select {
	case <-r.l.Gone():
		return true
	default:
		return false
	}
}

func (r *runner) exitErr() error {
	if l, ok := r.l.(interface{ LostBroker() bool }); ok && l.LostBroker() {
		return codeErr(CodeBrokerLost, errors.New("the connection to the HCS access broker ended (the service stopped or restarted), and the broker terminated the VM"))
	}
	return codeErr(CodeVMExited, errors.New("the VM stopped on its own; see the console log"))
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
		if err := r.l.ShutDown(); err != nil {
			r.log.Warn("HCS shutdown refused too; terminating the VM", "err", err)
			return
		}
	}
	r.wait(t)
}

func (r *runner) wait(t time.Time) {
	// A broker lease learns of the power-off by polling; have it poll often now.
	if l, ok := r.l.(interface{ ExpectGone() }); ok {
		l.ExpectGone()
	}
	select {
	case <-r.l.Gone():
		r.step("guest powered off in %v", time.Since(t).Round(time.Millisecond))
	case <-time.After(r.cfg.ShutdownTimeout):
		r.log.Warn("the guest did not power off in time; terminating the VM", "timeout", r.cfg.ShutdownTimeout)
	}
}

// stopVM terminates the VM and closes its lease, after an orderly shutdown if none was
// tried yet (Run failed before the control channel existed).
func (r *runner) stopVM() {
	defer func() { _ = r.l.Close() }()
	if !r.exited() && !r.shutdownTried {
		r.shutdownTried = true
		if r.cfg.OnStopping != nil {
			r.cfg.OnStopping()
		}
		if err := r.l.ShutDown(); err == nil {
			r.wait(time.Now())
		}
	}
	if err := r.l.Terminate(); err != nil {
		r.log.Warn("terminate", "err", err)
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
		args := []any{"trims", s.Trims, "trimmed", HumanSize(s.Trimmed - min(s.Trimmed, g.trimmed))}
		// While the guard fails closed its reservation holds the free space: no fsFree,
		// which readers of this line (the MCP disk tool) take as the guest's free space.
		if !failingClosed(s.Guard) && s.FSSize != 0 {
			args = append(args, "fsFree", HumanSize(s.FSFree))
		}
		g.log.Info("guest fstrim", args...)
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
