package spaceguard

import (
	"math"
	"math/big"
	"strings"
	"testing"
)

// planSeed is one FuzzPlan corpus entry in argument order.
type planSeed struct {
	hostFree, vhdxAlloc, vhdxMeta, vhdxMax        uint64
	device, size, used, free, reserved            uint64
	paused                                        bool
	margin, floor, resumeGap, hysteresis, quantum uint64
	write                                         uint64 // guest write for the TOCTOU property
	lowerBy                                       uint64 // host free drop for monotonicity
}

func planSeeds() []planSeed {
	const m = math.MaxUint64
	c := testCfg
	d := DefaultConfig
	sz := 2*gib - 100*mib
	return []planSeed{
		// TestPlan.
		{400 * mib, 100 * mib, 4 * mib, 2*gib + 8*mib, 2 * gib, sz, 96 * mib, sz - 96*mib, 0, false, c.Margin, c.Floor, c.ResumeGap, c.Hysteresis, c.Quantum, 50 * mib, 390 * mib},
		// The measured HCS case: fresh ext4 on 2 GiB, 6 MiB allocated, 4 MiB VHDX metadata.
		{382 * mib, 6 * mib, 4 * mib, 2*gib + 64*mib, 2 * gib, 1992552 * 1024, 50 * mib, 1992552*1024 - 50*mib, 0, false, c.Margin, c.Floor, c.ResumeGap, c.Hysteresis, c.Quantum, 200 * mib, mib},
		// Inside / at top / above the band, below by one byte.
		{gib + 64*mib, 500 * mib, 0, 0, 2 * gib, sz, 1030 * mib, sz - 1030*mib, 530 * mib, false, c.Margin, c.Floor, c.ResumeGap, c.Hysteresis, c.Quantum, mib, 1},
		{gib + 64*mib, 500 * mib, 0, 0, 2 * gib, sz, 1056 * mib, sz - 1056*mib, 556 * mib, false, c.Margin, c.Floor, c.ResumeGap, c.Hysteresis, c.Quantum, mib, 1},
		{gib + 64*mib, 500 * mib, 0, 0, 2 * gib, sz, 1057 * mib, sz - 1057*mib, 557 * mib, false, c.Margin, c.Floor, c.ResumeGap, c.Hysteresis, c.Quantum, mib, 1},
		{gib + 64*mib, 500 * mib, 0, 0, 2 * gib, sz, 1024*mib - 1, sz - 1024*mib + 1, 524*mib - 1, false, c.Margin, c.Floor, c.ResumeGap, c.Hysteresis, c.Quantum, mib, 1},
		// Short: unbacked used blocks exceed what can be reserved.
		{64 * mib, 4 * mib, 4 * mib, 0, 2 * gib, sz, gib, sz - gib, 0, false, c.Margin, c.Floor, c.ResumeGap, c.Hysteresis, c.Quantum, 0, 0},
		// Unlimited, headroom exactly equals spare, and one byte over.
		{3*gib + 64*mib, gib, 0, 4 * gib, 2 * gib, 2 * gib, gib, gib, gib / 2, false, c.Margin, c.Floor, c.ResumeGap, c.Hysteresis, c.Quantum, 1, 1},
		{3*gib + 64*mib - 1, gib, 0, 4 * gib, 2 * gib, 2 * gib, gib, gib, gib / 2, false, c.Margin, c.Floor, c.ResumeGap, c.Hysteresis, c.Quantum, 1, 1},
		// Full VHDX, host below margin and floor, paused.
		{0, 4 * gib, 0, 4 * gib, 2 * gib, 2 * gib, gib, gib, 0, true, c.Margin, c.Floor, c.ResumeGap, c.Hysteresis, c.Quantum, 0, 0},
		// Paused near the resume threshold.
		{48*mib - 1, 0, 0, 0, 2 * gib, 2 * gib, gib, gib, 0, true, c.Margin, c.Floor, c.ResumeGap, c.Hysteresis, c.Quantum, 0, 0},
		{48 * mib, 0, 0, 0, 2 * gib, 2 * gib, gib, gib, 0, true, c.Margin, c.Floor, c.ResumeGap, c.Hysteresis, c.Quantum, 0, 0},
		// Errors: allocation above max, metadata above allocation, invalid guest, invalid config.
		{gib, 5 * gib, 0, 4 * gib, 2 * gib, 2 * gib, gib, gib, 0, false, c.Margin, c.Floor, c.ResumeGap, c.Hysteresis, c.Quantum, 0, 0},
		{gib, 4 * mib, 5 * mib, 0, 2 * gib, 2 * gib, gib, gib, 0, false, c.Margin, c.Floor, c.ResumeGap, c.Hysteresis, c.Quantum, 0, 0},
		{gib, 0, 0, 0, 9, 10, 0, 0, 0, false, c.Margin, c.Floor, c.ResumeGap, c.Hysteresis, c.Quantum, 0, 0},
		{gib, 0, 0, 0, 10, 10, 11, 0, 0, false, c.Margin, c.Floor, c.ResumeGap, c.Hysteresis, c.Quantum, 0, 0},
		{gib, 0, 0, 0, 10, 10, 2, 8, 3, false, c.Margin, c.Floor, c.ResumeGap, c.Hysteresis, c.Quantum, 0, 0},
		{gib, 0, 0, 0, 10, 10, 0, 10, 0, false, 5, 5, 0, 0, 1, 0, 0},
		{gib, 0, 0, 0, 10, 10, 0, 10, 0, false, 0, 0, 0, 0, 1, 0, 0},
		{gib, 0, 0, 0, 10, 10, 0, 10, 0, false, 2, 1, 0, 0, 0, 0, 0},
		// MaxUint64 combinations.
		{m, 0, 0, 0, m, m, 0, m, 0, false, c.Margin, c.Floor, c.ResumeGap, c.Hysteresis, c.Quantum, m, m},
		{m, 0, 0, m, m, m, m, 0, m, true, m, m - 1, m, m, m, 1, 1},
		{m, m, m, m, m, m, m / 2, m / 2, m / 4, true, 1, 0, m, m, 1, m, m},
		{m, 0, 0, 0, m, m, 0, m, 0, false, m, 0, m, m, m, m, m},
		{0, 0, 0, 0, m, m, 1, m - 1, 1, true, 1, 0, m, 0, 1, 0, 0},
		{m - 1, 0, 0, 0, m, m, m - 3, 3, m - 3, false, 2, 1, 0, 3, 2, 1, m},
		{100 * gib, gib, 4 * mib, 0, 64 * gib, 63 * gib, 10 * gib, 53 * gib, 0, false, d.Margin, d.Floor, d.ResumeGap, d.Hysteresis, d.Quantum, 10 * gib, 99 * gib},
	}
}

