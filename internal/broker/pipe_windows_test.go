//go:build windows

package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/broker/proto"
)

var pipeSeq atomic.Int64

func testPipeName(t *testing.T) string {
	return fmt.Sprintf(`\\.\pipe\hyperbutler_broker_test_%d_%d_%s`, windows.GetCurrentProcessId(), pipeSeq.Add(1),
		strings.NewReplacer("/", "_", "\\", "_").Replace(t.Name()))
}

// servePipe starts a server with a fake backend on a fresh pipe for the current user.
func servePipe(t *testing.T) (*PipeListener, *FakeBackend) {
	t.Helper()
	me := currentSID(t)
	s, be := newTestServer(t, Config{AllowedSIDs: []string{me}, Logf: t.Logf})
	l, err := ListenPipe(PipeConfig{Name: testPipeName(t), ClientSID: me, Instances: 2})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(l) }()
	t.Cleanup(func() {
		l.Close()
		if err := <-done; err != nil && !errors.Is(err, ErrListenerClosed) {
			t.Errorf("Serve: %v", err)
		}
	})
	return l, be
}

func dial(t *testing.T, name string) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Impersonation level: the broker needs it to identify the caller and to take
	// handles from it. go-winio's DialPipe connects anonymously.
	c, err := winio.DialPipeAccessImpLevel(ctx, name, windows.GENERIC_READ|windows.GENERIC_WRITE, winio.PipeImpLevelImpersonation)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// isClosed reports whether err means the server closed the connection (go-winio reports
// a disconnected pipe as ERROR_PIPE_NOT_CONNECTED rather than io.EOF).
func isClosed(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED) || errors.Is(err, windows.ERROR_BROKEN_PIPE)
}

func roundTrip(t *testing.T, c net.Conn, req proto.Request) proto.Reply {
	t.Helper()
	b, err := proto.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(b); err != nil {
		t.Fatal(err)
	}
	return readReply(t, c)
}

func readReply(t *testing.T, c net.Conn) proto.Reply {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var buf [proto.MaxFrameSize]byte
	f, err := proto.ReadFrame(c, &buf)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	r, err := proto.DecodeReply(f)
	if err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	return r
}

// A name that already exists, for example because another process squats on it, makes
// ListenPipe fail with ErrPipeExists instead of joining the existing pipe.
func TestPipeSquatting(t *testing.T) {
	name := testPipeName(t)
	n16, _ := windows.UTF16PtrFromString(name)
	// The squatter gives everyone full access, including FILE_CREATE_PIPE_INSTANCE.
	sd, err := windows.SecurityDescriptorFromString("D:(A;;GA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	h, err := windows.CreateNamedPipe(n16, windows.PIPE_ACCESS_DUPLEX, windows.PIPE_TYPE_BYTE,
		windows.PIPE_UNLIMITED_INSTANCES, 512, 512, 0, sa)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)

	l, err := ListenPipe(PipeConfig{Name: name})
	if err == nil {
		l.Close()
		t.Fatal("ListenPipe joined a squatted pipe")
	}
	if !errors.Is(err, ErrPipeExists) {
		t.Fatalf("err = %v, want ErrPipeExists", err)
	}
}

func TestPipeSecondListenerFails(t *testing.T) {
	l, _ := servePipe(t)
	if l2, err := ListenPipe(PipeConfig{Name: l.Name()}); !errors.Is(err, ErrPipeExists) {
		if l2 != nil {
			l2.Close()
		}
		t.Fatalf("second ListenPipe: %v, want ErrPipeExists", err)
	}
}

func TestPipeSDDL(t *testing.T) {
	got := PipeSDDL("S-1-5-18", sidA)
	want := "O:S-1-5-18G:S-1-5-18D:P(A;;GA;;;S-1-5-18)(A;;0x0012019b;;;" + sidA + ")"
	if got != want {
		t.Fatalf("PipeSDDL = %s, want %s", got, want)
	}
	if clientPipeAccess&0x4 != 0 {
		t.Fatal("clients may create pipe instances")
	}
	if !strings.HasSuffix(PipeSDDL("S-1-5-18", ""), ";;;IU)") {
		t.Fatal("shared endpoint is not limited to interactive users")
	}
}

