package main

import (
	"fmt"
	"math"
	"slices"
	"time"
)

// Point is one sample of one metric: T is seconds since the run started.
type Point struct {
	T, V float64
}

// Limit is how much growth over the steady-state window counts as noise. The allowed
// growth is max(Abs, Rel × median level in the window).
type Limit struct {
	Abs float64 // in the metric's unit (bytes, handles, goroutines)
	Rel float64 // fraction of the window's median level
}

// Options controls Analyze.
type Options struct {
	Warmup    time.Duration // samples before this are left out
	MinWindow time.Duration // a shorter steady-state window fails
	// Bucket is the width of the time buckets each metric is reduced to before the
	// fit. It must be longer than the slowest periodic swing of every sampled metric,
	// so each bucket holds at least one trough. For the Go heap that is bounded: the
	// runtime forces a GC at least every 2 minutes. OS-level metrics (working set,
	// vmmem) have no such bound; under the soak's steady workload they are
	// quasi-stationary, which the default of 5 minutes assumes.
	Bucket time.Duration
	// MinBuckets is the fewest non-empty buckets the window must have.
	MinBuckets int
	Limit      Limit
}

// Verdict is the trend analysis of one metric.
type Verdict struct {
	Metric       string
	Points       int           // samples in the steady-state window
	Buckets      int           // non-empty buckets the fit used
	Window       time.Duration // time from the first to the last sample in the window
	Median       float64       // median level in the window
	SlopePerHour float64       // Theil–Sen slope of the bucket floors, unit per hour
	Growth       float64       // SlopePerHour × Window: fitted growth across the window
	Shift        float64       // median of the last fifth of bucket floors minus the first fifth
	Allowed      float64       // growth (and shift) that still counts as noise
	Pass         bool
	Reason       string // why it failed, or "" when it passed
}

func (v Verdict) String() string {
	status := "PASS"
	if !v.Pass {
		status = "FAIL: " + v.Reason
	}
	return fmt.Sprintf("%-16s n=%-5d buckets=%-4d window=%-8s median=%-12.0f slope=%+.1f/h growth=%+.1f shift=%+.1f allowed=%.1f  %s",
		v.Metric, v.Points, v.Buckets, v.Window.Round(time.Second), v.Median, v.SlopePerHour, v.Growth, v.Shift, v.Allowed, status)
}

const (
	// maxFitPoints bounds the Theil–Sen fit, which is quadratic in its input. 24 h in
	// 5-minute buckets is 288 points.
	maxFitPoints = 1000
	// minPerBucket is the fewest samples a bucket needs to count.
	minPerBucket = 3
	// floorQuantile picks each bucket's floor: a low quantile rather than the minimum,
	// so one sample read just after Windows trimmed the working set does not set it.
	floorQuantile = 0.10
	// maxMissingBuckets is the fraction of the window's buckets that may be empty
	// (missed samples) before the analysis refuses to judge.
	maxMissingBuckets = 0.2
)

// Analyze judges whether one metric grows over the steady-state window (after
// opts.Warmup). It fails the metric when either test exceeds the noise limit:
//
//   - Slope: each Bucket of samples is reduced to its floor (10th percentile), the
//     level the metric falls back to after each GC or cache flush; a Theil–Sen line
//     through the floors gives the fitted growth across the window. Using floors of
//     buckets longer than the sawtooth period removes the GC sawtooth, which would
//     otherwise bias a pairwise slope upward when the window holds few teeth.
//   - Shift: the median floor of the last fifth of the buckets minus that of the first
//     fifth. Theil–Sen ignores growth confined to less than about 30 % of the window (a
//     leak that starts late, or a single step), because the pairs that do not straddle
//     it are the majority; the shift test sees those.
//
// It fails closed: too few samples or buckets, too many empty buckets, a window
// shorter than MinWindow, or a non-finite value is a failure, never a pass.
func Analyze(metric string, pts []Point, opts Options) Verdict {
	v := Verdict{Metric: metric}
	if opts.Bucket <= 0 {
		v.Reason = "bucket width must be positive"
		return v
	}
	minBuckets := max(opts.MinBuckets, 5)
	start := opts.Warmup.Seconds()
	var win []Point
	for _, p := range pts {
		if math.IsNaN(p.T) || math.IsInf(p.T, 0) || math.IsNaN(p.V) || math.IsInf(p.V, 0) {
			v.Reason = "non-finite sample"
			return v
		}
		if p.T >= start {
			win = append(win, p)
		}
	}
	v.Points = len(win)
	if len(win) < minBuckets*minPerBucket {
		v.Reason = fmt.Sprintf("only %d samples after warm-up, need %d", len(win), minBuckets*minPerBucket)
		return v
	}
	slices.SortStableFunc(win, func(a, b Point) int {
		switch {
		case a.T < b.T:
			return -1
		case a.T > b.T:
			return 1
		}
		return 0
	})
	span := win[len(win)-1].T - win[0].T
	v.Window = time.Duration(span * float64(time.Second))
	if v.Window < opts.MinWindow || span <= 0 {
		v.Reason = fmt.Sprintf("steady-state window %s is shorter than %s", v.Window.Round(time.Second), opts.MinWindow)
		return v
	}
	vals := make([]float64, len(win))
	for i, p := range win {
		vals[i] = p.V
	}
	v.Median = median(vals)

	floors, expected := bucketFloors(win, opts.Bucket.Seconds())
	v.Buckets = len(floors)
	if len(floors) < minBuckets {
		v.Reason = fmt.Sprintf("only %d buckets of %s with data, need %d", len(floors), opts.Bucket, minBuckets)
		return v
	}
	if missing := expected - len(floors); float64(missing) > maxMissingBuckets*float64(expected) {
		v.Reason = fmt.Sprintf("%d of %d buckets have too few samples", missing, expected)
		return v
	}

	slope, ok := theilSen(downsample(floors, maxFitPoints))
	if !ok {
		v.Reason = "no two buckets at different times"
		return v
	}
	v.SlopePerHour = slope * 3600
	v.Growth = slope * span
	// The last fifth catches late onsets; the last tenth catches growth confined to
	// the final hours, which the fifth's median would still hide.
	k, k2 := max(1, len(floors)/5), max(1, len(floors)/10)
	base := median(values(floors[:k]))
	v.Shift = max(median(values(floors[len(floors)-k:])), median(values(floors[len(floors)-k2:]))) - base
	v.Allowed = math.Max(opts.Limit.Abs, opts.Limit.Rel*math.Abs(v.Median))
	switch {
	case v.Growth > v.Allowed:
		v.Reason = fmt.Sprintf("fitted growth %+.1f over %s exceeds the noise limit %.1f",
			v.Growth, v.Window.Round(time.Second), v.Allowed)
	case v.Shift > v.Allowed:
		v.Reason = fmt.Sprintf("level rose %+.1f from the first to the last fifth of the window, above the noise limit %.1f",
			v.Shift, v.Allowed)
	default:
		v.Pass = true
	}
	return v
}

