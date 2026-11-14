package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// metrics are the CSV columns after t_seconds, in order.
var metrics = []string{"ws_private", "private_bytes", "ws_private_adj", "private_bytes_adj",
	"handles", "threads", "goroutines", "heap_inuse", "vmmem_ws_private", "vmwp_ws_private",
	"broker_private_bytes", "broker_handles"}

// adjusted maps each scavenger-adjusted metric to the raw metric it replaces in the
// verdict. On Windows the Go scavenger decommits the pages it returns to the OS
// (heap_released), and reusing them commits them again, so the engine's private working
// set and private bytes move opposite to heap_released, by up to its size. Adding
// heap_released back cancels that; what is left is growth the runtime cannot give back.
var adjusted = []struct{ adj, raw string }{
	{"ws_private_adj", "ws_private"},
	{"private_bytes_adj", "private_bytes"},
}

// committed maps each working-set metric of the engine to the committed-memory metric
// judged in its place. A private page that is resident is also committed, so a leak of
// private memory raises private bytes; the private working set adds no leak signal of
// its own, only residency, which Windows and outside readers change: the working-set
// guard trims it, and a process that reads the engine's memory (an antivirus scan after
// a signature update, in the v0.2.0 24 h run) makes committed pages resident again. The
// working set is still reported, with the guard's trims, for the memory budgets
// (REQ-MEM-001). ADR 0003, guardrail 3, change of 2026-11-14.
var committed = []struct{ ws, commit string }{
	{"ws_private", "private_bytes"},
	{"ws_private_adj", "private_bytes_adj"},
}

// trimsSeries is the series of the engine's working-set guard trim count
// (diag.Stats.WorkingSetTrims, cumulative). It is not a judged metric.
const trimsSeries = "ws_trims"

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
// is judged instead, and judge says so. A working-set metric is analyzed and printed,
// with the guard's trims, but its commit metric is judged in its place, held to the
// working set's allowance (metrics lists each working set before its commit metric).
func judge(out io.Writer, c verdictConfig, series map[string][]Point) bool {
	ok := true
	adjOf, rawOf := map[string]string{}, map[string]string{}
	for _, a := range adjusted {
		adjOf[a.raw], rawOf[a.adj] = a.adj, a.raw
	}
	commitOf := map[string]string{}
	for _, w := range committed {
		commitOf[w.ws] = w.commit
	}
	// judgedInstead names the metric judged in m's place, or "" when m itself is judged:
	// the adjusted metric for a raw one, then, for a working set, the commit metric that
	// is judged (adjusted, or raw without heap_released).
	judgedInstead := func(m string) string {
		if c, has := commitOf[m]; has && len(series[c]) > 0 {
			if adj, has := adjOf[c]; has && len(series[adj]) > 0 {
				return adj
			}
			return c
		}
		if adj, has := adjOf[m]; has && len(series[adj]) > 0 {
			return adj
		}
		return ""
	}
	// standIn holds, for a commit metric judged in a working set's place, the growth the
	// working set's own limit allowed. Private bytes sit above the private working set,
	// so a relative limit on their median would allow more; the commit metric is held
	// to the working set's allowance, never looser than before the change.
	standIn := map[string]float64{}
	for _, m := range metrics {
		pts := series[m]
		raw, isAdj := rawOf[m]
		if len(pts) == 0 {
			switch {
			case isAdj && len(series[raw]) > 0:
				_, _ = fmt.Fprintf(out, "%-17s skipped: no heap_released samples (no diag endpoint), %s stands in\n", m, raw)
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
		if a, has := standIn[m]; has {
			lim = Limit{Abs: a}
		}
		v := Analyze(m, pts, Options{Warmup: c.warmup, MinWindow: c.minWindow, Bucket: c.bucket, MinBuckets: minBuckets, Limit: lim})
		v.NotJudged = judgedInstead(m)
		if j := v.NotJudged; j != "" && commitOf[m] != "" && v.Allowed > 0 {
			if prev, has := standIn[j]; !has || v.Allowed < prev {
				standIn[j] = v.Allowed
			}
		}
		_, _ = fmt.Fprintln(out, v)
		if _, ws := commitOf[m]; ws {
			if note := trimNote(series[trimsSeries], c.warmup); note != "" {
				_, _ = fmt.Fprintf(out, "%-17s %s\n", "", note)
			}
		}
		if v.NotJudged == "" {
			ok = ok && v.Pass
		}
	}
	return ok
}

// trimNote says when the working-set guard trimmed the engine's working set after the
// warm-up, from the cumulative trim count; "" when it did not, or was not sampled. A
// trim, and an outside read that often comes with it, moves the working set in a step
// that is not a leak.
func trimNote(trims []Point, warmup time.Duration) string {
	var at []string
	prev, seen := 0.0, false
	for _, p := range trims {
		if seen && p.V > prev && p.T >= warmup.Seconds() {
			at = append(at, (time.Duration(p.T) * time.Second).String())
		}
		prev, seen = p.V, true
	}
	if len(at) == 0 {
		return ""
	}
	const maxListed = 10
	more := ""
	if len(at) > maxListed {
		more = fmt.Sprintf(" and %d more", len(at)-maxListed)
		at = at[:maxListed]
	}
	return fmt.Sprintf("note: the working-set guard trimmed after the warm-up at %s%s; a step in the working set there is residency, not a leak",
		strings.Join(at, ", "), more)
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
