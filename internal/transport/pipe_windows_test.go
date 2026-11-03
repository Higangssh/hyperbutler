//go:build windows

package transport

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/winipc"
)

var pipeSeq atomic.Int64

func testPipeName(t *testing.T) string {
	return fmt.Sprintf(`\\.\pipe\hyperbutler_test_%d_%d_%s`, windows.GetCurrentProcessId(), pipeSeq.Add(1),
		strings.NewReplacer("/", "_", "\\", "_").Replace(t.Name()))
}

func dialPipe(t *testing.T, name string) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := winipc.DialPipe(ctx, name)
	if err != nil {
		t.Fatalf("dial %s: %v", name, err)
	}
	return c
}

// acceptPair returns a connected (client, server) pair on a fresh ListenPipe listener.
func acceptPair(t *testing.T) (client net.Conn, server *pipeConn, ln net.Listener) {
	t.Helper()
	name := testPipeName(t)
	ln, err := ListenPipe(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	client = dialPipe(t, name)
	t.Cleanup(func() { client.Close() })
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	server = r.c.(*pipeConn)
	t.Cleanup(func() { server.Close() })
	return client, server, ln
}

func TestPipeSecurityDescriptor(t *testing.T) {
	_, server, _ := acceptPair(t)
	sd, err := windows.GetSecurityInfo(windows.Handle(server.Fd()), windows.SE_KERNEL_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	me := tu.User.Sid

	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatal(err)
	}
	if !owner.Equals(me) {
		t.Errorf("owner = %s, want %s", owner, me)
	}
	ctl, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	if ctl&windows.SE_DACL_PROTECTED == 0 {
		t.Error("DACL is not protected")
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if dacl == nil {
		t.Fatal("NULL DACL grants everyone access")
	}
	if dacl.AceCount != 1 {
		t.Fatalf("DACL has %d ACEs, want 1: %s", dacl.AceCount, sd)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
		t.Fatalf("ACE type = %d, want allow", ace.Header.AceType)
	}
	aceSid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !aceSid.Equals(me) {
		t.Fatalf("ACE grants %s, want only %s", aceSid, me)
	}
	t.Logf("pipe SDDL: %s", sd)
}

func TestPipeRemoteClientsRejected(t *testing.T) {
	_, server, _ := acceptPair(t)
	var flags uint32
	if err := windows.GetNamedPipeInfo(windows.Handle(server.Fd()), &flags, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if flags&windows.PIPE_TYPE_MESSAGE == 0 {
		t.Error("pipe is not in message mode; CloseWrite would not reach the Docker CLI")
	}
	// PIPE_REJECT_REMOTE_CLIENTS is not reported by GetNamedPipeInfo; winipc's
	// createInstance sets it (FILE_PIPE_REJECT_REMOTE_CLIENTS) on every instance.
}

func TestListenPipeRejectsBadNames(t *testing.T) {
	for _, name := range []string{"", `\\server\pipe\x`, `\\.\pipe\`, `\\.\pipe\a\b`, `C:\x`} {
		if ln, err := ListenPipe(name); err == nil {
			ln.Close()
			t.Errorf("ListenPipe(%q) succeeded", name)
		}
	}
}

func TestListenPipeRefusesExistingName(t *testing.T) {
	name := testPipeName(t)
	ln, err := ListenPipe(name)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if ln2, err := ListenPipe(name); err == nil {
		ln2.Close()
		t.Fatal("second listener on the same name succeeded; a squatter could share it")
	}
}

func TestPipeCloseWriteDeliversEOFAfterData(t *testing.T) {
	client, server, _ := acceptPair(t)
	data := make([]byte, 32<<10)
	_, _ = rand.Read(data)
	if _, err := server.Write(data); err != nil {
		t.Fatal(err)
	}
	// The client has not read yet, so CloseWrite must wait for it to drain the pipe:
	// otherwise the zero-byte EOF message would be swallowed by the data read.
	done := make(chan error, 1)
	go func() { done <- server.CloseWrite() }()
	time.Sleep(50 * time.Millisecond)
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("client read %d bytes, want %d", len(got), len(data))
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CloseWrite did not return after the client drained the pipe")
	}
	if _, err := server.Write([]byte("x")); err == nil {
		t.Fatal("Write after CloseWrite succeeded")
	}
	// Reverse direction still works after the server's half-close.
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(server, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("server read %q, %v", buf, err)
	}
}

// TestPipeCloseWriteWithReaderWaiting covers the common case: the client is already
// blocked in Read when the server half-closes.
func TestPipeCloseWriteWithReaderWaiting(t *testing.T) {
	client, server, _ := acceptPair(t)
	got := make(chan []byte, 1)
	go func() {
		_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
		b, _ := io.ReadAll(client)
		got <- b
	}()
	for i := 0; i < 100; i++ {
		if _, err := fmt.Fprintf(server, "line %d\n", i); err != nil {
			t.Fatal(err)
		}
	}
	if err := server.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	b := <-got
	if !bytes.HasSuffix(b, []byte("line 99\n")) {
		t.Fatalf("client did not get all data before EOF: %d bytes", len(b))
	}
}

// TestPipeUnreadDataSurvivesClose: closing the server handle must not discard data the
// client has not read yet (a container's last output line).
func TestPipeUnreadDataSurvivesClose(t *testing.T) {
	client, server, _ := acceptPair(t)
	if _, err := server.Write([]byte("last words")); err != nil {
		t.Fatal(err)
	}
	server.Close()
	time.Sleep(50 * time.Millisecond)
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, string(got), "last words")
}

// TestPipeCloseInterruptsCloseWrite: a client that stops reading must not be able to
// hang CloseWrite past Close (this is what makes shutdown bounded).
func TestPipeCloseInterruptsCloseWrite(t *testing.T) {
	_, server, _ := acceptPair(t)
	chunk := make([]byte, 16<<10)
	for {
		_ = server.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		if _, err := server.Write(chunk); err != nil {
			break // the pipe buffer is full and the client is not reading
		}
	}
	_ = server.SetWriteDeadline(time.Time{})
	done := make(chan error, 1)
	go func() { done <- server.CloseWrite() }()
	select {
	case err := <-done:
		t.Fatalf("CloseWrite returned %v while the client had unread data", err)
	case <-time.After(200 * time.Millisecond):
	}
	closed := make(chan struct{})
	go func() { server.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung")
	}
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("CloseWrite = %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CloseWrite still blocked after Close")
	}
}

// TestPipeCloseWriteClientGone: the client disconnects with data still queued; the
// drain wait must end with an error instead of polling forever.
func TestPipeCloseWriteClientGone(t *testing.T) {
	client, server, _ := acceptPair(t)
	if _, err := server.Write([]byte("never read")); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.CloseWrite() }()
	time.Sleep(50 * time.Millisecond)
	client.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("CloseWrite succeeded on a disconnected pipe")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CloseWrite kept waiting for a client that is gone")
	}
}
func TestPipePeerClosed(t *testing.T) {
	client, server, _ := acceptPair(t)
	if server.peerClosed() {
		t.Fatal("peerClosed with a connected client")
	}
	if err := client.(closeWriter).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(server); err != nil || len(b) != 0 {
		t.Fatalf("server read %q, %v; want EOF", b, err)
	}
	if server.peerClosed() {
		t.Fatal("peerClosed after a half-close only")
	}
	client.Close()
	deadline := time.Now().Add(5 * time.Second)
	for !server.peerClosed() {
		if time.Now().After(deadline) {
			t.Fatal("peerClosed never reported the disconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	server.Close()
	if !server.peerClosed() {
		t.Fatal("peerClosed after Close")
	}
}

func pipeProxy(t *testing.T, d *fakeDockerd) (name string, stop func() error) {
	t.Helper()
	name = testPipeName(t)
	ln, err := ListenPipe(name)
	if err != nil {
		t.Fatal(err)
	}
	stop = runProxy(t, &Proxy{Upstream: d.dialer(), peerPollInterval: 20 * time.Millisecond}, ln)
	return name, stop
}

// TestPipeProxyAttach is the full host path the Docker CLI uses: a pipe client on the
// named pipe, upgrade, CloseWrite on stdin, output after the half-close.
func TestPipeProxyAttach(t *testing.T) {
	d := newFakeDockerd(t)
	name, _ := pipeProxy(t, d)
	for i := 0; i < 3; i++ {
		c := dialPipe(t, name)
		out := attachRoundTrip(t, c, "from the cli")
		mustEqual(t, out, "FROM THE CLI\nstdin closed\n")
		c.Close()
		mustEqual(t, d.waitClosed(t, 5*time.Second), "/attach")
	}
}

func TestPipeProxyHalfClosedClientKeepsStream(t *testing.T) {
	d := newFakeDockerd(t)
	name, _ := pipeProxy(t, d)
	c := dialPipe(t, name)
	defer c.Close()
	if _, err := io.WriteString(c, "GET /hold HTTP/1.1\r\nHost: docker\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	d.waitStarted(t)
	if err := c.(closeWriter).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	// Many poll intervals pass; the half-closed but connected client must keep its
	// stream.
	time.Sleep(300 * time.Millisecond)
	if n := d.open.Load(); n != 1 {
		t.Fatalf("upstream connections = %d, want 1", n)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 15)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "HTTP/1.1 200 OK" {
		t.Fatalf("read %q, %v", buf, err)
	}
}

// trackedConn records when the proxy closes an upstream connection.
type trackedConn struct {
	*net.TCPConn
	once   sync.Once
	closed chan struct{}
}

func (c *trackedConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.TCPConn.Close()
}

// TestPipeProxyVanishedClientFreesUpstream: a CLI killed after closing stdin must not
// keep an idle upstream stream open.
func TestPipeProxyVanishedClientFreesUpstream(t *testing.T) {
	d := newFakeDockerd(t)
	conns := make(chan *trackedConn, 1)
	name := testPipeName(t)
	ln, err := ListenPipe(name)
	if err != nil {
		t.Fatal(err)
	}
	runProxy(t, &Proxy{
		peerPollInterval: 20 * time.Millisecond,
		Upstream: DialerFunc(func(ctx context.Context) (net.Conn, error) {
			c, err := d.dialer().DialContext(ctx)
			if err != nil {
				return nil, err
			}
			tc := &trackedConn{TCPConn: c.(*net.TCPConn), closed: make(chan struct{})}
			conns <- tc
			return tc, nil
		}),
	}, ln)
	c := dialPipe(t, name)
	if _, err := io.WriteString(c, "GET /hold HTTP/1.1\r\nHost: docker\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	d.waitStarted(t)
	up := <-conns
	_ = c.(closeWriter).CloseWrite()
	time.Sleep(100 * time.Millisecond)
	select {
	case <-up.closed:
		t.Fatal("upstream closed while the client was only half-closed")
	default:
	}
	c.Close()
	// /hold itself would only notice on its 2s heartbeat; the proxy must act first.
	select {
	case <-up.closed:
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("upstream still open after the client vanished")
	}
}
func TestPipeProxyShutdownNoLeak(t *testing.T) {
	d := newFakeDockerd(t)
	// winipc starts a process-wide I/O completion goroutine on first use and never
	// stops it; start it before taking the baseline so the test works in isolation.
	warm, _, _ := acceptPair(t)
	warm.Close()
	base := runtime.NumGoroutine()
	name := testPipeName(t)
	ln, err := ListenPipe(name)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- (&Proxy{Upstream: d.dialer(), peerPollInterval: 20 * time.Millisecond}).Serve(ctx, ln)
	}()
	var conns []net.Conn
	for i := 0; i < 10; i++ {
		c := dialPipe(t, name)
		conns = append(conns, c)
		switch i % 3 {
		case 0:
			_, _ = io.WriteString(c, "GET /events HTTP/1.1\r\nHost: docker\r\n\r\n")
			d.waitStarted(t)
		case 1:
			// Half-closed and idle: the shutdown must still tear it down.
			_, _ = io.WriteString(c, "GET /hold HTTP/1.1\r\nHost: docker\r\n\r\n")
			d.waitStarted(t)
			_ = c.(closeWriter).CloseWrite()
		case 2:
			mustEqual(t, attachRoundTrip(t, c, "z"), "Z\nstdin closed\n")
		}
	}
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
	for _, c := range conns {
		c.Close()
	}
	for range conns {
		d.waitClosed(t, 5*time.Second)
	}
	// The pipe name must be free again.
	ln2, err := ListenPipe(name)
	if err != nil {
		t.Fatalf("pipe name still in use after shutdown: %v", err)
	}
	ln2.Close()
	checkNoGoroutineLeak(t, base)
}
