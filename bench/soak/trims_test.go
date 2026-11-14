package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// trimRun is a run whose working set steps up by 14 MiB at 40 min, as in the v0.2.0 24 h
// run after an outside read; its private bytes grow by commit(t). The guard trims once,
// just before the step.
func trimRun(commit func(t float64) float64) map[string][]Point {
	ws := func(t float64) float64 {
		if t >= 40*60 {
			return 54 * mib
		}
		return 40 * mib
	}
	s := runSeries(ws, func(float64) float64 { return 3 * mib })
	pb := func(t float64) float64 { return 90*mib + commit(t) }
	for _, m := range []string{"private_bytes", "private_bytes_adj"} {
		for i := range s[m] {
			s[m][i].V = pb(s[m][i].T)
			if m == "private_bytes_adj" {
				s[m][i].V += 3 * mib
			}
		}
	}
	for t := 0.0; t <= hour; t += 10 {
		n := 0.0
		if t >= 40*60-10 {
			n = 1
		}
		s[trimsSeries] = append(s[trimsSeries], Point{T: t, V: n})
	}
	return s
}

func TestWorkingSetStepJudgedByCommit(t *testing.T) {
	var out strings.Builder
	flat := func(float64) float64 { return 0 }
	if err := report(&out, soakVerdictConfig(), trimRun(flat), 100, 0, "", nil, nil, nil, false); err != nil {
		t.Fatalf("a working-set step with flat commit failed: %v\n%s", err, out.String())
	}
	for _, w := range []string{
		"reported, not judged (see private_bytes_adj); alone: FAIL",
		"note: the working-set guard trimmed after the warm-up at 39m50s",
	} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("output lacks %q:\n%s", w, out.String())
		}
	}

	// The same step with 3 MiB committed over the hour is a leak: 3 MiB is within private
	// bytes' own 5 % (4.6 MiB) but not the working set's allowance (2 MiB).
	out.Reset()
	leak := func(t float64) float64 { return 3 * mib * t / hour }
	if err := report(&out, soakVerdictConfig(), trimRun(leak), 100, 0, "", nil, nil, nil, false); err == nil {
		t.Fatalf("commit growth passed:\n%s", out.String())
	}
	if lineOf(out.String(), "private_bytes_adj") == "" {
		t.Errorf("private_bytes_adj did not fail:\n%s", out.String())
	}
}

func TestTrimNote(t *testing.T) {
	pts := func(vs ...float64) []Point {
		var p []Point
		for i, v := range vs {
			p = append(p, Point{T: float64(i) * 600, V: v})
		}
		return p
	}
	for name, tc := range map[string]struct {
		trims []Point
		want  string
	}{
		"none":            {pts(0, 0, 0), ""},
		"not sampled":     {nil, ""},
		"during warm-up":  {pts(0, 1, 1, 1), ""},
		"after warm-up":   {pts(0, 0, 0, 1, 1, 2), "at 30m0s, 50m0s;"},
		"starts non-zero": {pts(5, 5, 5), ""},
	} {
		got := trimNote(tc.trims, 15*time.Minute)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: trimNote = %q, want %q", name, got, tc.want)
		}
	}
	var many []float64
	for i := range 15 {
		many = append(many, float64(i))
	}
	if got := trimNote(pts(many...), 0); !strings.Contains(got, "and 4 more") {
		t.Errorf("15 trims: %q", got)
	}
}

func TestReadCSVTrims(t *testing.T) {
	var b strings.Builder
	b.WriteString("t_seconds,ws_private,private_bytes,heap_released,ws_trims\n")
	for i := range 4 {
		fmt.Fprintf(&b, "%d,1,2,0,%d\n", i*10, i/2)
	}
	s, _, err := readCSV(strings.NewReader(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if got := s[trimsSeries]; len(got) != 4 || got[3].V != 1 {
		t.Fatalf("ws_trims = %v", got)
	}
	if _, _, err := readCSV(strings.NewReader("t_seconds,ws_trims\n0,x\n")); err == nil {
		t.Fatal("bad ws_trims read without error")
	}
}