func TestPipeSecurityDescriptor(t *testing.T) {
	l, _ := servePipe(t)
	sd, err := windows.GetSecurityInfo(l.insts[0].h, windows.SE_KERNEL_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	me, err := windows.StringToSid(currentSID(t))
	if err != nil {
		t.Fatal(err)
	}
	// Compare SIDs, not SDDL text: SDDL abbreviates some accounts (the built-in
	// Administrator, RID 500, which CI runners use, is "LA").
	// The server and client are the same user in this test, so the two ACEs are for the
	// same SID; check that nobody else appears.
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(me) {
		t.Fatalf("pipe SD %s: owner is not %s (%v)", sd, me, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 2 {
		t.Fatalf("pipe SD %s: want a DACL with 2 ACEs (%v)", sd, err)
	}
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !sid.Equals(me) {
			t.Fatalf("pipe SD %s: ACE %d is not an allow ACE for %s", sd, i, me)
		}
	}
}

func TestPipeEndToEnd(t *testing.T) {
	l, be := servePipe(t)
	c := dial(t, l.Name())

	if r := roundTrip(t, c, proto.Request{Op: proto.OpVersion}); r.ProtoVersion != proto.Version {
		t.Fatalf("version: %+v", r)
	}
	wantCode(t, roundTrip(t, c, proto.Request{Op: proto.OpStart}), proto.CodeNoDataLocation)

	// Pass a handle of this (client) process: the server duplicates it from the client
	// process found through the pipe, and validates it for real.
	f, err := os.Open(goodDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	wantOK(t, roundTrip(t, c, proto.Request{Op: proto.OpSetDataLocation, Handle: uint64(f.Fd())}), proto.OpSetDataLocation)
	f.Close() // the broker holds its own handle now

	r := roundTrip(t, c, proto.Request{Op: proto.OpStart})
	wantOK(t, r, proto.OpStart)
	if r.State != proto.StateRunning || !r.DataLocationSet {
		t.Fatalf("start: %+v", r)
	}
	if be.Starts[currentSID(t)] != 1 {
		t.Fatalf("starts keyed by %v, want the pipe client's SID", be.Starts)
	}
}

// On a real pipe, the lease's connection outlives IdleTimeout, and Close still ends its
// read, which has no deadline, and with it the VM.
func TestPipeLeaseReadAndClose(t *testing.T) {
	me := currentSID(t)
	s, be := newTestServer(t, Config{AllowedSIDs: []string{me}, Logf: t.Logf, IdleTimeout: 200 * time.Millisecond})
	l, err := ListenPipe(PipeConfig{Name: testPipeName(t), ClientSID: me, Instances: 2})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(l) }()
	c := dial(t, l.Name())
	f, err := os.Open(goodDir(t))
	if err != nil {
		t.Fatal(err)
	}
	wantOK(t, roundTrip(t, c, proto.Request{Op: proto.OpSetDataLocation, Handle: uint64(f.Fd())}), proto.OpSetDataLocation)
	f.Close()
	wantOK(t, roundTrip(t, c, proto.Request{Op: proto.OpStart}), proto.OpStart)
	time.Sleep(700 * time.Millisecond)
	if r := roundTrip(t, c, proto.Request{Op: proto.OpStatus}); r.State != proto.StateRunning {
		t.Fatalf("status after the idle wait: %+v", r)
	}
	start := time.Now()
	l.Close()
	if err := <-done; err != nil && !errors.Is(err, ErrListenerClosed) {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Close took %v with the lease's read waiting", d)
	}
	if be.Stops[me] != 1 {
		t.Fatalf("after Close: %d stops, want 1", be.Stops[me])
	}
}

func TestPipeIdentifyIsClientToken(t *testing.T) {
	l, _ := servePipe(t)
	c := dial(t, l.Name())
	// The backend keys by the token SID; status for it works, which needs Identify to
	// have succeeded on the real pipe (it would be AccessDenied otherwise).
	wantOK(t, roundTrip(t, c, proto.Request{Op: proto.OpStatus}), proto.OpStatus)
}

// A client that connects with an anonymous security QoS (go-winio's default) gives the
// broker no identity and is rejected.
func TestPipeAnonymousClientRejected(t *testing.T) {
	l, _ := servePipe(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := winio.DialPipeContext(ctx, l.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	wantCode(t, roundTrip(t, c, proto.Request{Op: proto.OpStatus}), proto.CodeAccessDenied)
}

func TestPipeOversizeFrameClosesConnection(t *testing.T) {
	l, _ := servePipe(t)
	c := dial(t, l.Name())
	if _, err := c.Write([]byte{0xff, 0xff, 1, 2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	wantCode(t, readReply(t, c), proto.CodeBadRequest)
	var b [1]byte
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(b[:]); !isClosed(err) {
		t.Fatalf("after bad frame: %v, want the server to disconnect", err)
	}
}

// An idle client is disconnected after IdleTimeout, and Close interrupts connections
// that are waiting.
func TestPipeIdleAndClose(t *testing.T) {
	me := currentSID(t)
	s, _ := newTestServer(t, Config{IdleTimeout: 300 * time.Millisecond})
	l, err := ListenPipe(PipeConfig{Name: testPipeName(t), ClientSID: me, Instances: 1})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(l) }()

	c := dial(t, l.Name())
	var b [1]byte
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(b[:]); !isClosed(err) {
		t.Fatalf("idle client: %v, want the server to disconnect", err)
	}

	// The single instance is free again; a second client blocks in a request read when
	// Close is called.
	c2 := dial(t, l.Name())
	_ = c2
	start := time.Now()
	l.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Close took %v", d)
	}
	if err := s.Serve(l); !errors.Is(err, ErrListenerClosed) {
		t.Fatalf("Serve after Close: %v", err)
	}
}

func TestServeTwiceFails(t *testing.T) {
	l, _ := servePipe(t)
	s, _ := newTestServer(t, Config{})
	// servePipe's Serve may not have registered yet; wait for it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		l.mu.Lock()
		started := l.started
		l.mu.Unlock()
		if started || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := s.Serve(l); !errors.Is(err, ErrAlreadyServing) {
		t.Fatalf("second Serve: %v, want ErrAlreadyServing", err)
	}
}

// The shared-endpoint form (interactive users) produces a valid, live pipe whose DACL
// has exactly the broker and IU.
func TestPipeSharedEndpointACL(t *testing.T) {
	l, err := ListenPipe(PipeConfig{Name: testPipeName(t), Instances: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	sd, err := windows.GetSecurityInfo(l.insts[0].h, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	s := sd.String()
	if !strings.Contains(s, ";;;IU)") || strings.Count(s, "(") != 2 {
		t.Fatalf("shared endpoint DACL %s", s)
	}
}

// The NPFS root (\\.\pipe\) opens as a directory File object on a non-disk device, which
// exercises the local-disk check without needing a network share.
func TestDataDirNotLocalDisk(t *testing.T) {
	h := openDir(t, `\\.\pipe\`, false, 0)
	_, err := take(t, h)
	var re *rejectError
	if !errors.As(err, &re) || re.code != proto.CodeDataLocationRejected {
		t.Fatalf("err = %v, want rejection", err)
	}
	if re.reason != "not on a local disk" && re.reason != "cannot open the handle as a directory" {
		t.Fatalf("rejected for %q", re.reason)
	}
	t.Logf("rejected for %q", re.reason)
}

func TestListenPipeRejectsBadConfig(t *testing.T) {
	for _, cfg := range []PipeConfig{
		{Name: `\\server\pipe\x`},
		{Name: testPipeName(t), Instances: maxPipeInstances + 1},
		{Name: testPipeName(t), ClientSID: "not-a-sid"},
	} {
		if l, err := ListenPipe(cfg); err == nil {
			l.Close()
			t.Errorf("ListenPipe(%+v) accepted", cfg)
		}
	}
}
