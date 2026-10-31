package diag

import (
	"sync"
	"sync/atomic"
	"time"
)

// WorkingSetGuard undoes outside memory scans. Another process that reads this
// process's whole address space (on the reference machine something, most likely the
// antivirus, did so five minutes after start while a bench ran PowerShell) makes
// every committed page resident, including memory the process never touches: the Go
// runtime's 8 MiB heap index, idle thread stacks, untouched data-section variables. The
// private working set then equals the commit and stays there
// (docs/research/mem-reclaim.md, section 3).
//
// Two signatures mark an outside read; both compare a sample with the lowest private
// working set of the last GuardWindow:
//   - Full: a rise of at least GuardRise to within GuardSlack of the commit. The process
//     itself never touches all of its commit (the heap index alone keeps megabytes
//     untouched); growth it does (Go heap, native DLL heaps, new thread stacks) raises
//     the commit too and keeps the gap.
//   - Partial: a rise of at least GuardBigRise while the commit grew by at most
//     GuardFlat. A reader that skips part of the address space leaves a gap to the
//     commit, but the commit does not grow (seen once, in a run without the PowerShell
//     sampler: +33 MiB, about the size of Go's FIPS buffer, 15 MiB under the commit;
//     its cause is not known). The process faulting its own pages back after Windows
//     (or the guard) trimmed them looks the same, so the rule is confirmed by the
//     executable's data section: its resident pages must be GuardBigRise above their
//     level when the guard started. The process never touches the 32 MiB FIPS buffer,
//     so its data section can only refault to about where it started. In effect this
//     rule means "the FIPS buffer became resident"; a reader that skips .data is caught
//     only by the full rule. The rise and flat-commit conditions only spare the data
//     read on ordinary ticks. Known false positive: with GODEBUG=fips140=on the process
//     fills the buffer itself, and the guard trims once (harmless, rate limited).
//
// On either the guard empties the working set once
// (SetProcessWorkingSetSizeEx(-1, -1)). Pages the process uses fault back from the
// standby list; the ones only the scan touched stay out. The trim takes pages out of
// the working set, which is what REQ-MEM-001/002 count; Windows frees the RAM when it
// repurposes them. It does not prevent the scan and does nothing outside Windows.
type WorkingSetGuard struct {
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// Guard parameters.
const (
	GuardInterval = 5 * time.Second
	GuardWindow   = 30 * time.Second
	GuardRise     = 8 << 20
	GuardSlack    = 4 << 20
	GuardBigRise  = 16 << 20
	GuardFlat     = 1 << 20
	// GuardMinGap is the shortest time between two trims.
	GuardMinGap = time.Minute
)

// workingSetTrims counts the guard's trims, for /debug/stats.
var workingSetTrims atomic.Int64

// guardSample is one reading.
type guardSample struct {
	at      time.Time
	private uint64 // private working set
	commit  uint64 // private bytes
}

// guardState decides when to trim. It is separate from the loop so the rule is
// testable without the OS.
type guardState struct {
	window, minGap time.Duration
	rise, slack    uint64
	bigRise, flat  uint64
	dataRef        uint64 // resident bytes of the executable's data section at start
	recent         []guardSample
	lastTrim       time.Time
}

// Rule names, for the log.
const (
	ruleFull    = "full"
	rulePartial = "partial"
)

// observe records s and reports which rule says to trim now ("" for none). data, if
// not nil, reads the executable's resident data-section bytes; it is called only to
// confirm the partial rule.
func (g *guardState) observe(s guardSample, data func() (uint64, bool)) string {
	keep := g.recent[:0]
	for _, r := range g.recent {
		if s.at.Sub(r.at) <= g.window {
			keep = append(keep, r)
		}
	}
	g.recent = keep
	low := s
	for _, r := range g.recent {
		if r.private < low.private {
			low = r
		}
	}
	g.recent = append(g.recent, s)
	rule := ""
	switch {
	case s.private >= low.private+g.rise && s.private+g.slack >= s.commit:
		rule = ruleFull
	case g.bigRise > 0 && data != nil && s.private >= low.private+g.bigRise && s.commit <= low.commit+g.flat:
		if d, ok := data(); ok && d >= g.dataRef+g.bigRise {
			rule = rulePartial
		}
	}
	if rule == "" {
		return ""
	}
	if !g.lastTrim.IsZero() && s.at.Sub(g.lastTrim) < g.minGap {
		return ""
	}
	g.lastTrim = s.at
	// Keep the pre-scan low: refaulting the real working set after the trim is not a
	// rise, only a second scan back to the full commit is.
	g.recent = append(g.recent[:0], guardSample{at: s.at, private: low.private, commit: s.commit})
	return rule
}

// guardOS is what the loop needs from the operating system.
type guardOS struct {
	read func() (private, commit uint64, ok bool)
	trim func() bool
	// data reads the executable's resident data-section bytes; nil disables the
	// partial rule.
	data func() (uint64, bool)
}

// StartWorkingSetGuard starts the guard. trimmed, if not nil, is called after each trim
// with the rule that fired ("full" or "partial") and the private working set and commit
// before it. Stop must be called.
func StartWorkingSetGuard(trimmed func(rule string, private, commit uint64)) *WorkingSetGuard {
	if !guardSupported {
		w := &WorkingSetGuard{stop: make(chan struct{}), done: make(chan struct{})}
		close(w.done)
		return w
	}
	t := time.NewTicker(GuardInterval)
	return startGuard(t.C, t.Stop, guardOS{read: processMemory, trim: emptyWorkingSet, data: dataSectionReader()}, trimmed)
}

func startGuard(tick <-chan time.Time, stopTick func(), os guardOS, trimmed func(rule string, private, commit uint64)) *WorkingSetGuard {
	w := &WorkingSetGuard{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		defer stopTick()
		g := &guardState{window: GuardWindow, minGap: GuardMinGap, rise: GuardRise, slack: GuardSlack, bigRise: GuardBigRise, flat: GuardFlat}
		data := os.data
		if data != nil {
			if d, ok := data(); ok {
				g.dataRef = d
			} else {
				data = nil
			}
		}
		for {
			select {
			case <-w.stop:
				return
			case now := <-tick:
				p, c, ok := os.read()
				if !ok {
					continue
				}
				rule := g.observe(guardSample{at: now, private: p, commit: c}, data)
				if rule == "" || !os.trim() {
					continue
				}
				workingSetTrims.Add(1)
				if trimmed != nil {
					trimmed(rule, p, c)
				}
			}
		}
	}()
	return w
}

// Stop ends the guard and waits for it. It is safe to call more than once and on nil.
func (w *WorkingSetGuard) Stop() {
	if w == nil {
		return
	}
	w.once.Do(func() { close(w.stop) })
	<-w.done
}
