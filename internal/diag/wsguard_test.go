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
		trim           bool
	}
	for _, tc := range []struct {
		name  string
		steps []step
	}{
		{"steady", []step{{0, 19, 35, false}, {5, 20, 35, false}, {10, 19, 35, false}}},
		{"scan in one step", []step{{0, 19, 35, false}, {5, 34, 35, true}}},
		{"old engine scan", []step{{0, 19, 66, false}, {5, 66, 67, true}}},
		// A scan that spans two samples still counts against the low.
		{"scan over two steps", []step{{0, 19, 35, false}, {5, 26, 35, false}, {10, 34, 35, true}}},
		// Growth the process does itself raises the commit as well.
		{"heap growth", []step{{0, 19, 35, false}, {5, 60, 76, false}}},
		{"native heap growth", []step{{0, 19, 35, false}, {5, 30, 46, false}}},
		{"committed then touched", []step{{0, 19, 35, false}, {5, 19, 43, false}, {35, 27, 43, false}}},
		// The process touching all it has except the slack is still use if it climbed slowly.
		{"slow climb", []step{{0, 19, 35, false}, {20, 24, 35, false}, {40, 29, 35, false}, {60, 33, 35, false}}},
		// After a trim the engine refaults its real working set: not a rise.
		{"refault after trim", []step{{0, 19, 35, false}, {5, 34, 35, true}, {10, 3, 35, false}, {15, 12, 35, false}, {70, 19, 35, false}, {75, 21, 35, false}}},
		// A partial read: +33 MiB with the commit flat, still 15 MiB under it.
		{"partial scan", []step{{0, 19, 67, false}, {10, 52, 67, true}}},
		{"partial scan, commit up by 1 MiB", []step{{0, 19, 66, false}, {10, 52, 67, true}}},
		{"partial-sized rise with commit growth", []step{{0, 19, 67, false}, {10, 52, 69, false}}},
		{"heap reuse under the big rise", []step{{0, 19, 67, false}, {5, 28, 67, false}, {10, 34, 67, false}}},
		{"partial scan after a full one is rate limited", []step{{0, 19, 67, false}, {5, 66, 67, true}, {10, 2, 67, false}, {20, 40, 67, false}, {70, 40, 67, false}}},
		{"second scan, rate limited", []step{{0, 19, 35, false}, {5, 34, 35, true}, {10, 3, 35, false}, {15, 34, 35, false}, {70, 34, 35, false}, {75, 3, 35, false}, {80, 34, 35, true}}},
	} {
		g := &guardState{window: 30 * time.Second, minGap: time.Minute, rise: 8 * mib, slack: 4 * mib, bigRise: 16 * mib, flat: 1 * mib}
		for i, s := range tc.steps {
			at := t0.Add(time.Duration(s.sec) * time.Second)
			if got := g.observe(guardSample{at: at, private: s.private * mib, commit: s.commt * mib}); got != s.trim {
				t.Errorf("%s: step %d (t=%ds): trim %v, want %v", tc.name, i, s.sec, got, s.trim)
			}
		}
	}
}

// TestGuardLoop drives the loop with a fake OS: a scan triggers exactly one trim and one
// callback, and Stop ends the goroutine (TestMain checks for leaks).
func TestGuardLoop(t *testing.T) {
	tick := make(chan time.Time)
	readings := []uint64{19, 19, 34, 34, 34}
	i, trims := 0, 0
	stopped := false
	called := make(chan uint64, 4)
	g := startGuard(tick, func() { stopped = true }, guardOS{
		read: func() (uint64, uint64, bool) {
			p := readings[min(i, len(readings)-1)] * mib
			i++
			return p, 35 * mib, true
		},
		trim: func() bool { trims++; return true },
	}, func(p, _ uint64) { called <- p })
	before := workingSetTrims.Load()
	t0 := time.Unix(1000, 0)
	for k := range readings {
		tick <- t0.Add(time.Duration(5*k) * time.Second)
	}
	g.Stop()
	g.Stop()
	if !stopped || trims != 1 || len(called) != 1 || <-called != 34*mib || workingSetTrims.Load() != before+1 {
		t.Fatalf("stopped %v trims %d callbacks %d counter +%d", stopped, trims, len(called), workingSetTrims.Load()-before)
	}
	var nilGuard *WorkingSetGuard
	nilGuard.Stop()
	StartWorkingSetGuard(nil).Stop()
}
