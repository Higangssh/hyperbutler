package main

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// shareRelay keeps the host's bind-mount file system (ADR 0007) mounted: it dials the
// host's share server (queues connections), mounts it with internal/share/relay, and
// starts over with a bounded backoff whenever the host is not listening (yet) or the
// session ends. The loop is portable; share_linux.go supplies the vsock dialer, the
// relay and the unmount.
type shareRelay struct {
	queues int
	logf   func(string, ...any)
	// dial opens one connection to the host's share server.
	dial func() (io.ReadWriteCloser, error)
	// start mounts the file system over conns (relay.Start).
	start func(conns []io.ReadWriteCloser) (relayHandle, error)
	// unmount detaches a mount an earlier relay left (it is dead).
	unmount func()
	// minBackoff and maxBackoff bound the wait between attempts.
	minBackoff, maxBackoff time.Duration

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// relayHandle is the part of *relay.Relay the loop uses.
type relayHandle interface {
	Wait() error
	Close() error
}

// run mounts and remounts until shutdown is called.
func (s *shareRelay) run() {
	defer close(s.done)
	var backoff time.Duration
	var lastErr string
	// retry logs err (each distinct failure once: the host may simply not serve shares)
	// and waits; it reports false when shutdown was called meanwhile.
	retry := func(err error) bool {
		if msg := err.Error(); msg != lastErr {
			s.logf("shares: %v; retrying (backoff up to %v)", err, s.maxBackoff)
			lastErr = msg
		}
		backoff = min(max(2*backoff, s.minBackoff), s.maxBackoff)
		t := time.NewTimer(backoff)
		defer t.Stop()
		select {
		case <-t.C:
			return true
		case <-s.stop:
			return false
		}
	}
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		s.unmount()
		conns, err := s.dialAll()
		if err != nil {
			if !retry(err) {
				return
			}
			continue
		}
		r, err := s.start(conns)
		if err != nil {
			closeAll(conns)
			if !retry(err) {
				return
			}
			continue
		}
		s.logf("shares: mounted (%d queues)", len(conns))
		backoff, lastErr = 0, ""
		ended := make(chan error, 1)
		go func() { ended <- r.Wait() }()
		select {
		case err := <-ended:
			_ = r.Close()
			if err == nil {
				err = errors.New("unmounted")
			}
			if !retry(fmt.Errorf("session ended: %w", err)) {
				return
			}
		case <-s.stop:
			_ = r.Close()
			<-ended //nolint:errcheck // ends because r was closed; stopping, nothing to report
			return
		}
	}
}

func (s *shareRelay) dialAll() ([]io.ReadWriteCloser, error) {
	var conns []io.ReadWriteCloser
	for len(conns) < s.queues {
		c, err := s.dial()
		if err != nil {
			closeAll(conns)
			return nil, fmt.Errorf("connect to the host: %w", err)
		}
		conns = append(conns, c)
	}
	return conns, nil
}

// shutdown unmounts, stops the loop and waits for it. It may be called more than once.
func (s *shareRelay) shutdown() {
	s.stopOnce.Do(func() { close(s.stop) })
	<-s.done
}

func closeAll(conns []io.ReadWriteCloser) {
	for _, c := range conns {
		_ = c.Close()
	}
}
