//go:build windows

package broker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/guest/image"
	"github.com/Higangssh/hyperbutler/internal/imageset"
	"github.com/Higangssh/hyperbutler/internal/vm"
	"github.com/Higangssh/hyperbutler/internal/vmtemplate"
)

// OwnerTagPrefix starts the HCS Owner of every VM the broker creates; the owner's SID
// follows (ADR 0004 rule 9). The startup sweep terminates the ones it does not track.
const OwnerTagPrefix = "hyperbutler-broker/"

// vmNamePrefix starts the compute system name of every broker VM.
const vmNamePrefix = "hyperbutler-broker-"

// ImageDirName is the guest image folder next to the broker executable, the only place
// the broker boots an image from (Implementation decision 3).
const ImageDirName = "image"

// hcsSystemFileAccess is the read-only access the system disk is granted, when the installer
// has not given every VM read access to the image folder (vm.VMGroupCanRead).
const hcsSystemFileAccess = windows.FILE_GENERIC_READ

// HCSConfig configures an HCSBackend.
type HCSConfig struct {
	// StateDir is a directory of the broker's own (created with a DACL for the broker's
	// account only), where the runtime id probe creates its file.
	StateDir string
	// AllowUnpinned boots an image without the SHA-256 pin (a development build, which
	// carries none). Without it an unpinned broker refuses every start.
	AllowUnpinned bool
	// Logf receives the backend's events.
	Logf func(format string, args ...any)

	// imageDir replaces the image folder next to the executable, and api the HCS
	// calls, in tests.
	imageDir string
	api      hcsAPI
}

// hcsAPI is every HCS call the backend makes; hcsCalls makes them through internal/vm.
type hcsAPI interface {
	Create(id string, cfg *vm.Config) (hcsSystem, error)
	Open(id string) (hcsSystem, error)
	// List lists every compute system the broker may see, of any owner.
	List() ([]vm.Summary, error)
	// Probe returns the runtime id compute system id will have and the access
	// HcsGrantVmAccess gives its account, using a file in dir.
	Probe(dir, id string) (guid.GUID, uint32, error)
}

type hcsSystem interface {
	Start() error
	RuntimeID() (guid.GUID, error)
	State() (string, error)
	Pause() error
	Resume() error
	ShutDown() error
	Terminate() error
	Close() error
}

type hcsCalls struct{}

