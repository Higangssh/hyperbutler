package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"

	"github.com/Higangssh/hyperbutler/bench/internal/hostmon"
)

// Phase is the part of the run a sample belongs to.
type Phase string

const (
	PhaseBaseline Phase = "baseline" // idle, before the workload
	PhaseWorkload Phase = "workload" // the fill container runs
	PhaseAfter    Phase = "after"    // the container has exited; the reclaim window
)

// Sample is one reading of the installation's host memory.
type Sample struct {
	T            float64 // seconds since the fill container exited (negative before)
	Phase        Phase
	EngineWS     uint64 // engine (hyperbutler.exe) private working set, bytes
	VMMemWS      uint64 // this VM's vmmem private working set
	VMWPWS       uint64 // this VM's vmwp private working set
	HelpersWS    uint64 // the engine's child processes
	VMMemPriv    uint64 // this VM's vmmem private bytes (commit), for diagnosis only
	EnginePriv   uint64 // engine private bytes (commit), for diagnosis only
	EngineFaults uint64 // engine page fault count so far (0 with -sampler cim), for diagnosis only
	Foreign      string // other VMs or engines seen since the previous sample ("" when none)
}

// Total is host memory as REQ-MEM-001 counts it, which REQ-MEM-002 reuses.
func (s Sample) Total() uint64 { return s.EngineWS + s.VMMemWS + s.VMWPWS + s.HelpersWS }

// Criteria are the REQ-MEM-002 limits.
type Criteria struct {
	Limit    uint64        // REQ-MEM-001 target + margin, bytes
	Within   time.Duration // after the container exits
	Workload uint64        // bytes the container filled (anonymous + page cache)
}

// Summary is the verdict of one run.
type Summary struct {
	Criteria
	N          int
	Overlapped int

	Baseline Stat   // total, baseline phase
	Peak     uint64 // highest total in the workload phase and after
	Growth   uint64 // Peak ??baseline median

	// Reached is the first time after the exit from which every later sample of the
	// window is at or below Limit (NaN: never). AtWithin is the total of the first
	// sample at or after Within, End the last sample's.
	Reached          float64
	AtWithin, End    uint64
	AtWithinT, EndT  float64
	EndOverBaseline  int64 // End ??baseline median, bytes
	MinAfter         uint64
	MinAfterT        float64
	CoversWindow     bool // the samples run past Within
	WorkloadSeen     bool // the peak grew by at least half of the workload
	Pass             bool
	VerdictText      string
	VMMemPrivAtEnd   uint64
	VMMemPrivAtPeak  uint64
	VMMemPrivBaseMed float64

	// Where the total is, baseline median and last sample: the engine, and the VM
	// (vmmem + vmwp). The verdict uses only the total; these say which side to look at.
	EngineBase, VMBase float64
	EngineEnd, VMEnd   uint64
	EngineCommitEnd    uint64 // engine private bytes at the end: a working set far below it was trimmed or never touched
}

// Stat is a median and range.
type Stat struct{ Median, Min, Max float64 }

func stat(xs []float64) Stat {
	lo, hi := hostmon.MinMax(xs)
	return Stat{Median: hostmon.Median(xs), Min: lo, Max: hi}
}

// Summarize judges ss against c. A run passes only if no other VM or engine overlapped
// it, the workload visibly grew host memory, the samples cover the window, and host
// memory stayed at or below the limit from some time ??Within until the end.
func Summarize(ss []Sample, c Criteria) Summary {
	sum := Summary{Criteria: c, N: len(ss), Reached: math.NaN(), MinAfter: math.MaxUint64}
	var base, basePriv, engBase, vmBase []float64
	for _, s := range ss {
		if s.Foreign != "" {
			sum.Overlapped++
		}
		switch s.Phase {
		case PhaseBaseline:
			base = append(base, float64(s.Total()))
			basePriv = append(basePriv, float64(s.VMMemPriv))
			engBase = append(engBase, float64(s.EngineWS))
			vmBase = append(vmBase, float64(s.VMMemWS+s.VMWPWS))
		case PhaseWorkload, PhaseAfter:
			if s.Total() > sum.Peak {
				sum.Peak, sum.VMMemPrivAtPeak = s.Total(), s.VMMemPriv
			}
		}
	}
	sum.Baseline = stat(base)
	sum.VMMemPrivBaseMed = hostmon.Median(basePriv)
	sum.EngineBase, sum.VMBase = hostmon.Median(engBase), hostmon.Median(vmBase)
	if len(base) > 0 && float64(sum.Peak) > sum.Baseline.Median {
		sum.Growth = sum.Peak - uint64(sum.Baseline.Median)
	}
	sum.WorkloadSeen = c.Workload > 0 && sum.Growth >= c.Workload/2

	var after []Sample
	for _, s := range ss {
		if s.Phase == PhaseAfter {
			after = append(after, s)
		}
	}
	// Reached: walk back from the end while samples stay at or under the limit.
	first := len(after)
	for i := len(after) - 1; i >= 0 && after[i].Total() <= c.Limit; i-- {
		first = i
	}
	if first < len(after) {
		sum.Reached = after[first].T
	}
	within := c.Within.Seconds()
	for _, s := range after {
		if s.Total() < sum.MinAfter {
			sum.MinAfter, sum.MinAfterT = s.Total(), s.T
		}
		if sum.AtWithinT == 0 && s.T >= within {
			sum.AtWithin, sum.AtWithinT = s.Total(), s.T
		}
	}
	if len(after) == 0 {
		sum.MinAfter = 0
	} else {
		last := after[len(after)-1]
		sum.End, sum.EndT, sum.VMMemPrivAtEnd = last.Total(), last.T, last.VMMemPriv
		sum.EngineEnd, sum.VMEnd = last.EngineWS, last.VMMemWS+last.VMWPWS
		sum.EngineCommitEnd = last.EnginePriv
		sum.CoversWindow = last.T >= within
		if len(base) > 0 {
			sum.EndOverBaseline = int64(last.Total()) - int64(sum.Baseline.Median)
		}
	}
	met := !math.IsNaN(sum.Reached) && sum.Reached <= within
	switch {
	case len(after) == 0:
		sum.VerdictText = "FAIL: no samples after the workload"
	case sum.Overlapped > 0:
		sum.VerdictText = fmt.Sprintf("NOT VALID: another VM or engine ran during %d samples", sum.Overlapped)
	case !sum.WorkloadSeen:
		sum.VerdictText = fmt.Sprintf("NOT VALID: host memory grew by %s only, less than half of the %s workload", mib(float64(sum.Growth)), mib(float64(c.Workload)))
	case !sum.CoversWindow:
		sum.VerdictText = "NOT VALID: the samples end before the reclaim window does"
	case !met:
		sum.VerdictText = "FAIL"
	default:
		sum.Pass = true
		sum.VerdictText = "PASS"
	}
	return sum
}

