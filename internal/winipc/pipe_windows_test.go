//go:build windows

package winipc

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var pipeSeq atomic.Int64

func testPipeName(tb testing.TB) string {
	return fmt.Sprintf(`\\.\pipe\hyperbutler_winipc_test_%d_%d_%s`, windows.GetCurrentProcessId(), pipeSeq.Add(1),
		strings.NewReplacer("/", "_", "\\", "_").Replace(tb.Name()))
}

// userSDDL grants only the current user access, as transport.CurrentUserSDDL does.
func userSDDL(tb testing.TB) string {
	tb.Helper()
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		tb.Fatal(err)
	}
	sid := tu.User.Sid.String()
	return "O:" + sid + "D:P(A;;GA;;;" + sid + ")"
}

func testListen(tb testing.TB, msg bool) (*PipeListener, string) {
	tb.Helper()
	name := testPipeName(tb)
	ln, err := ListenPipe(name, &PipeConfig{SecurityDescriptor: userSDDL(tb), MessageMode: msg,
		InputBufferSize: 64 << 10, OutputBufferSize: 64 << 10})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { ln.Close() })
	return ln.(*PipeListener), name
}

func dialTest(tb testing.TB, name string) *PipeConn {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := DialPipe(ctx, name)
	if err != nil {
		tb.Fatalf("dial %s: %v", name, err)
	}
	tb.Cleanup(func() { c.Close() })
	return c.(*PipeConn)
}

// pipePair returns a connected (client, server) pair.
func pipePair(tb testing.TB, msg bool) (client, server *PipeConn) {
	tb.Helper()
	ln, name := testListen(tb, msg)
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	client = dialTest(tb, name)
	r := <-ch
	if r.err != nil {
		tb.Fatal(r.err)
	}
	server = r.c.(*PipeConn)
	tb.Cleanup(func() { server.Close() })
	return client, server
}

func randBytes(tb testing.TB, n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		tb.Fatal(err)
	}
	return b
}

func TestPipeRoundTripBothWays(t *testing.T) {
	client, server := pipePair(t, true)
	if !server.IsServer() || client.IsServer() {
		t.Fatal("IsServer is wrong")
	}
	up, down := randBytes(t, 3<<20), randBytes(t, 2<<20)
	var wg sync.WaitGroup
	var gotUp, gotDown []byte
	var errUp, errDown error
	wg.Add(2)
	go func() {
		defer wg.Done()
		gotUp = make([]byte, len(up))
		_, errUp = io.ReadFull(server, gotUp)
	}()
	go func() {
		defer wg.Done()
		gotDown = make([]byte, len(down))
		_, errDown = io.ReadFull(client, gotDown)
	}()
	go func() { _, _ = client.Write(up) }()
	if _, err := server.Write(down); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if errUp != nil || errDown != nil {
		t.Fatal(errUp, errDown)
	}
	if !bytes.Equal(gotUp, up) || !bytes.Equal(gotDown, down) {
		t.Fatal("data corrupted")
	}
}