// bucketFloors cuts sorted win into buckets of width seconds, starting at the first
// sample, and returns (mean T, floor V) for each bucket with at least minPerBucket
// samples, plus how many buckets the window spans.
func bucketFloors(win []Point, width float64) ([]Point, int) {
	t0 := win[0].T
	expected := int((win[len(win)-1].T-t0)/width) + 1
	var out []Point
	var vals []float64
	for i := 0; i < len(win); {
		b := math.Floor((win[i].T - t0) / width)
		j := i
		var tsum float64
		vals = vals[:0]
		for j < len(win) && math.Floor((win[j].T-t0)/width) == b {
			tsum += win[j].T
			vals = append(vals, win[j].V)
			j++
		}
		if j-i >= minPerBucket {
			out = append(out, Point{T: tsum / float64(j-i), V: quantile(vals, floorQuantile)})
		}
		i = j
	}
	return out, expected
}

// theilSen returns the median of the slopes between every pair of points at different
// times. pts must be sorted by T.
func theilSen(pts []Point) (float64, bool) {
	slopes := make([]float64, 0, len(pts)*(len(pts)-1)/2)
	for i := range pts {
		for j := i + 1; j < len(pts); j++ {
			dt := pts[j].T - pts[i].T
			if dt <= 0 {
				continue
			}
			slopes = append(slopes, (pts[j].V-pts[i].V)/dt)
		}
	}
	if len(slopes) == 0 {
		return 0, false
	}
	return median(slopes), true
}

// downsample reduces sorted pts to at most max points: consecutive equal-sized buckets,
// each replaced by (mean T, median V).
func downsample(pts []Point, max int) []Point {
	if len(pts) <= max {
		return pts
	}
	out := make([]Point, 0, max)
	vals := make([]float64, 0, len(pts)/max+1)
	for b := 0; b < max; b++ {
		lo, hi := b*len(pts)/max, (b+1)*len(pts)/max
		if lo == hi {
			continue
		}
		vals = vals[:0]
		var tsum float64
		for _, p := range pts[lo:hi] {
			tsum += p.T
			vals = append(vals, p.V)
		}
		out = append(out, Point{T: tsum / float64(hi-lo), V: median(vals)})
	}
	return out
}

func values(pts []Point) []float64 {
	out := make([]float64, len(pts))
	for i, p := range pts {
		out[i] = p.V
	}
	return out
}

// median sorts xs in place and returns its median. xs must not be empty.
func median(xs []float64) float64 {
	slices.Sort(xs)
	n := len(xs)
	if n%2 == 1 {
		return xs[n/2]
	}
	return (xs[n/2-1] + xs[n/2]) / 2
}

// quantile sorts xs in place and returns its q-quantile (nearest rank). xs must not be
// empty.
func quantile(xs []float64, q float64) float64 {
	slices.Sort(xs)
	i := int(math.Ceil(q*float64(len(xs)))) - 1
	return xs[min(max(i, 0), len(xs)-1)]
}
