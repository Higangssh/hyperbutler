//go:build windows

package engine

import (
	"sync/atomic"
	"testing"
	"time"
)

// Review finding 4: the console-less engine must turn a sign-out or shutdown into its
// normal stop, and hold WM_ENDSESSION until the stop is done.
func TestWatchSessionEnd(t *testing.T) {
	var stops atomic.Int32
	done := make(chan struct{})
	unwatch, err := WatchSessionEnd(func() { stops.Add(1) }, done)
	if err != nil {
		t.Fatal(err)
	}
	defer unwatch()
	r, err := sendSessionMessage(wmQueryEndSession, 0)
	if err != nil || len(r) != 1 || r[0] != 1 {
		t.Fatalf("WM_QUERYENDSESSION: %v %v", r, err)
	}
	if stops.Load() != 1 {
		t.Fatalf("stop called %d times", stops.Load())
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		close(done)
	}()
	t0 := time.Now()
	if _, err := sendSessionMessage(wmEndSession, 1); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(t0); el < 150*time.Millisecond || el > 5*time.Second {
		t.Fatalf("WM_ENDSESSION returned after %v, want it to wait for the stop", el)
	}
	unwatch()
	if _, err := sendSessionMessage(wmQueryEndSession, 0); err == nil {
		t.Fatal("window still there after unwatch")
	}
}
