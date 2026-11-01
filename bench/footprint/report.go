package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Budgets of the GUI (ADR 0008).
const (
	budgetWSBytes    = 100 << 20
	budgetProcs      = 8
	budgetFirstPaint = 2 * time.Second
	budgetGone       = 2 * time.Second
)

// run is one launch of the window.
type run struct {
	Paint      time.Duration // launch to the "painted" line
	SelfMs     int64         // the window's own figure, from its process creation
	Procs      int
	WS         uint64 // private working set, all processes, bytes
	Commit     uint64 // private bytes, all processes
	Names      map[string]int
	Sockets    int // TCP or UDP sockets the window's processes hold
	SocketList []string
	Gone       time.Duration // close to the last process gone
	LeftAt2s   int           // processes left 2 s after the close
	Left       []string
	PerProc    []string // each process's figures (-detail)
	// Probe is the WebRTC probe's report and ProbeSockets the sockets seen while it ran
	// (-check-webrtc).
	Probe        string
	ProbeSockets []string
}

// verdict judges runs against the budgets and returns the failures.
func verdict(runs []run) []string {
	var fail []string
	if len(runs) == 0 {
		return []string{"no runs"}
	}
	for i, r := range runs {
		if r.WS > budgetWSBytes {
			fail = append(fail, fmt.Sprintf("run %d: %.1f MiB private working set is over %d MiB", i+1, mib(r.WS), budgetWSBytes>>20))
		}
		if r.Procs > budgetProcs {
			fail = append(fail, fmt.Sprintf("run %d: %d processes is over %d", i+1, r.Procs, budgetProcs))
		}
		if r.LeftAt2s > 0 {
			fail = append(fail, fmt.Sprintf("run %d: %d processes left 2 s after the close (%s)", i+1, r.LeftAt2s, strings.Join(r.Left, ", ")))
		}
		if r.Sockets > 0 {
			fail = append(fail, fmt.Sprintf("run %d: the window's processes hold %d network sockets", i+1, r.Sockets))
		}
	}
	warm := runs
	if len(runs) > 1 {
		warm = runs[1:] // the first run warms the file cache and the profile
	}
	if p := median(durations(warm, func(r run) time.Duration { return r.Paint })); p > budgetFirstPaint {
		fail = append(fail, fmt.Sprintf("warm first paint %v (median) is over %v", p.Round(time.Millisecond), budgetFirstPaint))
	}
	return fail
}

func durations(runs []run, f func(run) time.Duration) []time.Duration {
	out := make([]time.Duration, len(runs))
	for i, r := range runs {
		out[i] = f(r)
	}
	return out
}

func median[T int | int64 | uint64 | time.Duration | float64](xs []T) T {
	if len(xs) == 0 {
		return 0
	}
	s := append([]T(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func maxOf[T int | int64 | uint64 | time.Duration](xs []T) T {
	var m T
	for _, x := range xs {
		m = max(m, x)
	}
	return m
}

func mib(b uint64) float64 { return float64(b) / (1 << 20) }

func names(m map[string]int) string {
	var s []string
	for k, v := range m {
		s = append(s, fmt.Sprintf("%s x%d", k, v))
	}
	sort.Strings(s)
	return strings.Join(s, ", ")
}

// summary prints the medians and maxima over runs.
func summary(w io.Writer, runs []run) {
	ws := make([]uint64, len(runs))
	commit := make([]uint64, len(runs))
	procs := make([]int, len(runs))
	gone := make([]time.Duration, len(runs))
	left := 0
	for i, r := range runs {
		ws[i], commit[i], procs[i], gone[i] = r.WS, r.Commit, r.Procs, r.Gone
		if r.LeftAt2s > 0 {
			left++
		}
	}
	warm := runs
	if len(runs) > 1 {
		warm = runs[1:]
	}
	paint := durations(warm, func(r run) time.Duration { return r.Paint })
	_, _ = fmt.Fprintf(w, "runs %d\n", len(runs))
	_, _ = fmt.Fprintf(w, "private working set  median %.1f MiB  max %.1f MiB  (budget %d MiB)\n", mib(median(ws)), mib(maxOf(ws)), budgetWSBytes>>20)
	_, _ = fmt.Fprintf(w, "private bytes        median %.1f MiB  max %.1f MiB\n", mib(median(commit)), mib(maxOf(commit)))
	_, _ = fmt.Fprintf(w, "processes            median %d  max %d  (budget %d)\n", median(procs), maxOf(procs), budgetProcs)
	_, _ = fmt.Fprintf(w, "first paint, warm    median %d ms  max %d ms  (budget %d ms; first run %d ms)\n",
		median(paint).Milliseconds(), maxOf(paint).Milliseconds(), budgetFirstPaint.Milliseconds(), runs[0].Paint.Milliseconds())
	_, _ = fmt.Fprintf(w, "close to last gone   median %d ms  max %d ms\n", median(gone).Milliseconds(), maxOf(gone).Milliseconds())
	_, _ = fmt.Fprintf(w, "left 2 s after close %d of %d closes  (budget 0)\n", left, len(runs))
}