func TestPipeSecurityDescriptorAndMode(t *testing.T) {
	_, server := pipePair(t, true)
	h := windows.Handle(server.Fd())
	sd, err := windows.GetSecurityInfo(h, windows.SE_KERNEL_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	tu, _ := windows.GetCurrentProcessToken().GetTokenUser()
	owner, _, err := sd.Owner()
	if err != nil || !owner.Equals(tu.User.Sid) {
		t.Fatalf("owner %v, %v", owner, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 1 {
		t.Fatalf("DACL %v, %v: %s", dacl, err, sd)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	if sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)); !sid.Equals(tu.User.Sid) {
		t.Fatalf("ACE for %s", sid)
	}
	var flags uint32
	if err := windows.GetNamedPipeInfo(h, &flags, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if flags&windows.PIPE_TYPE_MESSAGE == 0 || flags&windows.PIPE_SERVER_END == 0 {
		t.Fatalf("pipe flags %#x", flags)
	}
}

func TestPipeDefaultSecurityDescriptor(t *testing.T) {
	name := testPipeName(t)
	ln, err := ListenPipe(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()
	c := dialTest(t, name)
	if err := c.CloseWrite(); err == nil {
		t.Fatal("CloseWrite on a byte-type pipe succeeded")
	}
}

func TestListenPipeRefusesExistingName(t *testing.T) {
	_, name := testListen(t, true)
	if ln, err := ListenPipe(name, nil); err == nil {
		ln.Close()
		t.Fatal("second listener on the same name succeeded")
	}
}

func TestListenPipeBadName(t *testing.T) {
	for _, name := range []string{"", "x\x00y"} {
		if ln, err := ListenPipe(name, nil); err == nil {
			ln.Close()
			t.Errorf("ListenPipe(%q) succeeded", name)
		}
	}
}

// The placeholder instance holds the name: no client can connect to it, and the name
// is free again once the listener and its connections are closed.
func TestPipeNameLifetime(t *testing.T) {
	name := testPipeName(t)
	ln, err := ListenPipe(name, &PipeConfig{MessageMode: true})
	if err != nil {
		t.Fatal(err)
	}
	// The one waiting instance takes this client; the next finds the pipe busy, since
	// the placeholder never accepts one.
	c1 := dialTest(t, name)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if c, err := DialPipe(ctx, name); err == nil {
		c.Close()
		t.Fatal("second client connected while no instance was waiting")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy dial: %v", err)
	}
	s1, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	if _, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept after Close: %v", err)
	}
	// The accepted connection outlives the listener. The pipe has no buffer quota, so
	// the write completes only as the server reads.
	go func() { _, _ = c1.Write([]byte("hi")) }()
	buf := make([]byte, 2)
	if _, err := io.ReadFull(s1, buf); err != nil {
		t.Fatal(err)
	}
	s1.Close()
	c1.Close()
	ln2, err := ListenPipe(name, nil)
	if err != nil {
		t.Fatalf("name still taken: %v", err)
	}
	ln2.Close()
}

func TestPipeDialMissing(t *testing.T) {
	start := time.Now()
	_, err := DialPipe(context.Background(), testPipeName(t))
	if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		t.Fatalf("dial missing pipe: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("dial of a missing pipe waited")
	}
	if _, err := DialPipeImp(context.Background(), testPipeName(t), ImpLevel(0x12345)); err == nil {
		t.Fatal("bad impersonation level accepted")
	}
}

func TestPipeAcceptCloseUnblocks(t *testing.T) {
	ln, _ := testListen(t, true)
	errc := make(chan error, 1)
	go func() {
		_, err := ln.Accept()
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	ln.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept still blocked after Close")
	}
}

// A client that connects and leaves before Accept must not break the listener.
func TestPipeClientGoneBeforeAccept(t *testing.T) {
	ln, name := testListen(t, true)
	c := dialTest(t, name)
	c.Close()
	go func() {
		time.Sleep(50 * time.Millisecond)
		c2, err := DialPipe(context.Background(), name)
		if err == nil {
			_, _ = c2.Write([]byte("second"))
			time.Sleep(200 * time.Millisecond)
			c2.Close()
		}
	}()
	// The first Accept may return the departed client (reads EOF) or skip it.
	for i := 0; i < 2; i++ {
		s, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 6)
		_ = s.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err = io.ReadFull(s, buf)
		s.Close()
		if err == nil {
			if string(buf) != "second" {
				t.Fatalf("read %q", buf)
			}
			return
		}
		if !errors.Is(err, io.EOF) {
			t.Fatalf("read: %v", err)
		}
	}
	t.Fatal("second client never accepted")
}

func TestPipeManyClients(t *testing.T) {
	ln, name := testListen(t, true)
	const n = 32
	var wg sync.WaitGroup
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			c, err := DialPipe(ctx, name)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			msg := []byte(fmt.Sprintf("client %d", i))
			if _, err := c.Write(msg); err != nil {
				errs <- err
				return
			}
			if err := c.(*PipeConn).CloseWrite(); err != nil {
				errs <- err
				return
			}
			got, err := io.ReadAll(c)
			if err != nil || !bytes.Equal(got, msg) {
				errs <- fmt.Errorf("echo %q: %w", got, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestPipeCloseWriteDeliversEOFAfterData(t *testing.T) {
	for _, dir := range []string{"server", "client"} {
		t.Run(dir, func(t *testing.T) {
			client, server := pipePair(t, true)
			w, r := server, client
			if dir == "client" {
				w, r = client, server
			}
			data := randBytes(t, 200<<10) // more than the pipe buffer
			done := make(chan error, 1)
			go func() {
				if _, err := w.Write(data); err != nil {
					done <- err
					return
				}
				done <- w.CloseWrite()
			}()
			time.Sleep(50 * time.Millisecond)
			_ = r.SetReadDeadline(time.Now().Add(5 * time.Second))
			got, err := io.ReadAll(r)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("read %d bytes, %v; want %d", len(got), err, len(data))
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte("x")); err == nil {
				t.Fatal("Write after CloseWrite succeeded")
			}
			if err := w.CloseWrite(); err == nil {
				t.Fatal("second CloseWrite succeeded")
			}
			// EOF is sticky, and the other direction still works.
			if n, err := r.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
				t.Fatalf("Read after EOF: %d, %v", n, err)
			}
			if _, err := r.Write([]byte("ping")); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 4)
			_ = w.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.ReadFull(w, buf); err != nil || string(buf) != "ping" {
				t.Fatalf("reverse read %q, %v", buf, err)
			}
		})
	}
}

// A peer that stops reading must not hang CloseWrite past Close.
func TestPipeCloseInterruptsCloseWrite(t *testing.T) {
	_, server := pipePair(t, true)
	chunk := make([]byte, 16<<10)
	for {
		_ = server.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		if _, err := server.Write(chunk); err != nil {
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("write: %v", err)
			}
			break
		}
	}
	_ = server.SetWriteDeadline(time.Time{})
	done := make(chan error, 1)
	go func() { done <- server.CloseWrite() }()
	select {
	case err := <-done:
		t.Fatalf("CloseWrite returned %v with unread data", err)
	case <-time.After(200 * time.Millisecond):
	}
	closed := make(chan struct{})
	go func() { server.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung")
	}
	if err := <-done; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("CloseWrite = %v, want net.ErrClosed", err)
	}
}

