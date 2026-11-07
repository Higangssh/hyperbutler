//go:build windows

package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/broker"
	"github.com/Higangssh/hyperbutler/internal/imageset"
	"github.com/Higangssh/hyperbutler/internal/netstack"
	"github.com/Higangssh/hyperbutler/internal/transport"
	"github.com/Higangssh/hyperbutler/internal/vm"
)

// SessionConfig describes one agent sandbox session's VM (ADR 0009). The session manager
// (`hyperbutler sandbox`) holds these; the Docker VM and its engine are not involved.
type SessionConfig struct {
	Layout Layout
	// Slot is the broker's slot, 1..proto.MaxSlot; it names the session's disk.
	Slot uint8
	// BrokerPipe boots the VM through the HCS access broker. Empty calls HCS from this
	// process (developer mode), from the image at Manifest.
	BrokerPipe string
	Manifest   string
	CPUs       int
	MemoryMB   int
	// DiskBlank says the session's disk was never written: the guest may format it.
	DiskBlank bool
	// Egress decides what the session may reach; nil is the `open` mode.
	Egress netstack.Egress
	// ConsoleLog receives the guest's serial console in developer mode.
	ConsoleLog io.Writer
	Logger     *slog.Logger
}

// SessionVM is a running session's VM and network.
type SessionVM struct {
	Lease
	info RuntimeInfo
	net  *guestNet
	stop context.CancelFunc
	img  *imageset.Set
}

// SessionDiskPath is the session disk of slot, the name the broker opens below the data
// location.
func SessionDiskPath(l Layout, slot uint8) string {
	return filepath.Join(l.Data, broker.SessionDiskName(slot))
}

// SessionVMPrefix starts a session's compute system name in developer mode. It is not
// VMPrefix: the Docker engine's clean-up of its own leftovers must not reach sessions.
func (l Layout) SessionVMPrefix() string { return "hyperbutler-sbx-" + l.Tag() + "-" }

// CreateSessionDisk makes the sparse disk of slot, which must not exist, and returns the
// host allocation of a disk nothing wrote to yet: SessionDiskBlank compares against it.
func CreateSessionDisk(l Layout, slot uint8, size uint64) (metadataBytes uint64, err error) {
	if err := MkdirPrivate(l.Data); err != nil {
		return 0, err
	}
	path := SessionDiskPath(l, slot)
	if _, err := os.Lstat(path); err == nil {
		return 0, fmt.Errorf("engine: %s already exists", path)
	}
	tmp := path + ".new"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	if err := vm.CreateSparseVHDX(tmp, size); err != nil {
		return 0, codeErr(CodeDataDisk, err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err := ownByUser(tmp); err != nil {
		return 0, codeErr(CodeDataDisk, err)
	}
	u, err := vm.Usage(tmp)
	if err != nil {
		return 0, err
	}
	if !u.Sparse {
		return 0, codeErr(CodeDataDisk, fmt.Errorf("%s did not become a sparse file; the volume must be NTFS", tmp))
	}
	return u.Allocated, os.Rename(tmp, path)
}

// SessionDiskBlank reports whether the guest never wrote to the session's disk.
func SessionDiskBlank(l Layout, slot uint8, metadataBytes uint64) (bool, error) {
	u, err := vm.Usage(SessionDiskPath(l, slot)) // refuses reparse points
	if err != nil {
		return false, err
	}
	return u.Allocated <= metadataBytes, nil
}

// BootSession boots a session's VM and starts its network. ctx bounds the boot; the VM
// lasts until Close, which is called after the guest has shut down.
func BootSession(ctx context.Context, c SessionConfig) (_ *SessionVM, err error) {
	log := c.Logger
	if log == nil {
		log = slog.Default()
	}
	step := func(format string, args ...any) { log.Debug(fmt.Sprintf(format, args...)) }
	g, netSpec, err := setupNet(NetConfig{Egress: c.Egress, NoDiag: true}, log)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			g.close()
		}
	}()
	bs := BootSpec{CPUs: c.CPUs, MemoryMB: c.MemoryMB, Net: netSpec, BlankDisk: c.DiskBlank, Slot: c.Slot}
	var provider VMProvider
	var img *imageset.Set
	if c.BrokerPipe != "" {
		provider = &brokerProvider{pipe: c.BrokerPipe, dataDir: c.Layout.Data, log: log}
	} else {
		pin, _ := imageset.SandboxPin() // none in a development build
		if img, err = imageset.Open(c.Manifest, pin); err != nil {
			return nil, err
		}
		defer func() {
			if err != nil {
				_ = img.Close()
			}
		}()
		tu, terr := windows.GetCurrentProcessToken().GetTokenUser()
		if terr != nil {
			return nil, terr
		}
		provider = &hcsProvider{
			cfg: &Config{
				VMID:    fmt.Sprintf("%s%d", c.Layout.SessionVMPrefix(), c.Slot),
				Backend: HCSBackend{}, Logger: log, ConsoleLog: c.ConsoleLog,
			},
			sid: tu.User.Sid.String(), img: img, kernel: img.Kernel, initrd: img.Initrd,
			disk: &dataDisk{path: SessionDiskPath(c.Layout, c.Slot), blank: c.DiskBlank}, step: step,
		}
	}
	lease, info, err := provider.Boot(ctx, bs)
	if err != nil {
		return nil, err
	}
	// The network lasts as long as the VM, not as long as the caller's boot context.
	rctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-lease.Gone():
			cancel()
		case <-rctx.Done():
		}
	}()
	if err := g.start(rctx, info.RuntimeID, info.Secret, step); err != nil {
		cancel()
		_ = lease.Terminate()
		_ = lease.Close()
		return nil, err
	}
	return &SessionVM{Lease: lease, info: info, net: g, stop: cancel, img: img}, nil
}

// Dialer dials a port of the guest. A connect to a port nobody listens on yet stays
// pending for about 30 s, so each attempt gets one second until wait ends.
func (s *SessionVM) Dialer(port uint32, wait time.Duration) transport.Dialer {
	d := &transport.HvsockDialer{Target: transport.HvsockTarget{VMID: s.info.RuntimeID, ServiceID: transport.VsockServiceID(port), Secret: s.info.Secret}}
	return transport.DialerFunc(func(ctx context.Context) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, wait)
		defer cancel()
		for {
			actx, acancel := context.WithTimeout(ctx, time.Second)
			c, err := d.DialContext(actx)
			acancel()
			if err == nil || ctx.Err() != nil {
				return c, err
			}
			select {
			case <-ctx.Done():
			case <-time.After(50 * time.Millisecond):
			}
		}
	})
}

// Close ends the network and releases the VM, which stops if it still runs.
func (s *SessionVM) Close() error {
	s.stop()
	err := s.Lease.Close()
	s.net.close()
	if s.img != nil {
		_ = s.img.Close()
	}
	return err
}
