package main

import (
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/bench/internal/netbench"
	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

func TestReport(t *testing.T) {
	runs := []runRec{
		{Test: "dl", Side: "host", Result: netbench.Result{Gbit: 10}},
		{Test: "dl", Side: "container", Result: netbench.Result{Gbit: 4}, EngineCPU: 1, EngineWS: 50e6},
		{Test: "dl", Side: "host", Result: netbench.Result{Gbit: 12}},
		{Test: "dl", Side: "container", Result: netbench.Result{Gbit: 6}, EngineCPU: 1.5, EngineWS: 70e6},
		{Test: "rr", Side: "host", Result: netbench.Result{N: 10, P50us: 20, P99us: 40, P999us: 80, MaxUs: 100}},
		{Test: "rr", Side: "container", Result: netbench.Result{N: 10, P50us: 50, P99us: 120, P999us: 300, MaxUs: 900}},
		{Test: "cps", Side: "host", Result: netbench.Result{ConnsPS: 2000}},
		{Test: "cps", Side: "container", Result: netbench.Result{ConnsPS: 1000}},
	}
	out := Report(runs)
	for _, want := range []string{
		"| dl | Gbit/s | 11.0 | 10.0–12.0 | 5.00 | 4.00–6.00 | 45% | 1.25 | 0.25 | 70.0 |",
		"| rr | p99 µs | 40.0 |",
		"300%",
		"| cps | conn/s | 2000 |",
		"| rr | container | 50.0 | 120 | 300 | 900 |",
		"4 runs, 0 corrupt.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

func TestMedian(t *testing.T) {
	for _, c := range []struct {
		in   []float64
		want float64
	}{{[]float64{3, 1, 2}, 2}, {[]float64{4, 1, 3, 2}, 2.5}, {[]float64{7}, 7}} {
		if got := median(c.in); got != c.want {
			t.Errorf("median(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
