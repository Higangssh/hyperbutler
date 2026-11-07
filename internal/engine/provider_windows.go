//go:build windows

package engine

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/Higangssh/hyperbutler/internal/guid"
	"github.com/Higangssh/hyperbutler/internal/imageset"
	"github.com/Higangssh/hyperbutler/internal/vm"
	"github.com/Higangssh/hyperbutler/internal/vmsecret"
	"github.com/Higangssh/hyperbutler/internal/vmtemplate"
)

// BootSpec is what the engine asks a VMProvider for: sizes, feature bits and the typed
// network, nothing a provider could mistake for a path or a command line. The provider
// renders the VM from it with vmtemplate.
type BootSpec struct {
	CPUs, MemoryMB int
	// Net is the guest network as the stack runs it, nil for none.
	Net *vmtemplate.Net
	// Share and DaemonConfig add the share's service and daemon.json's wait.
	Share, DaemonConfig bool
	// BlankDisk says the data disk was never written: the guest may format it.
	BlankDisk bool
	// Slot is 0 for the Docker VM and 1..proto.MaxSlot for a sandbox session (ADR 0009),
	// which boots the sandbox image and offers sbxd's ports.
	Slot uint8
}

// RuntimeInfo names a booted VM.
type RuntimeInfo struct {
	// VMID is the compute system's name, empty when the provider does not say (the
	// broker names its VMs itself).
	VMID string
	// RuntimeID is the VM's runtime GUID: the VM ID hvsock addresses.
	RuntimeID guid.GUID
	// Secret is the VM's secret (ADR 0004 rule 6), which every hvsock connection to the
	// guest begins with. Both providers make one per boot, the broker on its side.
	Secret vmsecret.Secret
}

// Pauser pauses and resumes a VM (the free-space guard's last resort).
type Pauser interface {
	Pause() error
	Resume() error
}

// Lease is a booted VM, for as long as the engine holds it. Closing it releases the VM:
// the in-process provider closes the compute system (ShouldTerminateOnLastHandleClosed),
// the broker's terminates the VM when the connection closes (ADR 0004 Implementation
// decision 6).
type Lease interface {
	Pauser
	// State is the HCS state: "Running", "Paused", "Stopped", ...
	State() (string, error)
	// ShutDown asks for an orderly power-off through HCS; the VM stops when it has.
	ShutDown() error
	// Terminate stops the VM at once.
	Terminate() error
	Close() error
	// Gone is closed once the VM has stopped, on its own or not.
	Gone() <-chan struct{}
}

// VMProvider boots the VM. The in-process provider calls HCS from this process
// (developer mode, ADR 0004 option A); the broker provider asks the HCS access broker.
type VMProvider interface {
	Boot(ctx context.Context, spec BootSpec) (Lease, RuntimeInfo, error)
}

// hcsProvider is the in-process provider: it grants the VM its disks, creates and
// starts the compute system through cfg.Backend, and follows the serial console.
type hcsProvider struct {
	cfg            *Config
	sid            string
	img            *imageset.Set // nil for an initramfs-only image
	kernel, initrd string
	disk           *dataDisk // nil without a data disk
	step           func(string, ...any)
}

