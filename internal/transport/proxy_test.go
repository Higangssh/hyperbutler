package transport

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProxyPing(t *testing.T) {
	d := newFakeDockerd(t)
	ln := tcpListener(t)
	runProxy(t, &Proxy{Upstream: d.dialer()}, ln)

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var nd net.Dialer
			return nd.DialContext(ctx, "tcp", ln.Addr().String())
		},
	}}
	defer client.CloseIdleConnections()
	for i := 0; i < 3; i++ { // keep-alive: several requests on one connection
		resp, err := client.Get("http://docker/_ping")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		mustEqual(t, string(body), "OK")
	}
}

// TestProxyUpgradeHalfClose is the docker attach / exec pattern: the client closes stdin
// with CloseWrite and must still receive everything the server writes afterwards.
func TestProxyUpgradeHalfClose(t *testing.T) {
	d := newFakeDockerd(t)
	ln := tcpListener(t)
	runProxy(t, &Proxy{Upstream: d.dialer()}, ln)

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	out := attachRoundTrip(t, c, "hello world")
	mustEqual(t, out, "HELLO WORLD\nstdin closed\n")
	c.Close()
	mustEqual(t, d.waitClosed(t, 5*time.Second), "/attach")
}

// TestProxyUpstreamHalfCloseFirst checks the reverse direction: the server half-closes
// first and the client can keep sending.
func TestProxyUpstreamHalfCloseFirst(t *testing.T) {
	d := newFakeDockerd(t)
	ln := tcpListener(t)
	runProxy(t, &Proxy{Upstream: d.dialer()}, ln)

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, "GET /banner HTTP/1.1\r\nHost: docker\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(c) // ends when the proxy forwards the server's half-close
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, string(got), "banner")
	if _, err := io.WriteString(c, "still talking"); err != nil {
		t.Fatalf("write after server half-close: %v", err)
	}
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case tr := <-d.trailer:
		mustEqual(t, string(tr), "still talking")
	case <-time.After(5 * time.Second):
		t.Fatal("server never saw the client's trailing bytes")
	}
}

func TestProxyLargeBidirectional(t *testing.T) {
	ln := tcpListener(t)
	upLn := tcpListener(t)
	defer upLn.Close()

	const size = 4 << 20
	up := make([]byte, size)
	down := make([]byte, size)
	_, _ = rand.Read(up)
	_, _ = rand.Read(down)

	// Upstream: reads everything while writing everything, then half-closes.
	upGot := make(chan [32]byte, 1)
	go func() {
		c, err := upLn.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := sha256.New()
			_, _ = io.Copy(h, c)
			var sum [32]byte
			copy(sum[:], h.Sum(nil))
			upGot <- sum
		}()
		_, _ = c.Write(down)
		_ = c.(*net.TCPConn).CloseWrite()
		wg.Wait()
	}()
	runProxy(t, &Proxy{Upstream: TCPDialer{Address: upLn.Addr().String()}}, ln)

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	downGot := make(chan [32]byte, 1)
	go func() {
		h := sha256.New()
		_, _ = io.Copy(h, c)
		var sum [32]byte
		copy(sum[:], h.Sum(nil))
		downGot <- sum
	}()
	if _, err := c.Write(up); err != nil {
		t.Fatal(err)
	}
	_ = c.(*net.TCPConn).CloseWrite()
	if got := <-upGot; got != sha256.Sum256(up) {
		t.Fatal("upstream received corrupted data")
	}
	if got := <-downGot; got != sha256.Sum256(down) {
		t.Fatal("client received corrupted data")
	}
}

