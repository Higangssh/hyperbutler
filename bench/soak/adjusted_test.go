package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

// scavenged is how much of the Go heap the scavenger has returned at t, in 256 KiB steps:
// from 7 MiB down by 4 MiB over the hour, as the runtime reuses released pages.
func scavenged(t float64) float64 {
	return 7*mib - math.Floor(4*mib*t/hour/(256<<10))*(256<<10)
}

// runSeries builds a run's series sample by sample, as the soak does: ws is the engine's
// private working set at t, its private bytes are ws plus 50 MiB, and released, when not
// nil, is heap_released at t. Every other metric is flat.
func runSeries(ws, released func(t float64) float64) map[string][]Point {
	s := map[string][]Point{}
	for t := 0.0; t <= hour; t += 10 {
		row := map[string]float64{"ws_private": ws(t), "private_bytes": ws(t) + 50*mib}
		for _, m := range metrics {
			if _, ok := row[m]; !ok && !isAdjusted(m) {
				row[m] = 100
			}
		}
		if released != nil {
			addAdjusted(row, released(t))
		}
		for _, m := range metrics {
			if v, ok := row[m]; ok {
				s[m] = append(s[m], Point{T: t, V: v})
			}
		}
	}
	return s
}

// soakVerdictConfig has the soak's default limits for the engine's memory.
func soakVerdictConfig() verdictConfig {
	c := testVerdictConfig()
	for _, m := range []string{"ws_private", "private_bytes", "ws_private_adj", "private_bytes_adj"} {
		c.limits[m] = Limit{Abs: mib, Rel: 0.05}
	}
	c.minWindow = 40 * time.Minute
	return c
}

func TestAdjustedMetrics(t *testing.T) {
	// The scavenger: ws rises exactly as heap_released falls (+4 MiB over the hour,
	// twice the 2 MiB allowed at 40 MiB).
	reuse := func(t float64) float64 { return 40*mib + (7*mib - scavenged(t)) }
	for name, tc := range map[string]struct {
		ws, released func(float64) float64
		pass         bool
		want         []string
		fail         string // the metric that must fail
	}{
		"scavenger reuse": {ws: reuse, released: scavenged, pass: true, want: []string{
			"reported, not judged (see ws_private_adj); alone: FAIL",
			"reported, not judged (see private_bytes_adj); alone: PASS",
		}},
		"leak, heap_released flat": {ws: reuse, released: func(float64) float64 { return 5 * mib },
			fail: "ws_private_adj"},
		"leak under scavenger reuse": {ws: func(t float64) float64 { return reuse(t) + 4*mib*t/hour }, released: scavenged,
			fail: "ws_private_adj"},
		"no heap_released: raw judged": {ws: reuse, want: []string{
			"ws_private_adj    skipped: no heap_released samples (no diag endpoint), ws_private is judged instead",
		}, fail: "ws_private"},
		"no heap_released, flat": {ws: func(float64) float64 { return 40 * mib }, pass: true},
	} {
		var out strings.Builder
		err := report(&out, soakVerdictConfig(), runSeries(tc.ws, tc.released), 100, 0, "", nil, nil, nil, false)
		if (err == nil) != tc.pass {
			t.Errorf("%s: pass=%v, want %v:\n%s", name, err == nil, tc.pass, out.String())
		}
		for _, w := range tc.want {
			if !strings.Contains(out.String(), w) {
				t.Errorf("%s: output lacks %q:\n%s", name, w, out.String())
			}
		}
		if tc.fail != "" && !strings.HasPrefix(lineOf(out.String(), tc.fail), fmt.Sprintf("%-17s", tc.fail)) {
			t.Errorf("%s: %s did not fail:\n%s", name, tc.fail, out.String())
		}
	}
}

// lineOf returns the verdict line of metric in out if it failed, or "".
func lineOf(out, metric string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, fmt.Sprintf("%-17s ", metric)) && strings.Contains(l, "  FAIL: ") {
			return l
		}
	}
	return ""
}

func TestAddAdjusted(t *testing.T) {
	row := map[string]float64{"ws_private": 40, "handles": 3}
	addAdjusted(row, 2)
	if row["ws_private_adj"] != 42 {
		t.Fatalf("ws_private_adj = %v, want 42", row["ws_private_adj"])
	}
	if _, ok := row["private_bytes_adj"]; ok {
		t.Fatal("private_bytes_adj set without private_bytes")
	}
}

// oldCSV writes s in the column layout of soak CSVs from before the adjusted metrics,
// with heap_released among the extra columns, unless released is nil.
func oldCSV(ws, released func(float64) float64) string {
	var b strings.Builder
	b.WriteString("t_seconds,ws_private,private_bytes,handles,threads,goroutines,heap_inuse,vmmem_ws_private,vmwp_ws_private,heap_alloc,heap_idle,heap_released,foreign\n")
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	for t := 0.0; t <= hour; t += 10 {
		rel := ""
		if released != nil {
			rel = f(released(t))
		}
		fmt.Fprintf(&b, "%.1f,%s,%s,100,100,100,100,100,,1,2,%s,\n", t, f(ws(t)), f(ws(t)+50*mib), rel)
	}
	return b.String()
}

func TestJudgeCSV(t *testing.T) {
	reuse := func(t float64) float64 { return 40*mib + (7*mib - scavenged(t)) }
	c := soakVerdictConfig()
	c.minWindow = 0
	var out strings.Builder
	if err := judgeCSV(&out, strings.NewReader(oldCSV(reuse, scavenged)), c); err != nil {
		t.Fatalf("scavenger reuse failed: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "[vmwp_ws_private]") || !strings.Contains(out.String(), "vmwp_ws_private   skipped") {
		t.Fatalf("empty column not reported as skipped:\n%s", out.String())
	}
	out.Reset()
	if err := judgeCSV(&out, strings.NewReader(oldCSV(reuse, func(float64) float64 { return 5 * mib })), c); err == nil {
		t.Fatalf("leak with heap_released flat passed:\n%s", out.String())
	}
	out.Reset()
	if err := judgeCSV(&out, strings.NewReader(oldCSV(reuse, nil)), c); err == nil ||
		!strings.Contains(out.String(), "ws_private is judged instead") {
		t.Fatalf("CSV without heap_released did not judge the raw metric (err %v):\n%s", err, out.String())
	}

	for name, in := range map[string]string{
		"no header":      "",
		"no t_seconds":   "ws_private\n1\n",
		"bad value":      "t_seconds,ws_private\n0,x\n",
		"bad t_seconds":  "t_seconds,ws_private\n,1\n",
		"non-finite":     "t_seconds,ws_private\n0,NaN\n",
		"bad heap value": "t_seconds,ws_private,heap_released\n0,1,-\n",
	} {
		if _, _, err := readCSV(strings.NewReader(in)); err == nil {
			t.Errorf("%s: read without error", name)
		}
	}
}
