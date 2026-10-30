package guestctl

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Higangssh/hyperbutler/internal/spaceguard"
)

// Handler is the guest side of the protocol: what the guest daemon does with the host's
// messages. Server calls SetBudget and ConnLost from its reader goroutine, Status from its
// status goroutine, and Trim and CacheTrim from worker goroutines (at most one of each at
// a time). Implementations must be safe for that.
type Handler interface {
	// SetBudget applies a new host budget.
	SetBudget(spaceguard.Budget)
	// ConnLost says the connection that sent the budgets is gone: the guard must fail
	// closed until a new budget arrives.
	ConnLost()
	// Trim runs FITRIM on the data file system.
	Trim(ctx context.Context) TrimResult
	// CacheTrim runs the page-cache reclaim.
	CacheTrim(ctx context.Context) CacheResult
	// Status reports the guest's state.
	Status() Status
}

// Shutdowner is implemented by a Handler that can power the guest off. Server calls
// Shutdown from its reader goroutine; it must start the shutdown and return at once.
// The guest daemon advertises FeatureShutdown only with such a Handler.
type Shutdowner interface {
	Shutdown() error
}

// Server serves one host connection at a time (the guest daemon closes the previous one
// when a new one arrives).
type Server struct {
	Handler  Handler
	Features Features
	// Instance identifies this run of the guest daemon (random at start).
	Instance uint64
	// StatusEvery is the status report period (default 1 s).
	StatusEvery time.Duration
	// HelloTimeout bounds the wait for the host's Hello (default 5 s).
	HelloTimeout time.Duration
	// WriteTimeout bounds each frame write (default 5 s). A host that stops reading
	// loses the connection instead of blocking the guest.
	WriteTimeout time.Duration
	// Logf, if set, receives protocol errors.
	Logf func(format string, args ...any)
}

func (s *Server) defaults() (status, hello, write time.Duration) {
	status, hello, write = s.StatusEvery, s.HelloTimeout, s.WriteTimeout
	if status <= 0 {
		status = time.Second
	}
	if hello <= 0 {
		hello = 5 * time.Second
	}
	if write <= 0 {
		write = 5 * time.Second
	}
	return
}

// outQueue is how many frames may wait for the writer. Replies are bounded by the
// in-flight rules (one trim, one cache trim) plus pings; the reader blocks when it is
// full, which pushes back on the host.
const outQueue = 16

// ServeConn runs the protocol on conn until it fails, ctx ends, or the host breaks the
// protocol. It always closes conn and calls Handler.ConnLost before returning.
func (s *Server) ServeConn(ctx context.Context, conn net.Conn) error {
	statusEvery, helloTimeout, writeTimeout := s.defaults()
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		_ = conn.Close()
		// Fail closed first: a running FITRIM cannot be interrupted, and the guard must
		// not keep the old budget while ServeConn waits for it.
		s.Handler.ConnLost()
		wg.Wait()
	}()
	go func() {
		<-ctx.Done()
		_ = conn.Close() // unblocks the reader
	}()

	var buf [MaxBody]byte
	_ = conn.SetReadDeadline(time.Now().Add(helloTimeout))
	body, err := ReadFrame(conn, &buf)
	if err != nil {
		return fmt.Errorf("guestctl: read hello: %w", err)
	}
	seq, m, err := Decode(body, FromHost)
	if err != nil {
		return err
	}
	h, ok := m.(Hello)
	if !ok {
		return fmt.Errorf("%w: first frame is %v, not hello", ErrMalformed, m.Type())
	}
	if h.Min > Version || h.Max < Version {
		return fmt.Errorf("%w: host speaks %d..%d", ErrVersion, h.Min, h.Max)
	}
	_ = conn.SetReadDeadline(time.Time{})

	out := make(chan []byte, outQueue)
	writeErr := make(chan error, 1)
	send := func(seq uint32, m Msg) bool {
		f, err := Encode(seq, m)
		if err != nil { // a Handler returned an invalid value; never send it
			s.logf("guestctl: not sending %v: %v", m.Type(), err)
			return true
		}
		select {
		case out <- f:
			return true
		case <-ctx.Done():
			return false
		}
	}
	wg.Add(1)
	go func() { // writer
		defer wg.Done()
		for {
			select {
			case f := <-out:
				_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
				if _, err := conn.Write(f); err != nil {
					writeErr <- err
					cancel()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	if !send(seq, HelloReply{Version: Version, Features: s.Features & knownFeatures, Instance: s.Instance}) {
		return ctx.Err()
	}
	wg.Add(1)
	go func() { // status reports
		defer wg.Done()
		t := time.NewTicker(statusEvery)
		defer t.Stop()
		var n uint32
		for {
			n++
			// Statuses are periodic: when the writer is backed up, drop this one rather
			// than queue behind it.
			if f, err := Encode(n, s.Handler.Status()); err == nil {
				select {
				case out <- f:
				default:
				}
			} else {
				s.logf("guestctl: bad status: %v", err)
			}
			select {
			case <-t.C:
			case <-ctx.Done():
				return
			}
		}
	}()

	var trimBusy, cacheBusy bool
	var busyMu sync.Mutex
	run := func(busy *bool, seq uint32, req Type, fn func() Msg) {
		busyMu.Lock()
		if *busy {
			busyMu.Unlock()
			send(seq, ErrorReply{Code: ErrBusy, Request: req})
			return
		}
		*busy = true
		busyMu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := fn()
			busyMu.Lock()
			*busy = false
			busyMu.Unlock()
			send(seq, r)
		}()
	}

	for {
		body, err := ReadFrame(conn, &buf)
		if err != nil {
			select {
			case werr := <-writeErr:
				return fmt.Errorf("guestctl: write: %w", werr)
			default:
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		seq, m, err := Decode(body, FromHost)
		if err != nil {
			return err
		}
		switch v := m.(type) {
		case Budget:
			s.Handler.SetBudget(v.Budget)
		case TrimRequest:
			if s.Features&FeatureTrim == 0 {
				send(seq, ErrorReply{Code: ErrUnsupported, Request: TTrim})
				continue
			}
			run(&trimBusy, seq, TTrim, func() Msg { return s.Handler.Trim(ctx) })
		case CacheTrimRequest:
			if s.Features&FeatureCacheTrim == 0 {
				send(seq, ErrorReply{Code: ErrUnsupported, Request: TCacheTrim})
				continue
			}
			run(&cacheBusy, seq, TCacheTrim, func() Msg { return s.Handler.CacheTrim(ctx) })
		case Ping:
			if !send(seq, Pong(v)) {
				return ctx.Err()
			}
		case ShutdownRequest:
			sd, ok := s.Handler.(Shutdowner)
			if s.Features&FeatureShutdown == 0 || !ok {
				send(seq, ErrorReply{Code: ErrUnsupported, Request: TShutdown})
				continue
			}
			if err := sd.Shutdown(); err != nil {
				s.logf("guestctl: shutdown: %v", err)
				send(seq, ErrorReply{Code: ErrUnsupported, Request: TShutdown})
				continue
			}
			send(seq, ShutdownAccepted{})
		default: // a second Hello
			return fmt.Errorf("%w: unexpected %v", ErrMalformed, m.Type())
		}
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// ErrClosed is returned for requests on a closed client.
var ErrClosed = errors.New("guestctl: connection closed")
