//go:build windows

package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/broker"
	"github.com/Higangssh/hyperbutler/internal/broker/proto"
	"github.com/Higangssh/hyperbutler/internal/vmtemplate"
)

// brokerPoll is how often the broker provider asks for the VM's status: it notices a
// VM that stopped on its own, and keeps the connection (the lease) inside the broker's
// idle timeout.
const brokerPoll = 10 * time.Second

// brokerProvider boots the VM through the HCS access broker on pipe (ADR 0004). The
// broker opens its own guest image and the data disk below dataDir by handle; this
// process sends only sizes, feature bits and the typed network, and dials the VM's
// hvsock services by the runtime id it gets back, as in developer mode. No serial
// console pipe is opened (Implementation decision 7): the VM's exit is learned from the
// lease, the status and the guest control channel.
type brokerProvider struct {
	pipe    string
	dataDir string
	log     *slog.Logger
	poll    time.Duration
}

// brokerClient is what the provider needs of broker.Client (a fake in tests).
type brokerClient interface {
	SetDataLocation(h windows.Handle) error
	Start(req proto.Request) (proto.Reply, error)
	Status() (proto.Reply, error)
	Pause(resume bool) (proto.Reply, error)
	Stop(force bool) error
	Close() error
}

// dialBroker is broker.Dial (replaced in tests).
var dialBroker = func(ctx context.Context, pipe string) (brokerClient, error) { return broker.Dial(ctx, pipe) }

// startRequest renders bs as a start request.
func startRequest(bs BootSpec) (proto.Request, error) {
	if bs.CPUs < 1 || bs.CPUs > proto.MaxCPUs || bs.MemoryMB < proto.MinMemoryMB || bs.MemoryMB > proto.MaxMemoryMB {
		return proto.Request{}, fmt.Errorf("engine: %d CPUs and %d MiB are outside what the broker accepts", bs.CPUs, bs.MemoryMB)
	}
	req := proto.Request{Op: proto.OpStart, CPUs: uint32(bs.CPUs), MemoryMB: uint32(bs.MemoryMB)}
	if bs.Share {
		req.Features |= proto.FeatureShare
	}
	if bs.DaemonConfig {
		req.Features |= proto.FeatureDaemonConfig
	}
	if bs.BlankDisk {
		req.Features |= proto.FeatureBlankDisk
	}
	if n := bs.Net; n != nil {
		if n.Port != 0 && n.Port != vmtemplate.NetPort {
			return proto.Request{}, fmt.Errorf("engine: the broker runs the network link on port %d only", vmtemplate.NetPort)
		}
		if err := n.Validate(); err != nil {
			return proto.Request{}, err
		}
		req.Features |= proto.FeatureNet
		req.NetAddr = n.Subnet.Masked().Addr().As4()
		req.NetPrefix = uint8(n.Subnet.Bits()) //nolint:gosec // G115: Validate keeps the prefix in 8..29
		req.NetMTU = uint16(n.MTU)             //nolint:gosec // G115: Validate keeps the MTU in 576..65520
		if n.Offload {
			req.NetFlags |= proto.NetOffload
			switch n.CRC {
			case vmtemplate.CRCOff:
				req.NetFlags |= proto.NetCRCOff
			case vmtemplate.CRCStrict:
				req.NetFlags |= proto.NetCRCStrict
			}
		}
		if n.HostGateway {
			req.NetFlags |= proto.NetHostGateway
		}
	}
	return req, req.Validate()
}

