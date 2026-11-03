package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultDialTimeout bounds how long one upstream dial may take.
	DefaultDialTimeout = 10 * time.Second
	// DefaultMaxConns bounds concurrent proxied connections. When the limit is reached
	// the proxy stops accepting, so new clients wait (named-pipe clients see the pipe as
	// busy) instead of exhausting host or guest resources.
	DefaultMaxConns = 1024

	defaultPeerPollInterval = time.Second
	copyBufferSize          = 64 << 10
	maxAcceptBackoff        = time.Second
)

// Proxy forwards every connection accepted on a listener to a fresh upstream connection.
// The zero value is not usable: Upstream must be set. A Proxy may serve several
// listeners at once; its fields must not change while it is serving.
type Proxy struct {
	// Upstream opens the connection to the guest for each accepted client.
	Upstream Dialer
	// DialTimeout bounds each upstream dial. Zero means DefaultDialTimeout.
	DialTimeout time.Duration
	// MaxConns bounds concurrent connections. Zero means DefaultMaxConns.
	MaxConns int
	// Logger receives connection-level errors. Nil discards them.
	Logger *slog.Logger
	// RewriteCreate, if set, rewrites the body of every POST /containers/create (see
	// rewrite.go); all other traffic still passes unchanged. Nil keeps the proxy
	// byte-level.
	RewriteCreate CreateRewriter
	// CheckStart, if set, is asked before every POST /containers/{name}/start and can
	// refuse it (see rewrite.go). Like RewriteCreate, it makes the proxy follow request
	// boundaries in the client-to-guest direction.
	CheckStart StartChecker

	// peerPollInterval is how often a half-closed client is checked for having gone
	// away entirely. Zero means defaultPeerPollInterval. Tests shorten it.
	peerPollInterval time.Duration
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, copyBufferSize); return &b }}

// Serve accepts connections on ln and forwards each one until ctx is done or ln fails.
//
// Serve takes ownership of ln and closes it before returning. When ctx is done, Serve
// stops accepting, closes every active client and upstream connection, waits for all of
// its goroutines to exit, and returns nil. If ln fails with a non-temporary error, Serve
// does the same cleanup and returns that error.
func (p *Proxy) Serve(ctx context.Context, ln net.Listener) error {
	if p.Upstream == nil {
		_ = ln.Close()
		return errors.New("transport: Proxy.Upstream is nil")
	}
	ctx, cancel := context.WithCancel(ctx)
	stopCloser := context.AfterFunc(ctx, func() { _ = ln.Close() })

	var (
		wg      sync.WaitGroup
		sem     = make(chan struct{}, p.maxConns())
		backoff time.Duration
		err     error
	)
	for {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		conn, aerr := ln.Accept()
		if aerr != nil {
			<-sem
			if ctx.Err() != nil {
				break
			}
			if errors.Is(aerr, net.ErrClosed) {
				err = fmt.Errorf("transport: accept: %w", aerr)
				break
			}
			// Unknown accept errors are retried with a capped backoff so a failing
			// listener cannot spin the CPU.
			backoff = min(max(2*backoff, 5*time.Millisecond), maxAcceptBackoff)
			p.logger().Warn("transport: accept failed; retrying", "err", aerr, "backoff", backoff)
			t := time.NewTimer(backoff)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
			}
			continue
		}
		backoff = 0
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			p.handle(ctx, conn)
		}()
	}

	cancel()
	stopCloser()
	_ = ln.Close()
	wg.Wait()
	return err
}

func (p *Proxy) handle(ctx context.Context, client net.Conn) {
	dctx, dcancel := context.WithTimeout(ctx, p.dialTimeout())
	upstream, err := p.Upstream.DialContext(dctx)
	dcancel()
	if err != nil {
		_ = client.Close()
		if ctx.Err() == nil {
			p.logger().Warn("transport: upstream dial failed", "err", err)
		}
		return
	}
	var h *filterHooks
	if p.RewriteCreate != nil || p.CheckStart != nil {
		h = &filterHooks{rewrite: p.RewriteCreate, checkStart: p.CheckStart}
	}
	forward(ctx, client, upstream, p.pollInterval(), p.logger(), h)
}

func (p *Proxy) dialTimeout() time.Duration {
	if p.DialTimeout > 0 {
		return p.DialTimeout
	}
	return DefaultDialTimeout
}

