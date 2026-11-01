package main

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/Higangssh/hyperbutler/bench/internal/netbench"
)

// runRec is one measurement on one side.
type runRec struct {
	Test       string          `json:"test"`
	Side       string          `json:"side"` // host or container
	Round      int             `json:"round"`
	Result     netbench.Result `json:"result"`
	EngineCPU  float64         `json:"engine_cores,omitempty"` // engine CPU time over wall time
	EngineWS   uint64          `json:"engine_private_ws,omitempty"`
	EnginePriv uint64          `json:"engine_private_bytes,omitempty"`
	EngineHeap uint64          `json:"engine_heap_inuse,omitempty"`
	GCs        uint32          `json:"gcs,omitempty"`
	GCPauseMs  float64         `json:"gc_pause_ms,omitempty"`
}

// side collects one side of one test.
type side struct {
	vals  []float64 // the test's main metric, one per run
	cores []float64
	ws    []float64 // MB
	priv  []float64 // MB
	heap  []float64 // MB
	gcs   []float64
}

// metric returns a run's main number and its unit. Latency reports p99 here; the
// percentiles are printed in their own table.
func metric(test string, r netbench.Result) (float64, string) {
	switch {
	case strings.HasSuffix(test, "cps"):
		return r.ConnsPS, "conn/s"
	case strings.Contains(test, "rr"):
		return r.P99us, "p99 µs"
	}
	return r.Gbit, "Gbit/s"
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func span(v []float64) string {
	if len(v) == 0 {
		return "-"
	}
	lo, hi := v[0], v[0]
	for _, x := range v {
		lo, hi = math.Min(lo, x), math.Max(hi, x)
	}
	return fmt.Sprintf("%s–%s", num(lo), num(hi))
}

func num(x float64) string {
	switch {
	case math.IsNaN(x):
		return "-"
	case x >= 100:
		return fmt.Sprintf("%.0f", x)
	case x >= 10:
		return fmt.Sprintf("%.1f", x)
	}
	return fmt.Sprintf("%.2f", x)
}

// Report renders the runs as Markdown: one row per test with host and container
// medians (and ranges), their ratio, and the engine's CPU and memory during the
// container runs.
func Report(runs []runRec) string {
	var order []string
	by := map[string]map[string]*side{}
	units := map[string]string{}
	lat := map[string]map[string][][4]float64{}
	for _, r := range runs {
		if by[r.Test] == nil {
			by[r.Test] = map[string]*side{"host": {}, "container": {}}
			lat[r.Test] = map[string][][4]float64{}
			order = append(order, r.Test)
		}
		s := by[r.Test][r.Side]
		if s == nil {
			continue
		}
		v, u := metric(r.Test, r.Result)
		units[r.Test] = u
		s.vals = append(s.vals, v)
		if r.Side == "container" {
			s.cores = append(s.cores, r.EngineCPU)
			if r.EngineWS > 0 {
				s.ws = append(s.ws, float64(r.EngineWS)/1e6)
			}
			if r.EngineHeap > 0 {
				s.heap = append(s.heap, float64(r.EngineHeap)/1e6)
			}
			if r.EnginePriv > 0 {
				s.priv = append(s.priv, float64(r.EnginePriv)/1e6)
			}
			s.gcs = append(s.gcs, float64(r.GCs))
		}
		if r.Result.N > 0 && r.Result.P50us > 0 {
			lat[r.Test][r.Side] = append(lat[r.Test][r.Side], [4]float64{r.Result.P50us, r.Result.P99us, r.Result.P999us, r.Result.MaxUs})
		}
	}
	var b strings.Builder
	b.WriteString("| Test | Unit | Host (raw) median | Host range | Container median | Container range | Container / host | Engine cores | Engine cores per Gbit/s | Engine private WS, max MB | Engine private bytes, max MB | Engine Go heap in use, max MB | GCs per run |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, t := range order {
		h, c := by[t]["host"], by[t]["container"]
		hm, cm := median(h.vals), median(c.vals)
		ratio := cm / hm
		perG := "-"
		if units[t] == "Gbit/s" && cm > 0 {
			perG = num(median(c.cores) / cm)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %.0f%% | %s | %s | %s | %s | %s | %s |\n", t, units[t],
			num(hm), span(h.vals), num(cm), span(c.vals), ratio*100,
			num(median(c.cores)), perG, num(maxOf(c.ws)), num(maxOf(c.priv)), num(maxOf(c.heap)), num(median(c.gcs)))
	}
	var bulk, corrupt int64
	for _, r := range runs {
		if r.Result.Gbit > 0 && r.Test != "url" {
			bulk++
			corrupt += r.Result.Corrupt
		}
	}
	if bulk > 0 {
		fmt.Fprintf(&b, "\nBulk streams verified (download: one %d KiB block in %d; upload: CRC-32C of every byte): %d runs, %d corrupt.\n",
			netbench.BlockSize>>10, netbench.VerifyEvery, bulk, corrupt)
	}
	var hasLat bool
	for _, t := range order {
		if len(lat[t]["container"]) > 0 {
			hasLat = true
		}
	}
	if hasLat {
		b.WriteString("\n| Latency test | Side | p50 µs | p99 µs | p99.9 µs | max µs (medians of runs) |\n|---|---|---|---|---|---|\n")
		for _, t := range order {
			for _, sd := range []string{"host", "container"} {
				ls := lat[t][sd]
				if len(ls) == 0 {
					continue
				}
				var cols [4][]float64
				for _, l := range ls {
					for i := range l {
						cols[i] = append(cols[i], l[i])
					}
				}
				fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", t, sd, num(median(cols[0])), num(median(cols[1])), num(median(cols[2])), num(median(cols[3])))
			}
		}
	}
	return b.String()
}

func maxOf(v []float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	m := v[0]
	for _, x := range v {
		m = math.Max(m, x)
	}
	return m
}
