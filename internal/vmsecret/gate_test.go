package vmsecret

import (
	"errors"
	"net"
	"testing"
	"time"
)

// A stalled peer (31 bytes, then nothing) holds up no one: the host connecting right
// after it is admitted at once, and the stalled one is refused after the timeout.
func TestGateStalledPeerThenHost(t *testing.T) {
	s := testSecret()
	g := &Gate{Secret: s, Timeout: time.Second}
	type res struct {
		name string
		err  error
		at   time.Duration
	}
	out := make(chan res, 2)
	start := time.Now()

	stalled, stalledG := net.Pipe()
	defer stalled.Close()
	g.Go(stalledG, func(err error) { out <- res{"stalled", err, time.Since(start)} })
	go func() { _, _ = stalled.Write(s[:Size-1]) }()

	host, hostG := net.Pipe()
	defer host.Close()
	g.Go(hostG, func(err error) { out <- res{"host", err, time.Since(start)} })
	go func() { _ = Send(host, s) }()

	first := <-out
	if first.name != "host" || first.err != nil || first.at > 500*time.Millisecond {
		t.Fatalf("first result %+v, want the host admitted at once", first)
	}
	second := <-out
	if second.name != "stalled" || !errors.Is(second.err, ErrMismatch) || second.at > 3*time.Second {
		t.Fatalf("second result %+v", second)
	}
	_ = hostG.Close()
}

// Over MaxPending, a new connection is closed at once with ErrBusy; a slot frees when a
// check ends.
func TestGatePendingBound(t *testing.T) {
	s := testSecret()
	g := &Gate{Secret: s, Timeout: 300 * time.Millisecond, MaxPending: 2}
	done := make(chan error, 3)
	var peers []net.Conn
	for range 2 {
		a, b := net.Pipe()
		peers = append(peers, a)
		g.Go(b, func(err error) { done <- err })
	}
	a, b := net.Pipe()
	defer a.Close()
	busy := make(chan error, 1)
	g.Go(b, func(err error) { busy <- err })
	select {
	case err := <-busy:
		if !errors.Is(err, ErrBusy) {
			t.Fatalf("third: %v, want ErrBusy", err)
		}
	default:
		t.Fatal("ErrBusy was not reported before Go returned")
	}
	if _, err := a.Read(make([]byte, 1)); err == nil {
		t.Fatal("the busy connection was not closed")
	}
	for range 2 {
		if err := <-done; !errors.Is(err, ErrMismatch) {
			t.Fatalf("silent peer: %v", err)
		}
	}
	for _, p := range peers {
		p.Close()
	}
	// Free again.
	c, d := net.Pipe()
	defer c.Close()
	ok := make(chan error, 1)
	g.Go(d, func(err error) { ok <- err })
	if err := Send(c, s); err != nil {
		t.Fatal(err)
	}
	if err := <-ok; err != nil {
		t.Fatal(err)
	}
	d.Close()
}

// The zero Gate admits no one.
func TestGateZero(t *testing.T) {
	var g Gate
	a, b := net.Pipe()
	defer a.Close()
	done := make(chan error, 1)
	g.Go(b, func(err error) { done <- err })
	_, _ = a.Write(make([]byte, Size))
	if err := <-done; !errors.Is(err, ErrMismatch) {
		t.Fatalf("zero gate: %v", err)
	}
}
