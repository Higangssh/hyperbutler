//go:build windows

package broker

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/broker/proto"
)

// The tests here are the client's half of the M6 review's finding 1 (ADR 0004): a
// host's sleep while a request waits for its reply must not end the connection, and so
// the lease and the VM; the connection's real end still must.

// Script steps for scriptConn's Reads.
const (
	stepTimeout = -1 // fail with os.ErrDeadlineExceeded, reading nothing
	stepBroken  = -2 // fail with another error
)

// scriptConn is a net.Conn whose Reads follow script: a step > 0 reads at most that
// many bytes; the steps above fail. With the script used up it reads normally.
type scriptConn struct {
	net.Conn
	mu     sync.Mutex
	script []int
	// writeErr, if set, is what every Write returns, writing nothing.
	writeErr error
}

func (s *scriptConn) Write(b []byte) (int, error) {
	s.mu.Lock()
	err := s.writeErr
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return s.Conn.Write(b)
}

func (s *scriptConn) left() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.script)
}

func (s *scriptConn) Read(b []byte) (int, error) {
	s.mu.Lock()
	step := 0
	if len(s.script) > 0 {
		step, s.script = s.script[0], s.script[1:]
	}
	s.mu.Unlock()
	switch {
	case step == stepTimeout:
		return 0, os.ErrDeadlineExceeded
	case step == stepBroken:
		return 0, errors.New("the pipe broke")
	case step > 0 && len(b) > step:
		b = b[:step]
	}
	return s.Conn.Read(b)
}

// fakeServer answers the requests that arrive on conn: respond gets the request's
// index and the request and returns the bytes to write, or nil to close the connection.
// It returns the requests it read once conn is closed.
func fakeServer(t *testing.T, conn net.Conn, respond func(i int, req proto.Request) []byte) <-chan []proto.Request {
	t.Helper()
	got := make(chan []proto.Request, 1)
	done := make(chan struct{})
	t.Cleanup(func() { _ = conn.Close(); <-done })
	go func() {
		defer close(done)
		var reqs []proto.Request
		defer func() { got <- reqs }()
		defer conn.Close()
		var buf [proto.MaxFrameSize]byte
		for i := 0; ; i++ {
			f, err := proto.ReadFrame(conn, &buf)
			if err != nil {
				return
			}
			req, err := proto.DecodeRequest(f)
			if err != nil {
				t.Errorf("request %d: %v", i, err)
				return
			}
			reqs = append(reqs, req)
			b := respond(i, req)
			if b == nil {
				return
			}
			if _, err := conn.Write(b); err != nil {
				return
			}
		}
	}()
	return got
}

// reply encodes a success reply to op with state st (nil, which closes the connection,
// if it cannot).
func reply(t *testing.T, op proto.Op, st proto.State) []byte {
	t.Helper()
	b, err := proto.EncodeReply(statusReply(op, st))
	if err != nil {
		t.Errorf("encode a reply to %s: %v", op, err)
		return nil
	}
	return b
}

// statusReply is a success reply to op with state st and a valid size.
func statusReply(op proto.Op, st proto.State) proto.Reply {
	return proto.Reply{Type: proto.ReplyFlag | uint8(op), State: st, CPUs: 1, MemoryMB: proto.MinMemoryMB}
}

// slept is an awake clock that never moves: the host slept through every wait.
func slept() time.Duration { return 0 }

// awakeClock is an awake clock that moves on by step at every reading: the host was
// awake through every wait, and the broker silent.
func awakeClock(step time.Duration) func() time.Duration {
	var mu sync.Mutex
	var now time.Duration
	return func() time.Duration {
		mu.Lock()
		defer mu.Unlock()
		now += step
		return now
	}
}

// newScriptClient is a Client on one end of a net.Pipe whose Reads follow script, and
// the server's end.
func newScriptClient(t *testing.T, awake func() time.Duration, script ...int) (*Client, *scriptConn, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	sc := &scriptConn{Conn: a, script: script}
	c := &Client{c: sc, awake: awake}
	t.Cleanup(func() { _ = c.Close(); _ = b.Close() })
	return c, sc, b
}

func wantLost(t *testing.T, err error, what string) {
	t.Helper()
	if !errors.Is(err, ErrLost) {
		t.Fatalf("%s: %v, want ErrLost", what, err)
	}
	if errors.Is(err, ErrNoReply) {
		t.Fatalf("%s: %v is ErrNoReply too", what, err)
	}
}