func (hcsCalls) Create(id string, cfg *vm.Config) (hcsSystem, error) {
	s, err := vm.Create(id, cfg)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (hcsCalls) Open(id string) (hcsSystem, error) {
	s, err := vm.Open(id)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (hcsCalls) List() ([]vm.Summary, error) { return vm.List("") }

func (hcsCalls) Probe(dir, id string) (guid.GUID, uint32, error) { return vm.ProbeVMAccount(dir, id) }

// HCSBackend runs each owner's VM through HCS. It is the broker's only HCS user.
//
// For every start it builds the VM document with vmtemplate from the request's typed
// fields and its own files only:
//
//   - the guest image from the image folder next to its executable, held open
//     deny-write and checked against image.json and the pin compiled in (imageset);
//   - the data disk opened by handle below the validated data location (openDataDisk),
//     held open without FILE_SHARE_DELETE, and handed to HCS by the final path of that
//     handle;
//   - every hvsock service descriptor for the caller's SID only.
//
// The VM's account is given access to the disks by an entry set on the open handles
// (vm.AccountSID of the runtime id it will have), never by path.
type HCSBackend struct {
	cfg      HCSConfig
	imageDir string
	api      hcsAPI

	mu  sync.Mutex
	vms map[string]*hcsVM // by owner SID
}

type hcsVM struct {
	name     string
	sys      hcsSystem
	rid      guid.GUID
	account  string
	img      *imageset.Set
	disk     *diskFile
	sysACE   windows.Handle // the system disk's handle the entry was set on, or 0
	cpus     uint32
	memoryMB uint32
}

// NewHCSBackend returns a backend that boots the image in the image folder next to the
// running executable.
func NewHCSBackend(cfg HCSConfig) (*HCSBackend, error) {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if !filepath.IsAbs(cfg.StateDir) {
		return nil, errors.New("broker: the HCS backend needs a state directory")
	}
	dir := cfg.imageDir
	if dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(filepath.Dir(exe), ImageDirName)
	}
	api := cfg.api
	if api == nil {
		api = hcsCalls{}
	}
	return &HCSBackend{cfg: cfg, imageDir: dir, api: api, vms: make(map[string]*hcsVM)}, nil
}

// OwnerTag is the HCS Owner of sid's VMs.
func OwnerTag(sid string) string { return OwnerTagPrefix + sid }

// Sweep terminates every compute system that carries the broker's owner tag and is not
// one of this backend's (ADR 0004 rule 9): VMs a crashed broker left, whose vmmem
// would otherwise stay (REQ-MEM-001). It returns how many it terminated.
func (b *HCSBackend) Sweep() int {
	list, err := b.api.List()
	if err != nil {
		b.cfg.Logf("broker: list compute systems: %v", err)
		return 0
	}
	b.mu.Lock()
	tracked := make(map[string]bool, len(b.vms))
	for _, v := range b.vms {
		tracked[v.name] = true
	}
	b.mu.Unlock()
	n := 0
	for _, s := range list {
		if !strings.HasPrefix(s.Owner, OwnerTagPrefix) || tracked[s.ID] {
			continue
		}
		sys, err := b.api.Open(s.ID)
		if err != nil {
			b.cfg.Logf("broker: open untracked VM %s: %v", s.ID, err)
			continue
		}
		if err := sys.Terminate(); err != nil {
			b.cfg.Logf("broker: terminate untracked VM %s: %v", s.ID, err)
		} else {
			b.cfg.Logf("broker: terminated untracked VM %s of %s (%s)", s.ID, strings.TrimPrefix(s.Owner, OwnerTagPrefix), s.State)
			n++
		}
		_ = sys.Close()
	}
	return n
}

// Close terminates every VM the backend runs.
func (b *HCSBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for owner, v := range b.vms {
		if err := v.sys.Terminate(); err != nil {
			b.cfg.Logf("broker: terminate %s: %v", v.name, err)
		}
		b.release(v)
		delete(b.vms, owner)
	}
	return nil
}

func randomHex(n int) string {
	p := make([]byte, n)
	_, _ = rand.Read(p)
	return hex.EncodeToString(p)
}

// Start implements Backend.
func (b *HCSBackend) Start(_ context.Context, spec VMSpec) (VMStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if v := b.vms[spec.Owner]; v != nil {
		if st, err := b.statusLocked(spec.Owner, v); err != nil || st.Running {
			return st, err
		}
	}
	if spec.DataDir == nil {
		return VMStatus{}, fmt.Errorf("%w: no data location", ErrDiskRejected)
	}
	if spec.Net != nil && spec.Net.Port != 0 {
		return VMStatus{}, errors.New("broker: the network link runs on its fixed port only")
	}
	pin, pinned := imageset.Pin()
	if !pinned && !b.cfg.AllowUnpinned {
		return VMStatus{}, fmt.Errorf("%w: this broker carries no image pin (a development build needs -allow-unpinned-image)", ErrImageRejected)
	}
	img, err := imageset.Open(filepath.Join(b.imageDir, image.ManifestName), pin)
	if err != nil {
		return VMStatus{}, fmt.Errorf("%w: %w", ErrImageRejected, err)
	}
	v := &hcsVM{img: img, cpus: spec.CPUs, memoryMB: spec.MemoryMB}
	ok := false
	defer func() {
		if !ok {
			b.release(v)
		}
	}()
	if v.disk, err = openDataDisk(spec.DataDir.Handle(), DataDiskName, spec.Owner); err != nil {
		return VMStatus{}, err
	}
	v.name = vmNamePrefix + randomHex(8)
	rid, mask, err := b.api.Probe(b.cfg.StateDir, v.name)
	if err != nil {
		return VMStatus{}, fmt.Errorf("broker: learn the VM's account: %w", err)
	}
	v.rid, v.account = rid, vm.AccountSID(rid)
	if err := v.disk.grant(v.account, mask); err != nil {
		v.account = "" // nothing to revoke from the data disk
		return VMStatus{}, fmt.Errorf("broker: grant the VM the data disk: %w", err)
	}
	if err := v.grantSystem(); err != nil {
		return VMStatus{}, err
	}
	n, err := vmtemplate.Build(&vmtemplate.Spec{
		Owner: OwnerTag(spec.Owner), UserSID: spec.Owner,
		MemoryMB: int(spec.MemoryMB), CPUs: int(spec.CPUs),
		Kernel: img.Kernel, Initrd: img.Initrd, System: img.System,
		Data: v.disk.Path(), BlankData: spec.BlankDisk,
		Net: spec.Net, Share: spec.Share, DaemonConfig: spec.DaemonConfig,
	})
	if err != nil {
		return VMStatus{}, err
	}
	if v.sys, err = b.api.Create(v.name, n); err != nil {
		return VMStatus{}, err
	}
	if err := v.sys.Start(); err != nil {
		_ = v.sys.Terminate()
		return VMStatus{}, err
	}
	got, err := v.sys.RuntimeID()
	if err != nil {
		_ = v.sys.Terminate()
		return VMStatus{}, err
	}
	if got != rid {
		// Its account is not the one the disks were granted to.
		_ = v.sys.Terminate()
		return VMStatus{}, fmt.Errorf("broker: %s has runtime id %s, its access was granted for %s", v.name, got, rid)
	}
	ok = true
	b.vms[spec.Owner] = v
	b.cfg.Logf("broker: started %s for %s (runtime id %s)", v.name, spec.Owner, rid)
	return v.status(true, false), nil
}

// grantSystem gives the VM read access to the system disk, unless every VM may read it
// already (the installer's entry for NT VIRTUAL MACHINE\Virtual Machines). The entry is
// set on a handle reopened from the held one, so no path is resolved again.
func (v *hcsVM) grantSystem() error {
	f := v.img.SystemFile()
	if f == nil {
		return fmt.Errorf("%w: the image has no system disk", ErrImageRejected)
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err == nil && vm.VMGroupCanReadSD(sd) {
		return nil
	}
	h, err := reOpenFile(windows.Handle(f.Fd()), windows.READ_CONTROL|windows.WRITE_DAC, windows.FILE_SHARE_READ, 0)
	if err != nil {
		return fmt.Errorf("%w: the system disk is not readable by VMs and its access cannot be set: %w", ErrImageRejected, err)
	}
	if err := grantHandle(h, v.account, hcsSystemFileAccess); err != nil {
		_ = windows.CloseHandle(h)
		return fmt.Errorf("broker: grant the VM the system disk: %w", err)
	}
	v.sysACE = h
	return nil
}

// release closes v's system handle and revokes and closes everything else it holds.
func (b *HCSBackend) release(v *hcsVM) {
	if v.sys != nil {
		_ = v.sys.Close()
		v.sys = nil
	}
	if v.disk != nil {
		if v.account != "" {
			if err := v.disk.revoke(v.account); err != nil {
				b.cfg.Logf("broker: revoke %s on the data disk: %v", v.account, err)
			}
		}
		_ = v.disk.Close()
		v.disk = nil
	}
	if v.sysACE != 0 {
		if err := revokeHandle(v.sysACE, []string{v.account}); err != nil {
			b.cfg.Logf("broker: revoke %s on the system disk: %v", v.account, err)
		}
		_ = windows.CloseHandle(v.sysACE)
		v.sysACE = 0
	}
	if v.img != nil {
		_ = v.img.Close()
		v.img = nil
	}
}

func (v *hcsVM) status(running, paused bool) VMStatus {
	if !running {
		return VMStatus{}
	}
	return VMStatus{Running: true, Paused: paused, ID: v.rid.ToArray(), CPUs: v.cpus, MemoryMB: v.memoryMB}
}

// statusLocked asks HCS for v's state; a stopped VM is released and forgotten.
func (b *HCSBackend) statusLocked(owner string, v *hcsVM) (VMStatus, error) {
	s, err := v.sys.State()
	if err != nil {
		return VMStatus{}, err
	}
	switch s {
	case "Stopped":
		b.cfg.Logf("broker: %s of %s stopped", v.name, owner)
		b.release(v)
		delete(b.vms, owner)
		return VMStatus{}, nil
	case "Paused":
		return v.status(true, true), nil
	}
	return v.status(true, false), nil
}

// Status implements Backend.
func (b *HCSBackend) Status(_ context.Context, owner string) (VMStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v := b.vms[owner]
	if v == nil {
		return VMStatus{}, nil
	}
	return b.statusLocked(owner, v)
}

// Stop implements Backend. A forced stop terminates the VM; otherwise HCS asks the guest
// to power off, and a later status finds it stopped.
func (b *HCSBackend) Stop(_ context.Context, owner string, force bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	v := b.vms[owner]
	if v == nil {
		return ErrNotRunning
	}
	if !force {
		return v.sys.ShutDown()
	}
	err := v.sys.Terminate()
	b.release(v)
	delete(b.vms, owner)
	return err
}

// Resize implements Backend. A running VM keeps its size; the new one applies at the
// next start.
func (b *HCSBackend) Resize(context.Context, string, uint32, uint32) error {
	return errors.New("broker: a running VM cannot be resized; stop it first")
}

// Pause implements Backend.
func (b *HCSBackend) Pause(_ context.Context, owner string, resume bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	v := b.vms[owner]
	if v == nil {
		return ErrNotRunning
	}
	if resume {
		return v.sys.Resume()
	}
	return v.sys.Pause()
}
