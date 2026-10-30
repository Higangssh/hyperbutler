package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

func TestMain(m *testing.M) {
	leaktest.VerifyTestMain(m)
}

var lim01 = Limits{Median: 8 * time.Second, P95: 12 * time.Second}

func runsOf(secs ...float64) []Run {
	var rs []Run
	for _, s := range secs {
		rs = append(rs, Run{Seconds: s, StopSecs: 1})
	}
	return rs
}

func TestSummarize(t *testing.T) {
	// 1 warm-up + 20 valid runs 2.0 .. 3.9 s.
	rs := []Run{{Seconds: 30, Warmup: true}}
	for i := range 20 {
		rs = append(rs, Run{Seconds: 2 + float64(i)/10, StopSecs: 1.3})
	}
	r := Summarize(rs, 20, lim01)
	if !r.Pass || r.N != 20 || r.Discarded != 1 || r.P95 != 3.8 || r.Median < 2.949 || r.Median > 2.951 || r.Max != 3.9 || r.StopMedian != 1.3 {
		t.Fatalf("%+v", r)
	}
	var b bytes.Buffer
	Print(&b, rs, r)
	if !strings.Contains(b.String(), "warm-up, discarded") || !strings.Contains(b.String(), "PASS") {
		t.Fatal(b.String())
	}

	// Overlapped runs are excluded, so they neither help nor hurt, but count against n.
	ov := append(runsOf(2, 2, 2), Run{Seconds: 60, Foreign: "vmmem 1"})
	if r := Summarize(ov, 3, lim01); !r.Pass || r.Overlapped != 1 || r.Max != 2 {
		t.Fatalf("overlap: %+v", r)
	}
	if r := Summarize(ov, 4, lim01); r.Pass {
		t.Fatal("too few valid runs passed")
	}
	// The median limit, the p95 limit, and a failed run each fail it.
	if Summarize(runsOf(9, 9, 9), 3, lim01).Pass {
		t.Fatal("slow median passed")
	}
	tail := runsOf(2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 13, 13)
	if r := Summarize(tail, 20, lim01); r.Pass || r.P95 != 13 {
		t.Fatalf("slow p95 passed: %+v", r)
	}
	failed := append(runsOf(2, 2, 2), Run{Err: "boom"})
	if r := Summarize(failed, 3, lim01); r.Pass || r.Failed != 1 {
		t.Fatalf("failed run passed: %+v", r)
	}
	if Summarize(nil, 1, lim01).Pass {
		t.Fatal("nothing passed")
	}
}

func TestRunValid(t *testing.T) {
	for _, r := range []Run{{Warmup: true}, {Foreign: "x"}, {Err: "x"}} {
		if r.Valid() {
			t.Errorf("%+v valid", r)
		}
	}
	if !(Run{Seconds: 1, FirstBoot: true}).Valid() {
		t.Error("a first-boot run is valid")
	}
}
