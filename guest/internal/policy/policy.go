// Package policy holds the guest daemon's pure decision logic, with no system calls, so
// it is tested and fuzzed on every platform:
//
//   - Trim decides when to FITRIM the data file system: periodically, and soon after a
//     large delete, seen as a jump in statfs free space (REQ-DISK-001).
//   - Cache decides when to reclaim page cache: when the guest is idle and holds more
//     file cache than it should keep (REQ-MEM-002). HCS only returns memory the guest
//     frees (free page reporting via hv_balloon); clean page cache is never freed on its
//     own while there is no memory pressure.
//
// Parsers for /proc/meminfo, /proc/stat and /proc/vmstat are here too.
package policy

import (
	"time"
)

// Trim is the fstrim policy. The zero value is not useful; start from DefaultTrim.
type Trim struct {
	// Period is the longest time between two trims.
	Period time.Duration
	// DeleteBytes is the rise in free space, above the lowest free space seen since the
	// last trim, that counts as a large delete.
	DeleteBytes uint64
	// Settle: after a large delete, trim once free space has stopped rising for this
	// long. It must exceed the ext4 journal commit interval (5 s), because ext4 frees
	// blocks, and FITRIM can only trim them, after the delete is committed.
	Settle time.Duration
	// MaxDelay: trim at the latest this long after the delete was first seen, even if
	// free space is still rising (a long prune).
	MaxDelay time.Duration
	// MinInterval: never trim more often than this.
	MinInterval time.Duration

	lastTrim  time.Time
	low       uint64 // lowest free space since the last trim
	lastFree  uint64
	lastRise  time.Time
	jumpStart time.Time
	started   bool
}

// DefaultTrim meets REQ-DISK-001's 5 minutes with a wide margin: a delete is trimmed
// within Settle..MaxDelay, anything else within Period.
var DefaultTrim = Trim{
	Period:      2 * time.Minute,
	DeleteBytes: 64 << 20,
	Settle:      6 * time.Second,
	MaxDelay:    30 * time.Second,
	MinInterval: 5 * time.Second,
}

// TrimReason says why Observe asked for a trim.
type TrimReason int

const (
	NoTrim TrimReason = iota
	TrimPeriodic
	TrimDelete
)

func (r TrimReason) String() string {
	switch r {
	case TrimPeriodic:
		return "periodic"
	case TrimDelete:
		return "delete"
	}
	return "none"
}

// Observe takes one free-space sample and says whether to trim now. The caller trims
// and then calls Trimmed.
func (p *Trim) Observe(now time.Time, free uint64) TrimReason {
	if !p.started {
		p.started = true
		p.lastTrim, p.low, p.lastFree = now, free, free
		return NoTrim
	}
	if free < p.low {
		p.low = free
	}
	if free > p.lastFree {
		p.lastRise = now
	}
	p.lastFree = free
	jumped := free-p.low >= p.DeleteBytes // free >= low always
	if jumped && p.jumpStart.IsZero() {
		p.jumpStart = now
	}
	if !jumped {
		p.jumpStart = time.Time{}
	}
	if now.Sub(p.lastTrim) < p.MinInterval {
		return NoTrim
	}
	if jumped && (now.Sub(p.lastRise) >= p.Settle || now.Sub(p.jumpStart) >= p.MaxDelay) {
		return TrimDelete
	}
	if now.Sub(p.lastTrim) >= p.Period {
		return TrimPeriodic
	}
	return NoTrim
}

// Trimmed records a trim that finished at now with free bytes free. Trims requested by
// someone else (the host, the guard) are recorded too, so they reset the policy.
func (p *Trim) Trimmed(now time.Time, free uint64) {
	p.started = true
	p.lastTrim, p.low, p.lastFree = now, free, free
	p.jumpStart = time.Time{}
}

// Cache is the page-cache policy.
type Cache struct {
	// Keep is how much file cache to leave in place: hot binaries and libraries.
	Keep uint64
	// Threshold: reclaim only when file cache exceeds Keep by at least this much, so
	// small growth does not cause churn.
	Threshold uint64
	// IdleFor is how long the guest must be idle first.
	IdleFor time.Duration
	// IdleCPU is the busy fraction of all CPUs below which a sample counts as idle.
	IdleCPU float64
	// IdleIO is the page-in plus page-out rate (bytes/s) below which a sample counts as
	// idle.
	IdleIO float64
	// MinInterval is the shortest time between two reclaims.
	MinInterval time.Duration

	prev        Sample
	havePrev    bool
	idleSince   time.Time
	lastReclaim time.Time
}

// DefaultCache returns memory within REQ-MEM-002's 60 s: 10 s idle detection, then a
// reclaim; HCS takes the freed pages back within about 15 s.
var DefaultCache = Cache{
	Keep:        32 << 20,
	Threshold:   64 << 20,
	IdleFor:     10 * time.Second,
	IdleCPU:     0.10,
	IdleIO:      4 << 20,
	MinInterval: 30 * time.Second,
}

// Sample is one reading of the guest's activity counters.
type Sample struct {
	T         time.Time
	CPUBusy   uint64 // jiffies, /proc/stat "cpu" line, everything but idle and iowait
	CPUTotal  uint64 // jiffies, all fields
	PageIO    uint64 // bytes paged in plus out, /proc/vmstat pgpgin+pgpgout (KiB) * 1024
	FileCache uint64 // bytes, Active(file)+Inactive(file)
}

// Observe takes one sample and returns how many bytes of file cache to reclaim now (0:
// none).
func (p *Cache) Observe(s Sample) uint64 {
	prev, had := p.prev, p.havePrev
	p.prev, p.havePrev = s, true
	if !had || !s.T.After(prev.T) || s.CPUTotal <= prev.CPUTotal || s.CPUBusy < prev.CPUBusy || s.PageIO < prev.PageIO {
		// First sample, or counters went backwards or stood still (no CPU time passed,
		// so nothing is known about idleness): start over.
		p.idleSince = time.Time{}
		return 0
	}
	dt := s.T.Sub(prev.T).Seconds()
	busy := float64(s.CPUBusy-prev.CPUBusy) / float64(s.CPUTotal-prev.CPUTotal)
	io := float64(s.PageIO-prev.PageIO) / dt
	if busy >= p.IdleCPU || io >= p.IdleIO {
		p.idleSince = time.Time{}
		return 0
	}
	if p.idleSince.IsZero() {
		p.idleSince = prev.T
	}
	if s.T.Sub(p.idleSince) < p.IdleFor {
		return 0
	}
	if !p.lastReclaim.IsZero() && s.T.Sub(p.lastReclaim) < p.MinInterval {
		return 0
	}
	if s.FileCache <= p.Keep || s.FileCache-p.Keep < p.Threshold {
		return 0
	}
	return s.FileCache - p.Keep
}

// Reclaimed records a reclaim at t.
func (p *Cache) Reclaimed(t time.Time) { p.lastReclaim = t }
