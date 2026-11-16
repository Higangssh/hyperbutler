package diag

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// timeline gives the process's private working set, commit and resident data section
// (MiB) at second t after the start, given the trims so far (seconds). It lets a case
// say what happens after a trim without knowing when the guard will trim.
type timeline func(t int, trims []int) (private, commit, data float64)

// runGuard feeds a timeline to the guard at its 5 s cadence for secs seconds and
// returns the rules that fired, as "rule@t".
func runGuard(settle time.Duration, secs int, f timeline) []string {
	g := &guardState{window: GuardWindow, minGap: GuardMinGap, settle: settle, rise: GuardRise, slack: GuardSlack, bigRise: GuardBigRise, flat: GuardFlat, dataRef: 7 * mib}
	t0 := time.Unix(1000, 0)
	var trims []int
	var fired []string
	for t := 0; t <= secs; t += 5 {
		p, c, d := f(t, trims)
		data := func() (uint64, bool) { return uint64(d * mib), true }
		if rule := g.observe(guardSample{at: t0.Add(time.Duration(t) * time.Second), private: uint64(p * mib), commit: uint64(c * mib)}, data); rule != "" {
			trims = append(trims, t)
			fired = append(fired, fmt.Sprintf("%s@%d", rule, t))
		}
	}
	return fired
}

// soakRefault is the refault after the first trim of the v0.2.0 soak (2026-11-11 10:16,
// 10 s samples, MiB): 37.1 right after the trim, then 46.9, 50.4, 51.3 ... 53.6 at five
// minutes, creeping on to 54.
func soakRefault(dt int) float64 {
	pts := []struct {
		t int
		v float64
	}{{0, 37.1}, {10, 46.9}, {20, 50.4}, {30, 51.3}, {60, 52.0}, {120, 52.8}, {180, 53.1}, {300, 53.6}, {600, 54.2}}
	if dt >= pts[len(pts)-1].t {
		return pts[len(pts)-1].v
	}
	for i := 1; i < len(pts); i++ {
		if dt < pts[i].t {
			a, b := pts[i-1], pts[i]
			return a.v + (b.v-a.v)*float64(dt-a.t)/float64(b.t-a.t)
		}
	}
	return pts[0].v
}

// sinceLast is how long ago the last trim was, or -1.
func sinceLast(t int, trims []int) int {
	if len(trims) == 0 {
		return -1
	}
	return t - trims[len(trims)-1]
}

