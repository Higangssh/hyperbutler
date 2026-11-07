//go:build windows

package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/broker"
	"github.com/Higangssh/hyperbutler/internal/broker/proto"
	"github.com/Higangssh/hyperbutler/internal/guid"
	"github.com/Higangssh/hyperbutler/internal/vmtemplate"
)

// brokerPoll is how often the broker provider asks for the VM's status: it notices a
// VM that stopped on its own. It is not what keeps the lease: the broker sets no idle
// limit on the lease's connection, and the client neither ends it on a late reply
// (broker.ErrNoReply) nor counts a host's sleep against a request's timeout, so a
// sleep during a poll ends nothing. Only the connection's end (broker.ErrLost) does.
const brokerPoll = 10 * time.Second

// brokerShutdownPoll replaces brokerPoll once a shutdown was asked for, so the engine
// sees the power-off when it happens, not at the next 10 s poll (it took a stop from
// 1.3 s in-process to 8.7 s through the broker).
const brokerShutdownPoll = 100 * time.Millisecond

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
	Version() (uint8, string, error)
	SetDataLocation(h windows.Handle) error
	Start(req proto.Request) (proto.Reply, error)
	StatusSlot(slot uint8) (proto.Reply, error)
	PauseSlot(slot uint8, resume bool) (proto.Reply, error)
	StopSlot(slot uint8, force bool) error
	Close() error
}

// dialBroker is broker.Dial (replaced in tests).
var dialBroker = func(ctx context.Context, pipe string) (brokerClient, error) { return broker.Dial(ctx, pipe) }

