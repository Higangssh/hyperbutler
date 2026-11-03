//go:build windows

package winipc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/guid"
)

// vsockService is the hvsock service ID of AF_VSOCK port p (hvaddr.VsockServiceID,
// repeated here to keep this package a leaf). Loopback listeners on these IDs need no
// registration.
func vsockService(p uint32) guid.GUID {
	return guid.GUID{Data1: p, Data2: 0xfacb, Data3: 0x11e6, Data4: [8]byte{0xbd, 0x58, 0x64, 0x00, 0x6a, 0x79, 0x86, 0xd3}}
}

func randLoopbackAddr() HvsockAddr {
	return HvsockAddr{VMID: LoopbackVMID, ServiceID: vsockService(rand.Uint32())}
}

// hvListen listens on a random loopback address, or skips the test where AF_HYPERV is
// not available (Hyper-V is off).
func hvListen(tb testing.TB) (*HvsockListener, HvsockAddr) {
	tb.Helper()
	for i := 0; ; i++ {
		addr := randLoopbackAddr()
		ln, err := ListenHvsock(addr)
		if errors.Is(err, windows.WSAEADDRINUSE) && i < 3 {
			continue
		}
		if errors.Is(err, windows.WSAEAFNOSUPPORT) || errors.Is(err, windows.WSAEPROTONOSUPPORT) {
			tb.Skipf("AF_HYPERV not available: %v", err)
		}
		if err != nil {
			tb.Fatal(err)
		}
		tb.Cleanup(func() { ln.Close() })
		return ln.(*HvsockListener), addr
	}
}

func hvPair(tb testing.TB) (client, server *HvsockConn) {
	tb.Helper()
	ln, addr := hvListen(tb)
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := DialHvsock(ctx, addr)
	if err != nil {
		tb.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		c.Close()
		tb.Fatal(r.err)
	}
	client, server = c.(*HvsockConn), r.c.(*HvsockConn)
	tb.Cleanup(func() { client.Close(); server.Close() })
	return client, server
}

func TestHvsockRoundTripAndAddresses(t *testing.T) {
	client, server := hvPair(t)
	if got := client.RemoteAddr().(HvsockAddr); got.VMID != LoopbackVMID {
		t.Errorf("client remote %v", got)
	}
	if client.LocalAddr().Network() != "hvsock" || server.RemoteAddr().Network() != "hvsock" {
		t.Error("network name")
	}
	t.Logf("client %v -> %v, server %v <- %v", client.LocalAddr(), client.RemoteAddr(), server.LocalAddr(), server.RemoteAddr())
	up, down := randBytes(t, 4<<20), randBytes(t, 1<<20)
	var wg sync.WaitGroup
	var gotUp, gotDown []byte
	var e1, e2 error
	wg.Add(2)
	go func() { defer wg.Done(); gotUp = make([]byte, len(up)); _, e1 = io.ReadFull(server, gotUp) }()
	go func() { defer wg.Done(); gotDown = make([]byte, len(down)); _, e2 = io.ReadFull(client, gotDown) }()
	go func() { _, _ = client.Write(up) }()
	if _, err := server.Write(down); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if e1 != nil || e2 != nil || !bytes.Equal(gotUp, up) || !bytes.Equal(gotDown, down) {
		t.Fatalf("transfer: %v %v", e1, e2)
	}
}

// CloseWrite delivers EOF after the data, and the other direction keeps working: the
// Docker API relay (attach, exec) and the netstack link rely on it.
func TestHvsockHalfClose(t *testing.T) {
	client, server := hvPair(t)
	data := randBytes(t, 1<<20)
	go func() {
		_, _ = client.Write(data)
		_ = client.CloseWrite()
	}()
	_ = server.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(server)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read %d, %v", len(got), err)
	}
	if _, err := client.Write([]byte("x")); err == nil {
		t.Error("Write after CloseWrite succeeded")
	}
	if _, err := server.Write([]byte("reply")); err != nil {
		t.Fatal(err)
	}
	if err := server.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err = io.ReadAll(client)
	if err != nil || string(got) != "reply" {
		t.Fatalf("reply %q, %v", got, err)
	}
}

