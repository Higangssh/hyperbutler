package engine

import (
	"sync"
	"time"
)

// guestFSEvery, guestFSStepPct and guestFSStepMin bound how often the guest file system's use goes to
// state.json (REQ-DISK-009): hyperbutlerd reports it every second, and a state.json
// write per report would be wasted. A change of guestFSStepPct percent of the size (or
// guestFSStepMin, whichever is more) is written at once, any other change after
// guestFSEvery.
const (
	guestFSEvery   = time.Minute
	guestFSStepPct = 0.5
	guestFSStepMin = 64 << 20
)

// guestFSThrottle decides which guest reports of the data file system are published.
type guestFSThrottle struct {
	mu   sync.Mutex
	last GuestFSState
	seen bool
}

// next returns the state to publish for a report of size and free bytes at now, or
// false when the last one published is still close enough. A report without a size
// (no data disk) or with more free than size (a confused guest) is never published.
func (t *guestFSThrottle) next(size, free uint64, now time.Time) (*GuestFSState, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if size == 0 || free > size {
		return nil, false
	}
	cur := GuestFSState{SizeBytes: size, FreeBytes: free, Updated: now.UTC().Truncate(time.Second)}
	if t.seen {
		if cur.SizeBytes == t.last.SizeBytes && cur.FreeBytes == t.last.FreeBytes {
			return nil, false
		}
		step := max(uint64(float64(size)*guestFSStepPct/100), guestFSStepMin)
		delta := max(cur.FreeBytes, t.last.FreeBytes) - min(cur.FreeBytes, t.last.FreeBytes)
		if cur.SizeBytes == t.last.SizeBytes && delta < step && now.Sub(t.last.Updated) < guestFSEvery {
			return nil, false
		}
	}
	t.last, t.seen = cur, true
	out := cur
	return &out, true
}
