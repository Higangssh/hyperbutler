package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// memConn is one end of an in-memory full-duplex stream with half-close.
type memConn struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func memPair() (*memConn, *memConn) {
	ar, bw := io.Pipe()
	br, aw := io.Pipe()
	return &memConn{r: ar, w: aw}, &memConn{r: br, w: bw}
}

func (c *memConn) Read(b []byte) (int, error)  { return c.r.Read(b) }
func (c *memConn) Write(b []byte) (int, error) { return c.w.Write(b) }
func (c *memConn) CloseWrite() error           { return c.w.Close() }
func (c *memConn) Close() error {
	_ = c.r.CloseWithError(net.ErrClosed)
	_ = c.w.CloseWithError(net.ErrClosed)
	return nil
}

// tcpPair returns two connected TCP sockets. TCP stands in for vsock and the Unix
// socket here: both half-close the same way, and it works on every test host.
func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() { c, err := ln.Accept(); ch <- res{c, err} }()
	a, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	t.Cleanup(func() { a.Close(); r.c.Close() })
	return a.(*net.TCPConn), r.c.(*net.TCPConn)
}

func runRelay(a, b stream) <-chan error {
	done := make(chan error, 1)
	go func() { done <- relay(a, b) }()
	return done
}

func waitRelay(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("relay did not return")
		return nil
	}
}

func TestRelay(t *testing.T) {
	tests := []struct {
		name string
		// clientFirst: the host side half-closes before the dockerd side answers
		// (a plain HTTP request). Otherwise dockerd finishes first while the host keeps
		// sending (attach after the container exits).
		clientFirst bool
	}{
		{"client half-closes first", true},
		{"server half-closes first", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hostApp, hostRelay := tcpPair(t)
			dockerRelay, dockerApp := tcpPair(t)
			done := runRelay(hostRelay, dockerRelay)

			up := []byte("GET /_ping HTTP/1.1\r\nHost: docker\r\n\r\n")
			down := []byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nOK")
			if !tt.clientFirst {
				mustWrite(t, dockerApp, down)
				mustCloseWrite(t, dockerApp)
				expectRead(t, hostApp, down)
			}
			mustWrite(t, hostApp, up)
			mustCloseWrite(t, hostApp)
			expectRead(t, dockerApp, up)
			if tt.clientFirst {
				mustWrite(t, dockerApp, down)
				mustCloseWrite(t, dockerApp)
				expectRead(t, hostApp, down)
			}
			if err := waitRelay(t, done); err != nil {
				t.Fatalf("relay: %v", err)
			}
		})
	}
}

// After a half-close in one direction, the other direction must keep flowing, in
// several writes, until its own EOF.
func TestRelayKeepsReverseDirectionOpen(t *testing.T) {
	hostApp, hostRelay := tcpPair(t)
	dockerRelay, dockerApp := tcpPair(t)
	done := runRelay(hostRelay, dockerRelay)

	mustCloseWrite(t, hostApp) // stdin closed at once
	expectRead(t, dockerApp, nil)
	for i := 0; i < 3; i++ {
		mustWrite(t, dockerApp, []byte("line\n"))
		buf := make([]byte, 5)
		if _, err := io.ReadFull(hostApp, buf); err != nil {
			t.Fatalf("read %d after half-close: %v", i, err)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("relay returned early: %v", err)
	default:
	}
	mustCloseWrite(t, dockerApp)
	expectRead(t, hostApp, nil)
	if err := waitRelay(t, done); err != nil {
		t.Fatal(err)
	}
}

// When one side resets, relay must return and close the other side, so dockerd does not
// keep a stream (docker events, logs -f) open for a host that has gone.
func TestRelayErrorClosesBoth(t *testing.T) {
	hostApp, hostRelay := tcpPair(t)
	dockerRelay, dockerApp := tcpPair(t)
	done := runRelay(hostRelay, dockerRelay)

	_ = hostApp.SetLinger(0) // RST instead of FIN
	_ = hostApp.Close()
	// The reset surfaces either on the next read of hostRelay or on the write to it.
	mustWrite(t, dockerApp, []byte("event\n"))
	for i := 0; i < 50; i++ {
		if _, err := dockerApp.Write([]byte("event\n")); err != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := waitRelay(t, done); err == nil {
		t.Fatal("relay returned nil after a reset")
	}
	_ = dockerApp.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.Copy(io.Discard, dockerApp); err != nil && isTimeout(err) {
		t.Fatalf("docker side was not closed: %v", err)
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// failingStream fails CloseWrite, the case of a transport without half-close support.
type failingStream struct{ *memConn }

func (failingStream) CloseWrite() error { return errors.New("no half-close") }

func TestRelayCloseWriteFailureClosesBoth(t *testing.T) {
	hostApp, hostRelay := memPair()
	dockerRelay, dockerApp := memPair()
	done := runRelay(hostRelay, failingStream{dockerRelay})

	if err := hostApp.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := waitRelay(t, done); err == nil {
		t.Fatal("relay ignored a CloseWrite failure")
	}
	if _, err := dockerApp.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("docker side read %v, want closed", err)
	}
	if _, err := hostApp.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("host side read %v, want closed", err)
	}
}

func mustWrite(t *testing.T, c io.Writer, b []byte) {
	t.Helper()
	if _, err := c.Write(b); err != nil {
		t.Fatal(err)
	}
}

func mustCloseWrite(t *testing.T, c interface{ CloseWrite() error }) {
	t.Helper()
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
}

// expectRead reads until EOF and compares.
func expectRead(t *testing.T, c net.Conn, want []byte) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("read %q, want %q", got, want)
	}
	_ = c.SetReadDeadline(time.Time{})
}