func (p *hcsProvider) Boot(ctx context.Context, bs BootSpec) (_ Lease, _ RuntimeInfo, err error) {
	cfg, log := p.cfg, p.cfg.Logger
	grant := cfg.granter()
	var granted []string
	revoke := func() {
		// In reverse: the system disk first, as the deferred revokes of the engine did.
		for i := len(granted) - 1; i >= 0; i-- {
			if err := cfg.Backend.RevokeAccess(cfg.VMID, granted[i]); err != nil {
				log.Warn("revoke VM access", "path", granted[i], "err", err)
			}
		}
	}
	defer func() {
		if err != nil {
			revoke()
		}
	}()
	consolePipe := `\\.\pipe\` + cfg.VMID + "-com1"
	// A fresh secret per boot, as the broker makes one per start: both modes give the
	// guest the same barrier.
	secret, err := vmsecret.New()
	if err != nil {
		return nil, RuntimeInfo{}, err
	}
	spec := &vmtemplate.Spec{
		Owner: VMOwner, UserSID: p.sid, Secret: secret,
		MemoryMB: bs.MemoryMB, CPUs: bs.CPUs,
		Kernel: p.kernel, Initrd: p.initrd,
		Net: bs.Net, Share: bs.Share, DaemonConfig: bs.DaemonConfig, Sandbox: bs.Slot != 0,
		ConsolePipe: consolePipe,
		Dev:         vmtemplate.Dev{CmdLine: cfg.CmdLine, ExtraCmdLine: cfg.ExtraCmdLine, ExtraServices: cfg.ExtraServices},
	}
	if d := p.disk; d != nil {
		// The VM worker runs as a per-VM virtual account; it needs an ACE on the VHDX.
		if err := grant(d.path); err != nil {
			return nil, RuntimeInfo{}, err
		}
		granted = append(granted, d.path)
		spec.Data, spec.BlankData = d.path, bs.BlankDisk
	}
	if img := p.img; img != nil {
		// The system disk, read-only at the hypervisor: the guest cannot change it. Under
		// Program Files (the MSI) the user cannot change its DACL, so the grant fails; the
		// installer gives every VM read access there instead (vm.VMGroupCanRead).
		if err := grant(img.System); err != nil {
			if !vm.IsAccessDenied(err) || !vm.VMGroupCanRead(img.System) {
				return nil, RuntimeInfo{}, err
			}
			log.Debug("system disk readable by all VMs; no per-VM grant", "path", img.System)
		} else {
			granted = append(granted, img.System)
		}
		spec.System = img.System
	}
	vcfg, err := vmtemplate.Build(spec)
	if err != nil {
		return nil, RuntimeInfo{}, err
	}
	m, err := cfg.Backend.Create(cfg.VMID, vcfg)
	if err != nil {
		return nil, RuntimeInfo{}, err
	}
	l := &hcsLease{m: m, gone: make(chan struct{}), revoke: revoke}
	defer func() {
		if err != nil {
			_ = m.Terminate()
			l.closeVM()
		}
	}()
	p.step("created %s", cfg.VMID)
	if err := m.Start(); err != nil {
		return nil, RuntimeInfo{}, err
	}
	p.step("started")
	console, err := openConsole(ctx, consolePipe)
	if err != nil {
		return nil, RuntimeInfo{}, err
	}
	// Closed by Close after the VM is gone: an early close would look like a VM exit.
	l.console = console
	consoleDone := make(chan struct{})
	go func() {
		streamConsole(console, cfg.ConsoleLog, p.step)
		close(consoleDone)
	}()
	go l.watch(consoleDone)
	rid, err := m.RuntimeID()
	if err != nil {
		return nil, RuntimeInfo{}, err
	}
	return l, RuntimeInfo{VMID: cfg.VMID, RuntimeID: rid, Secret: secret}, nil
}

// hcsLease is a compute system this process created.
type hcsLease struct {
	m       Machine
	console io.Closer
	revoke  func()

	gone     chan struct{}
	goneOnce sync.Once
	stopOnce sync.Once
	stop     chan struct{} // closed by Close, ends watch
	stopMu   sync.Mutex
}

func (l *hcsLease) State() (string, error) { return l.m.State() }
func (l *hcsLease) Pause() error           { return l.m.Pause() }
func (l *hcsLease) Resume() error          { return l.m.Resume() }
func (l *hcsLease) ShutDown() error        { return l.m.ShutDown() }
func (l *hcsLease) Terminate() error       { return l.m.Terminate() }
func (l *hcsLease) Gone() <-chan struct{}  { return l.gone }

func (l *hcsLease) markGone() { l.goneOnce.Do(func() { close(l.gone) }) }

func (l *hcsLease) stopCh() chan struct{} {
	l.stopMu.Lock()
	defer l.stopMu.Unlock()
	if l.stop == nil {
		l.stop = make(chan struct{})
	}
	return l.stop
}

// watch marks the VM gone when its console closes or HCS reports it stopped.
func (l *hcsLease) watch(consoleDone <-chan struct{}) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	stop := l.stopCh()
	for {
		select {
		case <-consoleDone:
			l.markGone()
			return
		case <-l.gone:
			return
		case <-stop:
			return
		case <-t.C:
			if s, err := l.m.State(); err == nil && s == "Stopped" {
				l.markGone()
				return
			}
		}
	}
}

// Close closes the console and the compute system and revokes the VM's access to the
// disks. Call it after Terminate.
func (l *hcsLease) Close() error {
	if l.closeVM() {
		l.revoke()
	}
	return nil
}

// closeVM closes the console and the compute system, once; it reports whether this call
// did.
func (l *hcsLease) closeVM() (closed bool) {
	l.stopOnce.Do(func() {
		close(l.stopCh())
		if l.console != nil {
			_ = l.console.Close()
		}
		_ = l.m.Close()
		closed = true
	})
	return closed
}

// errBrokerNeedsDisk refuses a broker start without a data disk: the broker boots only
// with a data location.
var errBrokerNeedsDisk = errors.New("engine: the broker boots a VM with a data disk only")