func (p *Proxy) maxConns() int {
	if p.MaxConns > 0 {
		return p.MaxConns
	}
	return DefaultMaxConns
}

func (p *Proxy) pollInterval() time.Duration {
	if p.peerPollInterval > 0 {
		return p.peerPollInterval
	}
	return defaultPeerPollInterval
}

func (p *Proxy) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return discardLogger
}

var discardLogger = slog.New(slog.DiscardHandler)

// closeWriter is implemented by connections that can shut down their send side only.
type closeWriter interface {
	CloseWrite() error
}

// peerProber is implemented by client connections that can tell, without reading,
// whether the peer has closed its end completely. Named pipes need it: a zero-byte
// half-close message and a full disconnect both read as io.EOF.
type peerProber interface {
	peerClosed() bool
}

// unlockedCloseWriter is implemented by connections whose CloseWrite synchronizes with
// Close on its own and may wait until Close is called. halfConn must not hold its lock
// around such a CloseWrite, or Close could never run.
type unlockedCloseWriter interface {
	closeWriter
	closeWriteWithoutLock()
}

var errNoHalfClose = errors.New("transport: connection does not support CloseWrite")

// halfConn serializes CloseWrite against Close, for connections whose CloseWrite is not
// safe to overlap with Close (winipc's are, but other net.Conn implementations need not
// be). Read and Write are not wrapped; Close unblocks them.
type halfConn struct {
	conn   net.Conn
	mu     sync.Mutex
	closed bool
}

func (h *halfConn) CloseWrite() error {
	if u, ok := h.conn.(unlockedCloseWriter); ok {
		return u.CloseWrite()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return net.ErrClosed
	}
	cw, ok := h.conn.(closeWriter)
	if !ok {
		return errNoHalfClose
	}
	return cw.CloseWrite()
}

func (h *halfConn) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	h.mu.Unlock()
	return h.conn.Close()
}

// rejectWait bounds how long a refused create waits for dockerd to finish the responses
// it owes on the connection, and then for the client to read the refusal.
const rejectWait = 10 * time.Second

// forward copies bytes in both directions between client and upstream until both
// directions have finished, ctx is done, or either side fails. It always closes both
// connections and returns only after its goroutines have exited.
//
// A clean EOF in one direction is forwarded as CloseWrite on the destination, and the
// other direction keeps running. Any other error, or a destination that cannot
// half-close, tears down both connections.
//
// With h set, the client-to-upstream direction runs through the request filter
// (rewrite.go).
func forward(ctx context.Context, client, upstream net.Conn, poll time.Duration, log *slog.Logger, h *filterHooks) {
	c := &halfConn{conn: client}
	u := &halfConn{conn: upstream}

	// cctx is this connection's: done when the proxy stops or the connection is torn
	// down. The start check runs under it.
	cctx, ccancel := context.WithCancel(ctx)
	defer ccancel()
	var once sync.Once
	abort := func() {
		once.Do(func() {
			ccancel()
			_ = c.Close()
			_ = u.Close()
		})
	}
	stop := context.AfterFunc(ctx, abort)

	upDone := make(chan struct{}) // closed when upstream->client has finished
	// rejecting is set before a refused create half-closes the upstream: the
	// upstream->client direction then leaves the client open for the refusal.
	var rejecting atomic.Bool
	var wg sync.WaitGroup
	wg.Add(2)

	// client -> upstream
	go func() {
		defer wg.Done()
		if h == nil {
			if !copyHalf(u, client, log, "client->upstream") {
				abort()
				return
			}
		} else {
			f := newReqFilter(readerOnly{client}, writerOnly{u.conn}, h.rewrite)
			f.checkStart, f.ctx = whileConnected(h.checkStart, client, poll), cctx
			err := f.run()
			var rej *rejection
			if errors.As(err, &rej) {
				rejecting.Store(true)
				refuse(ctx, c, u, client, upDone, rej, abort, log)
				return
			}
			if !closeHalf(u, err, log, "client->upstream") {
				abort()
				return
			}
		}
		// The client finished sending. If it can tell us that it disconnected
		// entirely, watch for that: otherwise a client that vanished without reading
		// would keep an idle upstream stream (docker events, a quiet attach) open
		// forever.
		prober, ok := client.(peerProber)
		if !ok {
			return
		}
		t := time.NewTicker(poll)
		defer t.Stop()
		for {
			select {
			case <-upDone:
				return
			case <-t.C:
				if prober.peerClosed() {
					abort()
					return
				}
			}
		}
	}()

	// upstream -> client
	go func() {
		defer wg.Done()
		defer close(upDone)
		if !copyHalfThen(c, upstream, log, "upstream->client", func() bool { return !rejecting.Load() }) {
			abort()
		}
	}()

	wg.Wait()
	stop()
	// once.Do waits for an abort that is already running (for example one started by
	// ctx), so both connections are closed when forward returns.
	abort()
}

