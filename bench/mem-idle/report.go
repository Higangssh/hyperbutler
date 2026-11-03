package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/Higangssh/hyperbutler/bench/internal/hostmon"
	"github.com/Higangssh/hyperbutler/internal/diag"
)

// Sample is one 10-second reading of the idle installation.
type Sample struct {
	T         float64 // seconds since `hyperbutler start` was invoked
	EngineWS  uint64  // engine (hyperbutler.exe) private working set, bytes
	VMMemWS   uint64  // this VM's vmmem private working set
	VMWPWS    uint64  // this VM's vmwp private working set
	HelpersWS uint64  // the engine's child processes, if any
	BrokerWS  uint64  // the HCS access broker service (hyperbutler-broker.exe), 0 without it
	Threads   uint64  // engine thread count (perf counter)
	Stats     diag.Stats
	Foreign   string // other VMs or engines seen at this sample ("" when none)
}

// Total is the REQ-MEM-001 host memory of one sample.
// The broker service is counted whole: it is one process per machine, and with a
// single user all of it is there for that user's VM.
func (s Sample) Total() uint64 { return s.EngineWS + s.VMMemWS + s.VMWPWS + s.HelpersWS + s.BrokerWS }

// Schedule returns the sample offsets from the start: from, from+interval, ... up to
// and including to. REQ-MEM-001 samples every 10 s from minute 5 to minute 10, which
// is 31 samples.
func Schedule(from, to, interval time.Duration) []time.Duration {
	if interval <= 0 || to < from || from < 0 || (to-from)/interval >= maxSamples {
		return nil
	}
	n := int((to-from)/interval) + 1
	s := make([]time.Duration, n)
	for i := range s {
		s[i] = from + time.Duration(i)*interval
	}
	return s
}

// maxSamples bounds a schedule (a day at 1 s).
const maxSamples = 86400

// Summary is the result of one idle measurement.
type Summary struct {
	N          int
	Overlapped int // samples during which another VM or engine was running

	Engine, VMMem, VMWP, Helpers, Broker, Total Stat // private working set, bytes

	// The engine's Go runtime, medians over the window. GC slack is memory the heap
	// holds but does not use: HeapIdle - HeapReleased, plus HeapInuse - HeapAlloc
	// (fragmentation inside spans).
	HeapAlloc, HeapInuse, HeapRetained, StackInuse, GoSys Stat
	Goroutines, Handles                                   Stat
	// OSPrivate is the engine's commit charge (private bytes) as Windows counts it. The
	// gap to the private working set is committed memory never touched, such as Go
	// 1.27's 32 MiB FIPS entropy scratch buffer in .bss; anything that reads the whole
	// process (a scanner, a dump) makes it resident. NativeHeap is the Windows heaps.
	OSPrivate, NativeHeap Stat
	NumGC                 uint32 // GCs during the window

	// Theil–Sen slopes over the window, bytes per hour. Five minutes cannot show a slow
	// leak; these only flag a fast one, and tell heap growth from other growth.
	EngineWSSlope, HeapAllocSlope float64

	Target uint64 // REQ-MEM-001 limit on the Total median, bytes
	Pass   bool
}

func osField(s Sample, f func(*diag.OSMemory) uint64) float64 {
	if s.Stats.OS == nil {
		return 0
	}
	return float64(f(s.Stats.OS))
}

// Stat is a median and range.
type Stat struct{ Median, Min, Max float64 }

func stat(xs []float64) Stat {
	lo, hi := hostmon.MinMax(xs)
	return Stat{Median: hostmon.Median(xs), Min: lo, Max: hi}
}

// Summarize reduces the samples. The verdict is the median Total against target; a
// run with any overlapped sample does not pass, because another VM on the host makes
// the numbers suspect (the caller re-runs it).
func Summarize(ss []Sample, target uint64) Summary {
	sum := Summary{N: len(ss), Target: target}
	col := func(f func(Sample) float64) []float64 {
		v := make([]float64, len(ss))
		for i, s := range ss {
			v[i] = f(s)
		}
		return v
	}
	ts := col(func(s Sample) float64 { return s.T })
	engine := col(func(s Sample) float64 { return float64(s.EngineWS) })
	heapAlloc := col(func(s Sample) float64 { return float64(s.Stats.MemStats.HeapAlloc) })
	sum.Engine = stat(engine)
	sum.VMMem = stat(col(func(s Sample) float64 { return float64(s.VMMemWS) }))
	sum.VMWP = stat(col(func(s Sample) float64 { return float64(s.VMWPWS) }))
	sum.Helpers = stat(col(func(s Sample) float64 { return float64(s.HelpersWS) }))
	sum.Broker = stat(col(func(s Sample) float64 { return float64(s.BrokerWS) }))
	sum.Total = stat(col(func(s Sample) float64 { return float64(s.Total()) }))
	sum.HeapAlloc = stat(heapAlloc)
	sum.HeapInuse = stat(col(func(s Sample) float64 { return float64(s.Stats.MemStats.HeapInuse) }))
	sum.HeapRetained = stat(col(func(s Sample) float64 {
		m := s.Stats.MemStats
		return float64(m.HeapIdle-min(m.HeapReleased, m.HeapIdle)) + float64(m.HeapInuse-min(m.HeapAlloc, m.HeapInuse))
	}))
	sum.StackInuse = stat(col(func(s Sample) float64 { return float64(s.Stats.MemStats.StackInuse) }))
	sum.GoSys = stat(col(func(s Sample) float64 { return float64(s.Stats.MemStats.Sys) }))
	sum.Goroutines = stat(col(func(s Sample) float64 { return float64(s.Stats.Goroutines) }))
	sum.Handles = stat(col(func(s Sample) float64 { return float64(s.Stats.Handles) }))
	sum.OSPrivate = stat(col(func(s Sample) float64 { return osField(s, func(o *diag.OSMemory) uint64 { return o.PrivateBytes }) }))
	sum.NativeHeap = stat(col(func(s Sample) float64 { return osField(s, func(o *diag.OSMemory) uint64 { return o.HeapCommitted }) }))
	if len(ss) > 0 {
		sum.NumGC = ss[len(ss)-1].Stats.MemStats.NumGC - min(ss[0].Stats.MemStats.NumGC, ss[len(ss)-1].Stats.MemStats.NumGC)
	}
	sum.EngineWSSlope = hostmon.SlopePerHour(ts, engine)
	sum.HeapAllocSlope = hostmon.SlopePerHour(ts, heapAlloc)
	for _, s := range ss {
		if s.Foreign != "" {
			sum.Overlapped++
		}
	}
	sum.Pass = len(ss) > 0 && sum.Overlapped == 0 && sum.Total.Median <= float64(target)
	return sum
}

