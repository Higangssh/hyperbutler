package main

import (
	"bytes"
	"encoding/csv"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

func TestMain(m *testing.M) {
	leaktest.VerifyTestMain(m)
}

const mib1 = 1 << 20

// fakeRun builds a run: baseline at base, a workload peak, then the after-phase totals at
// 10 s steps from +0 s.
func fakeRun(base, peak uint64, after ...uint64) []Sample {
	var ss []Sample
	for i := 0; i < 5; i++ {
		ss = append(ss, Sample{T: float64(-100 + i*2), Phase: PhaseBaseline, VMMemWS: base})
	}
	ss = append(ss, Sample{T: -10, Phase: PhaseWorkload, VMMemWS: peak})
	for i, v := range after {
		ss = append(ss, Sample{T: float64(i * 10), Phase: PhaseAfter, VMMemWS: v})
	}
	return ss
}

var crit = Criteria{Limit: 500e6, Within: 60 * time.Second, Workload: 8 << 30}

func TestSummarizePass(t *testing.T) {
	s := Summarize(fakeRun(300*mib1, 8500*mib1, 8000*mib1, 4000*mib1, 400*mib1, 410*mib1, 405*mib1, 400*mib1, 400*mib1, 400*mib1), crit)
	if !s.Pass || s.Reached != 20 || s.AtWithin != 400*mib1 || s.AtWithinT != 60 || s.End != 400*mib1 || !s.CoversWindow || !s.WorkloadSeen {
		t.Fatalf("%+v", s)
	}
	if s.Baseline.Median != 300*mib1 || s.Peak != 8500*mib1 || s.Growth != 8200*mib1 || s.EndOverBaseline != 100*mib1 {
		t.Fatalf("%+v", s)
	}
	if s.VMBase != 300*mib1 || s.VMEnd != 400*mib1 || s.EngineBase != 0 || s.EngineEnd != 0 {
		t.Fatalf("split: %+v", s)
	}
	var b bytes.Buffer
	s.Print(&b)
	if !strings.Contains(b.String(), "REQ-MEM-002: PASS") || !strings.Contains(b.String(), "+20 s") {
		t.Fatal(b.String())
	}
}

func TestSummarizeFails(t *testing.T) {
	for name, tc := range map[string]struct {
		ss   []Sample
		want string
	}{
		"never low": {fakeRun(300*mib1, 8500*mib1, 8000*mib1, 4000*mib1, 4000*mib1, 4000*mib1, 4000*mib1, 4000*mib1, 4000*mib1), "FAIL"},
		// Low briefly inside the window, then up again: not "returned".
		"dips then rises": {fakeRun(300*mib1, 8500*mib1, 400*mib1, 400*mib1, 4000*mib1, 4000*mib1, 4000*mib1, 4000*mib1, 4000*mib1), "FAIL"},
		// Under the limit at +60 s, above it once later: "returns to" means "and stays".
		"rises late":   {fakeRun(300*mib1, 8500*mib1, 4000*mib1, 400*mib1, 400*mib1, 400*mib1, 400*mib1, 400*mib1, 400*mib1, 600*mib1, 400*mib1), "FAIL"},
		"too late":     {fakeRun(300*mib1, 8500*mib1, 8000*mib1, 8000*mib1, 8000*mib1, 8000*mib1, 8000*mib1, 8000*mib1, 8000*mib1, 400*mib1), "FAIL"},
		"no growth":    {fakeRun(300*mib1, 400*mib1, 400*mib1, 400*mib1, 400*mib1, 400*mib1, 400*mib1, 400*mib1, 400*mib1), "NOT VALID: host memory grew"},
		"short window": {fakeRun(300*mib1, 8500*mib1, 400*mib1, 400*mib1), "NOT VALID: the samples end"},
		"no after":     {fakeRun(300*mib1, 8500*mib1), "FAIL: no samples"},
	} {
		s := Summarize(tc.ss, crit)
		if s.Pass || !strings.HasPrefix(s.VerdictText, tc.want) {
			t.Errorf("%s: pass=%v %q, want %q", name, s.Pass, s.VerdictText, tc.want)
		}
	}
	ss := fakeRun(300*mib1, 8500*mib1, 400*mib1, 400*mib1, 400*mib1, 400*mib1, 400*mib1, 400*mib1, 400*mib1)
	ss[3].Foreign = "vmmem 42"
	if s := Summarize(ss, crit); s.Pass || s.Overlapped != 1 || !strings.HasPrefix(s.VerdictText, "NOT VALID: another VM") {
		t.Errorf("overlap: %+v", s)
	}
	if s := Summarize(nil, crit); s.Pass || !math.IsNaN(s.Reached) {
		t.Errorf("empty: %+v", s)
	}
}

func TestWriteCSV(t *testing.T) {
	ss := fakeRun(300*mib1, 8500*mib1, 400*mib1)
	ss[0].Foreign = "vmmem 1, vmwp.exe 2"
	var b bytes.Buffer
	if err := WriteCSV(&b, ss); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&b).ReadAll()
	if err != nil || len(rows) != len(ss)+1 || len(rows[0]) != len(csvHeader) || rows[1][8] != "vmmem 1, vmwp.exe 2" || rows[len(rows)-1][1] != "after" {
		t.Fatalf("%v %v", rows, err)
	}
}