func mib(b float64) string { return strconv.FormatFloat(b/(1<<20), 'f', 1, 64) + " MiB" }

func mb(b float64) string { return strconv.FormatFloat(b/1e6, 'f', 0, 64) + " MB" }

// Print writes the summary as the table docs/ records.
func (s Summary) Print(w io.Writer) {
	fmt.Fprintf(w, "\nREQ-MEM-002 memory reclaim, %d samples (private working set of engine + vmmem + vmwp + helpers):\n\n", s.N)
	fmt.Fprintf(w, "| Point | Total | vmmem private bytes |\n|---|---|---|\n")
	fmt.Fprintf(w, "| baseline (idle, median) | %s (%s) | %s |\n", mib(s.Baseline.Median), mb(s.Baseline.Median), mib(s.VMMemPrivBaseMed))
	fmt.Fprintf(w, "| peak | %s | %s |\n", mib(float64(s.Peak)), mib(float64(s.VMMemPrivAtPeak)))
	if s.AtWithinT > 0 {
		fmt.Fprintf(w, "| exit + %.0f s | %s (%s) | |\n", s.AtWithinT, mib(float64(s.AtWithin)), mb(float64(s.AtWithin)))
	}
	fmt.Fprintf(w, "| lowest after exit (+%.0f s) | %s | |\n", s.MinAfterT, mib(float64(s.MinAfter)))
	fmt.Fprintf(w, "| end (exit + %.0f s) | %s (%s) | %s |\n", s.EndT, mib(float64(s.End)), mb(float64(s.End)), mib(float64(s.VMMemPrivAtEnd)))
	fmt.Fprintf(w, "\nGrowth over baseline at peak %s for a %s workload; end ??baseline %+.1f MiB\n",
		mib(float64(s.Growth)), mib(float64(s.Workload)), float64(s.EndOverBaseline)/(1<<20))
	fmt.Fprintf(w, "Engine %s at baseline, %s at the end (commit %s); VM (vmmem + vmwp) %s at baseline, %s at the end\n",
		mib(s.EngineBase), mib(float64(s.EngineEnd)), mib(float64(s.EngineCommitEnd)), mib(s.VMBase), mib(float64(s.VMEnd)))
	reached := "never"
	if !math.IsNaN(s.Reached) {
		reached = fmt.Sprintf("+%.0f s", s.Reached)
	}
	fmt.Fprintf(w, "At or below %s (%s) for good from: %s; required within %s\n", mb(float64(s.Limit)), mib(float64(s.Limit)), reached, s.Within)
	fmt.Fprintf(w, "\nREQ-MEM-002: %s\n", s.VerdictText)
}

var csvHeader = []string{"t_seconds", "phase", "engine_ws", "vmmem_ws", "vmwp_ws", "helpers_ws", "total", "vmmem_private", "foreign", "engine_private", "engine_faults"}

// WriteCSV writes every sample.
func WriteCSV(w io.Writer, ss []Sample) error {
	cw := csv.NewWriter(w)
	_ = cw.Write(csvHeader)
	u := func(v uint64) string { return strconv.FormatUint(v, 10) }
	for _, s := range ss {
		_ = cw.Write([]string{strconv.FormatFloat(s.T, 'f', 1, 64), string(s.Phase), u(s.EngineWS), u(s.VMMemWS), u(s.VMWPWS),
			u(s.HelpersWS), u(s.Total()), u(s.VMMemPriv), s.Foreign, u(s.EnginePriv), u(s.EngineFaults)})
	}
	cw.Flush()
	return cw.Error()
}