// whileConnected is check with its context also canceled once client, polled every
// poll, has disconnected entirely, so a client that gives up during a start check (the
// docker CLI interrupted) does not keep the check's requests to dockerd going. Nothing
// else reads the client meanwhile: its direction of the proxy is in the check. A client
// that cannot tell (peerProber; TCP in tests) gets check unchanged, under the
// connection's context.
func whileConnected(check StartChecker, client net.Conn, poll time.Duration) StartChecker {
	prober, ok := client.(peerProber)
	if check == nil || !ok {
		return check
	}
	return func(ctx context.Context, ref string) error {
		ctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			t := time.NewTicker(poll)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if prober.peerClosed() {
						cancel()
						return
					}
				}
			}
		}()
		err := check(ctx, ref)
		cancel()
		<-done
		return err
	}
}

// refuse answers a refused create or start: no more requests go upstream, dockerd finishes the
// responses it owes, then the client gets the refusal and the connection closes. What
// the client still sends is read and discarded meanwhile (bounded), so a client blocked
// writing a large body can get to reading the answer.
func refuse(ctx context.Context, c, u *halfConn, client net.Conn, upDone <-chan struct{}, rej *rejection, abort func(), log *slog.Logger) {
	defer abort()
	log.Warn("transport: refused a request", "err", rej.msg)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(io.Discard, io.LimitReader(readerOnly{client}, maxRejectDrain))
	}()
	defer func() { abort(); <-drained }()
	t := time.AfterFunc(rejectWait, abort)
	defer t.Stop()
	if err := u.CloseWrite(); err != nil {
		return
	}
	select {
	case <-upDone:
	case <-ctx.Done():
		return
	}
	if _, err := c.conn.Write(rej.response()); err != nil {
		return
	}
	if err := c.CloseWrite(); err != nil {
		return
	}
	// The client reads the answer and closes; the drain sees that as EOF.
	select {
	case <-drained:
	case <-ctx.Done():
	}
}

// copyHalf copies src into dst and then half-closes dst. It reports whether the direction
// finished cleanly.
func copyHalf(dst *halfConn, src net.Conn, log *slog.Logger, dir string) bool {
	return copyHalfThen(dst, src, log, dir, nil)
}

// copyHalfThen is copyHalf; when the copy ends cleanly and halfClose returns false,
// dst is left open.
func copyHalfThen(dst *halfConn, src net.Conn, log *slog.Logger, dir string, halfClose func() bool) bool {
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	// Hide ReadFrom/WriteTo so io.CopyBuffer always uses the pooled, bounded buffer.
	_, err := io.CopyBuffer(writerOnly{dst.conn}, readerOnly{src}, *bp)
	if halfClose != nil && err == nil && !halfClose() {
		return true
	}
	return closeHalf(dst, err, log, dir)
}

// closeHalf finishes one direction after its copy returned err: it half-closes dst if
// the copy was clean, and reports whether the direction finished cleanly.
func closeHalf(dst *halfConn, err error, log *slog.Logger, dir string) bool {
	if err != nil {
		if !errors.Is(err, net.ErrClosed) {
			log.Debug("transport: copy failed", "dir", dir, "err", err)
		}
		return false
	}
	if err := dst.CloseWrite(); err != nil {
		if !errors.Is(err, net.ErrClosed) {
			log.Debug("transport: half-close failed", "dir", dir, "err", err)
		}
		return false
	}
	return true
}

type readerOnly struct{ r io.Reader }

func (r readerOnly) Read(b []byte) (int, error) { return r.r.Read(b) }

type writerOnly struct{ w io.Writer }

func (w writerOnly) Write(b []byte) (int, error) { return w.w.Write(b) }
