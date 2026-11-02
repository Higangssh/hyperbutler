//go:build windows

package broker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/broker/proto"
)

// squat creates a pipe of name the way another user could, granting everyone access.
func squat(t *testing.T, name string) windows.Handle {
	t.Helper()
	n16, _ := windows.UTF16PtrFromString(name)
	sd, err := windows.SecurityDescriptorFromString("D:(A;;GA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateNamedPipe(n16, windows.PIPE_ACCESS_DUPLEX, windows.PIPE_TYPE_BYTE,
		windows.PIPE_UNLIMITED_INSTANCES, 512, 512, 0, &windows.SecurityAttributes{SecurityDescriptor: sd})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// A per-user name that is already taken is reported with its own code and nothing is
// served under another name for that request; the next request draws a new name.
// (Another user cannot know the name in advance: the random part is new each time.)
func TestUserEndpointSquatted(t *testing.T) {
	me := currentSID(t)
	s, _ := newTestServer(t, Config{AllowedSIDs: []string{me}, Logf: t.Logf})
	name := testPipeName(t)
	taken, err := newUserPipeName(name, me)
	if err != nil {
		t.Fatal(err)
	}
	h := squat(t, taken)
	defer windows.CloseHandle(h)
	e, err := ListenEndpoints(s, EndpointsConfig{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	collide := 2 // the next two names collide with the squatter's
	e.newName = func(shared, sid string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if collide > 0 {
			collide--
			return taken, nil
		}
		return newUserPipeName(shared, sid)
	}
	done := make(chan error, 1)
	go func() { done <- e.Serve() }()
	defer func() { e.Close(); <-done }()

	c := dial(t, name)
	wantCode(t, roundTrip(t, c, proto.Request{Op: proto.OpOpenEndpoint}), proto.CodeEndpointSquatted)
	e.mu.Lock()
	n := len(e.users)
	e.mu.Unlock()
	if n != 0 {
		t.Fatal("an endpoint was created after all")
	}
	// The client fails loudly with the code.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Dial(ctx, name); !IsCode(err, proto.CodeEndpointSquatted) {
		t.Fatalf("Dial on a squatted endpoint: %v", err)
	}
	// The next request draws a new name, which nobody holds.
	r := roundTrip(t, c, proto.Request{Op: proto.OpOpenEndpoint})
	if !ValidUserPipeName(name, me, r.PipeName) || r.PipeName == taken {
		t.Fatalf("after the collision: %+v", r)
	}
}

func TestEndpointsLimits(t *testing.T) {
	me := currentSID(t)
	s, _ := newTestServer(t, Config{validateDataDir: acceptAll})
	e, err := ListenEndpoints(s, EndpointsConfig{Name: testPipeName(t), Instances: 1, UserInstances: 1, MaxUsers: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := ListenEndpoints(s, EndpointsConfig{Name: testPipeName(t)}); err == nil {
		t.Fatal("a second Endpoints on one server")
	}
	if _, err := e.open("not-a-sid"); err == nil {
		t.Fatal("opened an endpoint for a bad SID")
	}
	first, err := e.open(me)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := e.open(me); again != first {
		t.Fatal("a second open made another endpoint")
	}
	// At the cap, an idle endpoint (no connection, no VM) is retired for a new user.
	if _, err := e.open(sidB); err != nil {
		t.Fatalf("idle endpoint not retired: %v", err)
	}
	// sidB's endpoint is idle too, so it is retired for me, and mine has a new name.
	if again, err := e.open(me); err != nil || again == first {
		t.Fatalf("reopen after retirement: %q, %v", again, err)
	}
	// An endpoint whose user has a connection, or a running VM, is not retired.
	s.mu.Lock()
	s.conns[me] = 1
	s.mu.Unlock()
	if _, err := e.open(sidB); !errors.Is(err, errTooManyEndpoints) {
		t.Fatalf("a busy endpoint was retired: %v", err)
	}
	s.mu.Lock()
	delete(s.conns, me)
	s.mu.Unlock()
	if _, err := s.cfg.Backend.Start(context.Background(), VMSpec{Owner: me, CPUs: 1, MemoryMB: 1024}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.open(sidB); !errors.Is(err, errTooManyEndpoints) {
		t.Fatalf("the endpoint of a running VM was retired: %v", err)
	}
	e.Close()
	if _, err := e.open(me); err == nil {
		t.Fatal("opened after Close")
	}
	s2, _ := newTestServer(t, Config{})
	if _, err := ListenEndpoints(s2, EndpointsConfig{Name: `\\.\pipe\` + strings.Repeat("x", proto.MaxPipeNameLen-40)}); err == nil {
		t.Fatal("accepted a shared name too long for per-user names")
	}
}

// The client goes through the shared endpoint to its own, and refuses an endpoint name
// not of its own form.
func TestClientUsesOwnEndpoint(t *testing.T) {
	e, s, be := serveEndpoints(t)
	me := currentSID(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, e.Name())
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(goodDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetDataLocation(windows.Handle(f.Fd())); err != nil {
		t.Fatal(err)
	}
	f.Close()
	r, err := c.Start(proto.Request{})
	if err != nil || r.Secret.IsZero() {
		t.Fatalf("start: secret set %v, %v", !r.Secret.IsZero(), err)
	}
	if be.StartCount(me) != 1 {
		t.Fatal("the start did not reach the backend for this user")
	}
	c.Close()

	// A shared endpoint that names another pipe is not followed: the shared one itself,
	// another user's form, or this user's prefix without the random part.
	other, _ := newUserPipeName(e.Name(), sidB)
	mine, _ := newUserPipeName(e.Name(), me)
	for _, name := range []string{e.Name(), other, mine[:len(mine)-16]} {
		s.mu.Lock()
		s.opener = func(string) (string, error) { return name, nil }
		s.mu.Unlock()
		if _, err := Dial(ctx, e.Name()); err == nil || !strings.Contains(err.Error(), "not this user's") {
			t.Fatalf("followed %q: %v", name, err)
		}
	}
	// A VM verb sent to the shared endpoint itself is refused.
	sc, _, err := dialPipe(ctx, e.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	if _, err := sc.Status(); !IsCode(err, proto.CodeWrongEndpoint) {
		t.Fatalf("status on the shared endpoint: %v", err)
	}
}

// Connections that never send a frame hold the shared endpoint only briefly: holding
// every instance, a Dial still gets through within 2 s. (In this test the silent
// connections are the dialer's own user, so the per-user cap applies to them too.)
func TestSharedEndpointSilentClients(t *testing.T) {
	me := currentSID(t)
	s, _ := newTestServer(t, Config{AllowedSIDs: []string{me}, FirstFrameTimeout: 300 * time.Millisecond, Logf: t.Logf})
	e, err := ListenEndpoints(s, EndpointsConfig{Name: testPipeName(t)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.Serve() }()
	defer func() { e.Close(); <-done }()

	// Hold every instance of the shared endpoint, saying nothing.
	for i := 0; i < maxPipeInstances; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		c, err := dialRaw(ctx, e.Name())
		cancel()
		if err != nil {
			break // the per-user cap already turns some away
		}
		t.Cleanup(func() { c.Close() })
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	c, err := Dial(ctx, e.Name())
	if err != nil {
		t.Fatalf("Dial behind %d silent clients: %v", maxPipeInstances, err)
	}
	c.Close()
	t.Logf("Dial took %v", time.Since(start))
}

// dialRaw connects to a pipe at impersonation level without any request.
func dialRaw(ctx context.Context, name string) (net.Conn, error) {
	c, _, err := dialPipe(ctx, name)
	if err != nil {
		return nil, err
	}
	return c.c, nil
}

// deadlineConn records the read deadlines the server sets.
type deadlineConn struct {
	conn
	reads []time.Duration
}

func (c *deadlineConn) SetReadDeadline(t time.Time) error {
	c.reads = append(c.reads, time.Until(t))
	return nil
}
func (c *deadlineConn) SetWriteDeadline(time.Time) error { return nil }

// The shared endpoint waits FirstFrameTimeout for the first frame and IdleTimeout after
// it; a per-user endpoint waits IdleTimeout throughout.
func TestFirstFrameTimeout(t *testing.T) {
	s, _ := newTestServer(t, Config{FirstFrameTimeout: time.Second, IdleTimeout: time.Minute})
	s.opener = func(string) (string, error) { return `\\.\pipe\x`, nil }
	two := encode(t, proto.Request{Op: proto.OpVersion}, proto.Request{Op: proto.OpVersion})
	for _, tc := range []struct {
		owner       string
		first, next time.Duration
	}{{"", time.Second, time.Minute}, {sidA, time.Minute, time.Minute}} {
		c := &deadlineConn{conn: conn{in: bytes.NewReader(two)}}
		if err := s.ServeConn(context.Background(), c, &localPeer{sid: sidA}, tc.owner); err != nil {
			t.Fatal(err)
		}
		if len(c.reads) < 2 || !near(c.reads[0], tc.first) || !near(c.reads[1], tc.next) {
			t.Fatalf("owner %q: read deadlines %v, want %v then %v", tc.owner, c.reads, tc.first, tc.next)
		}
	}
	// The default is short.
	s2, _ := newTestServer(t, Config{})
	if s2.cfg.FirstFrameTimeout <= 0 || s2.cfg.FirstFrameTimeout > 5*time.Second {
		t.Fatalf("default first-frame timeout %v", s2.cfg.FirstFrameTimeout)
	}
}

func near(a, b time.Duration) bool {
	d := a - b
	return d > -200*time.Millisecond && d < 200*time.Millisecond
}

// processPeer is a localPeer whose client process user is known before any frame.
type processPeer struct {
	localPeer
	procUser string
}

func (p *processPeer) ClientProcessUser() (string, error) { return p.procUser, nil }

// Before the first frame, a user's connections to the shared endpoint are capped by the
// user of the client process, so one user's silent connections cannot hold every
// instance; another user is not affected.
func TestSharedEndpointProcessCap(t *testing.T) {
	s, _ := newTestServer(t, Config{MaxConnsPerUser: 2})
	hold := make(chan struct{})
	defer close(hold)
	held := make(chan error, 2)
	for range 2 {
		c := &conn{in: blockingReader{hold}}
		go func() { held <- s.ServeConn(context.Background(), c, &processPeer{localPeer{sid: sidB}, sidB}, "") }()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		n := s.conns["process/"+sidB]
		s.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the silent connections were never counted")
		}
		time.Sleep(time.Millisecond)
	}
	third := &conn{in: io.MultiReader()}
	if err := s.ServeConn(context.Background(), third, &processPeer{localPeer{sid: sidB}, sidB}, ""); err == nil {
		t.Fatal("a third silent connection of the same user was kept")
	}
	if r := decodeReplies(t, third.out.Bytes()); len(r) != 1 || r[0].Code != proto.CodeBusy {
		t.Fatalf("third: %+v", r)
	}
	// User A still gets in.
	c := &conn{in: bytes.NewReader(encode(t, proto.Request{Op: proto.OpVersion}))}
	if err := s.ServeConn(context.Background(), c, &processPeer{localPeer{sid: sidA}, sidA}, ""); err != nil {
		t.Fatal(err)
	}
	if r := decodeReplies(t, c.out.Bytes()); len(r) != 1 || r[0].IsError() {
		t.Fatalf("A: %+v", r)
	}
}
