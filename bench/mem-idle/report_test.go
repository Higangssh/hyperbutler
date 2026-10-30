package main

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/diag"
	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

func TestMain(m *testing.M) {
	leaktest.VerifyTestMain(m)
}

func TestSchedule(t *testing.T) {
	s := Schedule(5*time.Minute, 10*time.Minute, 10*time.Second)
	if len(s) != 31 || s[0] != 5*time.Minute || s[30] != 10*time.Minute {
		t.Fatalf("REQ-MEM-001 schedule: %d samples, %v..%v", len(s), s[0], s[len(s)-1])
	}
	for _, bad := range [][3]time.Duration{{0, 0, 0}, {time.Minute, 0, time.Second}, {-time.Second, time.Second, time.Second}} {
		if Schedule(bad[0], bad[1], bad[2]) != nil {
			t.Errorf("Schedule%v accepted", bad)
		}
	}
}

const mb = 1 << 20

func sample(t float64, engine, vmmem uint64, alloc uint64) Sample {
	return Sample{T: t, EngineWS: engine, VMMemWS: vmmem, VMWPWS: 4 * mb, Stats: diag.Stats{Goroutines: 30, Handles: 200,
		MemStats: diag.MemStats{HeapAlloc: alloc, HeapInuse: alloc + mb, HeapIdle: 6 * mb, HeapReleased: 2 * mb, StackInuse: mb, Sys: 30 * mb, NumGC: uint32(t / 120)}}}
}

func TestSummarize(t *testing.T) {
	var ss []Sample
	for i := range 31 {
		ss = append(ss, sample(300+float64(i)*10, 16*mb, 300*mb, 5*mb))
	}
	s := Summarize(ss, 400e6)
	if !s.Pass || s.N != 31 || s.Total.Median != float64(320*mb) || s.HeapRetained.Median != float64(5*mb) || s.NumGC != 3 {
		t.Fatalf("%+v", s)
	}
	if s.EngineWSSlope != 0 || s.HeapAllocSlope != 0 {
		t.Fatalf("flat series has slope %v %v", s.EngineWSSlope, s.HeapAllocSlope)
	}
	var b bytes.Buffer
	s.Print(&b)
	if !strings.Contains(b.String(), "PASS") || !strings.Contains(b.String(), "| **total** | 320.0 MiB") {
		t.Fatal(b.String())
	}

	// Over the target: fails. The verdict is on the median, so a few high samples don't.
	hi := append([]Sample(nil), ss...)
	for i := range hi {
		hi[i].VMMemWS = 400 * mb
	}
	if Summarize(hi, 400e6).Pass {
		t.Fatal("over target passed")
	}
	spiky := append([]Sample(nil), ss...)
	for i := range 10 {
		spiky[i].VMMemWS = 900 * mb
	}
	if !Summarize(spiky, 400e6).Pass {
		t.Fatal("a minority of spikes failed the median")
	}

	// One sample next to another VM invalidates the run.
	ov := append([]Sample(nil), ss...)
	ov[7].Foreign = "vmmem [123]"
	so := Summarize(ov, 400e6)
	if so.Pass || so.Overlapped != 1 {
		t.Fatalf("overlap: %+v", so)
	}
	b.Reset()
	so.Print(&b)
	if !strings.Contains(b.String(), "NOT VALID") {
		t.Fatal(b.String())
	}

	if Summarize(nil, 400e6).Pass {
		t.Fatal("no samples passed")
	}
	// Inconsistent MemStats (released > idle, alloc > inuse) must not underflow.
	odd := sample(300, mb, mb, 9*mb)
	odd.Stats.MemStats.HeapReleased, odd.Stats.MemStats.HeapIdle = 9*mb, mb
	odd.Stats.MemStats.HeapInuse = mb
	if r := Summarize([]Sample{odd}, 400e6).HeapRetained.Median; r != 0 {
		t.Fatalf("retained %v", r)
	}
}

func TestWriteCSV(t *testing.T) {
	ss := []Sample{sample(300, 16*mb, 300*mb, 5*mb), sample(310, 17*mb, 301*mb, 6*mb)}
	ss[1].Foreign = "vmmem [1], vmwp [], engines []"
	var b bytes.Buffer
	if err := WriteCSV(&b, ss); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&b).ReadAll()
	if err != nil || len(rows) != 3 || len(rows[0]) != len(csvHeader) || rows[2][len(csvHeader)-1] != ss[1].Foreign || rows[1][5] != "335544320" {
		t.Fatalf("%v %v", rows, err)
	}
}