func TestPipeCloseWritePeerGone(t *testing.T) {
	client, server := pipePair(t, true)
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
		t.Fatal("CloseWrite kept waiting for a peer that is gone")
	}
}

func TestPipeUnreadDataSurvivesClose(t *testing.T) {
	client, server := pipePair(t, true)
	if _, err := server.Write([]byte("last words")); err != nil {
		t.Fatal(err)
	}
	server.Close()
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(client)
	if err != nil || string(got) != "last words" {
		t.Fatalf("read %q, %v", got, err)
	}
}

func TestPipeReadDeadline(t *testing.T) {
	client, server := pipePair(t, true)
	_ = client.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	start := time.Now()
	_, err := client.Read(make([]byte, 10))
	var ne net.Error
	if !errors.Is(err, os.ErrDeadlineExceeded) || !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("Read = %v, want a timeout", err)
	}
	if d := time.Since(start); d < 40*time.Millisecond || d > 3*time.Second {
		t.Fatalf("timed out after %v", d)
	}
	// An expired deadline fails at once, until it is moved.
	if _, err := client.Read(make([]byte, 10)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("second Read = %v", err)
	}
	_ = client.SetReadDeadline(time.Time{})
	// The connection still works, and no data was lost to the timeout.
	go func() { _, _ = server.Write([]byte("after")) }()
	buf := make([]byte, 5)
	if _, err := io.ReadFull(client, buf); err != nil || string(buf) != "after" {
		t.Fatalf("read %q, %v", buf, err)
	}
}