// A wait whose deadline passed while the host slept (the awake clock did not move) is
// made again for the same reply: the request succeeds, and was sent once.
func TestClientRewaitsAfterSleep(t *testing.T) {
	c, _, srv := newScriptClient(t, slept, stepTimeout, stepTimeout, stepTimeout)
	got := fakeServer(t, srv, func(_ int, req proto.Request) []byte { return reply(t, req.Op, proto.StateRunning) })
	r, err := c.Status()
	if err != nil || r.State != proto.StateRunning {
		t.Fatalf("status after a sleep: %+v, %v", r, err)
	}
	if r, err := c.Status(); err != nil || r.State != proto.StateRunning {
		t.Fatalf("the next status: %+v, %v", r, err)
	}
	_ = c.Close()
	if reqs := <-got; len(reqs) != 2 {
		t.Fatalf("the server read %d requests, want 2", len(reqs))
	}
}

// A reply that does not come in the request's awake time is ErrNoReply: the connection
// stays, and the next request reads the late reply first, checking it against the
// request it answers, then its own.
func TestClientLateReplyKeepsConnection(t *testing.T) {
	c, sc, srv := newScriptClient(t, awakeClock(clientIOTimeout), stepTimeout)
	got := fakeServer(t, srv, func(i int, req proto.Request) []byte {
		if i == 0 {
			return reply(t, req.Op, proto.StatePaused)
		}
		return reply(t, req.Op, proto.StateRunning)
	})
	_, err := c.Pause(false)
	if !errors.Is(err, ErrNoReply) || !errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, ErrLost) {
		t.Fatalf("pause with no reply in time: %v", err)
	}
	if !c.owing || c.owed != proto.OpPause {
		t.Fatalf("owing %v %s after the timeout", c.owing, c.owed)
	}
	// The late reply to the pause (a pause reply, Paused) is read first and dropped.
	r, err := c.Status()
	if err != nil || r.State != proto.StateRunning || r.Type != proto.ReplyFlag|uint8(proto.OpStatus) {
		t.Fatalf("status after a late reply: %+v, %v", r, err)
	}
	if c.owing {
		t.Fatal("still owing after the late reply was read")
	}
	sc.mu.Lock()
	left := len(sc.script)
	sc.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d script steps left", left)
	}
	_ = c.Close()
	reqs := <-got
	if len(reqs) != 2 || reqs[0].Op != proto.OpPause || reqs[1].Op != proto.OpStatus {
		t.Fatalf("the server read %+v", reqs)
	}
}

// A late reply that is not the owed request's breaks the protocol: the connection ends.
func TestClientLateReplyOfWrongType(t *testing.T) {
	c, _, srv := newScriptClient(t, awakeClock(clientIOTimeout), stepTimeout)
	fakeServer(t, srv, func(i int, req proto.Request) []byte {
		if i == 0 {
			return reply(t, proto.OpPause, proto.StatePaused) // not a status reply
		}
		return reply(t, req.Op, proto.StateRunning)
	})
	if _, err := c.Status(); !errors.Is(err, ErrNoReply) {
		t.Fatalf("status with no reply in time: %v", err)
	}
	_, err := c.Status()
	wantLost(t, err, "a late reply of the wrong type")
}

// A frame a timeout cut is resumed where it was, after a sleep and after ErrNoReply:
// the stream stays in step.
func TestClientResumesCutFrame(t *testing.T) {
	c, _, srv := newScriptClient(t, slept, 1, stepTimeout, 3, stepTimeout, 2)
	got := fakeServer(t, srv, func(_ int, req proto.Request) []byte { return reply(t, req.Op, proto.StateRunning) })
	if r, err := c.Status(); err != nil || r.State != proto.StateRunning {
		t.Fatalf("status with a cut reply: %+v, %v", r, err)
	}

	c.awake = awakeClock(clientIOTimeout)
	c.c.(*scriptConn).script = []int{1, stepTimeout}
	if _, err := c.Status(); !errors.Is(err, ErrNoReply) {
		t.Fatalf("status cut after one byte: %v", err)
	}
	if c.have != 1 {
		t.Fatalf("%d bytes of the cut reply kept, want 1", c.have)
	}
	if r, err := c.Status(); err != nil || r.State != proto.StateRunning {
		t.Fatalf("status after the cut reply: %+v, %v", r, err)
	}
	_ = c.Close()
	if reqs := <-got; len(reqs) != 3 {
		t.Fatalf("the server read %d requests, want 3", len(reqs))
	}
}