// FuzzPlan checks the policy's invariants on arbitrary measurements and tunables.
// Each input is checked as given (mostly invalid, exercising the error paths) and after
// a deterministic repair that makes it consistent (exercising the arithmetic).
func FuzzPlan(f *testing.F) {
	for _, s := range planSeeds() {
		f.Add(s.hostFree, s.vhdxAlloc, s.vhdxMeta, s.vhdxMax, s.device, s.size, s.used, s.free, s.reserved, s.paused,
			s.margin, s.floor, s.resumeGap, s.hysteresis, s.quantum, s.write, s.lowerBy)
	}
	f.Fuzz(func(t *testing.T, hostFree, vhdxAlloc, vhdxMeta, vhdxMax, device, size, used, free, reserved uint64, paused bool,
		margin, floor, resumeGap, hysteresis, quantum, write, lowerBy uint64) {
		in := Input{
			Host:   Host{Free: hostFree, VHDXAllocated: vhdxAlloc, VHDXMetadata: vhdxMeta, VHDXMax: vhdxMax},
			Guest:  Guest{Device: device, Size: size, Used: used, Free: free, Reserved: reserved},
			Paused: paused,
		}
		c := Config{Margin: margin, Floor: floor, ResumeGap: resumeGap, Hysteresis: hysteresis, Quantum: quantum}
		checkPlan(t, in, c, write, lowerBy)
		in, c = repair(in, c)
		if !checkPlan(t, in, c, write, lowerBy) {
			t.Fatalf("repaired input rejected: %+v %+v", in, c)
		}
	})
}

// repair maps any input onto a consistent one, keeping as many values as possible.
func repair(in Input, c Config) (Input, Config) {
	if c.Quantum == 0 {
		c.Quantum = 1
	}
	if c.Margin == 0 {
		c.Margin = 1
	}
	c.Floor %= c.Margin
	g := &in.Guest
	if g.Size == 0 {
		g.Size = 1
	}
	if g.Device < g.Size {
		g.Device, g.Size = g.Size, g.Device
		if g.Size == 0 {
			g.Size = 1
		}
	}
	g.Used = modPlus1(g.Used, g.Size)
	g.Free = modPlus1(g.Free, g.Size-g.Used)
	g.Reserved = modPlus1(g.Reserved, g.Used)
	h := &in.Host
	if h.VHDXMax != 0 && h.VHDXAllocated > h.VHDXMax {
		h.VHDXAllocated, h.VHDXMax = h.VHDXMax, h.VHDXAllocated
	}
	h.VHDXMetadata = modPlus1(h.VHDXMetadata, h.VHDXAllocated)
	return in, c
}

