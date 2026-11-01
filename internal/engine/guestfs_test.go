package engine

import (
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/guestctl"
)

func TestGuestFSThrottle(t *testing.T) {
	const gib = 1 << 30
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	var th guestFSThrottle
	if _, ok := th.next(guestctl.GuardOK, 0, 0, t0); ok {
		t.Fatal("a report without a size was published")
	}
	if _, ok := th.next(guestctl.GuardOK, gib, 2*gib, t0); ok {
		t.Fatal("more free than size was published")
	}
	st, ok := th.next(guestctl.GuardOK, 64*gib, 60*gib, t0)
	if !ok || st.SizeBytes != 64*gib || st.FreeBytes != 60*gib || !st.Updated.Equal(t0) {
		t.Fatalf("first report: %+v %v", st, ok)
	}
	if st.UsedBytes() != 4*gib || st.UsedPercent() != 6.25 {
		t.Fatalf("used %d %.2f", st.UsedBytes(), st.UsedPercent())
	}
	// A small change is held back until a minute has passed.
	if _, ok := th.next(guestctl.GuardOK, 64*gib, 60*gib-(1<<20), t0.Add(time.Second)); ok {
		t.Fatal("a 1 MiB change was published at once")
	}
	if _, ok := th.next(guestctl.GuardOK, 64*gib, 60*gib-(1<<20), t0.Add(time.Minute)); !ok {
		t.Fatal("a small change was not published after a minute")
	}
	// No change is never published again.
	if _, ok := th.next(guestctl.GuardOK, 64*gib, 60*gib-(1<<20), t0.Add(time.Hour)); ok {
		t.Fatal("an unchanged report was published")
	}
	// A large change goes at once.
	if st, ok := th.next(guestctl.GuardOK, 64*gib, 50*gib, t0.Add(time.Minute+time.Second)); !ok || st.FreeBytes != 50*gib {
		t.Fatalf("a 10 GiB change: %+v %v", st, ok)
	}
}

// TestGuestFSThrottleFailClosed is the first start's false "62.8 GiB of 62.8 GiB used in
// the VM (100%)": before the host's first budget the guest guard fails closed and holds
// all free space in its reservation file, and that report was published as use.
func TestGuestFSThrottleFailClosed(t *testing.T) {
	const gib = 1 << 30
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	var th guestFSThrottle
	// What hyperbutlerd sent on a new 64 GiB disk before the first budget.
	if st, ok := th.next(guestctl.GuardNoBudget, 67452317696, 16<<20, t0); ok {
		t.Fatalf("a fail-closed report was published: %+v", st)
	}
	if st, ok := th.next(guestctl.GuardStale, 67452317696, 16<<20, t0); ok {
		t.Fatalf("a stale-budget report was published: %+v", st)
	}
	// The first report under a budget is published, however soon after.
	st, ok := th.next(guestctl.GuardOK, 67452317696, 67451981824, t0.Add(time.Second))
	if !ok || st.UsedPercent() > 1 {
		t.Fatalf("first real report: %+v %v", st, ok)
	}
	// A guest without the guard reports plain statfs.
	var off guestFSThrottle
	if _, ok := off.next(guestctl.GuardOff, 64*gib, 60*gib, t0); !ok {
		t.Fatal("a report without the guard was not published")
	}
	if _, ok := off.next(guestctl.GuardOff, 0, 0, t0); ok {
		t.Fatal("a zero report was published")
	}
}

func TestStateGuestFSValidate(t *testing.T) {
	s := State{Version: stateVersion, Phase: PhaseStopped, GuestFS: &GuestFSState{SizeBytes: 10, FreeBytes: 11}}
	if s.Validate() == nil {
		t.Fatal("free > size accepted")
	}
	s.GuestFS = &GuestFSState{SizeBytes: 10, FreeBytes: 1}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}