func TestServeShutdownClosesActiveConnections(t *testing.T) {
	d := newFakeDockerd(t)
	ln := tcpListener(t)
	stop := runProxy(t, &Proxy{Upstream: d.dialer()}, ln)

	const n = 8
	var clients []net.Conn
	for i := 0; i < n; i++ {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
		if _, err := io.WriteString(c, "GET /events HTTP/1.1\r\nHost: docker\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		d.waitStarted(t)
	}
	start := time.Now()
	if err := stop(); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("shutdown took %v", el)
	}
	for i := 0; i < n; i++ {
		mustEqual(t, d.waitClosed(t, 5*time.Second), "/events")
	}
	for _, c := range clients {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadAll(c); err != nil {
			t.Fatalf("client not closed by shutdown: %v", err)
		}
		c.Close()
	}
	// The listener must be closed too.
	if c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		c.Close()
		t.Fatal("listener still accepting after shutdown")
	}
}

func TestServeNoGoroutineLeak(t *testing.T) {
	d := newFakeDockerd(t) // started before base so its accept loop is counted
	base := runtime.NumGoroutine()
	ln := tcpListener(t)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	p := &Proxy{Upstream: d.dialer()}
	go func() { errc <- p.Serve(ctx, ln) }()

	// Idle /events streams stay open while attach sessions come and go around them.
	var conns []net.Conn
	for i := 0; i < 20; i++ {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		if i%2 == 0 {
			mustEqual(t, attachRoundTrip(t, c, "x"), "X\nstdin closed\n")
		} else {
			_, _ = io.WriteString(c, "GET /events HTTP/1.1\r\nHost: docker\r\n\r\n")
			d.waitStarted(t)
		}
	}
	cancel()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	for _, c := range conns {
		c.Close()
	}
	for i := 0; i < 20; i++ {
		d.waitClosed(t, 5*time.Second)
	}
	checkNoGoroutineLeak(t, base)
}

func TestDialFailureClosesClient(t *testing.T) {
	ln := tcpListener(t)
	runProxy(t, &Proxy{Upstream: DialerFunc(func(context.Context) (net.Conn, error) {
		return nil, errors.New("guest is down")
	})}, ln)
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("read succeeded; want the client to be closed")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("client left open after dial failure")
	}
}

func TestDialTimeout(t *testing.T) {
	ln := tcpListener(t)
	runProxy(t, &Proxy{
		DialTimeout: 100 * time.Millisecond,
		Upstream: DialerFunc(func(ctx context.Context) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}),
	}, ln)
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _ = c.Read(make([]byte, 1))
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("client closed after %v; want about DialTimeout", el)
	}
}

// TestShutdownDuringDial checks that cancel aborts a dial that would otherwise hang.
func TestShutdownDuringDial(t *testing.T) {
	ln := tcpListener(t)
	dialing := make(chan struct{}, 1)
	stop := runProxy(t, &Proxy{
		DialTimeout: time.Hour,
		Upstream: DialerFunc(func(ctx context.Context) (net.Conn, error) {
			dialing <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		}),
	}, ln)
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	<-dialing
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestMaxConnsBackpressure(t *testing.T) {
	d := newFakeDockerd(t)
	ln := tcpListener(t)
	var dials atomic.Int64
	runProxy(t, &Proxy{
		MaxConns: 1,
		Upstream: DialerFunc(func(ctx context.Context) (net.Conn, error) {
			dials.Add(1)
			return d.dialer().DialContext(ctx)
		}),
	}, ln)

	first, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(first, "GET /events HTTP/1.1\r\nHost: docker\r\n\r\n")
	d.waitStarted(t)

	second, err := net.Dial("tcp", ln.Addr().String()) // sits in the kernel backlog
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	time.Sleep(200 * time.Millisecond)
	if n := dials.Load(); n != 1 {
		t.Fatalf("dials = %d while at MaxConns, want 1", n)
	}
	first.Close()
	d.waitClosed(t, 5*time.Second)
	out := attachRoundTrip(t, second, "ok")
	mustEqual(t, out, "OK\nstdin closed\n")
	if n := dials.Load(); n != 2 {
		t.Fatalf("dials = %d, want 2", n)
	}
}

func TestServeNilUpstream(t *testing.T) {
	ln := tcpListener(t)
	if err := (&Proxy{}).Serve(context.Background(), ln); err == nil {
		t.Fatal("want error for nil Upstream")
	}
	if _, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("listener not closed: %v", err)
	}
}

