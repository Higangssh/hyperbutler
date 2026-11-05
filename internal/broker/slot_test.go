//go:build windows

package broker

import (
	"testing"

	"github.com/Higangssh/hyperbutler/internal/broker/proto"
)

// TestSlots: each slot is its own VM with its own lease and size (ADR 0009); stopping or
// pausing one leaves the others alone.
func TestSlots(t *testing.T) {
	s, be := newTestServer(t, Config{validateDataDir: acceptAll})
	p := &localPeer{sid: sidA}
	c := openSession(t, s, p)
	wantOK(t, c.do(proto.Request{Op: proto.OpSetDataLocation, Handle: tempDirHandle(t)}), proto.OpSetDataLocation)

	docker := c.do(proto.Request{Op: proto.OpStart})
	wantOK(t, docker, proto.OpStart)
	one := c.do(proto.Request{Op: proto.OpStart, Slot: 1})
	wantOK(t, one, proto.OpStart)
	if one.VMID == docker.VMID || one.Secret == docker.Secret {
		t.Fatal("slot 1 got the Docker VM's id or secret")
	}
	// A session starts with the session defaults, not the Docker VM's.
	if one.CPUs != 2 || one.MemoryMB != 1024 || docker.MemoryMB != 2048 {
		t.Fatalf("sizes: docker %d MB, session %d CPUs %d MB", docker.MemoryMB, one.CPUs, one.MemoryMB)
	}
	if spec := be.Specs[sidA]; spec.Slot != 1 {
		t.Fatalf("the backend got slot %d", spec.Slot)
	}
	if st := c.do(proto.Request{Op: proto.OpStatus, Slot: 2}); st.State != proto.StateStopped {
		t.Fatalf("slot 2 never started: %+v", st)
	}

	wantOK(t, c.do(proto.Request{Op: proto.OpPause, Slot: 1}), proto.OpPause)
	if st := c.do(proto.Request{Op: proto.OpStatus}); st.State != proto.StateRunning {
		t.Fatalf("pausing slot 1 paused the Docker VM: %+v", st)
	}
	wantOK(t, c.do(proto.Request{Op: proto.OpStop, Slot: 1, Force: true}), proto.OpStop)
	if st := c.do(proto.Request{Op: proto.OpStatus}); st.State != proto.StateRunning || st.VMID != docker.VMID {
		t.Fatalf("stopping slot 1 stopped the Docker VM: %+v", st)
	}
	wantCode(t, c.do(proto.Request{Op: proto.OpStop, Slot: 1}), proto.CodeNotRunning)
	wantCode(t, c.do(proto.Request{Op: proto.OpPause, Slot: 3}), proto.CodeNotRunning)
}

// TestSlotLeases: one connection may hold several leases, and closing it terminates
// every VM it holds; another connection cannot take a held slot but may start a free one.
func TestSlotLeases(t *testing.T) {
	s, be := newTestServer(t, Config{validateDataDir: acceptAll})
	p := &localPeer{sid: sidA}
	holder := openSession(t, s, p)
	wantOK(t, holder.do(proto.Request{Op: proto.OpSetDataLocation, Handle: tempDirHandle(t)}), proto.OpSetDataLocation)
	wantOK(t, holder.do(proto.Request{Op: proto.OpStart}), proto.OpStart)
	wantOK(t, holder.do(proto.Request{Op: proto.OpStart, Slot: 1}), proto.OpStart)
	wantOK(t, holder.do(proto.Request{Op: proto.OpStart, Slot: proto.MaxSlot}), proto.OpStart)

	other := openSession(t, s, p)
	wantCode(t, other.do(proto.Request{Op: proto.OpStart, Slot: 1}), proto.CodeLeaseHeld)
	wantOK(t, other.do(proto.Request{Op: proto.OpStart, Slot: 2}), proto.OpStart)

	holder.close()
	if got := be.StopCount(sidA); got != 3 {
		t.Fatalf("closing the holder stopped %d VMs, want 3", got)
	}
	for _, slot := range []uint8{0, 1, proto.MaxSlot} {
		if st := other.do(proto.Request{Op: proto.OpStatus, Slot: slot}); st.State != proto.StateStopped {
			t.Fatalf("slot %d outlived its lease: %+v", slot, st)
		}
	}
	if st := other.do(proto.Request{Op: proto.OpStatus, Slot: 2}); st.State != proto.StateRunning {
		t.Fatalf("slot 2, held by the other connection, was stopped: %+v", st)
	}
	other.close()
	if got := be.StopCount(sidA); got != 4 {
		t.Fatalf("closing the other connection: %d stops in all, want 4", got)
	}
}

