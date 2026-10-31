package guestctl

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Higangssh/hyperbutler/internal/spaceguard"
)

// Supervisor keeps one control connection to the guest open: it dials, runs the
// handshake, re-sends the newest budget on every new connection, and redials with a
// capped backoff when the connection fails (the guest is still booting, hyperbutlerd was
// restarted, or the guest broke the protocol).
type Supervisor struct {
	// Dial opens a connection to the guest's control service, for example
	// HvsockDialer(runtimeID) on Windows.
	Dial func(ctx context.Context) (net.Conn, error)
	// Config is passed to every Client.
	Config ClientConfig
	// DialTimeout bounds one connection attempt, handshake included (2 s). A connect to a
	// guest port nobody listens on yet stays pending in hvsock for about 30 s, so a short
	// timeout and a retry find a booting guest sooner.
	DialTimeout time.Duration
	// MinBackoff and MaxBackoff bound the redial delay (100 ms and 5 s).
	MinBackoff, MaxBackoff time.Duration
	// Logf, if set, receives connection events.
	Logf func(format string, args ...any)
	// OnConnect, if set, runs in its own goroutine after every handshake, with a context
	// that ends when that connection does. Run waits for it before returning.
	OnConnect func(ctx context.Context, c *Client)

	hooks  sync.WaitGroup
	mu     sync.Mutex
	client *Client
	budget *spaceguard.Budget
	paused bool
	conns  int
	kick   chan struct{} // cut a backoff wait short (resume)
}

func (s *Supervisor) kickChan() chan struct{} {
	if s.kick == nil {
		s.kick = make(chan struct{}, 1)
	}
	return s.kick
}

// Run keeps the connection up until ctx ends.
func (s *Supervisor) Run(ctx context.Context) error {
	minB, maxB := s.MinBackoff, s.MaxBackoff
	if minB <= 0 {
		minB = 100 * time.Millisecond
	}
	if maxB < minB {
		maxB = max(minB, 5*time.Second)
	}
	s.mu.Lock()
	kick := s.kickChan()
	s.mu.Unlock()
	backoff := minB
	defer s.hooks.Wait()
	for {
		c, err := s.connect(ctx)
		if err == nil {
			t0 := time.Now()
			select {
			case <-c.Done():
				s.logf("guestctl: connection lost: %v", c.Err())
			case <-ctx.Done():
				c.Close()
			}
			s.mu.Lock()
			s.client = nil
			s.mu.Unlock()
			// Reset the backoff only after a connection that lasted: a guest that
			// completes the handshake and then drops or breaks the protocol must not
			// get the host redialing at the minimum rate forever.
			if time.Since(t0) >= maxB {
				backoff = minB
			}
		} else if ctx.Err() == nil {
			s.logf("guestctl: connect: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		case <-kick:
			backoff = minB
		}
		backoff = min(2*backoff, maxB)
	}
}

func (s *Supervisor) connect(ctx context.Context) (*Client, error) {
	timeout := s.DialTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := s.Dial(dctx)
	if err != nil {
		return nil, err
	}
	c, err := NewClient(dctx, conn, s.Config)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.client = c
	s.conns++
	// Under s.mu, so a concurrent SetBudget cannot be overwritten by an older budget.
	if s.paused {
		c.Suspend(true)
	} else if s.budget != nil {
		c.SetBudget(*s.budget)
	}
	s.mu.Unlock()
	h := c.Hello()
	s.logf("guestctl: connected (guest instance %016x, features 0x%x)", h.Instance, uint32(h.Features))
	if s.OnConnect != nil {
		hctx, cancel := context.WithCancel(context.Background())
		s.hooks.Add(2)
		go func() {
			defer s.hooks.Done()
			defer cancel()
			select {
			case <-c.Done():
			case <-hctx.Done():
			}
		}()
		go func() {
			defer s.hooks.Done()
			defer cancel()
			s.OnConnect(hctx, c)
		}()
	}
	return c, nil
}

// SetBudget sends b now if connected, and on every later connection. While paused it
// is only remembered.
func (s *Supervisor) SetBudget(b spaceguard.Budget) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.budget = &b
	if s.client != nil && !s.paused {
		s.client.SetBudget(b)
	}
}

// SetPaused tells the supervisor that the host paused (true) or resumed (false) the VM.
// While paused, the connection's read timeout is off and budgets are held back, so
// neither a silent guest nor a full send buffer tears the connection down. On resume the
// newest budget is sent.
func (s *Supervisor) SetPaused(paused bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paused = paused
	if s.client == nil {
		if !paused {
			// HcsPauseComputeSystem aborts hvsock connections, so after a resume the
			// guest's guard is counting down its lost grace: redial now.
			select {
			case s.kickChan() <- struct{}{}:
			default:
			}
		}
		return
	}
	s.client.Suspend(paused)
	if !paused && s.budget != nil {
		s.client.SetBudget(*s.budget)
	}
}

// Shutdown asks the guest to power off in order over the current connection, waiting
// up to ctx for one if the guest is still connecting.
func (s *Supervisor) Shutdown(ctx context.Context) error {
	for {
		if c := s.Client(); c != nil {
			return c.Shutdown(ctx)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("guestctl: no control connection: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Client returns the current client, or nil while disconnected.
func (s *Supervisor) Client() *Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

// Connections counts successful handshakes so far.
func (s *Supervisor) Connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

func (s *Supervisor) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}
