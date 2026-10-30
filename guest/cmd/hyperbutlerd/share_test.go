package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeConn struct{ closed atomic.Bool }

func (c *fakeConn) Read([]byte) (int, error)    { return 0, io.EOF }
func (c *fakeConn) Write(b []byte) (int, error) { return len(b), nil }
func (c *fakeConn) Close() error                { c.closed.Store(true); return nil }

type fakeRelay struct {
	ended  chan error
	closed atomic.Bool
	once   sync.Once
}

func (r *fakeRelay) Wait() error { return <-r.ended }
func (r *fakeRelay) Close() error {
	r.closed.Store(true)
	r.once.Do(func() { r.ended <- errors.New("closed") })
	return nil
}

type relayHarness struct {
	mu        sync.Mutex
	dials     int
	failDials int // the first failDials dials fail (the host is not listening yet)
	conns     []*fakeConn
	relays    chan *fakeRelay
	unmounts  atomic.Int32
	logs      []string
}

func newHarness(failDials int) (*relayHarness, *shareRelay) {
	h := &relayHarness{failDials: failDials, relays: make(chan *fakeRelay, 16)}
	s := &shareRelay{
		queues: 4,
		logf: func(f string, a ...any) {
			h.mu.Lock()
			h.logs = append(h.logs, fmt.Sprintf(f, a...))
			h.mu.Unlock()
		},
		dial: func() (io.ReadWriteCloser, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.dials++
			if h.dials <= h.failDials {
				return nil, errors.New("connection timed out")
			}
			c := &fakeConn{}
			h.conns = append(h.conns, c)
			return c, nil
		},
		start: func(conns []io.ReadWriteCloser) (relayHandle, error) {
			if len(conns) != 4 {
				return nil, fmt.Errorf("%d conns", len(conns))
			}
			r := &fakeRelay{ended: make(chan error, 1)}
			h.relays <- r
			return r, nil
		},
		unmount:    func() { h.unmounts.Add(1) },
		minBackoff: time.Millisecond, maxBackoff: 4 * time.Millisecond,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	return h, s
}

func (h *relayHarness) next(t *testing.T) *fakeRelay {
	t.Helper()
	select {
	case r := <-h.relays:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no mount")
		return nil
	}
}

// The host is not listening at first: the loop retries with backoff, logs the failure
// once, mounts when the host answers, remounts after the session ends, and shuts down
// cleanly (no goroutine left: TestMain checks).
func TestShareRelayRetriesAndRemounts(t *testing.T) {
	h, s := newHarness(25)
	go s.run()
	r1 := h.next(t)
	r1.ended <- errors.New("relay: read from host: EOF") // the host went away
	r2 := h.next(t)
	if !r1.closed.Load() {
		t.Fatal("the ended relay was not closed")
	}
	s.shutdown()
	s.shutdown() // idempotent
	if !r2.closed.Load() {
		t.Fatal("shutdown did not close the live relay")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dials != 25+8 {
		t.Fatalf("dials = %d", h.dials)
	}
	n := 0
	for _, l := range h.logs {
		if strings.Contains(l, "connection timed out") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the dial failure was logged %d times: %q", n, h.logs)
	}
	if h.unmounts.Load() < 2 {
		t.Fatalf("stale mount not detached before remounting")
	}
}

// A failed dial in the middle of a set closes the connections already open.
func TestShareRelayPartialDialClosesConns(t *testing.T) {
	h, s := newHarness(0)
	var calls atomic.Int32
	dial := s.dial
	s.dial = func() (io.ReadWriteCloser, error) {
		if calls.Add(1) == 3 {
			return nil, errors.New("refused")
		}
		return dial()
	}
	go s.run()
	h.next(t)
	s.shutdown()
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, c := range h.conns[:2] {
		if !c.closed.Load() {
			t.Fatalf("conn %d of the failed set left open", i)
		}
	}
}

// Shutdown while the host never answers returns promptly.
func TestShareRelayShutdownWhileRetrying(t *testing.T) {
	_, s := newHarness(1 << 30)
	go s.run()
	time.Sleep(20 * time.Millisecond)
	done := make(chan struct{})
	go func() { s.shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown hung")
	}
}
