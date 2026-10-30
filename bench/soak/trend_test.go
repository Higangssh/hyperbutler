package main

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

func TestMain(m *testing.M) {
	leaktest.VerifyTestMain(m, leaktest.IgnoreWinioIOCP)
}

const (
	mib  = 1 << 20
	hour = 3600.0
	day  = 24 * hour
)

// series samples f every step seconds for total seconds.
func series(total, step float64, f func(t float64) float64) []Point {
	var pts []Point
	for t := 0.0; t <= total; t += step {
		pts = append(pts, Point{T: t, V: f(t)})
	}
	return pts
}

func opts(warmup time.Duration, lim Limit) Options {
	return Options{Warmup: warmup, Bucket: 5 * time.Minute, MinBuckets: 6, Limit: lim}
}

func TestAnalyze(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	noise := func(amp float64) float64 { return (rng.Float64()*2 - 1) * amp }
	// A GC sawtooth: the level climbs linearly, then drops back, every period seconds.
	saw := func(t, period, amp float64) float64 { return amp * math.Mod(t, period) / period }
	// ramp is 0 before start and then grows by perHour.
	ramp := func(t, start, perHour float64) float64 { return math.Max(0, t-start) / hour * perHour }

	wsLim := Limit{Abs: mib, Rel: 0.05}
	heapLim := Limit{Abs: mib, Rel: 0.10}
	for _, tc := range []struct {
		name   string
		pts    []Point
		warmup time.Duration
		lim    Limit
		pass   bool
	}{
		// Flat processes must pass.
		{"flat with noise", series(day, 10, func(float64) float64 { return 7*mib + noise(200<<10) }), time.Hour, wsLim, true},
		{"flat GC sawtooth 5 min", series(day, 10, func(t float64) float64 { return 7*mib + saw(t, 300, 2*mib) + noise(50<<10) }), time.Hour, wsLim, true},
		{"warm-up ramp then flat", series(day, 10, func(t float64) float64 { return 3*mib + math.Min(t, 1800)/1800*4*mib + noise(100<<10) }), time.Hour, wsLim, true},
		{"occasional spikes", series(day, 10, func(t float64) float64 {
			if int(t)%3600 == 0 {
				return 40 * mib
			}
			return 7*mib + noise(100<<10)
		}), time.Hour, wsLim, true},
		{"occasional dips (working set trimmed)", series(day, 10, func(t float64) float64 {
			if int(t)%7200 < 30 && t > 12*hour {
				return 1 * mib
			}
			return 7*mib + noise(100<<10)
		}), time.Hour, wsLim, true},
		{"shrinking is fine", series(day, 10, func(t float64) float64 { return 9*mib - t/day*2*mib }), time.Hour, wsLim, true},
		// The 1 h main gate: few sawtooth teeth in the window. Go forces a GC at least
		// every 2 minutes, so periods up to 2 min (and a bit beyond) must pass.
		{"1 h flat heap sawtooth 2 min", series(hour, 10, func(t float64) float64 { return 8*mib + saw(t, 120, 8*mib) }), 10 * time.Minute, heapLim, true},
		{"1 h flat heap sawtooth 4 min", series(hour, 10, func(t float64) float64 { return 8*mib + saw(t, 240, 8*mib) }), 10 * time.Minute, heapLim, true},
		{"1 h flat heap sawtooth 2 min, phase shifted", series(hour, 10, func(t float64) float64 { return 8*mib + saw(t+77, 120, 8*mib) + noise(100<<10) }), 10 * time.Minute, heapLim, true},

		// Leaks must fail.
		// 100 KiB/h: 2.2 MiB over 23 h, above the 1 MiB / 5% limit.
		{"slow linear leak", series(day, 10, func(t float64) float64 { return 7*mib + t/hour*(100<<10) + noise(200<<10) }), time.Hour, wsLim, false},
		{"leak under GC sawtooth", series(day, 10, func(t float64) float64 {
			return 7*mib + t/hour*(150<<10) + saw(t, 300, 2*mib) + noise(50<<10)
		}), time.Hour, wsLim, false},
		{"staircase leak", series(day, 10, func(t float64) float64 { return 7*mib + math.Floor(t/(4*hour))*mib }), time.Hour, wsLim, false},
		{"late-onset leak at hour 20", series(day, 10, func(t float64) float64 { return 7*mib + ramp(t, 20*hour, mib) + noise(100<<10) }), time.Hour, wsLim, false},
		{"late-onset leak at hour 21.5", series(day, 10, func(t float64) float64 { return 7*mib + ramp(t, 21.5*hour, 2*mib) + noise(100<<10) }), time.Hour, wsLim, false},
		{"late-onset leak at hour 17", series(day, 10, func(t float64) float64 { return 7*mib + ramp(t, 17*hour, mib) + noise(100<<10) }), time.Hour, wsLim, false},
		{"single step of 3x the limit", series(day, 10, func(t float64) float64 {
			if t > 12*hour {
				return 10*mib + noise(100<<10)
			}
			return 7*mib + noise(100<<10)
		}), time.Hour, wsLim, false},
		{"single step of 100 MiB", series(day, 10, func(t float64) float64 {
			if t > 12*hour {
				return 107 * mib
			}
			return 7 * mib
		}), time.Hour, wsLim, false},
		{"handle leak one per 10 min", series(day, 10, func(t float64) float64 { return 300 + math.Floor(t/600) }), time.Hour, Limit{Abs: 20, Rel: 0.05}, false},
		{"1 h heap leak under sawtooth", series(hour, 10, func(t float64) float64 { return 8*mib + saw(t, 120, 8*mib) + t/hour*4*mib }), 10 * time.Minute, heapLim, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := Analyze("m", tc.pts, opts(tc.warmup, tc.lim))
			if v.Pass != tc.pass {
				t.Fatalf("Pass = %v, want %v: %s", v.Pass, tc.pass, v)
			}
		})
	}
}

