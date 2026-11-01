package main

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// metrics are the CSV columns after t_seconds, in order.
var metrics = []string{"ws_private", "private_bytes", "ws_private_adj", "private_bytes_adj",
	"handles", "threads", "goroutines", "heap_inuse", "vmmem_ws_private", "vmwp_ws_private"}

// adjusted maps each scavenger-adjusted metric to the raw metric it replaces in the
// verdict. On Windows the Go scavenger decommits the pages it returns to the OS
// (heap_released), and reusing them commits them again, so the engine's private working
// set and private bytes move opposite to heap_released, by up to its size. Adding
// heap_released back cancels that; what is left is growth the runtime cannot give back.
var adjusted = []struct{ adj, raw string }{
	{"ws_private_adj", "ws_private"},
	{"private_bytes_adj", "private_bytes"},
}

func isAdjusted(m string) bool {
	for _, a := range adjusted {
		if a.adj == m {
			return true
		}
	}
	return false
}

// addAdjusted adds the adjusted metrics to one sample's row, from the raw metrics and
// heap_released of the same sample.
func addAdjusted(row map[string]float64, heapReleased float64) {
	for _, a := range adjusted {
		if v, ok := row[a.raw]; ok {
			row[a.adj] = v + heapReleased
		}
	}
}

// verdictConfig is what report needs from the run's configuration.
type verdictConfig struct {
	warmup, minWindow, bucket time.Duration
	maxErrors                 int
	limits                    map[string]Limit
	// optedOut lists metrics the operator explicitly chose not to sample
	// (-diag-pipe none, -vmmem-pid -1, -vmwp-pid -1). Any other metric without samples fails.
	optedOut map[string]bool
}

// minBuckets is the fewest buckets the steady-state window must hold.
const minBuckets = 6

// judge prints a verdict per metric and reports whether every judged metric passed.
// A raw metric whose adjusted metric has samples is analyzed and printed but not
// judged; without adjusted samples (no heap_released: no diag endpoint) the raw metric
// is judged instead, and judge says so.
func judge(out io.Writer, c verdictConfig, series map[string][]Point) bool {
	ok := true
	adjOf, rawOf := map[string]string{}, map[string]string{}
	for _, a := range adjusted {
		adjOf[a.raw], rawOf[a.adj] = a.adj, a.raw
	}
	for _, m := range metrics {
		pts := series[m]
		raw, isAdj := rawOf[m]
		if len(pts) == 0 {
			switch {
			case isAdj && len(series[raw]) > 0:
				_, _ = fmt.Fprintf(out, "%-17s skipped: no heap_released samples (no diag endpoint), %s is judged instead\n", m, raw)
			case isAdj:
				// The raw metric's line already reports the missing samples.
				_, _ = fmt.Fprintf(out, "%-17s skipped: no %s samples\n", m, raw)
			case c.optedOut[m]:
				_, _ = fmt.Fprintf(out, "%-17s skipped (opted out)\n", m)
			default:
				ok = false
				_, _ = fmt.Fprintf(out, "%-17s FAIL: no samples\n", m)
			}
			continue
		}
		lim, have := c.limits[m]
		if !have {
			ok = false
			_, _ = fmt.Fprintf(out, "%-17s FAIL: no noise limit configured\n", m)
			continue
		}
		v := Analyze(m, pts, Options{Warmup: c.warmup, MinWindow: c.minWindow, Bucket: c.bucket, MinBuckets: minBuckets, Limit: lim})
		if adj, has := adjOf[m]; has && len(series[adj]) > 0 {
			v.NotJudged = adj
			_, _ = fmt.Fprintln(out, v)
			continue
		}
		_, _ = fmt.Fprintln(out, v)
		ok = ok && v.Pass
	}
	return ok
}

// report prints a verdict per metric and the workload totals, and returns nil only if
// the run passed. Every failure mode returns an error.
func report(out io.Writer, c verdictConfig, series map[string][]Point, ops, errs int, byKind string,
	failures, sampleErrs []string, fatal error, interrupted bool,
) error {
	_, _ = fmt.Fprintln(out)
	ok := judge(out, c, series)
	_, _ = fmt.Fprintf(out, "\nworkload: %d docker commands, %d failed (%s)\n", ops, errs, byKind)
	for _, f := range failures {
		_, _ = fmt.Fprintln(out, "  ", f)
	}
	if len(sampleErrs) > 0 {
		_, _ = fmt.Fprintf(out, "samples: %d failed, first: %s\n", len(sampleErrs), sampleErrs[0])
	}
	switch {
	case fatal != nil:
		return fatal
	case interrupted:
		return errors.New("interrupted")
	case ops == 0:
		return errors.New("the workload ran no docker commands")
	case errs > c.maxErrors:
		return fmt.Errorf("%d docker commands failed (allowed %d)", errs, c.maxErrors)
	case !ok:
		return errors.New("FAIL: a metric trends upward beyond noise, or has too little data")
	}
	_, _ = fmt.Fprintln(out, "PASS")
	return nil
}
