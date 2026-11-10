//go:build windows

package engine

import (
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procShutdownBlockReasonQuery = modUser32.NewProc("ShutdownBlockReasonQuery")

// hasShutdownBlockReason reports whether the one watch window has a reason set. A
// reason must exist only while WM_ENDSESSION waits for the stop: one left behind by a
// query that was never confirmed would claim a stop that is not running.
func hasShutdownBlockReason(t *testing.T) bool {
	t.Helper()
	watchMu.Lock()
	var hs []windows.HWND
	for h := range watches {
		hs = append(hs, h)
	}
	watchMu.Unlock()
	if len(hs) != 1 {
		t.Fatalf("%d watch windows, want 1", len(hs))
	}
	var n uint32
	r, _, _ := procShutdownBlockReasonQuery.Call(uintptr(hs[0]), 0, uintptr(unsafe.Pointer(&n)))
	return r != 0
}

// Review finding 4: the console-less engine must turn a sign-out or shutdown into its
// normal stop, and hold WM_ENDSESSION until the stop is done.
func TestWatchSessionEnd(t *testing.T) {
	var stops atomic.Int32
	done := make(chan struct{})
	unwatch, err := WatchSessionEnd(func(string) { stops.Add(1) }, done)
	if err != nil {
		t.Fatal(err)
	}
	defer unwatch()
	r, err := sendSessionMessage(wmQueryEndSession, 0, endSessionLogoff)
	if err != nil || len(r) != 1 || r[0] != 1 {
		t.Fatalf("WM_QUERYENDSESSION: %v %v", r, err)
	}
	if n := stops.Load(); n != 0 {
		t.Fatalf("stop called %d times on WM_QUERYENDSESSION, want 0 (the end may be cancelled)", n)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		close(done)
	}()
	t0 := time.Now()
	if _, err := sendSessionMessage(wmEndSession, 1, endSessionLogoff); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(t0); el < 150*time.Millisecond || el > 5*time.Second {
		t.Fatalf("WM_ENDSESSION returned after %v, want it to wait for the stop", el)
	}
	if n := stops.Load(); n != 1 {
		t.Fatalf("stop called %d times, want 1", n)
	}
	unwatch()
	if _, err := sendSessionMessage(wmQueryEndSession, 0, 0); err == nil {
		t.Fatal("window still there after unwatch")
	}
}

// Up to v0.2.0 the engine stopped its VM on WM_QUERYENDSESSION, so a session end that
// was queried and then cancelled (by another application, or a Restart Manager request
// another application refused) stopped it too. Only a WM_ENDSESSION that says the end is
// real may stop the VM; a confirmed close still does (ADR 0010).
func TestWatchSessionEndMessages(t *testing.T) {
	type step struct {
		m      uint32
		wp, lp uintptr
	}
	q := func(lp uintptr) step { return step{wmQueryEndSession, 0, lp} }
	end := func(wp, lp uintptr) step { return step{wmEndSession, wp, lp} }
	cases := []struct {
		name  string
		steps []step
		stops int32
	}{
		{"shutdown query alone", []step{q(0)}, 0},
		{"logoff query alone", []step{q(endSessionLogoff)}, 0},
		{"critical query alone", []step{q(endSessionCritical)}, 0},
		{"restart manager query alone", []step{q(endSessionCloseApp)}, 0},
		{"shutdown cancelled", []step{q(0), end(0, 0)}, 0},
		{"logoff cancelled", []step{q(endSessionLogoff), end(0, endSessionLogoff)}, 0},
		{"restart manager cancelled", []step{q(endSessionCloseApp), end(0, endSessionCloseApp)}, 0},
		{"cancelled, then queried again", []step{q(0), end(0, 0), q(0)}, 0},
		{"shutdown", []step{q(0), end(1, 0)}, 1},
		{"logoff", []step{q(endSessionLogoff), end(1, endSessionLogoff)}, 1},
		{"critical shutdown", []step{q(endSessionCritical), end(1, endSessionCritical)}, 1},
		{"critical logoff", []step{q(endSessionCritical | endSessionLogoff), end(1, endSessionCritical|endSessionLogoff)}, 1},
		// ADR 0010: the caller closes the process next either way; a clean guest
		// power-off beats being terminated with the VM running.
		{"restart manager closes the app", []step{q(endSessionCloseApp), end(1, endSessionCloseApp)}, 1},
		{"end without a query", []step{end(1, 0)}, 1},
		{"cancelled, then real", []step{q(0), end(0, 0), q(0), end(1, 0)}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stops atomic.Int32
			done := make(chan struct{})
			close(done) // the stop finishes at once; this test is about whether it starts
			unwatch, err := WatchSessionEnd(func(string) { stops.Add(1) }, done)
			if err != nil {
				t.Fatal(err)
			}
			defer unwatch()
			for _, s := range c.steps {
				t0 := time.Now()
				r, err := sendSessionMessage(s.m, s.wp, s.lp)
				if err != nil || len(r) != 1 {
					t.Fatalf("message %#x: %v %v", s.m, r, err)
				}
				if s.m == wmQueryEndSession {
					if r[0] != 1 {
						t.Fatalf("WM_QUERYENDSESSION returned %d, want 1 (never veto)", r[0])
					}
					if el := time.Since(t0); el > time.Second {
						t.Fatalf("WM_QUERYENDSESSION took %v, want it to return at once", el)
					}
				}
				if hasShutdownBlockReason(t) {
					t.Fatalf("a shutdown block reason is left after message %#x", s.m)
				}
			}
			if n := stops.Load(); n != c.stops {
				t.Fatalf("stop called %d times, want %d", n, c.stops)
			}
		})
	}
}

func TestStopsOnSessionEnd(t *testing.T) {
	for _, c := range []struct {
		m      uint32
		wp, lp uintptr
		want   bool
	}{
		{wmQueryEndSession, 0, 0, false},
		{wmQueryEndSession, 0, endSessionCloseApp, false},
		{wmQueryEndSession, 0, endSessionLogoff, false},
		{wmQueryEndSession, 0, endSessionCritical, false},
		{wmQueryEndSession, 1, endSessionLogoff, false}, // wParam is reserved on the query
		{wmEndSession, 0, 0, false},
		{wmEndSession, 0, endSessionCloseApp, false},
		{wmEndSession, 0, endSessionCritical | endSessionLogoff, false},
		{wmEndSession, 1, 0, true},
		{wmEndSession, 1, endSessionLogoff, true},
		{wmEndSession, 1, endSessionCritical, true},
		{wmEndSession, 1, endSessionCloseApp, true},
		{wmClose, 1, 0, false},
		{wmDestroy, 0, 0, false},
	} {
		if got := stopsOnSessionEnd(c.m, c.wp, c.lp); got != c.want {
			t.Errorf("stopsOnSessionEnd(%#x, %d, %#x) = %v, want %v", c.m, c.wp, c.lp, got, c.want)
		}
	}
}

func TestSessionEndWhy(t *testing.T) {
	for lp, want := range map[uintptr]string{
		0:                                     "shutdown or restart",
		endSessionLogoff:                      "sign-out",
		endSessionCloseApp:                    "an installer or updater (Restart Manager) closes the engine",
		endSessionCritical:                    "shutdown or restart, forced",
		endSessionCritical | endSessionLogoff: "sign-out, forced",
	} {
		if got := sessionEndWhy(lp); got != want {
			t.Errorf("sessionEndWhy(%#x) = %q, want %q", lp, got, want)
		}
	}
}