func TestHvsockDeadlines(t *testing.T) {
	client, server := hvPair(t)
	_ = client.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	_, err := client.Read(make([]byte, 1))
	var ne net.Error
	if !errors.Is(err, os.ErrDeadlineExceeded) || !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("Read = %v", err)
	}
	_ = client.SetReadDeadline(time.Time{})
	go func() { _, _ = server.Write([]byte("ok")) }()
	buf := make([]byte, 2)
	if _, err := io.ReadFull(client, buf); err != nil || string(buf) != "ok" {
		t.Fatalf("after timeout: %q, %v", buf, err)
	}
	// A write deadline ends a write the peer never drains.
	_ = server.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	chunk := make([]byte, 1<<20)
	sent := 0
	for {
		n, err := server.Write(chunk)
		sent += n
		if errors.Is(err, os.ErrDeadlineExceeded) {
			break
		}
		if err != nil {
			t.Fatalf("Write = %v", err)
		}
		if sent > 1<<30 {
			t.Fatal("1 GiB written to a peer that does not read")
		}
	}
	t.Logf("write timed out after %d bytes", sent)
}

func TestHvsockCloseUnblocks(t *testing.T) {
	client, server := hvPair(t)
	errc := make(chan error, 1)
	go func() {
		_, err := client.Read(make([]byte, 1))
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	client.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Read = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read still blocked after Close")
	}
	if err := client.CloseWrite(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("CloseWrite after Close = %v", err)
	}
	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := server.Read(make([]byte, 1)); err == nil {
		t.Fatal("peer read data from a closed socket")
	}
}

func TestHvsockAcceptClose(t *testing.T) {
	ln, _ := hvListen(t)
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
			t.Fatalf("Accept = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept still blocked after Close")
	}
}

// A dial to a service nobody listens on fails, is retried Retries times, and returns
// as soon as ctx ends.
func TestHvsockDialRetriesAndContext(t *testing.T) {
	_, _ = hvListen(t) // skips without AF_HYPERV
	addr := randLoopbackAddr()
	start := time.Now()
	_, err := (&HvsockDialer{Retries: 3, RetryWait: 50 * time.Millisecond}).Dial(context.Background(), addr)
	if err == nil {
		t.Fatal("dial with no listener succeeded")
	}
	t.Logf("no listener: %v after %v", err, time.Since(start))
	if canRedial(err) && time.Since(start) < 150*time.Millisecond {
		t.Errorf("retries did not wait: %v", time.Since(start))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start = time.Now()
	_, err = (&HvsockDialer{Retries: 1000, RetryWait: 20 * time.Millisecond}).Dial(ctx, addr)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dial = %v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("dial returned after %v", d)
	}

	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if _, err := DialHvsock(ctx, addr); !errors.Is(err, context.Canceled) {
		t.Fatalf("dial with a done ctx = %v", err)
	}
}

func TestHvsockManyConns(t *testing.T) {
	ln, addr := hvListen(t)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
				_ = c.(*HvsockConn).CloseWrite()
			}()
		}
	}()
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			c, err := DialHvsock(ctx, addr)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			msg := bytes.Repeat([]byte{byte(i)}, 100<<10)
			go func() { _, _ = c.Write(msg); _ = c.(*HvsockConn).CloseWrite() }()
			got, err := io.ReadAll(c)
			if err != nil || !bytes.Equal(got, msg) {
				errs <- errors.New("echo mismatch")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestHvsockAddrRaw(t *testing.T) {
	a := randLoopbackAddr()
	r := a.raw()
	b := (*[unsafe.Sizeof(rawHvsockAddr{})]byte)(unsafe.Pointer(&r))[:]
	got, err := rawHvsockAddrFromBytes(b)
	if err != nil || got != a {
		t.Fatalf("round trip %v, %v", got, err)
	}
	if a.String() != LoopbackVMID.String()+":"+a.ServiceID.String() {
		t.Fatal(a.String())
	}
}