// modPlus1 returns x mod (n+1), so the result is in [0, n], without overflowing n+1.
func modPlus1(x, n uint64) uint64 {
	if n == math.MaxUint64 {
		return x
	}
	return x % (n + 1)
}

func bi(x uint64) *big.Int { return new(big.Int).SetUint64(x) }

func posPart(x *big.Int) *big.Int {
	if x.Sign() < 0 {
		return new(big.Int)
	}
	return x
}

// checkPlan runs Plan and asserts its invariants. It reports whether Plan accepted the
// input.
func checkPlan(t *testing.T, in Input, c Config, write, lowerBy uint64) bool {
	t.Helper()
	d, err := Plan(in, c)
	wantErr := c.Validate() != nil || in.Guest.Validate() != nil ||
		(in.Host.VHDXMax != 0 && in.Host.VHDXAllocated > in.Host.VHDXMax) ||
		in.Host.VHDXMetadata > in.Host.VHDXAllocated
	if (err != nil) != wantErr {
		t.Fatalf("Plan err = %v, want error %v; in %+v cfg %+v", err, wantErr, in, c)
	}
	if err != nil {
		if d != (Decision{}) {
			t.Fatalf("non-zero decision %+v with error %v", d, err)
		}
		return false
	}
	g, h, r := in.Guest, in.Host, d.Reservation
	unreserved := new(big.Int).Add(bi(g.Free), bi(g.Reserved))
	if !unreserved.IsUint64() {
		t.Fatalf("free+reserved overflows for a valid guest %+v", g)
	}
	u := unreserved.Uint64()
	if r.Target > u || r.Need > u || r.Target < r.Need {
		t.Fatalf("bad reservation %+v, unreserved %d; in %+v cfg %+v", r, u, in, c)
	}

	spare := posPart(new(big.Int).Sub(bi(h.Free), bi(c.Margin)))
	backed := new(big.Int).Sub(bi(h.VHDXAllocated), bi(h.VHDXMetadata))
	if d.Budget.Unlimited {
		if h.VHDXMax == 0 || bi(h.VHDXMax-h.VHDXAllocated).Cmp(spare) > 0 {
			t.Fatalf("unlimited budget with VHDX headroom over spare: %+v cfg %+v", h, c)
		}
		if r.Need != 0 || r.Short != 0 || d.Budget.Bytes != 0 || d.Budget.Backed != 0 {
			t.Fatalf("unlimited budget %+v needs a reservation: %+v", d.Budget, r)
		}
	} else {
		if bi(d.Budget.Bytes).Cmp(spare) != 0 || bi(d.Budget.Backed).Cmp(backed) != 0 {
			t.Fatalf("budget %+v, want spare %s backed %s", d.Budget, spare, backed)
		}
		// The core invariant: exposure = max(Device - Target - Backed, 0) <= host free - margin,
		// unless Short says by how much it cannot be met.
		exposure := posPart(new(big.Int).Sub(new(big.Int).Sub(bi(g.Device), bi(r.Target)), backed))
		over := posPart(new(big.Int).Sub(exposure, spare))
		if over.Cmp(bi(r.Short)) != 0 {
			t.Fatalf("exposure %s over spare %s by %s, but short is %d; in %+v cfg %+v d %+v", exposure, spare, over, r.Short, in, c, d)
		}
		if r.Short > 0 && (r.Target != u || r.Need != u) {
			t.Fatalf("short %d but target %d, need %d != unreserved %d", r.Short, r.Target, r.Need, u)
		}
	}

	// Pause hysteresis.
	if h.Free < c.Floor && !d.Pause {
		t.Fatalf("host free %d below floor %d but not paused", h.Free, c.Floor)
	}
	resume := new(big.Int).Add(bi(c.Floor), bi(c.ResumeGap))
	if bi(h.Free).Cmp(resume) >= 0 && d.Pause {
		t.Fatalf("host free %d at or above resume %s but paused", h.Free, resume)
	}
	if !in.Paused && h.Free >= c.Floor && d.Pause {
		t.Fatalf("unpaused VM paused with host free %d >= floor %d", h.Free, c.Floor)
	}

	// No flapping: apply the target, measure again with the same host, get the same target.
	in2 := in
	in2.Guest = apply(g, r.Target)
	d2, err := Plan(in2, c)
	if err != nil {
		t.Fatalf("applied guest %+v rejected: %v", in2.Guest, err)
	}
	if d2.Reservation.Target != r.Target || d2.Reservation.Need != r.Need {
		t.Fatalf("flapping: %+v then %+v; in %+v cfg %+v", r, d2.Reservation, in, c)
	}

	// Monotonicity: less host free space never needs less reservation.
	in3 := in
	in3.Host.Free = satSub(h.Free, lowerBy)
	d3, err := Plan(in3, c)
	if err != nil {
		t.Fatalf("lower host free %d rejected: %v", in3.Host.Free, err)
	}
	if d3.Reservation.Need < r.Need || d3.Reservation.Short < r.Short {
		t.Fatalf("host free %d -> %d lowered need %d -> %d / short %d -> %d",
			h.Free, in3.Host.Free, r.Need, d3.Reservation.Need, r.Short, d3.Reservation.Short)
	}

	// TOCTOU: a guest write of w new bytes (taken from guest free, growing the VHDX by w
	// and shrinking host free by w) leaves the need unchanged, as long as nothing
	// saturates: host free stays above the margin, the VHDX stays within its maximum,
	// Backed stays within the device, and the need still fits in what is left to reserve.
	// (If the guard is already short, a write into free space makes it shorter: that is
	// what Short reports, not a stale budget.)
	w := modPlus1(write, g.Free)
	if !d.Budget.Unlimited && w > 0 && r.Short == 0 && r.Need <= u-w &&
		h.Free >= c.Margin && h.Free-c.Margin >= w &&
		h.VHDXAllocated <= math.MaxUint64-w && (h.VHDXMax == 0 || h.VHDXAllocated+w <= h.VHDXMax) &&
		new(big.Int).Add(backed, bi(w)).Cmp(bi(g.Device)) <= 0 {
		in4 := in
		in4.Host.Free -= w
		in4.Host.VHDXAllocated += w
		in4.Guest.Free -= w
		in4.Guest.Used += w
		d4, err := Plan(in4, c)
		if err != nil {
			t.Fatalf("guest write %d rejected: %v", w, err)
		}
		if d4.Budget.Unlimited || d4.Reservation.Need != r.Need || d4.Reservation.Short != 0 {
			t.Fatalf("guest write of %d changed the decision %+v -> %+v; in %+v cfg %+v", w, d, d4, in, c)
		}
	}
	return true
}

