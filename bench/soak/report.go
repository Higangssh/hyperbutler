package main

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// metrics are the CSV columns after t_seconds, in order.
var metrics = []string{"ws_private", "private_bytes", "handles", "threads", "goroutines", "heap_inuse", "vmmem_ws_private"}

// verdictConfig is what report needs from the run's configuration.
type verdictConfig struct {
	warmup, minWindow, bucket time.Duration
	maxErrors                 int
	limits                    map[string]Limit
	// optedOut lists metrics the operator explicitly chose not to sample
	// (-diag-pipe none, -vmmem-pid -1). Any other metric without samples fails.
	optedOut map[string]bool
}

// minBuckets is the fewest buckets the steady-state window must hold.
const minBuckets = 6

// report prints a verdict per metric and the workload totals, and returns nil only if
// the run passed. Every failure mode returns an error.
func report(out io.Writer, c verdictConfig, series map[string][]Point, ops, errs int, byKind string,
	failures, sampleErrs []string, fatal error, interrupted bool,
) error {
	ok := true
	fmt.Fprintln(out)
	for _, m := range metrics {
		pts := series[m]
		if len(pts) == 0 {
			if c.optedOut[m] {
				fmt.Fprintf(out, "%-16s skipped (opted out)\n", m)
			} else {
				ok = false
				fmt.Fprintf(out, "%-16s FAIL: no samples\n", m)
			}
			continue
		}
		lim, have := c.limits[m]
		if !have {
			ok = false
			fmt.Fprintf(out, "%-16s FAIL: no noise limit configured\n", m)
			continue
		}
		v := Analyze(m, pts, Options{Warmup: c.warmup, MinWindow: c.minWindow, Bucket: c.bucket, MinBuckets: minBuckets, Limit: lim})
		fmt.Fprintln(out, v)
		ok = ok && v.Pass
	}
	fmt.Fprintf(out, "\nworkload: %d docker commands, %d failed (%s)\n", ops, errs, byKind)
	for _, f := range failures {
		fmt.Fprintln(out, "  ", f)
	}
	if len(sampleErrs) > 0 {
		fmt.Fprintf(out, "samples: %d failed, first: %s\n", len(sampleErrs), sampleErrs[0])
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
	fmt.Fprintln(out, "PASS")
	return nil
}