// Moving the deadline while a Read waits applies to that Read.
func TestPipeDeadlineMovedWhileWaiting(t *testing.T) {
	client, _ := pipePair(t, true)
	_ = client.SetReadDeadline(time.Now().Add(time.Hour))
	errc := make(chan error, 1)
	go func() {
		_, err := client.Read(make([]byte, 1))
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	_ = client.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	select {
	case err := <-errc:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("Read = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shortened deadline not applied to a waiting Read")
	}
	// And a deadline in the past ends a waiting Read too.
	go func() {
		_ = client.SetReadDeadline(time.Time{})
		_, err := client.Read(make([]byte, 1))
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	_ = client.SetDeadline(time.Now().Add(-time.Second))
	select {
	case err := <-errc:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("Read = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("past deadline not applied to a waiting Read")
	}
}

func TestPipeCloseUnblocksReadAndWrite(t *testing.T) {
	client, server := pipePair(t, true)
	rerr := make(chan error, 1)
	go func() {
		_, err := client.Read(make([]byte, 10))
		rerr <- err
	}()
	werr := make(chan error, 1)
	go func() {
		// Fills the pipe; the server never reads.
		_, err := client.Write(make([]byte, 1<<20))
		werr <- err
	}()
	time.Sleep(100 * time.Millisecond)
	client.Close()
	for _, ch := range []chan error{rerr, werr} {
		select {
		case err := <-ch:
			if !errors.Is(err, net.ErrClosed) {
				t.Fatalf("got %v, want net.ErrClosed", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("operation still blocked after Close")
		}
	}
	if _, err := client.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Read after Close: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// The server sees the disconnect as EOF.
	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(server); err != nil {
		t.Fatalf("server read: %v", err)
	}
}

func TestPipePeerClosed(t *testing.T) {
	client, server := pipePair(t, true)
	if server.PeerClosed() {
		t.Fatal("PeerClosed with a connected client")
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(server); err != nil || len(b) != 0 {
		t.Fatalf("server read %q, %v", b, err)
	}
	if server.PeerClosed() {
		t.Fatal("PeerClosed after a half-close only")
	}
	client.Close()
	deadline := time.Now().Add(5 * time.Second)
	for !server.PeerClosed() {
		if time.Now().After(deadline) {
			t.Fatal("PeerClosed never reported the disconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	server.Close()
	if !server.PeerClosed() {
		t.Fatal("PeerClosed false after Close")
	}
}

// The impersonation level a client asks for is the one the server gets.
func TestPipeImpersonationLevel(t *testing.T) {
	for _, tc := range []struct {
		level ImpLevel
		want  uint32 // SECURITY_IMPERSONATION_LEVEL
	}{
		{ImpIdentification, windows.SecurityIdentification},
		{ImpImpersonation, windows.SecurityImpersonation},
	} {
		ln, name := testListen(t, true)
		ch := make(chan net.Conn, 1)
		go func() {
			c, _ := ln.Accept()
			ch <- c
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := DialPipeImp(ctx, name, tc.level)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		// The server must read before it can impersonate.
		if _, err := c.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		s := <-ch
		if _, err := io.ReadFull(s, make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
		got, err := impersonationLevel(windows.Handle(s.(*PipeConn).Fd()))
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("level %#x: server got %d, want %d", tc.level, got, tc.want)
		}
		s.Close()
		c.Close()
	}
}

var procImpersonateNamedPipeClient = windows.NewLazySystemDLL("advapi32.dll").NewProc("ImpersonateNamedPipeClient")

func impersonationLevel(h windows.Handle) (uint32, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if r, _, err := procImpersonateNamedPipeClient.Call(uintptr(h)); r == 0 {
		return 0, err
	}
	defer func() { _ = windows.RevertToSelf() }()
	var tok windows.Token
	if err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &tok); err != nil {
		return 0, err
	}
	defer tok.Close()
	var level, n uint32
	if err := windows.GetTokenInformation(tok, windows.TokenImpersonationLevel, (*byte)(unsafe.Pointer(&level)), 4, &n); err != nil {
		return 0, err
	}
	return level, nil
}
