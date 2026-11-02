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
	"time"

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

	// imageDir replaces the image folder next to the executable, api the HCS calls,
	// openDisk openDataDisk, and diskTimeout dataDirTimeout, in tests.
	imageDir    string
	api         hcsAPI
	openDisk    func(dir windows.Handle, name, sid string) (*diskFile, error)
	diskTimeout time.Duration
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
//   - the guest image from the image folder next to its executable, opened, checked
//     against image.json and the pin compiled in (imageset) once, and then held
//     deny-write for the backend's life, so every VM boots what was verified;
//   - the data disk opened by handle below the validated data location (openDataDisk),
//     held open without FILE_SHARE_DELETE, and handed to HCS by the final path of that
//     handle;
//   - every hvsock service descriptor for the caller's SID only.
//
// The VM's account is given access to the disks by an entry set on open handles
// (vm.AccountSID of the runtime id it will have), never by path.
//
// Each owner's operations are serialized by that owner's own lock; mu guards only the
// maps, so a start that waits on a slow file system or on HCS holds up no other owner,
// and no lease release.
type HCSBackend struct {
	cfg      HCSConfig
	imageDir string
	api      hcsAPI

	mu     sync.Mutex
	owners map[string]*ownerSlot // by owner SID
	names  map[string]bool       // compute system names of tracked VMs

	imgMu sync.Mutex
	img   *sharedImage // nil until the first start opens it
}

// ownerSlot is one owner's VM, with the lock that serializes that owner's operations.
type ownerSlot struct {
	mu sync.Mutex
	vm *hcsVM
}

// sharedImage is the verified image every VM boots, open for the backend's life.
type sharedImage struct {
	set *imageset.Set
	// vmReadable says the installer gave every VM read access to the system disk; then
	// it is never ACLed.
	vmReadable bool
	// dacl is the system disk reopened with WRITE_DAC (0 when vmReadable), and accounts
	// the VM accounts this backend put on it, both guarded by aclMu.
	aclMu    sync.Mutex
	dacl     windows.Handle
	accounts map[string]bool
}

