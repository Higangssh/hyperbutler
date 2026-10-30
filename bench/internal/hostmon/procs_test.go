package hostmon

import (
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

func TestMain(m *testing.M) {
	leaktest.VerifyTestMain(m, leaktest.IgnoreWinioIOCP)
}

func TestFindNewVM(t *testing.T) {
	other := []Proc{{PID: 10, PPID: 4, Name: "vmcompute.exe"}, {PID: 20, PPID: 10, Name: "vmwp.exe"}, {PID: 21, PPID: 20, Name: "vmmem"}}
	ours := append(slices.Clone(other), Proc{PID: 30, PPID: 10, Name: "VMWP.EXE"}, Proc{PID: 31, PPID: 30, Name: "vmmem"})
	got, err := FindNewVM(other, ours)
	if err != nil || got != (VMProcs{VMWP: 30, VMMem: 31}) {
		t.Fatalf("got %+v, %v", got, err)
	}

	for name, tc := range map[string]struct {
		before, after []Proc
		want          string
	}{
		"nothing new":             {other, other, "no new vmmem"},
		"vmmem under an old vmwp": {other, append(slices.Clone(other), Proc{PID: 31, PPID: 20, Name: "vmmem"}), "no new vmmem"},
		"new vmwp, no vmmem yet":  {other, append(slices.Clone(other), Proc{PID: 30, PPID: 10, Name: "vmwp.exe"}), "1 new vmwp"},
		"two VMs at once": {other, append(slices.Clone(ours),
			Proc{PID: 40, PPID: 10, Name: "vmwp.exe"}, Proc{PID: 41, PPID: 40, Name: "vmmem"}), "2 VMs"},
		"vmmem that is not a vmmem": {other, append(slices.Clone(other), Proc{PID: 30, PPID: 10, Name: "vmwp.exe"}, Proc{PID: 31, PPID: 30, Name: "vmmem.exe.evil"}), "no new vmmem"},
	} {
		if v, err := FindNewVM(tc.before, tc.after); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %+v, %v; want error %q", name, v, err, tc.want)
		}
	}
}

func TestFindForeign(t *testing.T) {
	ps := []Proc{
		{PID: 5, Name: "hyperbutler.exe"}, {PID: 6, Name: "HYPERBUTLER.EXE"}, {PID: 7, Name: "hyperbutler-dev.exe"},
		{PID: 20, Name: "vmwp.exe"}, {PID: 21, Name: "vmmem"}, {PID: 30, Name: "vmwp.exe"}, {PID: 31, Name: "vmmem"},
		{PID: 99, Name: "explorer.exe"},
	}
	f := FindForeign(ps, map[uint32]bool{5: true, 30: true, 31: true})
	if !slices.Equal(f.VMs, []uint32{21}) || !slices.Equal(f.Workers, []uint32{20}) || !slices.Equal(f.Engines, []uint32{6, 7}) || !f.Any() {
		t.Fatalf("%+v", f)
	}
	if f := FindForeign(ps[7:], nil); f.Any() || f.String() != "none" {
		t.Fatalf("explorer counted: %+v", f)
	}
}

func TestChildrenOf(t *testing.T) {
	ps := []Proc{{PID: 5, PPID: 1}, {PID: 6, PPID: 5}, {PID: 7, PPID: 5}, {PID: 8, PPID: 6}}
	if got := ChildrenOf(ps, 5); !slices.Equal(got, []uint32{6, 7}) {
		t.Fatal(got)
	}
	// A cycle (PID reuse can make one) must not loop; the system idle process is its
	// own parent.
	ps = append(ps, Proc{PID: 1, PPID: 8}, Proc{PID: 0, PPID: 0})
	d := Descendants(ps, 5)
	if len(d) != 5 || !d[8] || !d[1] || d[0] {
		t.Fatal(d)
	}
}

func TestStats(t *testing.T) {
	xs := []float64{5, 1, 4, 2, 3}
	if Median(xs) != 3 || Median([]float64{4, 1, 3, 2}) != 2.5 || !math.IsNaN(Median(nil)) {
		t.Fatal("median")
	}
	if !slices.Equal(xs, []float64{5, 1, 4, 2, 3}) {
		t.Fatal("input sorted in place")
	}
	// Nearest rank: p95 of 20 values is the 19th smallest.
	var twenty []float64
	for i := 20; i >= 1; i-- {
		twenty = append(twenty, float64(i))
	}
	if q := Quantile(twenty, 0.95); q != 19 {
		t.Fatalf("p95 = %v", q)
	}
	if Quantile(twenty, 1) != 20 || Quantile(twenty, 0.01) != 1 || !math.IsNaN(Quantile(twenty, 0)) || !math.IsNaN(Quantile(nil, 0.5)) || !math.IsNaN(Quantile(twenty, math.NaN())) {
		t.Fatal("quantile bounds")
	}
	if lo, hi := MinMax(xs); lo != 1 || hi != 5 {
		t.Fatal("minmax")
	}
	// 2 units per 10 s is 720 per hour; one outlier does not move Theil–Sen.
	ts := []float64{0, 10, 20, 30, 40, 50}
	vs := []float64{0, 2, 4, 1000, 8, 10}
	if s := SlopePerHour(ts, vs); math.Abs(s-720) > 1e-9 {
		t.Fatalf("slope %v", s)
	}
	if !math.IsNaN(SlopePerHour([]float64{1, 1}, []float64{1, 2})) || !math.IsNaN(SlopePerHour([]float64{1}, []float64{1, 2})) {
		t.Fatal("degenerate slope")
	}
}
