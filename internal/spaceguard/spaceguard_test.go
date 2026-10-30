package spaceguard

import (
	"math"
	"strings"
	"testing"
)

const (
	mib = uint64(1 << 20)
	gib = uint64(1 << 30)
)

var testCfg = Config{Margin: 64 * mib, Floor: 16 * mib, ResumeGap: 32 * mib, Hysteresis: 32 * mib, Quantum: mib}

func TestConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Config
		err  string
	}{
		{"default", DefaultConfig, ""},
		{"test", testCfg, ""},
		{"zero quantum", Config{Margin: 2, Floor: 1}, "quantum"},
		{"zero margin", Config{Quantum: 1}, "margin"},
		{"floor equals margin", Config{Margin: 5, Floor: 5, Quantum: 1}, "floor"},
		{"floor above margin", Config{Margin: 5, Floor: 6, Quantum: 1}, "floor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.Validate()
			if tc.err == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("got %v, want error containing %q", err, tc.err)
			}
		})
	}
}

func TestHostBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    Host
		want Budget
		err  bool
	}{
		{"plenty of host space, max unknown", Host{Free: 100 * gib, VHDXAllocated: gib, VHDXMetadata: 4 * mib}, Budget{Bytes: 100*gib - 64*mib, Backed: gib - 4*mib}, false},
		{"host below margin", Host{Free: 10 * mib}, Budget{Bytes: 0}, false},
		{"host exactly at margin", Host{Free: 64 * mib}, Budget{Bytes: 0}, false},
		{"VHDX cannot outgrow the host", Host{Free: 10 * gib, VHDXAllocated: gib, VHDXMax: 4 * gib}, Budget{Unlimited: true}, false},
		{"headroom exactly equals spare", Host{Free: 3*gib + 64*mib, VHDXAllocated: gib, VHDXMax: 4 * gib}, Budget{Unlimited: true}, false},
		{"headroom one byte over spare", Host{Free: 3*gib + 64*mib - 1, VHDXAllocated: gib, VHDXMax: 4 * gib}, Budget{Bytes: 3*gib - 1, Backed: gib}, false},
		{"full VHDX, host below margin", Host{Free: 0, VHDXAllocated: 4 * gib, VHDXMax: 4 * gib}, Budget{Unlimited: true}, false},
		{"allocation above max", Host{Free: gib, VHDXAllocated: 5 * gib, VHDXMax: 4 * gib}, Budget{}, true},
		{"metadata above allocation", Host{Free: gib, VHDXAllocated: 4 * mib, VHDXMetadata: 5 * mib}, Budget{}, true},
		{"max uint64 free", Host{Free: math.MaxUint64}, Budget{Bytes: math.MaxUint64 - 64*mib}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := HostBudget(tc.h, testCfg)
			if (err != nil) != tc.err {
				t.Fatalf("err = %v, want error %v", err, tc.err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestGuestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		g    Guest
		ok   bool
	}{
		{"consistent", Guest{Device: 12, Size: 10, Used: 4, Free: 6, Reserved: 2}, true},
		{"used+free below size", Guest{Device: 10, Size: 10, Used: 4, Free: 5}, true},
		{"zero size", Guest{Device: 10}, false},
		{"device below size", Guest{Device: 9, Size: 10}, false},
		{"used above size", Guest{Device: 10, Size: 10, Used: 11}, false},
		{"free above size", Guest{Device: 10, Size: 10, Free: 11}, false},
		{"sum above size", Guest{Device: 10, Size: 10, Used: 6, Free: 6}, false},
		{"sum overflows", Guest{Device: math.MaxUint64, Size: math.MaxUint64, Used: math.MaxUint64, Free: math.MaxUint64}, false},
		{"reservation above used", Guest{Device: 10, Size: 10, Used: 2, Free: 8, Reserved: 3}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.g.Validate(); (err == nil) != tc.ok {
				t.Fatalf("Validate() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

// guest2G is a 2 GiB device with a 1.9 GiB file system (the rest is ext4 metadata).
func guest2G(used, reserved uint64) Guest {
	const size = 2*gib - 100*mib
	return Guest{Device: 2 * gib, Size: size, Used: used, Free: size - used, Reserved: reserved}
}

func TestGuestReservation(t *testing.T) {
	// 500 MiB used, all of it backed; the 100 MiB of metadata is not backed.
	// exposure = 2048 - R - 500 MiB.
	base := guest2G(500*mib, 0)
	for _, tc := range []struct {
		name string
		g    Guest
		b    Budget
		want Reservation
	}{
		{"unlimited, nothing reserved", base, Budget{Unlimited: true}, Reservation{}},
		{"budget above exposure", base, Budget{Bytes: 2 * gib, Backed: 500 * mib}, Reservation{}},
		{"budget exactly exposure", base, Budget{Bytes: 1548 * mib, Backed: 500 * mib}, Reservation{}},
		{"budget 1 GiB: reserve 524 MiB + half band",
			base, Budget{Bytes: gib, Backed: 500 * mib}, Reservation{Target: 524*mib + 16*mib, Need: 524 * mib}},
		{"unbacked used blocks (zero-written file) raise the need",
			base, Budget{Bytes: gib, Backed: 200 * mib}, Reservation{Target: 824*mib + 16*mib, Need: 824 * mib}},
		{"VHDX backs deleted untrimmed blocks: lower need",
			base, Budget{Bytes: gib, Backed: 900 * mib}, Reservation{Target: 124*mib + 16*mib, Need: 124 * mib}},
		{"backed above device: nothing needed",
			base, Budget{Bytes: 0, Backed: 3 * gib}, Reservation{}},
		{"budget 0: reserve everything, still short of the unbacked metadata",
			base, Budget{Bytes: 0, Backed: 500 * mib}, Reservation{Target: 1448 * mib, Need: 1448 * mib, Short: 100 * mib}},
		{"inside the band: leave it",
			guest2G(1030*mib, 530*mib), Budget{Bytes: gib, Backed: 500 * mib}, Reservation{Target: 530 * mib, Need: 524 * mib}},
		{"at the top of the band: leave it",
			guest2G(1056*mib, 556*mib), Budget{Bytes: gib, Backed: 500 * mib}, Reservation{Target: 556 * mib, Need: 524 * mib}},
		{"above the band: shrink to set point",
			guest2G(1057*mib, 557*mib), Budget{Bytes: gib, Backed: 500 * mib}, Reservation{Target: 540 * mib, Need: 524 * mib}},
		{"below the band by one byte: grow",
			guest2G(1024*mib-1, 524*mib-1), Budget{Bytes: gib, Backed: 500 * mib}, Reservation{Target: 540 * mib, Need: 524 * mib}},
		{"budget became unlimited: release all",
			guest2G(gib, gib/2), Budget{Unlimited: true}, Reservation{Target: 0}},
		{"small leftover reservation inside band when nothing needed is kept",
			guest2G(gib, 10*mib), Budget{Unlimited: true}, Reservation{Target: 10 * mib}},
		{"nearly full guest, nothing needed",
			guest2G(2*gib-110*mib, 0), Budget{Bytes: 100*mib + 10*mib, Backed: 2*gib - 110*mib}, Reservation{}},
		{"set point capped at unreserved, need > 0",
			guest2G(2*gib-120*mib, 0), Budget{Bytes: 100 * mib, Backed: 2*gib - 120*mib}, Reservation{Target: 20 * mib, Need: 20 * mib}},
		{"unaligned need rounds up",
			base, Budget{Bytes: 1548*mib - 1000, Backed: 500 * mib}, Reservation{Target: 17 * mib, Need: 1000}},
		{"max values do not overflow",
			Guest{Device: math.MaxUint64, Size: math.MaxUint64, Free: math.MaxUint64}, Budget{},
			Reservation{Target: math.MaxUint64, Need: math.MaxUint64}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := GuestReservation(tc.g, tc.b, testCfg)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestGuestWriteDoesNotStaleBudget is the TOCTOU property: when the guest writes W new
// bytes after the host measured, the host's free space falls and the VHDX allocation
// grows by W. Planning on the new numbers gives the same need as on the old ones, so a
// budget computed before a write burst is still right after it.
func TestGuestWriteDoesNotStaleBudget(t *testing.T) {
	h := Host{Free: 400 * mib, VHDXAllocated: 100 * mib, VHDXMetadata: 4 * mib}
	g := guest2G(96*mib, 0)
	before, err := Plan(Input{Host: h, Guest: g}, testCfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []uint64{mib, 50 * mib, 200 * mib} {
		h2, g2 := h, g
		h2.Free -= w
		h2.VHDXAllocated += w
		g2.Used += w
		g2.Free -= w
		after, err := Plan(Input{Host: h2, Guest: g2}, testCfg)
		if err != nil {
			t.Fatal(err)
		}
		if after.Reservation.Need != before.Reservation.Need {
			t.Errorf("write of %d MiB moved need %d -> %d", w/mib, before.Reservation.Need, after.Reservation.Need)
		}
	}
}

func TestGuestReservationRejectsBadInput(t *testing.T) {
	if _, err := GuestReservation(Guest{Size: 10, Used: 11}, Budget{}, testCfg); err == nil {
		t.Fatal("inconsistent guest accepted")
	}
	if _, err := GuestReservation(Guest{Size: 10}, Budget{}, Config{}); err == nil {
		t.Fatal("invalid config accepted")
	}
}

// TestGuestReservationIdempotent checks that applying a decision and measuring again
// does not move the reservation (no flapping).
func TestGuestReservationIdempotent(t *testing.T) {
	g := guest2G(500*mib, 0)
	b := Budget{Bytes: gib, Backed: 500 * mib}
	r1, err := GuestReservation(g, b, testCfg)
	if err != nil {
		t.Fatal(err)
	}
	g = apply(g, r1.Target)
	r2, err := GuestReservation(g, b, testCfg)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Target != r1.Target {
		t.Fatalf("second decision %d != first %d", r2.Target, r1.Target)
	}
}

// apply models the guest after it set the reservation file to target bytes.
func apply(g Guest, target uint64) Guest {
	unreserved := g.Free + g.Reserved
	g.Used = g.Used - g.Reserved + target
	g.Free = unreserved - target
	g.Reserved = target
	return g
}

func TestPauseWanted(t *testing.T) {
	c := testCfg // floor 16 MiB, resume at 48 MiB
	for _, tc := range []struct {
		free   uint64
		paused bool
		want   bool
	}{
		{100 * mib, false, false},
		{16 * mib, false, false},
		{16*mib - 1, false, true},
		{0, false, true},
		{20 * mib, true, true},
		{48*mib - 1, true, true},
		{48 * mib, true, false},
		{math.MaxUint64, true, false},
	} {
		if got := PauseWanted(tc.free, tc.paused, c); got != tc.want {
			t.Errorf("PauseWanted(%d, %v) = %v, want %v", tc.free, tc.paused, got, tc.want)
		}
	}
	// ResumeGap so large that Floor+ResumeGap overflows: stays paused, never wraps.
	c.ResumeGap = math.MaxUint64
	if !PauseWanted(math.MaxUint64-1, true, c) {
		t.Error("overflowing resume threshold resumed the VM")
	}
}

func TestPlan(t *testing.T) {
	// Host: 400 MiB free on a 2 GiB VHDX that has 100 MiB allocated, 4 MiB of it metadata.
	in := Input{
		Host:  Host{Free: 400 * mib, VHDXAllocated: 100 * mib, VHDXMetadata: 4 * mib, VHDXMax: 2*gib + 8*mib},
		Guest: guest2G(96*mib, 0),
	}
	d, err := Plan(in, testCfg)
	if err != nil {
		t.Fatal(err)
	}
	if d.Budget != (Budget{Bytes: 336 * mib, Backed: 96 * mib}) {
		t.Fatalf("budget %+v", d.Budget)
	}
	if exposure := in.Guest.Device - d.Reservation.Target - d.Budget.Backed; exposure > 336*mib {
		t.Fatalf("exposure %d > budget", exposure)
	}
	if d.Pause {
		t.Fatal("paused with 400 MiB free")
	}
	in.Host.Free = 10 * mib
	if d, err = Plan(in, testCfg); err != nil || !d.Pause || d.Budget.Bytes != 0 {
		t.Fatalf("near-full host: %+v, %v", d, err)
	}
	in.Host.VHDXMax = 1
	if _, err := Plan(in, testCfg); err == nil {
		t.Fatal("bad host input accepted")
	}
}

func TestRoundUpCap(t *testing.T) {
	for _, tc := range []struct{ x, q, limit, want uint64 }{
		{0, 4, 10, 0},
		{1, 4, 10, 4},
		{4, 4, 10, 4},
		{9, 4, 10, 10},
		{11, 4, 10, 10},
		{math.MaxUint64 - 1, 4, math.MaxUint64, math.MaxUint64},
		{5, 1, 10, 5},
	} {
		if got := roundUpCap(tc.x, tc.q, tc.limit); got != tc.want {
			t.Errorf("roundUpCap(%d, %d, %d) = %d, want %d", tc.x, tc.q, tc.limit, got, tc.want)
		}
	}
}

func TestBudgetWire(t *testing.T) {
	for _, b := range []Budget{{Unlimited: true}, {}, {Bytes: 1, Backed: 2}, {Bytes: math.MaxUint64, Backed: math.MaxUint64}} {
		line := FormatBudget(b)
		got, err := ParseBudget(line)
		if err != nil || got != b || len(line) > MaxLineLen {
			t.Errorf("round trip %+v via %q: got %+v, %v", b, line, got, err)
		}
	}
	for _, line := range []string{"budget 5 7\r\n", "budget 5 7", "budget 5 7\n"} {
		if b, err := ParseBudget(line); err != nil || b != (Budget{Bytes: 5, Backed: 7}) {
			t.Errorf("ParseBudget(%q) = %+v, %v", line, b, err)
		}
	}
	for _, line := range []string{
		"", "budget", "budget ", "budget 5", "budget 5 ", "budget  5 7", "budget 5  7", "budget 5 7 ",
		"budget 5 7 9", "budget -1 0", "budget +1 0", "budget 1 +0", " budget 1 0", "budget 01 0",
		"budget 1 00", "budget 18446744073709551616 0", "budget 0 18446744073709551616", "budget 1e9 0",
		"budget 0x10 0", "budget 1_000 0", "Budget 1 0", "budget unlimited ", "budget unlimited 0",
		"budget 1 0\n\n", "budget 1 0\r", "budget unlimited\r", "budget 1 0\n\r", "budget 1 0\r\r\n",
		"budget \t1 0", "budget 1\t0", "budget " + strings.Repeat("9", 60),
	} {
		if b, err := ParseBudget(line); err == nil {
			t.Errorf("ParseBudget(%q) accepted: %+v", line, b)
		}
	}
}
