package guestctl

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Higangssh/winbox/internal/spaceguard"
)

type fakeHandler struct {
	mu        sync.Mutex
	budgets   []spaceguard.Budget
	lost      int
	trims     int
	trimGate  chan struct{} // if set, Trim waits for it
	trimStart chan struct{} // if set, Trim signals it on entry
	cacheRes  CacheResult
	status    Status
	budgetHit chan struct{}
}

func (h *fakeHandler) SetBudget(b spaceguard.Budget) {
	h.mu.Lock()
	h.budgets = append(h.budgets, b)
	h.mu.Unlock()
	if h.budgetHit != nil {
		select {
		case h.budgetHit <- struct{}{}:
		default:
		}
	}
}
func (h *fakeHandler) ConnLost() { h.mu.Lock(); h.lost++; h.mu.Unlock() }
func (h *fakeHandler) Trim(ctx context.Context) TrimResult {
	if h.trimStart != nil {
		h.trimStart <- struct{}{}
	}
	if h.trimGate != nil {
		select {
		case <-h.trimGate:
		case <-ctx.Done():
		}
	}
	h.mu.Lock()
	h.trims++
	h.mu.Unlock()
	return TrimResult{Trimmed: 123, Duration: time.Millisecond}
}
func (h *fakeHandler) CacheTrim(context.Context) CacheResult { return h.cacheRes }
func (h *fakeHandler) Status() Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}

func startPair(t *testing.T, h *fakeHandler, srv *Server, cfg ClientConfig) (*Client, chan error) {
	t.Helper()
	a, b := net.Pipe()
	srv.Handler = h
	errc := make(chan error, 1)
	go func() { errc <- srv.ServeConn(context.Background(), b) }()
	c, err := NewClient(context.Background(), a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, errc
}

func TestClientServer(t *testing.T) {
	h := &fakeHandler{status: Status{Guard: GuardOK, FSSize: 100}, cacheRes: CacheResult{Method: CacheDrop, Before: 10, After: 1}, budgetHit: make(chan struct{}, 1)}
	got := make(chan Status, 16)
	c, _ := startPair(t, h, &Server{Features: knownFeatures, Instance: 9, StatusEvery: 10 * time.Millisecond},
		ClientConfig{OnStatus: func(s Status) {
			select {
			case got <- s:
			default:
			}
		}})
	if c.Hello().Instance != 9 || c.Hello().Features != knownFeatures {
		t.Fatalf("hello %+v", c.Hello())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.SetBudget(spaceguard.Budget{Bytes: 1, Backed: 2})
	<-h.budgetHit
	h.mu.Lock()
	if len(h.budgets) != 1 || h.budgets[0] != (spaceguard.Budget{Bytes: 1, Backed: 2}) {
		t.Errorf("budgets %v", h.budgets)
	}
	h.mu.Unlock()
	tr, err := c.Trim(ctx)
	if err != nil || tr.Trimmed != 123 {
		t.Fatalf("trim %+v %v", tr, err)
	}
	cr, err := c.CacheTrim(ctx)
	if err != nil || cr != h.cacheRes {
		t.Fatalf("cache %+v %v", cr, err)
	}
	if _, err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-got:
		if s.FSSize != 100 {
			t.Errorf("status %+v", s)
		}
	case <-ctx.Done():
		t.Fatal("no status")
	}
}

func TestServerBusyAndUnsupported(t *testing.T) {
	h := &fakeHandler{trimGate: make(chan struct{}), trimStart: make(chan struct{}, 1)}
	c, _ := startPair(t, h, &Server{Features: FeatureTrim}, ClientConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first := make(chan error, 1)
	go func() { _, err := c.Trim(ctx); first <- err }()
	// Once the first trim is running, a second one must be refused.
	<-h.trimStart
	_, err := c.Trim(ctx)
	var re *RefusedError
	if !errors.As(err, &re) || re.Code != ErrBusy {
		t.Fatalf("second trim: %v", err)
	}
	close(h.trimGate)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if _, err := c.CacheTrim(ctx); !errors.As(err, &re) || re.Code != ErrUnsupported {
		t.Fatalf("cache trim: %v", err)
	}
}

// guestConn runs a fake guest that answers the hello and then writes raw frames.
func guestConn(t *testing.T, frames ...[]byte) (*Client, error) {
	t.Helper()
	a, b := net.Pipe()
	go func() {
		var buf [MaxBody]byte
		if _, err := ReadFrame(b, &buf); err != nil {
			return
		}
		hr, _ := Encode(0, HelloReply{Version: 1})
		b.Write(hr)
		for _, f := range frames {
			if _, err := b.Write(f); err != nil {
				return
			}
		}
		// Drain whatever the host sends.
		for {
			if _, err := ReadFrame(b, &buf); err != nil {
				return
			}
		}
	}()
	c, err := NewClient(context.Background(), a, ClientConfig{ReadTimeout: time.Second})
	if err != nil {
		return nil, err
	}
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		c.Close()
		t.Fatal("client did not close")
	}
	return c, c.Err()
}

func TestClientCutsOffBadGuest(t *testing.T) {
	pong, _ := Encode(5, Pong{Nonce: 1})
	trim, _ := Encode(0, TrimResult{})
	hello, _ := Encode(0, HelloReply{Version: 1})
	huge := []byte{0xff, 0xff, 0xff, 0xff}
	st, _ := Encode(1, Status{})
	var flood []byte
	for i := 0; i < 200; i++ {
		flood = append(flood, st...)
	}
	for name, f := range map[string][]byte{
		"unsolicited pong": pong,
		"unsolicited trim": trim,
		"second hello":     hello,
		"huge length":      huge,
		"status flood":     flood,
	} {
		_, err := guestConn(t, f)
		if !errors.Is(err, ErrViolation) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestClientReadTimeout(t *testing.T) {
	_, err := guestConn(t)
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("silent guest: %v", err)
	}
}

func TestServerRejectsNonHelloFirst(t *testing.T) {
	a, b := net.Pipe()
	h := &fakeHandler{}
	errc := make(chan error, 1)
	go func() { errc <- (&Server{Handler: h}).ServeConn(context.Background(), b) }()
	f, _ := Encode(1, Budget{spaceguard.Budget{Bytes: 1}})
	a.Write(f)
	if err := <-errc; !errors.Is(err, ErrMalformed) {
		t.Fatalf("got %v", err)
	}
	if h.lost != 1 || len(h.budgets) != 0 {
		t.Fatalf("lost %d budgets %v", h.lost, h.budgets)
	}
}

func TestSupervisorResendsBudget(t *testing.T) {
	h := &fakeHandler{budgetHit: make(chan struct{}, 1)}
	srv := &Server{Handler: h}
	var mu sync.Mutex
	var serverSide []net.Conn
	sup := &Supervisor{
		Dial: func(ctx context.Context) (net.Conn, error) {
			a, b := net.Pipe()
			mu.Lock()
			serverSide = append(serverSide, b)
			mu.Unlock()
			go srv.ServeConn(context.Background(), b)
			return a, nil
		},
		MinBackoff: time.Millisecond,
	}
	sup.SetBudget(spaceguard.Budget{Bytes: 7})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)
	<-h.budgetHit
	mu.Lock()
	serverSide[0].Close() // the guest daemon restarts
	mu.Unlock()
	<-h.budgetHit
	if n := sup.Connections(); n < 2 {
		t.Fatalf("connections %d", n)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, b := range h.budgets {
		if b.Bytes != 7 {
			t.Fatalf("budget %v", b)
		}
	}
}