// The connection's real end still ends it, at once and for good: the broker closing it
// (or exiting), Close, a broken pipe, and a reply that breaks the protocol.
func TestClientEndsOnLostConnection(t *testing.T) {
	cases := []struct {
		name    string
		script  []int
		respond func(t *testing.T, req proto.Request) []byte
		want    error
	}{
		{"broker closed", nil, func(*testing.T, proto.Request) []byte { return nil }, io.ErrUnexpectedEOF},
		{"pipe broke", []int{stepBroken}, nil, nil},
		{"reply of another verb", nil, func(t *testing.T, _ proto.Request) []byte { return reply(t, proto.OpPause, proto.StateRunning) }, nil},
		{"empty frame", nil, func(*testing.T, proto.Request) []byte { return []byte{0, 0} }, proto.ErrInvalid},
		{"frame too large", nil, func(*testing.T, proto.Request) []byte { return []byte{0xff, 0xff} }, proto.ErrFrameTooLarge},
		{"malformed reply", nil, func(*testing.T, proto.Request) []byte { return []byte{0, 2, 0x7f, 0x84} }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Even a host that slept through everything does not keep a lost connection.
			c, _, srv := newScriptClient(t, slept, tc.script...)
			respond := tc.respond
			if respond == nil {
				respond = func(t *testing.T, req proto.Request) []byte { return reply(t, req.Op, proto.StateRunning) }
			}
			fakeServer(t, srv, func(_ int, req proto.Request) []byte { return respond(t, req) })
			start := time.Now()
			_, err := c.Status()
			wantLost(t, err, tc.name)
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("%v, want %v", err, tc.want)
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Fatalf("the lost connection took %v to notice", d)
			}
			_, err2 := c.Status()
			wantLost(t, err2, "the next request")
			if !errors.Is(err2, err) && err2.Error() != err.Error() {
				t.Fatalf("the next request failed with %v, not %v", err2, err)
			}
		})
	}

	t.Run("closed while waiting", func(t *testing.T) {
		c, _, srv := newScriptClient(t, slept)
		hold := make(chan struct{})
		fakeServer(t, srv, func(int, proto.Request) []byte { <-hold; return nil })
		defer close(hold)
		done := make(chan error, 1)
		go func() { _, err := c.Status(); done <- err }()
		time.Sleep(50 * time.Millisecond)
		_ = c.Close()
		select {
		case err := <-done:
			wantLost(t, err, "a request when Close was called")
		case <-time.After(3 * time.Second):
			t.Fatal("Close did not end the request")
		}
		_, err := c.Status()
		wantLost(t, err, "a request after Close")
	})
}

// expiringConn makes its first Read's deadline one already past, as a deadline set
// before a host's sleep is when the host wakes, and closes timedOut when that Read
// failed.
type expiringConn struct {
	net.Conn
	once     sync.Once
	timedOut chan struct{}
}

func (e *expiringConn) Read(b []byte) (int, error) {
	first := false
	e.once.Do(func() { first = true })
	if !first {
		return e.Conn.Read(b)
	}
	_ = e.SetReadDeadline(time.Now().Add(-time.Second))
	n, err := e.Conn.Read(b)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		close(e.timedOut)
	}
	return n, err
}