// FuzzParseBudget checks that the wire parser never panics, only accepts short lines,
// and that every accepted line is exactly what FormatBudget writes (modulo "\n" vs "\r\n"
// vs no line ending).
func FuzzParseBudget(f *testing.F) {
	for _, s := range []string{
		"budget 0 0\n", "budget 1 2\n", "budget unlimited\n", "budget 18446744073709551615 18446744073709551615\n",
		"budget 5 7\r\n", "budget 5 7", "budget unlimited\r\n", "budget 5 7\r", "budget 5 7\n\r",
		"", "budget", "budget ", "budget 5", "budget -1 0", "budget +1 0", "budget 1 0 ", " budget 1 0", "budget 01 0",
		"budget 18446744073709551616 0", "budget 1e9 0", "budget 0x10 0", "budget 1_000 0", "Budget 1 0",
		"budget unlimited ", "budget 1 0\n\n", "budget \t1 0", "budget 1  0", "budget " + strings.Repeat("9", 60),
		"budget 99999999999999999999 0", "budget ١ 0", "budget 1 0\x00",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		b, err := ParseBudget(s)
		if err != nil {
			if b != (Budget{}) {
				t.Fatalf("ParseBudget(%q) = %+v with error %v", s, b, err)
			}
			return
		}
		if len(s) > MaxLineLen {
			t.Fatalf("accepted %d-byte line %q", len(s), s)
		}
		canon := FormatBudget(b)
		body := strings.TrimSuffix(canon, "\n")
		if s != canon && s != body+"\r\n" && s != body {
			t.Fatalf("ParseBudget(%q) = %+v, but FormatBudget gives %q", s, b, canon)
		}
		if got, err := ParseBudget(canon); err != nil || got != b {
			t.Fatalf("round trip of %+v: %q -> %+v, %v", b, canon, got, err)
		}
	})
}

// FuzzBudgetWireRoundTrip checks ParseBudget(FormatBudget(b)) == b for every budget.
func FuzzBudgetWireRoundTrip(f *testing.F) {
	f.Add(uint64(0), uint64(0), false)
	f.Add(uint64(math.MaxUint64), uint64(math.MaxUint64), false)
	f.Add(uint64(7), uint64(3), true)
	f.Fuzz(func(t *testing.T, n, backed uint64, unlimited bool) {
		b := Budget{Bytes: n, Backed: backed, Unlimited: unlimited}
		if unlimited {
			b.Bytes, b.Backed = 0, 0 // not carried on the wire when unlimited
		}
		line := FormatBudget(b)
		if len(line) > MaxLineLen {
			t.Fatalf("FormatBudget(%+v) = %d bytes, over MaxLineLen", b, len(line))
		}
		got, err := ParseBudget(line)
		if err != nil || got != b {
			t.Fatalf("round trip %+v: %q -> %+v, %v", b, line, got, err)
		}
	})
}