// startRequest renders bs as a start request.
func startRequest(bs BootSpec) (proto.Request, error) {
	if bs.CPUs < 1 || bs.CPUs > proto.MaxCPUs || bs.MemoryMB < proto.MinMemoryMB || bs.MemoryMB > proto.MaxMemoryMB {
		return proto.Request{}, fmt.Errorf("engine: %d CPUs and %d MiB are outside what the broker accepts", bs.CPUs, bs.MemoryMB)
	}
	req := proto.Request{Op: proto.OpStart, Slot: bs.Slot, CPUs: uint32(bs.CPUs), MemoryMB: uint32(bs.MemoryMB)}
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

// brokerError gives an error of the broker's client its code: another program's pipe
// (rule 5) is CodeBrokerSquatted, an older protocol CodeBrokerOld, any other error
// reply CodeBrokerRefused, and the rest (no pipe, a timeout, a lost connection)
// CodeBrokerUnavailable.
func brokerError(err error) error {
	var re *broker.ReplyError
	switch {
	case errors.Is(err, broker.ErrUntrustedPipe), broker.IsCode(err, proto.CodeEndpointSquatted):
		return codeErr(CodeBrokerSquatted, err)
	case broker.IsCode(err, proto.CodeUnsupportedVersion):
		return codeErr(CodeBrokerOld, err)
	case errors.As(err, &re):
		return codeErr(CodeBrokerRefused, err)
	}
	return codeErr(CodeBrokerUnavailable, err)
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
		return nil, RuntimeInfo{}, brokerError(fmt.Errorf("engine: cannot reach the HCS access broker on %s: %w", p.pipe, err))
	}
	defer func() {
		if err != nil {
			_ = c.Close() // ends the lease: the broker terminates a VM it started
		}
	}()
	pv, bv, err := c.Version()
	if err != nil {
		return nil, RuntimeInfo{}, brokerError(fmt.Errorf("engine: the HCS access broker on %s does not tell its version: %w", p.pipe, err))
	}
	if pv < proto.Version {
		return nil, RuntimeInfo{}, codeErr(CodeBrokerOld, fmt.Errorf("engine: the HCS access broker is version %s, protocol %d; this hyperbutler needs protocol %d or later", bv, pv, proto.Version))
	}
	p.log.Info("booting through the HCS access broker", "pipe", p.pipe, "brokerVersion", bv, "protocol", pv)
	h, err := openDirHandle(p.dataDir)
	if err != nil {
		return nil, RuntimeInfo{}, fmt.Errorf("engine: open the data directory %s: %w", p.dataDir, err)
	}
	err = c.SetDataLocation(h)
	_ = windows.CloseHandle(h)
	if err != nil {
		if broker.IsCode(err, proto.CodeDataLocationRejected) {
			// The service gives a reason but no fix; the same check run here gives both.
			if de, ok := IsDataDirError(CheckBrokerDataDir(p.dataDir)); ok {
				return nil, RuntimeInfo{}, codeErr(CodeBrokerRefused, fmt.Errorf("engine: %w", de))
			}
		}
		return nil, RuntimeInfo{}, brokerError(fmt.Errorf("engine: the broker refused the data directory %s: %w", p.dataDir, err))
	}
	r, err := c.Start(req)
	if err != nil {
		if broker.IsCode(err, proto.CodeDiskRejected) {
			if de, ok := IsDataDirError(CheckBrokerDataDir(p.dataDir)); ok {
				return nil, RuntimeInfo{}, codeErr(CodeBrokerRefused, fmt.Errorf("engine: %w", de))
			}
		}
		return nil, RuntimeInfo{}, brokerError(fmt.Errorf("engine: the broker did not start the VM: %w", err))
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
	l := &brokerLease{c: c, slot: bs.Slot, log: p.log, gone: make(chan struct{}), soon: make(chan struct{}), stop: make(chan struct{}), done: make(chan struct{})}
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
	c    brokerClient
	slot uint8
	log  *slog.Logger

	gone     chan struct{}
	goneOnce sync.Once
	// lost is set before gone closes when the connection, not the VM, ended.
	lost atomic.Bool
	// silent is set while the broker's last answer was none in time (broker.ErrNoReply).
	silent atomic.Bool
	// soon asks watch to poll every brokerShutdownPoll from now on.
	soon     chan struct{}
	soonOnce sync.Once
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

func (l *brokerLease) markGone() { l.goneOnce.Do(func() { close(l.gone) }) }

func (l *brokerLease) Gone() <-chan struct{} { return l.gone }

// ExpectGone tells the lease a power-off is under way: it polls often from now on.
func (l *brokerLease) ExpectGone() { l.soonOnce.Do(func() { close(l.soon) }) }

// LostBroker reports whether the lease ended because the connection to the broker did.
func (l *brokerLease) LostBroker() bool { return l.lost.Load() }

// note records whether the broker answered a request: err is its result.
func (l *brokerLease) note(err error) error {
	switch {
	case errors.Is(err, broker.ErrNoReply):
		l.silent.Store(true)
	case err == nil, isReplyError(err):
		l.silent.Store(false)
	}
	return err
}

func isReplyError(err error) bool {
	var re *broker.ReplyError
	return errors.As(err, &re)
}

// errBrokerSilent is what ShutDown and Terminate return, sending nothing, while the
// broker does not answer: the request would wait for the late reply first, so each would
// take a full timeout and reach the broker no sooner. The caller closes the lease, which
// ends the VM as Terminate would.
var errBrokerSilent = fmt.Errorf("engine: the HCS access broker does not answer; closing the lease ends the VM: %w", broker.ErrNoReply)

// watch polls the status until the VM is stopped or the connection is lost.
func (l *brokerLease) watch(every time.Duration) {
	defer close(l.done)
	t := time.NewTicker(every)
	defer t.Stop()
	soon := l.soon
	for {
		select {
		case <-l.stop:
			return
		case <-soon:
			soon = nil
			if brokerShutdownPoll < every {
				t.Reset(brokerShutdownPoll)
			}
			continue
		case <-t.C:
		}
		r, err := l.c.StatusSlot(l.slot)
		err = l.note(err)
		switch {
		case errors.Is(err, broker.ErrLost):
			// The connection is gone, and with it the lease: the broker terminates the VM.
			l.log.Warn("lost the connection to the broker", "err", err)
			l.lost.Store(true)
			l.markGone()
			return
		case err != nil:
			// An error reply, or no reply in time (broker.ErrNoReply): the connection
			// and the lease stay, and the next poll reads the late reply first.
			l.log.Warn("broker status", "err", err)
		case r.State == proto.StateStopped:
			l.markGone()
			return
		}
	}
}

func (l *brokerLease) State() (string, error) {
	r, err := l.c.StatusSlot(l.slot)
	if l.note(err) != nil {
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
	_, err := l.c.PauseSlot(l.slot, false)
	return l.note(err)
}

func (l *brokerLease) Resume() error {
	_, err := l.c.PauseSlot(l.slot, true)
	return l.note(err)
}

func (l *brokerLease) ShutDown() error {
	if l.silent.Load() {
		return errBrokerSilent
	}
	return l.note(l.c.StopSlot(l.slot, false))
}

func (l *brokerLease) Terminate() error {
	if l.silent.Load() {
		return errBrokerSilent
	}
	err := l.note(l.c.StopSlot(l.slot, true))
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