func (p *brokerProvider) Boot(ctx context.Context, bs BootSpec) (_ Lease, _ RuntimeInfo, err error) {
	if p.dataDir == "" {
		return nil, RuntimeInfo{}, errBrokerNeedsDisk
	}
	req, err := startRequest(bs)
	if err != nil {
		return nil, RuntimeInfo{}, err
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	c, err := dialBroker(dctx, p.pipe)
	cancel()
	if err != nil {
		return nil, RuntimeInfo{}, err
	}
	defer func() {
		if err != nil {
			_ = c.Close() // ends the lease: the broker terminates a VM it started
		}
	}()
	h, err := openDirHandle(p.dataDir)
	if err != nil {
		return nil, RuntimeInfo{}, fmt.Errorf("engine: open the data directory %s: %w", p.dataDir, err)
	}
	err = c.SetDataLocation(h)
	_ = windows.CloseHandle(h)
	if err != nil {
		return nil, RuntimeInfo{}, fmt.Errorf("engine: the broker refused the data directory %s: %w", p.dataDir, err)
	}
	r, err := c.Start(req)
	if err != nil {
		return nil, RuntimeInfo{}, fmt.Errorf("engine: the broker did not start the VM: %w", err)
	}
	if r.State == proto.StateStopped || r.VMID == ([16]byte{}) {
		return nil, RuntimeInfo{}, errors.New("engine: the broker started no VM")
	}
	if r.Secret.IsZero() {
		// The guest serves nothing without it (ADR 0004 rule 6).
		return nil, RuntimeInfo{}, errors.New("engine: the broker gave no VM secret")
	}
	poll := p.poll
	if poll <= 0 {
		poll = brokerPoll
	}
	l := &brokerLease{c: c, log: p.log, gone: make(chan struct{}), stop: make(chan struct{}), done: make(chan struct{})}
	go l.watch(poll)
	return l, RuntimeInfo{RuntimeID: guid.FromArray(r.VMID), Secret: r.Secret}, nil
}

// openDirHandle opens dir as a directory handle to pass to the broker.
func openDirHandle(dir string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	const fileListDirectory, fileTraverse, fileReadAttributes = 0x0001, 0x0020, 0x0080
	return windows.CreateFile(p, fileListDirectory|fileTraverse|fileReadAttributes|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
}

// brokerLease is a VM the broker runs for this connection.
type brokerLease struct {
	c   brokerClient
	log *slog.Logger

	gone     chan struct{}
	goneOnce sync.Once
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

func (l *brokerLease) markGone() { l.goneOnce.Do(func() { close(l.gone) }) }

func (l *brokerLease) Gone() <-chan struct{} { return l.gone }

// watch polls the status until the VM is stopped or the connection is lost.
func (l *brokerLease) watch(every time.Duration) {
	defer close(l.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
		}
		r, err := l.c.Status()
		switch {
		case err != nil && !isReplyError(err):
			// The connection is gone, and with it the lease: the broker terminates the VM.
			l.log.Warn("lost the connection to the broker", "err", err)
			l.markGone()
			return
		case err != nil:
			l.log.Warn("broker status", "err", err)
		case r.State == proto.StateStopped:
			l.markGone()
			return
		}
	}
}

func isReplyError(err error) bool {
	var re *broker.ReplyError
	return errors.As(err, &re)
}

func (l *brokerLease) State() (string, error) {
	r, err := l.c.Status()
	if err != nil {
		return "", err
	}
	switch r.State {
	case proto.StateRunning:
		return "Running", nil
	case proto.StatePaused:
		return "Paused", nil
	}
	l.markGone()
	return "Stopped", nil
}

func (l *brokerLease) Pause() error {
	_, err := l.c.Pause(false)
	return err
}

func (l *brokerLease) Resume() error {
	_, err := l.c.Pause(true)
	return err
}

func (l *brokerLease) ShutDown() error { return l.c.Stop(false) }

func (l *brokerLease) Terminate() error {
	err := l.c.Stop(true)
	if err == nil || broker.IsCode(err, proto.CodeNotRunning) {
		l.markGone()
		return nil
	}
	return err
}

// Close ends the lease: the broker terminates the VM if it still runs.
func (l *brokerLease) Close() error {
	l.stopOnce.Do(func() { close(l.stop) })
	err := l.c.Close() // also ends a status request in progress
	<-l.done
	return err
}
