//go:build windows

package broker

import (
	"context"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/broker/proto"
)

// TestClientEndToEnd drives a broker on a real pipe (fake backend) through the client,
// as the engine's broker provider does, and checks that closing the client ends the
// VM's lease.
func TestClientEndToEnd(t *testing.T) {
	e, _, be := serveEndpoints(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, e.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if v, _, err := c.Version(); err != nil || v != proto.Version {
		t.Fatalf("version %d, %v", v, err)
	}
	if _, err := c.Start(proto.Request{}); !IsCode(err, proto.CodeNoDataLocation) {
		t.Fatalf("start without a data location: %v", err)
	}
	f, err := os.Open(goodDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetDataLocation(windows.Handle(f.Fd())); err != nil {
		t.Fatal(err)
	}
	f.Close()
	r, err := c.Start(proto.Request{CPUs: 2, MemoryMB: 4096, Features: proto.FeatureNet,
		NetAddr: [4]byte{192, 168, 127, 0}, NetPrefix: 24, NetMTU: 9000, NetFlags: proto.NetOffload})
	if err != nil || r.State != proto.StateRunning || r.VMID == ([16]byte{}) {
		t.Fatalf("start: %+v, %v", r, err)
	}
	me := currentSID(t)
	if be.Specs[me].Net == nil || be.Specs[me].Net.MTU != 9000 {
		t.Fatalf("spec %+v", be.Specs[me])
	}
	if r, err := c.Pause(false); err != nil || r.State != proto.StatePaused {
		t.Fatalf("pause: %+v, %v", r, err)
	}
	if r, err := c.Pause(true); err != nil || r.State != proto.StateRunning {
		t.Fatalf("resume: %+v, %v", r, err)
	}
	if r, err := c.Status(); err != nil || r.State != proto.StateRunning {
		t.Fatalf("status: %+v, %v", r, err)
	}
	c.Close()
	if _, err := c.Status(); err == nil {
		t.Fatal("a closed client answered")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		be.mu.Lock()
		stops := be.Stops[me]
		be.mu.Unlock()
		if stops == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("closing the lease did not stop the VM")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTrustedPipeOwner(t *testing.T) {
	const self = "S-1-5-21-1-2-3-1001"
	for owner, want := range map[string]bool{
		"S-1-5-18":                 true,
		self:                       true,
		"S-1-5-80-1-2-3-4-5":       true,
		"S-1-5-80-":                false,
		"S-1-5-21-1-2-3-1002":      false,
		"S-1-5-32-544":             false,
		"S-1-5-19":                 false,
		"S-1-5-21-1-2-3-10010":     false,
		"S-1-5-83-1-1-2-3-4":       false,
		"S-1-5-80":                 false,
		"S-1-5-18-1":               false,
		"S-1-5-21-1-2-3-1001-1001": false,
	} {
		if got := trustedPipeOwner(owner, self); got != want {
			t.Errorf("%s: %v, want %v", owner, got, want)
		}
	}
}
