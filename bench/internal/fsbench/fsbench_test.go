package fsbench

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

func TestParse(t *testing.T) {
	rs, err := Parse([]byte("noise\n{\"name\":\"create\",\"n\":10,\"s\":0.5}\n\n{\"name\":\"stat\",\"s\":0.25}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if rs["create"].Num("s") != 0.5 || rs["stat"].Num("s") != 0.25 || !math.IsNaN(rs["stat"].Num("n")) {
		t.Fatalf("%v", rs)
	}
	if _, err := Parse([]byte("nothing")); err == nil {
		t.Fatal("no results accepted")
	}
	if _, err := Parse([]byte("{bad")); err == nil {
		t.Fatal("bad JSON accepted")
	}
}

func TestVerdicts(t *testing.T) {
	for _, c := range []struct {
		r, v string
	}{
		{tup(Slower(2, 1, 3)), "2.00× pass"},
		{tup(Slower(4, 1, 3)), "4.00× FAIL"},
		{tup(Slower(4, 1, 0)), "4.00× info"},
		{tup(Slower(1, math.NaN(), 3)), "n/a not measured"},
		{tup(Faster(1, 3, 3)), "3.00× pass"},
		{tup(Faster(1, 2, 3)), "2.00× FAIL"},
	} {
		if c.r != c.v {
			t.Errorf("got %q, want %q", c.r, c.v)
		}
	}
	if _, v := Faster(1, math.NaN(), 3); !strings.HasPrefix(v, "not measured") {
		t.Errorf("missing baseline: %q", v)
	}
	if !Report(&strings.Builder{}, "x", []Row{{Verdict: "pass"}, {Verdict: "not measured"}}) {
		t.Error("report failed without a FAIL")
	}
	if Report(&strings.Builder{}, "x", []Row{{Verdict: "FAIL"}}) {
		t.Error("report passed a FAIL")
	}
}

func tup(a, b string) string { return a + " " + b }

func TestWSLPath(t *testing.T) {
	if p, err := wslPath(`C:\Users\x\fsbench`); err != nil || p != "/mnt/c/Users/x/fsbench" {
		t.Fatalf("%q %v", p, err)
	}
	if q := Quantile([]float64{3, 1, 2}, 0.5); q != 2 {
		t.Fatalf("median %v", q)
	}
}

func TestVerdict(t *testing.T) {
	if err := Verdict([]Row{{Req: "R", Verdict: "pass"}, {Verdict: "info"}}); err != nil {
		t.Fatal(err)
	}
	if err := Verdict([]Row{{Req: "R", Verdict: "pass"}, {Req: "R", Verdict: "not measured"}}); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("unmeasured requirement: %v", err)
	}
	if err := Verdict([]Row{{Req: "R", Verdict: "FAIL"}, {Req: "R", Verdict: "not measured"}}); err == nil || errors.Is(err, ErrIncomplete) {
		t.Fatalf("failed requirement: %v", err)
	}
}
