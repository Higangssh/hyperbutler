package diag

import (
	"sync"
	"sync/atomic"
	"time"
)

// WorkingSetGuard undoes outside memory scans. Another process that reads this
// process's whole address space (on the reference machine something, most likely the
// antivirus, did so once, five minutes after start, while a bench ran PowerShell) makes
// every committed page resident, including memory the process never touches: the Go
// runtime's 8 MiB heap index, idle thread stacks, untouched data-section variables. The
// private working set then equals the commit and stays there
// (docs/research/mem-reclaim.md, section 3).
//
// That end state is the signature: the process itself never touches all of its commit
// (the heap index alone keeps megabytes untouched), so a private working set within
// GuardSlack of the commit, reached by a rise of at least GuardRise over the lowest
// value of the last GuardWindow, is an outside read. Growth the process does itself
// (Go heap, native DLL heaps, new thread stacks) raises the commit too and keeps the
// gap. On that signature the guard empties the working set once
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
	recent         []guardSample
	lastTrim       time.Time
}

// observe records s and reports whether to trim now.
func (g *guardState) observe(s guardSample) bool {
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
	if s.private < low.private+g.rise || s.private+g.slack < s.commit {
		return false
	}
	if !g.lastTrim.IsZero() && s.at.Sub(g.lastTrim) < g.minGap {
		return false
	}
	g.lastTrim = s.at
	// Keep the pre-scan low: refaulting the real working set after the trim is not a
	// rise, only a second scan back to the full commit is.
	g.recent = append(g.recent[:0], guardSample{at: s.at, private: low.private, commit: s.commit})
	return true
}

// guardOS is what the loop needs from the operating system.
type guardOS struct {
	read func() (private, commit uint64, ok bool)
	trim func() bool
}

// StartWorkingSetGuard starts the guard. trimmed, if not nil, is called after each trim
// with the private working set and the commit before it. Stop must be called.
func StartWorkingSetGuard(trimmed func(private, commit uint64)) *WorkingSetGuard {
	if !guardSupported {
		w := &WorkingSetGuard{stop: make(chan struct{}), done: make(chan struct{})}
		close(w.done)
		return w
	}
	t := time.NewTicker(GuardInterval)
	return startGuard(t.C, t.Stop, guardOS{read: processMemory, trim: emptyWorkingSet}, trimmed)
}

func startGuard(tick <-chan time.Time, stopTick func(), os guardOS, trimmed func(private, commit uint64)) *WorkingSetGuard {
	w := &WorkingSetGuard{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		defer stopTick()
		g := &guardState{window: GuardWindow, minGap: GuardMinGap, rise: GuardRise, slack: GuardSlack}
		for {
			select {
			case <-w.stop:
				return
			case now := <-tick:
				p, c, ok := os.read()
				if !ok || !g.observe(guardSample{at: now, private: p, commit: c}) || !os.trim() {
					continue
				}
				workingSetTrims.Add(1)
				if trimmed != nil {
					trimmed(p, c)
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