// TestSlotsOfOtherUsers: a slot is per user; one user's session is not another's.
func TestSlotsOfOtherUsers(t *testing.T) {
	s, _ := newTestServer(t, Config{validateDataDir: acceptAll})
	a := openSession(t, s, &localPeer{sid: sidA})
	wantOK(t, a.do(proto.Request{Op: proto.OpSetDataLocation, Handle: tempDirHandle(t)}), proto.OpSetDataLocation)
	wantOK(t, a.do(proto.Request{Op: proto.OpStart, Slot: 1}), proto.OpStart)
	b := openSession(t, s, &localPeer{sid: sidB})
	if st := b.do(proto.Request{Op: proto.OpStatus, Slot: 1}); st.State != proto.StateStopped || !st.Secret.IsZero() {
		t.Fatalf("user B sees user A's session: %+v", st)
	}
	wantCode(t, b.do(proto.Request{Op: proto.OpStop, Slot: 1, Force: true}), proto.CodeNotRunning)
	if st := a.do(proto.Request{Op: proto.OpStatus, Slot: 1}); st.State != proto.StateRunning {
		t.Fatalf("user B's stop reached user A's session: %+v", st)
	}
}

// TestTotalMemory: a start or resize that would take one user's running VMs past
// MaxTotalMemoryMB is refused (ADR 0009: sessions cannot starve the host).
func TestTotalMemory(t *testing.T) {
	s, _ := newTestServer(t, Config{validateDataDir: acceptAll, MaxTotalMemoryMB: 4096})
	c := openSession(t, s, &localPeer{sid: sidA})
	wantOK(t, c.do(proto.Request{Op: proto.OpSetDataLocation, Handle: tempDirHandle(t)}), proto.OpSetDataLocation)
	wantOK(t, c.do(proto.Request{Op: proto.OpStart}), proto.OpStart)                   // 2048
	wantOK(t, c.do(proto.Request{Op: proto.OpStart, Slot: 1}), proto.OpStart)          // 3072
	wantOK(t, c.do(proto.Request{Op: proto.OpStart, Slot: 2}), proto.OpStart)          // 4096
	wantCode(t, c.do(proto.Request{Op: proto.OpStart, Slot: 3}), proto.CodeOutOfRange) // 5120
	wantCode(t, c.do(proto.Request{Op: proto.OpResize, Slot: 1, MemoryMB: 2048}), proto.CodeOutOfRange)
	// A stopped VM no longer counts.
	wantOK(t, c.do(proto.Request{Op: proto.OpStop, Slot: 2, Force: true}), proto.OpStop)
	wantOK(t, c.do(proto.Request{Op: proto.OpStart, Slot: 3}), proto.OpStart)
	// Resizing a stopped slot is not checked until it starts.
	wantOK(t, c.do(proto.Request{Op: proto.OpResize, Slot: 4, MemoryMB: 8192}), proto.OpResize)
	wantCode(t, c.do(proto.Request{Op: proto.OpStart, Slot: 4}), proto.CodeOutOfRange)
}

// TestDataLocationBusyWhileASessionRuns: every slot's disk is below the data location,
// so it cannot move while any of them runs.
func TestDataLocationBusyWhileASessionRuns(t *testing.T) {
	s, _ := newTestServer(t, Config{validateDataDir: acceptAll})
	c := openSession(t, s, &localPeer{sid: sidA})
	wantOK(t, c.do(proto.Request{Op: proto.OpSetDataLocation, Handle: tempDirHandle(t)}), proto.OpSetDataLocation)
	wantOK(t, c.do(proto.Request{Op: proto.OpStart, Slot: 3}), proto.OpStart)
	wantCode(t, c.do(proto.Request{Op: proto.OpSetDataLocation, Handle: tempDirHandle(t)}), proto.CodeBusy)
	wantOK(t, c.do(proto.Request{Op: proto.OpStop, Slot: 3, Force: true}), proto.OpStop)
	wantOK(t, c.do(proto.Request{Op: proto.OpSetDataLocation, Handle: tempDirHandle(t)}), proto.OpSetDataLocation)
}

func TestNewRejectsBadSessionConfig(t *testing.T) {
	for name, cfg := range map[string]Config{
		"session cpus":       {Backend: NewFakeBackend(), MaxCPUs: 4, SessionCPUs: 5},
		"session memory low": {Backend: NewFakeBackend(), SessionMemory: 256},
		"session memory":     {Backend: NewFakeBackend(), MaxMemoryMB: 4096, DefaultMemory: 2048, SessionMemory: 8192},
		"total":              {Backend: NewFakeBackend(), MaxTotalMemoryMB: 256},
		"total below docker": {Backend: NewFakeBackend(), MaxTotalMemoryMB: 1024},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New accepted %+v", name, cfg)
		}
	}
}

func TestMemoryCap(t *testing.T) {
	quiet := func(string, ...any) {}
	for _, c := range []struct {
		phys uint64
		want uint32
	}{
		{0, 0},            // unknown: the server's default
		{2048, 2048},      // never below the Docker VM's default
		{8192, 6144},      // 75%
		{32 << 10, 16384}, // never above the per-VM maximum
		{512 << 10, 16384},
	} {
		if got := memoryCap(c.phys, quiet); got != c.want {
			t.Errorf("memoryCap(%d) = %d, want %d", c.phys, got, c.want)
		}
	}
	if physicalMemoryMB() == 0 {
		t.Error("physical memory unreadable")
	}
	if _, err := New(Config{Backend: NewFakeBackend(), MaxTotalMemoryMB: 6144}); err != nil {
		t.Fatalf("the cap of an 8 GB host: %v", err)
	}
}
