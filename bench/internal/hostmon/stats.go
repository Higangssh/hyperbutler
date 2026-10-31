package hostmon

import (
	"math"
	"slices"
)

// Quantile returns the nearest-rank q-quantile of xs (0 < q <= 1): the smallest value
// such that at least q×n of the values are at or below it. The median of an even count
// is the lower middle value under this rule, so Median is used for medians instead.
// It returns NaN for an empty slice or q outside (0, 1].
func Quantile(xs []float64, q float64) float64 {
	if len(xs) == 0 || !(q > 0 && q <= 1) {
		return math.NaN()
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	rank := int(math.Ceil(q * float64(len(s))))
	return s[max(rank, 1)-1]
}

// Median returns the median of xs (the mean of the two middle values for an even
// count), or NaN for an empty slice.
func Median(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// MinMax returns the smallest and largest value of xs (NaN, NaN when empty).
func MinMax(xs []float64) (lo, hi float64) {
	if len(xs) == 0 {
		return math.NaN(), math.NaN()
	}
	return slices.Min(xs), slices.Max(xs)
}

// maxSlopePoints bounds SlopePerHour, which is quadratic in its input.
const maxSlopePoints = 2000

// SlopePerHour is the Theil–Sen slope of v over t (t in seconds), in v units per hour:
// the median of the slopes between every pair of points with distinct t. It returns NaN
// with fewer than two distinct times or more than maxSlopePoints points.
func SlopePerHour(t, v []float64) float64 {
	if len(t) != len(v) || len(t) > maxSlopePoints {
		return math.NaN()
	}
	var slopes []float64
	for i := range t {
		for j := i + 1; j < len(t); j++ {
			if dt := t[j] - t[i]; dt != 0 { //nolint:gosec // G602: j < len(t) by the loop condition
				slopes = append(slopes, (v[j]-v[i])/dt)
			}
		}
	}
	return Median(slopes) * 3600
}
