package transport

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDockerd is a minimal stand-in for dockerd in the guest. It speaks just enough HTTP
// to exercise the proxy:
//
//	GET  /_ping    200 "OK", keep-alive
//	POST /attach   101 upgrade to a raw stream; echoes stdin upper-cased, and after the
//	               client half-closes writes "\nstdin closed\n" and half-closes too
//	GET  /banner   writes "banner", half-closes, then counts what the client still sends
//	GET  /events   200 headers, then stays idle until the client closes or half-closes
//	GET  /hold     200 headers, ignores a half-close, heartbeat every 2s
type fakeDockerd struct {
	ln      net.Listener
	wg      sync.WaitGroup
	open    atomic.Int64
	closed  chan string   // path of each connection's last request, sent when it ends
	trailer chan []byte   // bytes received by /banner after its half-close
	started chan struct{} // one value per /events request whose headers were sent
	stop    chan struct{} // closed at cleanup to end /hold loops
}

func newFakeDockerd(t *testing.T) *fakeDockerd {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDockerd{
		ln:      ln,
		closed:  make(chan string, 256),
		trailer: make(chan []byte, 16),
		started: make(chan struct{}, 256),
		stop:    make(chan struct{}),
	}
	d.wg.Add(1)
	go d.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		close(d.stop)
		d.wg.Wait()
	})
	return d
}

func (d *fakeDockerd) addr() string { return d.ln.Addr().String() }

func (d *fakeDockerd) dialer() Dialer { return TCPDialer{Address: d.addr()} }

func (d *fakeDockerd) serve() {
	defer d.wg.Done()
	for {
		c, err := d.ln.Accept()
		if err != nil {
			return
		}
		d.wg.Add(1)
		d.open.Add(1)
		go func() {
			defer d.wg.Done()
			defer d.open.Add(-1)
			last := d.handle(c.(*net.TCPConn))
			_ = c.Close()
			d.closed <- last
		}()
	}
}

func (d *fakeDockerd) handle(c *net.TCPConn) string {
	br := bufio.NewReader(c)
	last := ""
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return last
		}
		last = req.URL.Path
		switch req.URL.Path {
		case "/_ping":
			_, _ = io.Copy(io.Discard, req.Body)
			if _, err := io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nOK"); err != nil {
				return last
			}
		case "/attach":
			if !strings.EqualFold(req.Header.Get("Upgrade"), "tcp") {
				_, _ = io.WriteString(c, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
				return last
			}
			_, _ = io.WriteString(c, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			buf := make([]byte, 4096)
			for {
				n, err := br.Read(buf)
				if n > 0 {
					if _, werr := c.Write(bytes.ToUpper(buf[:n])); werr != nil {
						return last
					}
				}
				if err != nil {
					break // io.EOF: the client closed stdin
				}
			}
			_, _ = io.WriteString(c, "\nstdin closed\n")
			_ = c.CloseWrite()
			_, _ = io.Copy(io.Discard, br) // wait for the client to go away
			return last
		case "/banner":
			_, _ = io.WriteString(c, "banner")
			_ = c.CloseWrite()
			rest, _ := io.ReadAll(br)
			d.trailer <- rest
			return last
		case "/hold":
			// Like /events, but a client half-close does not end it: it stays silent
			// and only notices a dead peer when a heartbeat write fails, every 2s.
			_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n")
			d.started <- struct{}{}
			_, _ = io.Copy(io.Discard, br)
			for {
				select {
				case <-time.After(2 * time.Second):
				case <-d.stop:
					return last
				}
				if _, err := io.WriteString(c, "2\r\n{}\r\n"); err != nil {
					return last
				}
			}
		case "/events":
			_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n")
			d.started <- struct{}{}
			_, _ = io.Copy(io.Discard, br) // idle until closed
			return last
		default:
			_, _ = io.WriteString(c, "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n")
		}
	}
}

// waitClosed waits for one fakeDockerd connection to end and returns its last path.
func (d *fakeDockerd) waitClosed(t *testing.T, timeout time.Duration) string {
	t.Helper()
	select {
	case p := <-d.closed:
		return p
	case <-time.After(timeout):
		t.Fatalf("upstream connection still open after %v", timeout)
		return ""
	}
}

func (d *fakeDockerd) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-d.started:
	case <-time.After(5 * time.Second):
		t.Fatal("/events stream did not start")
	}
}

// runProxy serves p on ln in the background and returns a stop function that cancels it
// and returns Serve's error.
func runProxy(t *testing.T, p *Proxy, ln net.Listener) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- p.Serve(ctx, ln) }()
	var once sync.Once
	var serr error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case serr = <-errc:
			case <-time.After(10 * time.Second):
				serr = errors.New("Serve did not return within 10s of cancel")
			}
		})
		return serr
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return stop
}

func tcpListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// attachRoundTrip performs the Docker attach pattern on c: upgrade, send stdin, close
// stdin with CloseWrite, and read output until the server closes it.
func attachRoundTrip(t *testing.T, c net.Conn, stdin string) string {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, "POST /attach HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 0\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	if _, err := io.WriteString(c, stdin); err != nil {
		t.Fatal(err)
	}
	cw, ok := c.(closeWriter)
	if !ok {
		t.Fatalf("%T has no CloseWrite", c)
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	out, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("read after half-close: %v", err)
	}
	return string(out)
}

// checkNoGoroutineLeak fails the test if the goroutine count does not return to base.
func checkNoGoroutineLeak(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		n := runtime.NumGoroutine()
		if n <= base {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			buf = buf[:runtime.Stack(buf, true)]
			t.Fatalf("goroutines: %d, want <= %d\n%s", n, base, buf)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// tcpPair returns the two ends of one loopback TCP connection.
func tcpPair(tb testing.TB, ln net.Listener) (*net.TCPConn, *net.TCPConn) {
	tb.Helper()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	a, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		tb.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		tb.Fatal(r.err)
	}
	return a.(*net.TCPConn), r.c.(*net.TCPConn)
}

func mustEqual(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