type hcsVM struct {
	name       string
	sys        hcsSystem
	rid        guid.GUID
	account    string
	disk       *diskFile
	sysGranted bool // the system disk carries an entry for account
	cpus       uint32
	memoryMB   uint32
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
	if cfg.openDisk == nil {
		cfg.openDisk = openDataDisk
	}
	if cfg.diskTimeout <= 0 {
		cfg.diskTimeout = dataDirTimeout
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
	return &HCSBackend{cfg: cfg, imageDir: dir, api: api, owners: make(map[string]*ownerSlot), names: make(map[string]bool)}, nil
}

// OwnerTag is the HCS Owner of sid's VMs.
func OwnerTag(sid string) string { return OwnerTagPrefix + sid }

// slot returns owner's slot, creating it if create is set (nil otherwise).
func (b *HCSBackend) slot(owner string, create bool) *ownerSlot {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.owners[owner]
	if s == nil && create {
		s = &ownerSlot{}
		b.owners[owner] = s
	}
	return s
}

func (b *HCSBackend) track(name string, on bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if on {
		b.names[name] = true
	} else {
		delete(b.names, name)
	}
}

// Sweep terminates the compute systems that carry the owner tag tag (or, with tag
// empty, any broker owner tag) and are not this backend's (ADR 0004 rule 9): VMs a
// crashed broker left, whose vmmem would otherwise stay (REQ-MEM-001). A broker run in
// the foreground by one user passes OwnerTag of that user, so it never touches another
// user's VMs. It returns how many it terminated.
func (b *HCSBackend) Sweep(tag string) int {
	list, err := b.api.List()
	if err != nil {
		b.cfg.Logf("broker: list compute systems: %v", err)
		return 0
	}
	b.mu.Lock()
	tracked := make(map[string]bool, len(b.names))
	for n := range b.names {
		tracked[n] = true
	}
	b.mu.Unlock()
	n := 0
	for _, s := range list {
		if tag != "" && s.Owner != tag || !strings.HasPrefix(s.Owner, OwnerTagPrefix) || tracked[s.ID] {
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

// Close terminates every VM the backend runs and closes the image.
func (b *HCSBackend) Close() error {
	b.mu.Lock()
	slots := make(map[string]*ownerSlot, len(b.owners))
	for o, s := range b.owners {
		slots[o] = s
	}
	b.mu.Unlock()
	for _, s := range slots {
		s.mu.Lock()
		if v := s.vm; v != nil {
			if err := v.sys.Terminate(); err != nil {
				b.cfg.Logf("broker: terminate %s: %v", v.name, err)
			}
			b.release(v)
			s.vm = nil
		}
		s.mu.Unlock()
	}
	b.imgMu.Lock()
	defer b.imgMu.Unlock()
	if img := b.img; img != nil {
		if img.dacl != 0 {
			_ = windows.CloseHandle(img.dacl)
		}
		_ = img.set.Close()
		b.img = nil
	}
	return nil
}

func randomHex(n int) string {
	p := make([]byte, n)
	_, _ = rand.Read(p)
	return hex.EncodeToString(p)
}

// image opens and verifies the guest image on first use and keeps it. A failure is not
// kept: the next start tries again.
func (b *HCSBackend) image() (*sharedImage, error) {
	b.imgMu.Lock()
	defer b.imgMu.Unlock()
	if b.img != nil {
		return b.img, nil
	}
	pin, pinned := imageset.Pin()
	if !pinned && !b.cfg.AllowUnpinned {
		return nil, fmt.Errorf("%w: this broker carries no image pin (a development build needs -allow-unpinned-image)", ErrImageRejected)
	}
	set, err := imageset.Open(filepath.Join(b.imageDir, image.ManifestName), pin)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrImageRejected, err)
	}
	f := set.SystemFile()
	if f == nil {
		_ = set.Close()
		return nil, fmt.Errorf("%w: the image has no system disk", ErrImageRejected)
	}
	img := &sharedImage{set: set, accounts: map[string]bool{}}
	h := windows.Handle(f.Fd())
	if sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION); err == nil && vm.VMGroupCanReadSD(sd) {
		img.vmReadable = true
	} else {
		// Reopened by the held handle, so no path is resolved again.
		if img.dacl, err = reOpenFile(h, windows.READ_CONTROL|windows.WRITE_DAC, windows.FILE_SHARE_READ, 0); err != nil {
			_ = set.Close()
			return nil, fmt.Errorf("%w: the system disk is not readable by VMs and its access cannot be set: %w", ErrImageRejected, err)
		}
	}
	b.img = img
	return img, nil
}

// grantSystem gives v's account read access to the system disk, unless every VM may
// read it already. Entries of VM accounts this backend does not run (left by a crash)
// are removed; other owners' running VMs keep theirs.
func (img *sharedImage) grantSystem(v *hcsVM) error {
	if img.vmReadable {
		return nil
	}
	img.aclMu.Lock()
	defer img.aclMu.Unlock()
	on, err := vmAccountsOn(img.dacl)
	if err != nil {
		return err
	}
	var stale []string
	for _, a := range on {
		if !img.accounts[a] && a != v.account {
			stale = append(stale, a)
		}
	}
	if err := setDACLEntries(img.dacl, append(stale, v.account), v.account, hcsSystemFileAccess); err != nil {
		return fmt.Errorf("broker: grant the VM the system disk: %w", err)
	}
	img.accounts[v.account], v.sysGranted = true, true
	return nil
}

// revokeSystem removes v's own entry from the system disk.
func (img *sharedImage) revokeSystem(v *hcsVM) error {
	img.aclMu.Lock()
	defer img.aclMu.Unlock()
	delete(img.accounts, v.account)
	v.sysGranted = false
	return revokeHandle(img.dacl, []string{v.account})
}

// openDiskBounded runs cfg.openDisk but stops waiting when ctx ends or after
// diskTimeout. The open goes through whatever serves the caller's file (an oplock the
// caller holds on it, a ProjFS or cloud-files provider) and cannot be canceled; as with
// openDataDirBounded, the abandoned goroutine closes the handle if the call returns.
func (b *HCSBackend) openDiskBounded(ctx context.Context, dir windows.Handle, sid string) (*diskFile, error) {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.diskTimeout)
	defer cancel()
	type result struct {
		d   *diskFile
		err error
	}
	ch := make(chan result, 1)
	var mu sync.Mutex
	abandoned := false
	go func() {
		d, err := b.cfg.openDisk(dir, DataDiskName, sid)
		mu.Lock()
		defer mu.Unlock()
		if abandoned {
			if d != nil {
				_ = d.Close()
			}
			b.cfg.Logf("broker: data disk open finished after timeout: %v", err)
			return
		}
		ch <- result{d, err}
	}()
	select {
	case r := <-ch:
		return r.d, r.err
	case <-ctx.Done():
		mu.Lock()
		defer mu.Unlock()
		select {
		case r := <-ch:
			return r.d, r.err
		default:
		}
		abandoned = true
		return nil, fmt.Errorf("%w: the data disk did not open in time: %w", ErrDiskRejected, ctx.Err())
	}
}