// A 15-minute run with 1-minute buckets, as used for the short local run.
func TestAnalyzeShortRun(t *testing.T) {
	o := Options{Warmup: 3 * time.Minute, Bucket: time.Minute, MinBuckets: 6, Limit: Limit{Abs: 5}}
	steady := series(15*60, 10, func(t float64) float64 { return 40 + float64(int(t/10)%3) })
	if v := Analyze("goroutines", steady, o); !v.Pass {
		t.Fatalf("steady goroutines failed: %s", v)
	}
	leak := series(15*60, 10, func(t float64) float64 { return 40 + math.Floor(t/60) })
	if v := Analyze("goroutines", leak, o); v.Pass {
		t.Fatalf("goroutine leak passed: %s", v)
	}
}

func TestAnalyzeFailsClosed(t *testing.T) {
	lim := Limit{Abs: 1}
	flat := series(3600, 10, func(float64) float64 { return 5 })
	gappy := series(3600, 10, func(float64) float64 { return 5 })
	gappy = append(gappy[:60], gappy[300:]...) // 40 minutes missing
	for _, tc := range []struct {
		name string
		pts  []Point
		o    Options
	}{
		{"no samples", nil, opts(0, lim)},
		{"all inside warm-up", flat, opts(2*time.Hour, lim)},
		{"too few buckets after warm-up", flat, opts(50*time.Minute, lim)},
		{"window shorter than required", flat, Options{Bucket: 5 * time.Minute, MinWindow: 2 * time.Hour, Limit: lim}},
		{"zero bucket", flat, Options{Limit: lim}},
		{"samples missing", gappy, opts(0, lim)},
		{"all at the same time", []Point{{1, 1}, {1, 2}, {1, 3}, {1, 4}, {1, 5}}, opts(0, lim)},
		{"NaN value", append(append([]Point{}, flat...), Point{T: 3700, V: math.NaN()}), opts(0, lim)},
		{"Inf time", append(append([]Point{}, flat...), Point{T: math.Inf(1), V: 5}), opts(0, lim)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if v := Analyze("m", tc.pts, tc.o); v.Pass || v.Reason == "" {
				t.Fatalf("passed: %s", v)
			}
		})
	}
}

func TestTheilSenExact(t *testing.T) {
	pts := series(1000, 1, func(t float64) float64 { return 3 + 0.25*t })
	if s, ok := theilSen(pts); !ok || math.Abs(s-0.25) > 1e-12 {
		t.Fatalf("slope = %v, %v", s, ok)
	}
}

func TestDownsampleKeepsSlope(t *testing.T) {
	pts := series(86400, 10, func(t float64) float64 { return 1000 + t/3600 })
	d := downsample(pts, maxFitPoints)
	if len(d) != maxFitPoints {
		t.Fatalf("len = %d", len(d))
	}
	s, ok := theilSen(d)
	if !ok || math.Abs(s*3600-1) > 1e-3 {
		t.Fatalf("slope/h = %v", s*3600)
	}
	if got := downsample(pts[:10], maxFitPoints); len(got) != 10 {
		t.Fatalf("short input changed: %d", len(got))
	}
}

func TestAnalyzeUnsortedInput(t *testing.T) {
	pts := series(3600, 10, func(t float64) float64 { return t })
	rand.New(rand.NewPCG(3, 4)).Shuffle(len(pts), func(i, j int) { pts[i], pts[j] = pts[j], pts[i] })
	v := Analyze("m", pts, opts(0, Limit{Abs: 1}))
	if v.Pass || math.Abs(v.SlopePerHour-3600) > 60 {
		t.Fatalf("%s", v)
	}
}
