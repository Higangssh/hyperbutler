package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

func TestVerdict(t *testing.T) {
	good := run{Paint: 1500 * time.Millisecond, Procs: 7, WS: 80 << 20, Gone: 300 * time.Millisecond}
	if f := verdict([]run{good, good, good}); len(f) != 0 {
		t.Fatalf("good runs failed: %v", f)
	}
	slowFirst := good
	slowFirst.Paint = 3 * time.Second
	if f := verdict([]run{slowFirst, good, good}); len(f) != 0 {
		t.Fatalf("a slow first (cold) run failed the warm budget: %v", f)
	}
	for name, bad := range map[string]func(*run){
		"ws":      func(r *run) { r.WS = 101 << 20 },
		"procs":   func(r *run) { r.Procs = 9 },
		"left":    func(r *run) { r.LeftAt2s, r.Left = 1, []string{"msedgewebview2.exe"} },
		"sockets": func(r *run) { r.Sockets = 1 },
	} {
		r := good
		bad(&r)
		if f := verdict([]run{good, r}); len(f) != 1 {
			t.Errorf("%s: %v", name, f)
		}
	}
	slow := good
	slow.Paint = 2100 * time.Millisecond
	if f := verdict([]run{good, slow, slow}); len(f) != 1 || !strings.Contains(f[0], "first paint") {
		t.Errorf("slow warm paint: %v", f)
	}
	var b bytes.Buffer
	summary(&b, []run{good, slow})
	if !strings.Contains(b.String(), "budget 100 MiB") {
		t.Errorf("summary: %s", b.String())
	}
}
