package diag

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const (
	// MaxConns bounds the debug connections served at once (ADR 0003, guardrail 2).
	// Further clients wait in the pipe's accept queue.
	MaxConns = 4
	// MaxProfileSeconds caps ?seconds= on every /debug/pprof route.
	MaxProfileSeconds = 60

	readHeaderTimeout = 5 * time.Second
	// writeTimeout must exceed MaxProfileSeconds, or pprof refuses the request.
	writeTimeout = (MaxProfileSeconds + 15) * time.Second
	idleTimeout  = 30 * time.Second
)

// StatsPath is where Handler serves the Stats document.
const StatsPath = "/debug/stats"

// Handler returns the debug endpoint's routes:
//
//	/debug/stats          Stats as JSON
//	/debug/vars/NAME      what was published as NAME (Publish), as JSON
//	/debug/pprof/...      net/http/pprof (index, named profiles, profile, trace, ...)
//
// It does not use http.DefaultServeMux, so importing net/http/pprof elsewhere never
// widens it.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+StatsPath, serveStats)
	mux.HandleFunc("GET "+VarsPath+"{name}", serveVar)
	// Every pprof route gets the cap: Index serves the named profiles, which take
	// ?seconds= for delta profiles too.
	mux.HandleFunc("GET /debug/pprof/", limitSeconds(pprof.Index))
	mux.HandleFunc("GET /debug/pprof/cmdline", limitSeconds(pprof.Cmdline))
	mux.HandleFunc("GET /debug/pprof/profile", limitSeconds(pprof.Profile))
	mux.HandleFunc("GET /debug/pprof/symbol", limitSeconds(pprof.Symbol))
	mux.HandleFunc("GET /debug/pprof/trace", limitSeconds(pprof.Trace))
	return mux
}

func serveStats(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Snapshot())
}

// limitSeconds rejects a request unless every ?seconds= value is a number in
// [0, MaxProfileSeconds]. pprof reads the first value; checking all of them means no
// later duplicate can matter either.
func limitSeconds(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			http.Error(w, "diag: bad query", http.StatusBadRequest)
			return
		}
		for _, v := range q["seconds"] {
			if _, err := ParseSeconds(v); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		h(w, r)
	}
}

// ParseSeconds parses a pprof ?seconds= value. It accepts what pprof itself accepts (a
// decimal number; trace takes fractions) but only within [0, MaxProfileSeconds], so a
// request cannot hold a connection slot for longer than the cap.
func ParseSeconds(v string) (float64, error) {
	if len(v) > 32 {
		return 0, errors.New("diag: seconds is too long")
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || f < 0 || f > MaxProfileSeconds {
		return 0, fmt.Errorf("diag: seconds must be a number from 0 to %d", MaxProfileSeconds)
	}
	return f, nil
}

// Server is a running debug endpoint.
type Server struct {
	srv      *http.Server
	served   chan error
	once     sync.Once
	closeErr error

	mu      sync.Mutex
	idle    *sync.Cond // signaled when active drops to 0
	active  int        // handlers running
	closing bool       // Close has begun; new handlers are refused
}

// serve runs the debug endpoint on ln until Close. It owns ln.
func serve(ln net.Listener) *Server {
	s := &Server{served: make(chan error, 1)}
	s.idle = sync.NewCond(&s.mu)
	inner := Handler()
	s.srv = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Tracked so Close can wait for handlers that outlive their connection. A
			// handler that starts after Close began is refused instead, so the count
			// never rises from zero while Close waits.
			if !s.enter() {
				http.Error(w, "diag: shutting down", http.StatusServiceUnavailable)
				return
			}
			defer s.leave()
			inner.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    8 << 10,
	}
	lim := newLimitListener(ln, MaxConns)
	go func() { s.served <- s.srv.Serve(lim) }()
	return s
}

// Close stops the endpoint: it closes the listener and every open connection, then
// waits for the accept loop and all running handlers to return. pprof's profile and
// trace handlers stop early when their connection closes.
func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	s.once.Do(func() {
		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()
		err := s.srv.Close()
		if serr := <-s.served; serr != nil && !errors.Is(serr, http.ErrServerClosed) && err == nil {
			err = serr
		}
		s.mu.Lock()
		for s.active > 0 {
			s.idle.Wait()
		}
		s.mu.Unlock()
		s.closeErr = err
	})
	return s.closeErr
}

func (s *Server) enter() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.active++
	return true
}

func (s *Server) leave() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active--
	if s.active == 0 {
		s.idle.Broadcast()
	}
}

// limitListener holds at most n accepted connections open at once.
type limitListener struct {
	net.Listener
	sem  chan struct{}
	done chan struct{}
	once sync.Once
}

func newLimitListener(ln net.Listener, n int) *limitListener {
	return &limitListener{Listener: ln, sem: make(chan struct{}, n), done: make(chan struct{})}
}

func (l *limitListener) Accept() (net.Conn, error) {
	select {
	case l.sem <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitConn{Conn: c, release: func() { <-l.sem }}, nil
}

func (l *limitListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