// flakyListener fails Accept a few times with a non-closed error, then reports closed.
type flakyListener struct {
	net.Listener
	fails atomic.Int64
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.fails.Add(1) <= 3 {
		return nil, errors.New("transient")
	}
	return nil, net.ErrClosed
}

func TestServeRetriesAcceptErrors(t *testing.T) {
	ln := &flakyListener{Listener: tcpListener(t)}
	err := (&Proxy{Upstream: TCPDialer{Address: "127.0.0.1:1"}}).Serve(context.Background(), ln)
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Serve = %v, want net.ErrClosed", err)
	}
	if n := ln.fails.Load(); n != 4 {
		t.Fatalf("Accept called %d times, want 4", n)
	}
}

// TestForwardWithoutHalfClose: net.Pipe cannot CloseWrite, so a clean EOF must tear the
// connection down rather than hang.
func TestForwardWithoutHalfClose(t *testing.T) {
	base := runtime.NumGoroutine()
	cApp, cProxy := net.Pipe()
	uProxy, uApp := net.Pipe()
	done := make(chan struct{})
	go func() {
		forward(context.Background(), cProxy, uProxy, time.Second, discardLogger, nil)
		close(done)
	}()
	go func() { _, _ = io.Copy(io.Discard, uApp) }()
	_, _ = cApp.Write([]byte("x"))
	cApp.Close() // EOF on client->upstream; uProxy cannot CloseWrite
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("forward hung when the destination cannot half-close")
	}
	uApp.Close()
	checkNoGoroutineLeak(t, base)
}

// TestForwardContextCancel: cancelling ctx closes both sides and forward returns.
func TestForwardContextCancel(t *testing.T) {
	ln := tcpListener(t)
	defer ln.Close()
	cApp, cProxy := tcpPair(t, ln)
	uProxy, uApp := tcpPair(t, ln)
	defer cApp.Close()
	defer uApp.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		forward(ctx, cProxy, uProxy, time.Second, discardLogger, nil)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("forward did not return after cancel")
	}
	for _, c := range []net.Conn{cApp, uApp} {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadAll(c); err != nil {
			t.Fatalf("peer not closed: %v", err)
		}
	}
}

// fakeProber reports the client gone once gone is set.
type fakeProber struct {
	*net.TCPConn
	gone atomic.Bool
}

func (f *fakeProber) peerClosed() bool { return f.gone.Load() }

// TestForwardPeerProbe: after a client half-closes, a vanished client must free the
// upstream even if the upstream never sends again.
func TestForwardPeerProbe(t *testing.T) {
	ln := tcpListener(t)
	defer ln.Close()
	cApp, cProxy := tcpPair(t, ln)
	uProxy, uApp := tcpPair(t, ln)
	defer cApp.Close()
	defer uApp.Close()
	fp := &fakeProber{TCPConn: cProxy}
	done := make(chan struct{})
	go func() {
		forward(context.Background(), fp, uProxy, 10*time.Millisecond, discardLogger, nil)
		close(done)
	}()
	_ = cApp.CloseWrite()
	// Upstream sees EOF but stays open: the client is half-closed, not gone.
	_ = uApp.SetReadDeadline(time.Now().Add(5 * time.Second))
	if b, err := io.ReadAll(uApp); err != nil || len(b) != 0 {
		t.Fatalf("upstream read = %q, %v; want clean EOF", b, err)
	}
	time.Sleep(100 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("forward ended while the client was only half-closed")
	default:
	}
	if _, err := uApp.Write([]byte("late")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	_ = cApp.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(cApp, buf); err != nil || !bytes.Equal(buf, []byte("late")) {
		t.Fatalf("client read = %q, %v", buf, err)
	}
	fp.gone.Store(true)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("forward kept the upstream open after the client vanished")
	}
}
