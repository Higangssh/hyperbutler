package main

import (
	"fmt"
	"io"
	"time"

	"github.com/Higangssh/hyperbutler/bench/internal/hostmon"
)

// Run is one `hyperbutler start` → `docker info` measurement.
type Run struct {
	Seconds   float64 // from invoking `hyperbutler start` until `docker info` succeeded
	ReadyMs   int64   // the engine's own state.json readyMillis (VM create → dockerd answered)
	StopSecs  float64 // `hyperbutler stop`
	Foreign   string  // other VMs or engines seen during the run ("" when none)
	Warmup    bool    // discarded warm-up run
	FirstBoot bool    // the first start this tool saw since the host booted
	Err       string
}

// Valid reports whether the run counts towards the result.
func (r Run) Valid() bool { return !r.Warmup && r.Foreign == "" && r.Err == "" }

// Limits are REQ-START-001's targets.
type Limits struct {
	Median, P95 time.Duration
}

// Result is the summary over the valid runs.
type Result struct {
	N                     int
	Median, P95, Min, Max float64
	StopMedian            float64
	Discarded, Overlapped int
	Failed                int
	Pass                  bool
	Limits                Limits
}

// Summarize computes median and nearest-rank p95 over the valid runs and judges them
// against lim. It needs at least want valid runs to pass.
func Summarize(runs []Run, want int, lim Limits) Result {
	var xs, stops []float64
	r := Result{Limits: lim}
	for _, x := range runs {
		switch {
		case x.Err != "":
			r.Failed++
		case x.Warmup:
			r.Discarded++
		case x.Foreign != "":
			r.Overlapped++
		default:
			xs = append(xs, x.Seconds)
			stops = append(stops, x.StopSecs)
		}
	}
	r.N = len(xs)
	r.Median = hostmon.Median(xs)
	r.P95 = hostmon.Quantile(xs, 0.95)
	r.Min, r.Max = hostmon.MinMax(xs)
	r.StopMedian = hostmon.Median(stops)
	r.Pass = r.N >= want && r.Failed == 0 && r.Median <= lim.Median.Seconds() && r.P95 <= lim.P95.Seconds()
	return r
}

// Print writes the per-run table and the summary.
func Print(w io.Writer, runs []Run, r Result) {
	_, _ = fmt.Fprintf(w, "\n| # | start → docker info | engine ready | stop | note |\n|---|---|---|---|---|\n")
	for i, x := range runs {
		note := ""
		switch {
		case x.Err != "":
			note = "failed: " + x.Err
		case x.Warmup:
			note = "warm-up, discarded"
		case x.Foreign != "":
			note = "overlapped (" + x.Foreign + "), excluded"
		}
		if x.FirstBoot {
			note += " first start since host boot"
		}
		_, _ = fmt.Fprintf(w, "| %d | %.2f s | %d ms | %.2f s | %s |\n", i+1, x.Seconds, x.ReadyMs, x.StopSecs, note)
	}
	verdict := "PASS"
	if !r.Pass {
		verdict = "FAIL"
	}
	_, _ = fmt.Fprintf(w, "\nREQ-START-001: n = %d valid runs (%d warm-up discarded, %d overlapped, %d failed): median %.2f s, p95 %.2f s (min %.2f, max %.2f); stop median %.2f s. Limits: median ≤ %s, p95 ≤ %s: %s\n",
		r.N, r.Discarded, r.Overlapped, r.Failed, r.Median, r.P95, r.Min, r.Max, r.StopMedian, r.Limits.Median, r.Limits.P95, verdict)
}