func mib(b float64) string { return strconv.FormatFloat(b/(1<<20), 'f', 1, 64) + " MiB" }

// Print writes the summary as the table docs/ records.
func (s Summary) Print(w io.Writer) {
	_, _ = fmt.Fprintf(w, "\nREQ-MEM-001 idle memory, %d samples (private working set):\n\n", s.N)
	_, _ = fmt.Fprintf(w, "| Process | Median | Min | Max |\n|---|---|---|---|\n")
	for _, r := range []struct {
		name string
		st   Stat
	}{{"engine (hyperbutler.exe)", s.Engine}, {"vmmem (this VM)", s.VMMem}, {"vmwp (this VM)", s.VMWP}, {"engine helpers", s.Helpers}, {"broker service", s.Broker}, {"**total**", s.Total}} {
		_, _ = fmt.Fprintf(w, "| %s | %s | %s | %s |\n", r.name, mib(r.st.Median), mib(r.st.Min), mib(r.st.Max))
	}
	_, _ = fmt.Fprintf(w, "\nEngine Go runtime (medians): heap alloc %s, heap in use %s, GC slack (free but retained) %s, stacks %s, Go Sys %s; goroutines %.0f, handles %.0f; %d GCs in the window\n",
		mib(s.HeapAlloc.Median), mib(s.HeapInuse.Median), mib(s.HeapRetained.Median), mib(s.StackInuse.Median), mib(s.GoSys.Median), s.Goroutines.Median, s.Handles.Median, s.NumGC)
	_, _ = fmt.Fprintf(w, "Engine commit (private bytes) %s, of which Windows heaps %s; the gap to the working set is committed but untouched memory\n",
		mib(s.OSPrivate.Median), mib(s.NativeHeap.Median))
	_, _ = fmt.Fprintf(w, "Slopes over the window: engine WS %+.1f MiB/h, heap alloc %+.1f MiB/h (5 minutes only shows fast growth)\n",
		s.EngineWSSlope/(1<<20), s.HeapAllocSlope/(1<<20))
	verdict := "PASS"
	switch {
	case s.N == 0:
		verdict = "FAIL: no samples"
	case s.Overlapped > 0:
		verdict = fmt.Sprintf("NOT VALID: another VM or engine ran during %d samples", s.Overlapped)
	case !s.Pass:
		verdict = "FAIL"
	}
	_, _ = fmt.Fprintf(w, "\nTotal median %s (%.0f MB) against %.0f MB: %s\n", mib(s.Total.Median), s.Total.Median/1e6, float64(s.Target)/1e6, verdict)
}

// csvHeader names the CSV columns WriteCSV writes.
var csvHeader = []string{"t_seconds", "engine_ws", "vmmem_ws", "vmwp_ws", "helpers_ws", "broker_ws", "total", "engine_threads",
	"goroutines", "handles", "heap_alloc", "heap_inuse", "heap_idle", "heap_released", "stack_inuse", "go_sys", "num_gc",
	"os_private", "os_heap_committed", "foreign"}

// WriteCSV writes every sample.
func WriteCSV(w io.Writer, ss []Sample) error {
	cw := csv.NewWriter(w)
	_ = cw.Write(csvHeader)
	u := func(v uint64) string { return strconv.FormatUint(v, 10) }
	for _, s := range ss {
		m := s.Stats.MemStats
		_ = cw.Write([]string{strconv.FormatFloat(s.T, 'f', 1, 64), u(s.EngineWS), u(s.VMMemWS), u(s.VMWPWS), u(s.HelpersWS), u(s.BrokerWS), u(s.Total()), u(s.Threads),
			strconv.Itoa(s.Stats.Goroutines), strconv.FormatInt(s.Stats.Handles, 10), u(m.HeapAlloc), u(m.HeapInuse), u(m.HeapIdle), u(m.HeapReleased),
			u(m.StackInuse), u(m.Sys), strconv.FormatUint(uint64(m.NumGC), 10),
			strconv.FormatFloat(osField(s, func(o *diag.OSMemory) uint64 { return o.PrivateBytes }), 'f', 0, 64),
			strconv.FormatFloat(osField(s, func(o *diag.OSMemory) uint64 { return o.HeapCommitted }), 'f', 0, 64), s.Foreign})
	}
	cw.Flush()
	return cw.Error()
}
