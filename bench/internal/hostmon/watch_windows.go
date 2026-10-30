//go:build windows

package hostmon

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Watcher snapshots the host every interval in the background and remembers every
// foreign VM or engine process it saw, so a measurement learns about one that came and
// went between its own samples. The bench's own descendants (its hyperbutler CLI and
// docker calls) and the PIDs passed to SetOurs are not foreign.
type Watcher struct {
	mu     sync.Mutex
	ours   map[uint32]bool
	seen   map[uint32]string
	cancel context.CancelFunc
	done   chan struct{}
}

// Watch starts a Watcher. Stop must be called.
func Watch(every time.Duration) *Watcher {
	ctx, cancel := context.WithCancel(context.Background())
	w := &Watcher{ours: map[uint32]bool{}, seen: map[uint32]string{}, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		self := uint32(os.Getpid())
		for {
			if ps, err := Snapshot(); err == nil {
				mine := Descendants(ps, self)
				w.mu.Lock()
				for _, p := range ps {
					if !mine[p.PID] && isWatched(p) && len(w.seen) < maxSeen {
						w.seen[p.PID] = p.Name
					}
				}
				w.mu.Unlock()
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(every):
			}
		}
	}()
	return w
}

// maxSeen bounds the remembered processes between two Take calls.
const maxSeen = 1024

func isWatched(p Proc) bool {
	return isVMMem(p) || isName(p, VMWPName) || isName(p, EngineName) || isName(p, DevName)
}

// SetOurs marks pids as the bench's own (its engine and VM).
func (w *Watcher) SetOurs(pids ...uint32) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range pids {
		w.ours[p] = true
	}
}

// Take returns the foreign processes seen since the last Take ("" if none) and
// forgets them.
func (w *Watcher) Take() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for pid, name := range w.seen {
		if !w.ours[pid] {
			out = append(out, fmt.Sprintf("%s %d", name, pid))
		}
	}
	w.seen = map[uint32]string{}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// Stop ends the background snapshots.
func (w *Watcher) Stop() {
	w.cancel()
	<-w.done
}
