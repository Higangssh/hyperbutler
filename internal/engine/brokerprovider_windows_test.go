//go:build windows

package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/broker"
	"github.com/Higangssh/hyperbutler/internal/broker/proto"
	"github.com/Higangssh/hyperbutler/internal/vmsecret"
	"github.com/Higangssh/hyperbutler/internal/vmtemplate"
)

func TestStartRequest(t *testing.T) {
	r, err := startRequest(BootSpec{CPUs: 4, MemoryMB: 8192, Share: true, DaemonConfig: true, BlankDisk: true,
		Net: &vmtemplate.Net{Subnet: netip.MustParsePrefix("192.168.127.0/24"), MTU: 9000, Offload: true, HostGateway: true, CRC: vmtemplate.CRCStrict, Port: DefaultNetPort}})
	if err != nil {
		t.Fatal(err)
	}
	want := proto.Request{Op: proto.OpStart, CPUs: 4, MemoryMB: 8192,
		Features: proto.FeatureNet | proto.FeatureShare | proto.FeatureDaemonConfig | proto.FeatureBlankDisk,
		NetAddr:  [4]byte{192, 168, 127, 0}, NetPrefix: 24, NetMTU: 9000, NetFlags: proto.NetOffload | proto.NetHostGateway | proto.NetCRCStrict}
	if r != want {
		t.Fatalf("request %+v, want %+v", r, want)
	}
	// A plain link carries no CRC mode; no network, no network fields.
	r, err = startRequest(BootSpec{CPUs: 1, MemoryMB: 1024, Net: &vmtemplate.Net{Subnet: netip.MustParsePrefix("10.1.0.0/16"), MTU: 1500, CRC: vmtemplate.CRCOff}})
	if err != nil || r.NetFlags != 0 || r.Features != proto.FeatureNet {
		t.Fatalf("plain link: %+v, %v", r, err)
	}
	if r, err := startRequest(BootSpec{CPUs: 1, MemoryMB: 1024}); err != nil || r.Features != 0 || r.NetMTU != 0 {
		t.Fatalf("no network: %+v, %v", r, err)
	}
	for name, bs := range map[string]BootSpec{
		"cpus":   {CPUs: 0, MemoryMB: 1024},
		"memory": {CPUs: 1, MemoryMB: 100},
		"port":   {CPUs: 1, MemoryMB: 1024, Net: &vmtemplate.Net{Subnet: netip.MustParsePrefix("10.1.0.0/16"), MTU: 1500, Port: 4000}},
		"public": {CPUs: 1, MemoryMB: 1024, Net: &vmtemplate.Net{Subnet: netip.MustParsePrefix("8.8.0.0/16"), MTU: 1500}},
	} {
		if _, err := startRequest(bs); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// fakeBrokerClient answers status from a script.
type fakeBrokerClient struct {
	mu      sync.Mutex
	state   proto.State
	err     error
	stops   []bool
	pauses  []bool
	closed  bool
	statusN int
	// noSecret makes Start reply without a VM secret.
	noSecret bool
	// oldProto makes it report a protocol older than proto.Version; dataErr and
	// startErr are what SetDataLocation and Start return.
	oldProto          bool
	dataErr, startErr error
}

func (f *fakeBrokerClient) Version() (uint8, string, error) {
	if f.oldProto {
		return proto.Version - 1, "old", nil
	}
	return proto.Version, "test", nil
}
func (f *fakeBrokerClient) SetDataLocation(windows.Handle) error { return f.dataErr }
func (f *fakeBrokerClient) Start(proto.Request) (proto.Reply, error) {
	if f.startErr != nil {
		return proto.Reply{}, f.startErr
	}
	if f.noSecret {
		return proto.Reply{State: proto.StateRunning, VMID: [16]byte{1}}, nil
	}
	return proto.Reply{State: proto.StateRunning, VMID: [16]byte{1}, Secret: vmsecret.Secret{9}}, nil
}
func (f *fakeBrokerClient) StatusSlot(uint8) (proto.Reply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusN++
	return proto.Reply{State: f.state}, f.err
}
func (f *fakeBrokerClient) PauseSlot(_ uint8, resume bool) (proto.Reply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pauses = append(f.pauses, resume)
	return proto.Reply{State: proto.StatePaused}, nil
}
func (f *fakeBrokerClient) StopSlot(_ uint8, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, force)
	if force && f.state == proto.StateStopped {
		return &broker.ReplyError{Code: proto.CodeNotRunning}
	}
	return nil
}
func (f *fakeBrokerClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeBrokerClient) set(st proto.State, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state, f.err = st, err
}

func waitGone(t *testing.T, l Lease) {
	t.Helper()
	select {
	case <-l.Gone():
	case <-time.After(5 * time.Second):
		t.Fatal("the lease never reported the VM gone")
	}
}

func bootFake(t *testing.T, c *fakeBrokerClient) Lease {
	return bootFakeEvery(t, c, 5*time.Millisecond)
}

func bootFakeEvery(t *testing.T, c *fakeBrokerClient, poll time.Duration) Lease {
	t.Helper()
	saved := dialBroker
	dialBroker = func(context.Context, string) (brokerClient, error) { return c, nil }
	t.Cleanup(func() { dialBroker = saved })
	p := &brokerProvider{pipe: `\\.\pipe\x`, dataDir: t.TempDir(), log: slog.New(slog.DiscardHandler), poll: poll}
	l, info, err := p.Boot(context.Background(), BootSpec{CPUs: 1, MemoryMB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if info.VMID != "" || info.RuntimeID.ToArray() != [16]byte{1} || info.Secret != (vmsecret.Secret{9}) {
		t.Fatalf("runtime info %+v", info)
	}
	return l
}

// Once a power-off is expected, the lease notices it within brokerShutdownPoll, not at
// the next regular poll.
func TestBrokerLeaseExpectGone(t *testing.T) {
	c := &fakeBrokerClient{state: proto.StateRunning}
	l := bootFakeEvery(t, c, time.Hour)
	defer l.Close()
	c.set(proto.StateStopped, nil)
	l.(*brokerLease).ExpectGone()
	select {
	case <-l.Gone():
	case <-time.After(5 * time.Second):
		t.Fatal("the power-off was not noticed")
	}
}

// The broker lease notices a VM that stopped on its own, and a lost connection.
func TestBrokerLeaseGone(t *testing.T) {
	c := &fakeBrokerClient{state: proto.StateRunning}
	l := bootFake(t, c)
	if s, err := l.State(); err != nil || s != "Running" {
		t.Fatalf("state %q, %v", s, err)
	}
	if err := l.Pause(); err != nil {
		t.Fatal(err)
	}
	if err := l.Resume(); err != nil {
		t.Fatal(err)
	}
	c.set(proto.StateStopped, nil)
	waitGone(t, l)
	if l.(*brokerLease).LostBroker() {
		t.Error("a stopped VM was taken for a lost broker")
	}
	if err := l.Terminate(); err != nil {
		t.Fatalf("terminate a stopped VM: %v", err)
	}
	if err := l.Close(); err != nil || !c.closed {
		t.Fatalf("close: %v, closed %v", err, c.closed)
	}
	if fmt.Sprint(c.pauses) != "[false true]" || fmt.Sprint(c.stops) != "[true]" {
		t.Fatalf("pauses %v, stops %v", c.pauses, c.stops)
	}

	c = &fakeBrokerClient{state: proto.StateRunning}
	l = bootFake(t, c)
	c.set(proto.StateRunning, fmt.Errorf("%w: pipe broken", broker.ErrLost))
	waitGone(t, l)
	if !l.(*brokerLease).LostBroker() {
		t.Error("a lost connection was not reported as a lost broker")
	}
	l.Close()

	// A broker error reply is not a lost lease.
	c = &fakeBrokerClient{state: proto.StateRunning}
	l = bootFake(t, c)
	c.set(proto.StateRunning, &broker.ReplyError{Code: proto.CodeInternal})
	time.Sleep(50 * time.Millisecond)
	select {
	case <-l.Gone():
		t.Fatal("an error reply ended the lease")
	default:
	}
	l.Close()
}

// A status poll that got no reply in time, as when the host slept while it waited
// (ADR 0004, the M6 review's finding 1), keeps the lease: only the connection's end
// ends it, and a VM that stopped is still noticed after the timeouts.
func TestBrokerLeaseSurvivesNoReply(t *testing.T) {
	c := &fakeBrokerClient{state: proto.StateRunning}
	l := bootFake(t, c)
	defer l.Close()
	c.set(proto.StateRunning, fmt.Errorf("%w to status", broker.ErrNoReply))
	waitPolls(t, c, 5)
	select {
	case <-l.Gone():
		t.Fatal("a late status reply ended the lease")
	default:
	}
	// While the broker does not answer, a shutdown or terminate sends nothing (each
	// would wait a full timeout for the late reply first); the caller closes the lease.
	if err := l.ShutDown(); !errors.Is(err, broker.ErrNoReply) {
		t.Fatalf("shutdown while the broker is silent: %v", err)
	}
	if err := l.Terminate(); !errors.Is(err, broker.ErrNoReply) {
		t.Fatalf("terminate while the broker is silent: %v", err)
	}
	c.mu.Lock()
	sent := len(c.stops)
	c.mu.Unlock()
	if sent != 0 {
		t.Fatalf("%d stops sent to a silent broker", sent)
	}
	// Once it answers again they are sent.
	c.set(proto.StateRunning, nil)
	waitPolls(t, c, 2)
	if err := l.ShutDown(); err != nil {
		t.Fatalf("shutdown once the broker answers: %v", err)
	}
	c.set(proto.StateStopped, nil)
	waitGone(t, l)
	if l.(*brokerLease).LostBroker() {
		t.Error("a VM that stopped after late replies was taken for a lost broker")
	}

	// Any other error that is not the connection's end keeps it too.
	c2 := &fakeBrokerClient{state: proto.StateRunning}
	l2 := bootFake(t, c2)
	defer l2.Close()
	c2.set(proto.StateRunning, errors.New("broker: some request error"))
	waitPolls(t, c2, 5)
	select {
	case <-l2.Gone():
		t.Fatal("an error that is not ErrLost ended the lease")
	default:
	}
}

// waitPolls waits until the lease has polled c n more times.
func waitPolls(t *testing.T, c *fakeBrokerClient, n int) {
	t.Helper()
	c.mu.Lock()
	want := c.statusN + n
	c.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		got := c.statusN
		c.mu.Unlock()
		if got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d polls, want %d", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// A broker that starts a VM but gives no secret is refused: the guest would serve
// nothing.
func TestBrokerProviderNeedsSecret(t *testing.T) {
	c := &fakeBrokerClient{state: proto.StateRunning, noSecret: true}
	saved := dialBroker
	dialBroker = func(context.Context, string) (brokerClient, error) { return c, nil }
	t.Cleanup(func() { dialBroker = saved })
	p := &brokerProvider{pipe: `\\.\pipe\x`, dataDir: t.TempDir(), log: slog.New(slog.DiscardHandler)}
	if _, _, err := p.Boot(context.Background(), BootSpec{CPUs: 1, MemoryMB: 1024}); err == nil {
		t.Fatal("booted without a VM secret")
	}
	if !c.closed {
		t.Fatal("the lease was not closed")
	}
}

// TestBrokerProviderCodes: each way a broker boot fails has the code the CLI and the
// window explain, and a failed boot closes its connection.
func TestBrokerProviderCodes(t *testing.T) {
	refused := &broker.ReplyError{Code: proto.CodeDataLocationRejected, Message: "data location rejected"}
	for _, tc := range []struct {
		name    string
		dialErr error
		c       *fakeBrokerClient
		want    string
	}{
		{"no pipe", fmt.Errorf("broker: connect: %w", windows.ERROR_FILE_NOT_FOUND), nil, CodeBrokerUnavailable},
		{"squatted shared pipe", fmt.Errorf("broker: x: the pipe belongs to S-1-5-21-1-2-3-1001, not to the broker (%w)", broker.ErrUntrustedPipe), nil, CodeBrokerSquatted},
		{"squatted endpoint", fmt.Errorf("broker: open this user's endpoint: %w", &broker.ReplyError{Code: proto.CodeEndpointSquatted}), nil, CodeBrokerSquatted},
		{"other protocol", &broker.ReplyError{Code: proto.CodeUnsupportedVersion}, nil, CodeBrokerOld},
		{"caller refused", fmt.Errorf("broker: open this user's endpoint: %w", &broker.ReplyError{Code: proto.CodeAccessDenied}), nil, CodeBrokerRefused},
		{"old protocol", nil, &fakeBrokerClient{oldProto: true}, CodeBrokerOld},
		{"data directory refused", nil, &fakeBrokerClient{dataErr: refused}, CodeBrokerRefused},
		{"image refused", nil, &fakeBrokerClient{startErr: &broker.ReplyError{Code: proto.CodeImageRejected}}, CodeBrokerRefused},
		{"connection lost at start", nil, &fakeBrokerClient{startErr: errors.New("broker: connection lost: EOF")}, CodeBrokerUnavailable},
	} {
		saved := dialBroker
		dialBroker = func(context.Context, string) (brokerClient, error) {
			if tc.dialErr != nil {
				return nil, tc.dialErr
			}
			return tc.c, nil
		}
		p := &brokerProvider{pipe: `\\.\pipe\x`, dataDir: t.TempDir(), log: slog.New(slog.DiscardHandler)}
		_, _, err := p.Boot(context.Background(), BootSpec{CPUs: 1, MemoryMB: 1024})
		dialBroker = saved
		if got := ErrorCode(err); got != tc.want {
			t.Errorf("%s: code %q (%v), want %q", tc.name, got, err, tc.want)
		}
		if Describe(tc.want, "") == "" {
			t.Errorf("%s: %s has no explanation", tc.name, tc.want)
		}
		if tc.c != nil && !tc.c.closed {
			t.Errorf("%s: the connection was not closed", tc.name)
		}
	}
}

// TestBrokerProviderPipe boots through a broker served on real pipes (with the fake
// backend), the shared endpoint and then this user's own: the data directory is passed
// by handle and accepted, the start carries the spec and returns the VM secret, and
// closing the lease stops the VM.
func TestBrokerProviderPipe(t *testing.T) {
	sid, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	be := broker.NewFakeBackend()
	srv, err := broker.New(broker.Config{Backend: be, AllowedSIDs: []string{sid}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	pipe := fmt.Sprintf(`\\.\pipe\hyperbutler_engine_test_%d_broker`, windows.GetCurrentProcessId())
	eps, err := broker.ListenEndpoints(srv, broker.EndpointsConfig{Name: pipe, Instances: 2, UserInstances: 2})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- eps.Serve() }()
	defer func() { eps.Close(); <-done }()

	data := filepath.Join(t.TempDir(), "data")
	if err := MkdirPrivate(data); err != nil {
		t.Fatal(err)
	}
	p := &brokerProvider{pipe: pipe, dataDir: data, log: slog.New(slog.DiscardHandler)}
	l, info, err := p.Boot(context.Background(), BootSpec{CPUs: 2, MemoryMB: 2048, Share: true,
		Net: &vmtemplate.Net{Subnet: netip.MustParsePrefix("192.168.127.0/24"), MTU: 9000, Offload: true}})
	if err != nil {
		t.Fatal(err)
	}
	if info.RuntimeID.ToArray() == ([16]byte{}) || info.Secret.IsZero() {
		t.Fatal("no runtime id or no secret")
	}
	if s, err := l.State(); err != nil || s != "Running" {
		t.Fatalf("state %q, %v", s, err)
	}
	if n := be.StartCount(sid); n != 1 {
		t.Fatalf("starts %d", n)
	}
	l.Close()
	deadline := time.Now().Add(5 * time.Second)
	for be.StopCount(sid) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("closing the lease did not stop the VM")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, _, err := (&brokerProvider{pipe: pipe, log: slog.New(slog.DiscardHandler)}).Boot(context.Background(), BootSpec{CPUs: 1, MemoryMB: 1024}); !errors.Is(err, errBrokerNeedsDisk) {
		t.Fatalf("boot without a data disk: %v", err)
	}
}