func TestGuardSettle(t *testing.T) {
	const read = 100 // the outside read lands at t=100
	// soak: 42.1 MiB steady, a read to 59.1 with the data section resident, then after
	// every trim the soak's slow refault; how many trims it takes to stay down is
	// staysDown (0: never).
	soak := func(staysDown int, commitAt func(t int) float64) timeline {
		return func(t int, trims []int) (float64, float64, float64) {
			c := 89.6
			if commitAt != nil {
				c = commitAt(t)
			}
			switch dt := sinceLast(t, trims); {
			case t < read:
				return 42.1, c, 7
			case dt < 0:
				return 59.1, c, 39
			case staysDown > 0 && len(trims) >= staysDown:
				return min(41.5, soakRefault(dt)), c, 7
			default:
				return soakRefault(dt), c, 20
			}
		}
	}
	for _, tc := range []struct {
		name   string
		settle time.Duration
		secs   int
		f      timeline
		want   []string
	}{
		{"soak 10:16: the settle trim brings it back", GuardSettle, 1800, soak(2, nil), []string{"partial@100", "settle@400"}},
		{"refaults twice: two settle trims, then kept", GuardSettle, 3600, soak(0, nil), []string{"partial@100", "settle@400", "settle@700"}},
		{"settle check off", 0, 1800, soak(0, nil), []string{"partial@100"}},
		{"refault back to the low", GuardSettle, 1800, soak(1, nil), []string{"partial@100"}},
		// Commit noise at the first chance does not use up the check.
		{"commit noise when the check opens", GuardSettle, 1800, soak(2, func(t int) float64 {
			if t >= 400 && t < 420 {
				return 90.9
			}
			return 89.6
		}), []string{"partial@100", "settle@420"}},
		{"the engine grew after the read", GuardSettle, 1800, soak(0, func(t int) float64 {
			if t >= 200 {
				return 92.6
			}
			return 89.6
		}), []string{"partial@100"}},
		// A refault still climbing when the check opens is trimmed once it stops.
		{"still climbing when the check opens", GuardSettle, 1800, func(t int, trims []int) (float64, float64, float64) {
			switch dt := sinceLast(t, trims); {
			case t < read:
				return 42.1, 89.6, 7
			case dt < 0:
				return 59.1, 89.6, 39
			case len(trims) >= 2:
				return 41.5, 89.6, 7
			default:
				return min(37+float64(dt)*0.05, 57), 89.6, 20
			}
		}, []string{"partial@100", "settle@510"}},
		// A level reached only after the check closed is the process's own.
		{"slow refault after the check closed", GuardSettle, 1800, func(t int, trims []int) (float64, float64, float64) {
			switch dt := sinceLast(t, trims); {
			case t < read:
				return 42.1, 89.6, 7
			case dt < 0:
				return 59.1, 89.6, 39
			case dt < 700:
				return 41, 89.6, 7
			default:
				return 54, 89.6, 7
			}
		}, []string{"partial@100"}},
		// Windows trimmed the engine just before the read: the window's low (8 MiB) is not
		// its working set, so no check is armed and its return to 42 MiB stays.
		{"read during a refault after an OS trim", GuardSettle, 1800, func(t int, trims []int) (float64, float64, float64) {
			switch {
			case len(trims) > 0:
				return 42, 89.6, 7
			case t < 50:
				return 42, 89.6, 7
			case t == 50:
				return 8, 89.6, 1
			case t == 55:
				return 20, 89.6, 4
			case t == 60:
				return 26, 89.6, 5
			default:
				return 48, 89.6, 39
			}
		}, []string{"partial@65"}},
		// A second read while the check is pending, from the elevated level: the
		// baseline stays the first read's 42 MiB.
		{"second read during the follow-up", GuardSettle, 1800, func(t int, trims []int) (float64, float64, float64) {
			switch dt := sinceLast(t, trims); {
			case t < read:
				return 42.1, 89.6, 7
			case dt < 0:
				return 59.1, 89.6, 39
			case len(trims) == 1 && t >= 220:
				return 72, 89.6, 39
			case len(trims) >= 3:
				return 41.5, 89.6, 7
			default:
				return soakRefault(dt), 89.6, 20
			}
		}, []string{"partial@100", "partial@220", "settle@520"}},
	} {
		got := runGuard(tc.settle, tc.secs, tc.f)
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s: fired %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestGuardLoopSettle: the loop reports a settle trim to the callback and counts it.
func TestGuardLoopSettle(t *testing.T) {
	tick := make(chan time.Time)
	var at, trims atomic.Int64 // shared with the guard's goroutine
	called := make(chan string, 8)
	g := startGuard(tick, func() {}, guardOS{
		read: func() (uint64, uint64, bool) {
			switch {
			case at.Load() < 100:
				return 42 * mib, 89 * mib, true
			case trims.Load() == 0:
				return 59 * mib, 89 * mib, true
			case trims.Load() == 1:
				return 54 * mib, 89 * mib, true
			default:
				return 41 * mib, 89 * mib, true
			}
		},
		trim: func() bool { trims.Add(1); return true },
		data: func() (uint64, bool) {
			if at.Load() < 100 || trims.Load() > 1 {
				return 7 * mib, true
			}
			return 39 * mib, true
		},
	}, func(rule string, _, _ uint64) { called <- rule })
	before := workingSetTrims.Load()
	t0 := time.Unix(1000, 0)
	for s := int64(0); s <= 900; s += 5 {
		at.Store(s)
		tick <- t0.Add(time.Duration(s) * time.Second)
	}
	g.Stop()
	if trims.Load() != 2 || workingSetTrims.Load() != before+2 {
		t.Fatalf("trims %d, counter +%d", trims.Load(), workingSetTrims.Load()-before)
	}
	if a, b := <-called, <-called; a != rulePartial || b != ruleSettle {
		t.Fatalf("callbacks %q %q", a, b)
	}
}