// Start implements Backend.
func (b *HCSBackend) Start(ctx context.Context, spec VMSpec) (VMStatus, error) {
	s := b.slot(spec.Owner, true)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vm != nil {
		if st, err := b.statusLocked(spec.Owner, s); err != nil || st.Running {
			return st, err
		}
	}
	if spec.DataDir == nil {
		return VMStatus{}, fmt.Errorf("%w: no data location", ErrDiskRejected)
	}
	if spec.Net != nil && spec.Net.Port != 0 {
		return VMStatus{}, errors.New("broker: the network link runs on its fixed port only")
	}
	img, err := b.image()
	if err != nil {
		return VMStatus{}, err
	}
	v := &hcsVM{cpus: spec.CPUs, memoryMB: spec.MemoryMB}
	ok := false
	defer func() {
		if !ok {
			b.release(v)
		}
	}()
	if v.disk, err = b.openDiskBounded(ctx, spec.DataDir.Handle(), spec.Owner); err != nil {
		return VMStatus{}, err
	}
	if err := ctx.Err(); err != nil {
		return VMStatus{}, err
	}
	v.name = vmNamePrefix + randomHex(8)
	rid, mask, err := b.api.Probe(b.cfg.StateDir, v.name)
	if err != nil {
		return VMStatus{}, fmt.Errorf("broker: learn the VM's account: %w", err)
	}
	v.rid = rid
	account := vm.AccountSID(rid)
	if err := v.disk.grant(account, mask); err != nil {
		return VMStatus{}, fmt.Errorf("broker: grant the VM the data disk: %w", err)
	}
	v.account = account
	if err := img.grantSystem(v); err != nil {
		return VMStatus{}, err
	}
	n, err := vmtemplate.Build(&vmtemplate.Spec{
		Owner: OwnerTag(spec.Owner), UserSID: spec.Owner,
		MemoryMB: int(spec.MemoryMB), CPUs: int(spec.CPUs),
		Kernel: img.set.Kernel, Initrd: img.set.Initrd, System: img.set.System,
		Data: v.disk.Path(), BlankData: spec.BlankDisk,
		Net: spec.Net, Share: spec.Share, DaemonConfig: spec.DaemonConfig,
	})
	if err != nil {
		return VMStatus{}, err
	}
	// The last point where giving up costs nothing: HCS calls cannot be canceled.
	if err := ctx.Err(); err != nil {
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
	s.vm = v
	b.track(v.name, true)
	b.cfg.Logf("broker: started %s for %s (runtime id %s)", v.name, spec.Owner, rid)
	return v.status(true, false), nil
}

// release closes v's system handle and revokes and closes everything else it holds.
func (b *HCSBackend) release(v *hcsVM) {
	if v.sys != nil {
		_ = v.sys.Close()
		v.sys = nil
	}
	if v.name != "" {
		b.track(v.name, false)
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
	if v.sysGranted && v.account != "" {
		b.imgMu.Lock()
		img := b.img
		b.imgMu.Unlock()
		if img != nil {
			if err := img.revokeSystem(v); err != nil {
				b.cfg.Logf("broker: revoke %s on the system disk: %v", v.account, err)
			}
		}
	}
}

func (v *hcsVM) status(running, paused bool) VMStatus {
	if !running {
		return VMStatus{}
	}
	return VMStatus{Running: true, Paused: paused, ID: v.rid.ToArray(), CPUs: v.cpus, MemoryMB: v.memoryMB}
}

// statusLocked asks HCS for the state of s's VM; a stopped VM is released and
// forgotten. s.mu is held.
func (b *HCSBackend) statusLocked(owner string, s *ownerSlot) (VMStatus, error) {
	v := s.vm
	st, err := v.sys.State()
	if err != nil {
		return VMStatus{}, err
	}
	switch st {
	case "Stopped":
		b.cfg.Logf("broker: %s of %s stopped", v.name, owner)
		b.release(v)
		s.vm = nil
		return VMStatus{}, nil
	case "Paused":
		return v.status(true, true), nil
	}
	return v.status(true, false), nil
}

// locked runs fn with owner's slot locked, or returns ErrNotRunning if owner has no VM.
func (b *HCSBackend) locked(owner string, fn func(s *ownerSlot) error) error {
	s := b.slot(owner, false)
	if s == nil {
		return ErrNotRunning
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vm == nil {
		return ErrNotRunning
	}
	return fn(s)
}

// Status implements Backend.
func (b *HCSBackend) Status(_ context.Context, owner string) (VMStatus, error) {
	var st VMStatus
	err := b.locked(owner, func(s *ownerSlot) error {
		var err error
		st, err = b.statusLocked(owner, s)
		return err
	})
	if errors.Is(err, ErrNotRunning) {
		return VMStatus{}, nil
	}
	return st, err
}

// Stop implements Backend. A forced stop terminates the VM; otherwise HCS asks the guest
// to power off, and a later status finds it stopped.
func (b *HCSBackend) Stop(_ context.Context, owner string, force bool) error {
	return b.locked(owner, func(s *ownerSlot) error {
		v := s.vm
		if !force {
			return v.sys.ShutDown()
		}
		err := v.sys.Terminate()
		b.release(v)
		s.vm = nil
		return err
	})
}

// Resize implements Backend. A running VM keeps its size; the new one applies at the
// next start.
func (b *HCSBackend) Resize(context.Context, string, uint32, uint32) error {
	return errors.New("broker: a running VM cannot be resized; stop it first")
}

// Pause implements Backend.
func (b *HCSBackend) Pause(_ context.Context, owner string, resume bool) error {
	return b.locked(owner, func(s *ownerSlot) error {
		if resume {
			return s.vm.sys.Resume()
		}
		return s.vm.sys.Pause()
	})
}
