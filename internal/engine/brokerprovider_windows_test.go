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
}

func (f *fakeBrokerClient) SetDataLocation(windows.Handle) error { return nil }
func (f *fakeBrokerClient) Start(proto.Request) (proto.Reply, error) {
	return proto.Reply{State: proto.StateRunning, VMID: [16]byte{1}}, nil
}
func (f *fakeBrokerClient) Status() (proto.Reply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusN++
	return proto.Reply{State: f.state}, f.err
}
func (f *fakeBrokerClient) Pause(resume bool) (proto.Reply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pauses = append(f.pauses, resume)
	return proto.Reply{State: proto.StatePaused}, nil
}
func (f *fakeBrokerClient) Stop(force bool) error {
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
	t.Helper()
	saved := dialBroker
	dialBroker = func(context.Context, string) (brokerClient, error) { return c, nil }
	t.Cleanup(func() { dialBroker = saved })
	p := &brokerProvider{pipe: `\\.\pipe\x`, dataDir: t.TempDir(), log: slog.New(slog.DiscardHandler), poll: 5 * time.Millisecond}
	l, info, err := p.Boot(context.Background(), BootSpec{CPUs: 1, MemoryMB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if info.VMID != "" || info.RuntimeID.ToArray() != [16]byte{1} {
		t.Fatalf("runtime info %+v", info)
	}
	return l
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
	c.set(proto.StateRunning, errors.New("pipe broken"))
	waitGone(t, l)
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

// TestBrokerProviderPipe boots through a broker served on a real pipe (with the fake
// backend): the data directory is passed by handle and accepted, the start carries the
// spec, and closing the lease stops the VM.
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
	ln, err := broker.ListenPipe(broker.PipeConfig{Name: pipe, ClientSID: sid, Instances: 2})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	defer func() { ln.Close(); <-done }()

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
	if info.RuntimeID.ToArray() == ([16]byte{}) {
		t.Fatal("no runtime id")
	}
	if s, err := l.State(); err != nil || s != "Running" {
		t.Fatalf("state %q, %v", s, err)
	}
	if be.Starts[sid] != 1 {
		t.Fatalf("starts %v", be.Starts)
	}
	l.Close()
	deadline := time.Now().Add(5 * time.Second)
	for be.Stops[sid] != 1 {
		if time.Now().After(deadline) {
			t.Fatal("closing the lease did not stop the VM")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, _, err := (&brokerProvider{pipe: pipe, log: slog.New(slog.DiscardHandler)}).Boot(context.Background(), BootSpec{CPUs: 1, MemoryMB: 1024}); !errors.Is(err, errBrokerNeedsDisk) {
		t.Fatalf("boot without a data disk: %v", err)
	}
}