// On a real pipe: a read whose deadline had passed when it began (the overlapped read is
// canceled on its timeout) is made again, and the reply the broker sends after it is
// read whole.
func TestClientRewaitsOnPipe(t *testing.T) {
	l, err := ListenPipe(PipeConfig{Name: testPipeName(t), ClientSID: currentSID(t), Instances: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	inst := l.insts[0]
	timedOut := make(chan struct{})
	served := make(chan error, 1)
	go func() {
		served <- func() error {
			if err := inst.connect(); err != nil {
				return err
			}
			var buf [proto.MaxFrameSize]byte
			f, err := proto.ReadFrame(inst, &buf)
			if err != nil {
				return err
			}
			req, err := proto.DecodeRequest(f)
			if err != nil {
				return err
			}
			<-timedOut // the client's first wait has ended before the reply is sent
			b, err := proto.EncodeReply(statusReply(req.Op, proto.StateRunning))
			if err != nil {
				return err
			}
			_, err = inst.Write(b)
			return err
		}()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pc, err := dialPipeClient(ctx, l.Name())
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{c: &expiringConn{Conn: pc, timedOut: timedOut}, awake: slept}
	defer c.Close()
	type result struct {
		r   proto.Reply
		err error
	}
	got := make(chan result, 1)
	go func() {
		r, err := c.Status()
		got <- result{r, err}
	}()
	var res result
	var serveErr error
	serverDone := false
	for waiting := true; waiting; {
		select {
		case res = <-got:
			waiting = false
		case serveErr = <-served:
			serverDone = true
			served = nil
			if serveErr != nil {
				_ = c.Close()
				t.Fatalf("the server failed: %v (client: %+v)", serveErr, <-got)
			}
		case <-time.After(10 * time.Second):
			_ = c.Close()
			t.Fatalf("no reply (client: %+v)", <-got)
		}
	}
	if res.err != nil || res.r.State != proto.StateRunning {
		t.Fatalf("status after a deadline that passed in a sleep: %+v, %v", res.r, res.err)
	}
	select {
	case <-timedOut:
	default:
		t.Fatal("the first wait did not time out")
	}
	if !serverDone {
		if err := <-served; err != nil {
			t.Fatal(err)
		}
	}
}

// awakeTime moves on, and not faster than the monotonic clock (it stops in a sleep).
func TestAwakeTime(t *testing.T) {
	m0, a0 := time.Now(), awakeTime()
	time.Sleep(50 * time.Millisecond)
	a1, m1 := awakeTime(), time.Now()
	if a1 <= a0 {
		t.Fatalf("awake time did not move: %v then %v", a0, a1)
	}
	if d, m := a1-a0, m1.Sub(m0); d > m+32*time.Millisecond {
		t.Fatalf("awake time moved %v in %v", d, m)
	}
}

// The awake time is a budget over every wait of one request: with 6 s of awake time
// gone at each timed-out wait the request waits three times (15 s left, then 9, then 3)
// and ends with ErrNoReply, the rest of the script unread.
func TestClientAwakeBudget(t *testing.T) {
	c, sc, srv := newScriptClient(t, awakeClock(6*time.Second), stepTimeout, stepTimeout, stepTimeout, stepTimeout, stepTimeout)
	fakeServer(t, srv, func(_ int, req proto.Request) []byte { return reply(t, req.Op, proto.StateRunning) })
	if _, err := c.Status(); !errors.Is(err, ErrNoReply) {
		t.Fatalf("status with every wait timed out: %v", err)
	}
	if n := sc.left(); n != 2 {
		t.Fatalf("%d script steps left, want 2 (three waits)", n)
	}
}

// A start has its own, longer timeout: a wait that would have ended any other request
// is made again.
func TestClientStartTimeout(t *testing.T) {
	c, sc, srv := newScriptClient(t, awakeClock(clientIOTimeout), stepTimeout, stepTimeout)
	fakeServer(t, srv, func(_ int, req proto.Request) []byte { return reply(t, req.Op, proto.StateRunning) })
	if r, err := c.Start(proto.Request{}); err != nil || r.State != proto.StateRunning {
		t.Fatalf("start after two waits of %v: %+v, %v", clientIOTimeout, r, err)
	}
	if n := sc.left(); n != 0 {
		t.Fatalf("%d script steps left", n)
	}
}

// A failed or timed-out write ends the connection: part of the request may be sent.
func TestClientWriteFailureEnds(t *testing.T) {
	for _, werr := range []error{os.ErrDeadlineExceeded, errors.New("the pipe broke")} {
		c, sc, srv := newScriptClient(t, slept)
		fakeServer(t, srv, func(_ int, req proto.Request) []byte { return reply(t, req.Op, proto.StateRunning) })
		sc.mu.Lock()
		sc.writeErr = werr
		sc.mu.Unlock()
		_, err := c.Status()
		wantLost(t, err, "a request whose write failed with "+werr.Error())
	}
}

// On a real pipe: the broker's end closing (its process exiting closes it) ends the
// connection at once, though the host slept through the wait.
func TestClientEndsOnPipeClosed(t *testing.T) {
	l, err := ListenPipe(PipeConfig{Name: testPipeName(t), ClientSID: currentSID(t), Instances: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	inst := l.insts[0]
	served := make(chan error, 1)
	go func() {
		served <- func() error {
			if err := inst.connect(); err != nil {
				return err
			}
			var buf [proto.MaxFrameSize]byte
			if _, err := proto.ReadFrame(inst, &buf); err != nil {
				return err
			}
			return windows.DisconnectNamedPipe(inst.h)
		}()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pc, err := dialPipeClient(ctx, l.Name())
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{c: pc, awake: slept}
	defer c.Close()
	start := time.Now()
	_, err = c.Status()
	wantLost(t, err, "a request whose pipe the broker closed")
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("%v, want an unexpected EOF", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the closed pipe took %v to notice", d)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}
