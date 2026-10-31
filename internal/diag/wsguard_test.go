package diag

import (
	"testing"
	"time"
)

const mib = 1 << 20

func TestGuardRule(t *testing.T) {
	t0 := time.Unix(1000, 0)
	type step struct {
		sec            int
		private, commt uint64 // MiB
		data           uint64 // MiB of the data section resident (the start level is 7)
		rule           string
	}
	for _, tc := range []struct {
		name  string
		steps []step
	}{
		{"steady", []step{{0, 19, 35, 7, ""}, {5, 20, 35, 7, ""}, {10, 19, 35, 7, ""}}},
		{"scan in one step", []step{{0, 19, 35, 7, ""}, {5, 34, 35, 7, "full"}}},
		{"old engine scan", []step{{0, 19, 66, 7, ""}, {5, 66, 67, 39, "full"}}},
		// A scan that spans two samples still counts against the low.
		{"scan over two steps", []step{{0, 19, 35, 7, ""}, {5, 26, 35, 7, ""}, {10, 34, 35, 7, "full"}}},
		// Growth the process does itself raises the commit as well.
		{"heap growth", []step{{0, 19, 35, 7, ""}, {5, 60, 76, 7, ""}}},
		{"native heap growth", []step{{0, 19, 35, 7, ""}, {5, 30, 46, 7, ""}}},
		{"committed then touched", []step{{0, 19, 35, 7, ""}, {5, 19, 43, 7, ""}, {35, 27, 43, 7, ""}}},
		// The process touching all it has except the slack is still use if it climbed slowly.
		{"slow climb", []step{{0, 19, 35, 7, ""}, {20, 24, 35, 7, ""}, {40, 29, 35, 7, ""}, {60, 33, 35, 7, ""}}},
		// After a trim the engine refaults its real working set: not a rise.
		{"refault after trim", []step{{0, 19, 35, 7, ""}, {5, 34, 35, 7, "full"}, {10, 3, 35, 1, ""}, {15, 12, 35, 5, ""}, {70, 19, 35, 7, ""}, {75, 21, 35, 7, ""}}},
		{"second scan, rate limited", []step{{0, 19, 35, 7, ""}, {5, 34, 35, 7, "full"}, {10, 3, 35, 1, ""}, {15, 34, 35, 7, ""}, {70, 34, 35, 7, ""}, {75, 3, 35, 1, ""}, {80, 34, 35, 7, "full"}}},
		// A partial read: +33 MiB with the commit flat, the FIPS buffer resident.
		{"partial scan", []step{{0, 19, 67, 7, ""}, {10, 52, 67, 39, "partial"}}},
		{"partial scan, commit up by 1 MiB", []step{{0, 19, 66, 7, ""}, {10, 52, 67, 39, "partial"}}},
		{"partial-sized rise with commit growth", []step{{0, 19, 67, 7, ""}, {10, 52, 69, 39, ""}}},
		{"heap reuse under the big rise", []step{{0, 19, 67, 7, ""}, {5, 28, 67, 7, ""}, {10, 34, 67, 7, ""}}},
		// Refaulting after Windows trimmed the process (memory pressure, resume) looks
		// like a partial read in the working set, but the data section only returns to
		// its start level.
		{"OS trim then refault", []step{{0, 19, 67, 7, ""}, {5, 4, 67, 1, ""}, {10, 22, 67, 7, ""}}},
		{"refault after a guard trim, later activity", []step{{0, 19, 67, 7, ""}, {5, 66, 67, 39, "full"}, {10, 6, 67, 2, ""}, {60, 8, 67, 3, ""}, {65, 28, 67, 7, ""}, {70, 30, 67, 8, ""}}},
		{"partial after a full trim is rate limited", []step{{0, 19, 67, 7, ""}, {5, 66, 67, 39, "full"}, {10, 2, 67, 1, ""}, {20, 40, 67, 39, ""}, {70, 40, 67, 39, ""}, {75, 2, 67, 1, ""}, {80, 40, 67, 39, "partial"}}},
	} {
		g := &guardState{window: 30 * time.Second, minGap: time.Minute, rise: 8 * mib, slack: 4 * mib, bigRise: 16 * mib, flat: 1 * mib, dataRef: 7 * mib}
		for i, s := range tc.steps {
			at := t0.Add(time.Duration(s.sec) * time.Second)
			data := func() (uint64, bool) { return s.data * mib, true }
			if got := g.observe(guardSample{at: at, private: s.private * mib, commit: s.commt * mib}, data); got != s.rule {
				t.Errorf("%s: step %d (t=%ds): rule %q, want %q", tc.name, i, s.sec, got, s.rule)
			}
		}
	}
	// Without a data-section reader the partial rule is off.
	g := &guardState{window: 30 * time.Second, minGap: time.Minute, rise: 8 * mib, slack: 4 * mib, bigRise: 16 * mib, flat: 1 * mib}
	g.observe(guardSample{at: t0, private: 19 * mib, commit: 67 * mib}, nil)
	if got := g.observe(guardSample{at: t0.Add(5 * time.Second), private: 52 * mib, commit: 67 * mib}, nil); got != "" {
		t.Errorf("no data reader: rule %q", got)
	}
}

// TestGuardLoop drives the loop with a fake OS: a scan triggers exactly one trim and one
// callback, and Stop ends the goroutine (TestMain checks for leaks).
func TestGuardLoop(t *testing.T) {
	tick := make(chan time.Time)
	readings := []uint64{19, 19, 52, 52, 52}
	i, trims, dataReads := 0, 0, 0
	stopped := false
	type call struct {
		rule string
		p    uint64
	}
	called := make(chan call, 4)
	g := startGuard(tick, func() { stopped = true }, guardOS{
		read: func() (uint64, uint64, bool) {
			p := readings[min(i, len(readings)-1)] * mib
			i++
			return p, 67 * mib, true
		},
		trim: func() bool { trims++; return true },
		data: func() (uint64, bool) {
			dataReads++
			if dataReads == 1 {
				return 7 * mib, true // at start
			}
			return 39 * mib, true
		},
	}, func(rule string, p, _ uint64) { called <- call{rule, p} })
	before := workingSetTrims.Load()
	t0 := time.Unix(1000, 0)
	for k := range readings {
		tick <- t0.Add(time.Duration(5*k) * time.Second)
	}
	if !g.PartialRule() {
		t.Error("partial rule off with a working data reader")
	}
	g.Stop()
	g.Stop()
	if !stopped || trims != 1 || len(called) != 1 || workingSetTrims.Load() != before+1 {
		t.Fatalf("stopped %v trims %d callbacks %d counter +%d", stopped, trims, len(called), workingSetTrims.Load()-before)
	}
	if c := <-called; c.rule != rulePartial || c.p != 52*mib {
		t.Fatalf("callback %+v", c)
	}
	var nilGuard *WorkingSetGuard
	nilGuard.Stop()
	StartWorkingSetGuard(nil).Stop()
}
