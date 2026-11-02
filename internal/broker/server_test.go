//go:build windows

package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/broker/proto"
	"github.com/Higangssh/hyperbutler/internal/vmtemplate"
)

const (
	sidA = "S-1-5-21-1000-2000-3000-1001"
	sidB = "S-1-5-21-1000-2000-3000-1002"
)

// localPeer is a Peer for a client in this process: the identity is simulated, but
// handles are duplicated from this process for real, and impersonation is a no-op
// because the client is the current user.
type localPeer struct {
	sid         string
	identifyErr error
	identified  int
}

func (p *localPeer) Identify() (Identity, error) {
	p.identified++
	if p.identifyErr != nil {
		return Identity{}, p.identifyErr
	}
	return Identity{SID: p.sid}, nil
}

func (p *localPeer) DuplicateHandle(v uint64) (windows.Handle, error) {
	var dup windows.Handle
	cur := windows.CurrentProcess()
	err := windows.DuplicateHandle(cur, windows.Handle(v), cur, &dup, 0, false, windows.DUPLICATE_SAME_ACCESS)
	return dup, err
}

func (p *localPeer) Impersonate(fn func() error) error { return fn() }

// conn is an in-memory connection: reads come from in, writes go to out.
type conn struct {
	in  io.Reader
	out bytes.Buffer
}

func (c *conn) Read(b []byte) (int, error)  { return c.in.Read(b) }
func (c *conn) Write(b []byte) (int, error) { return c.out.Write(b) }

func newTestServer(t *testing.T, cfg Config) (*Server, *FakeBackend) {
	t.Helper()
	be := NewFakeBackend()
	if cfg.Backend == nil {
		cfg.Backend = be
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, be
}

func acceptAll(windows.Handle, string) error { return nil }

func encode(t *testing.T, reqs ...proto.Request) []byte {
	t.Helper()
	var b []byte
	for _, r := range reqs {
		f, err := proto.EncodeRequest(r)
		if err != nil {
			t.Fatal(err)
		}
		b = append(b, f...)
	}
	return b
}

func decodeReplies(t *testing.T, b []byte) []proto.Reply {
	t.Helper()
	var out []proto.Reply
	r := bytes.NewReader(b)
	var buf [proto.MaxFrameSize]byte
	for r.Len() > 0 {
		f, err := proto.ReadFrame(r, &buf)
		if err != nil {
			t.Fatalf("reply stream: %v", err)
		}
		rep, err := proto.DecodeReply(f)
		if err != nil {
			t.Fatalf("invalid reply %x: %v", f, err)
		}
		out = append(out, rep)
	}
	return out
}

// call sends reqs on one connection as peer p and returns the replies.
func call(t *testing.T, s *Server, p Peer, reqs ...proto.Request) []proto.Reply {
	t.Helper()
	c := &conn{in: bytes.NewReader(encode(t, reqs...))}
	if err := s.ServeConn(context.Background(), c, p); err != nil {
		t.Fatalf("ServeConn: %v", err)
	}
	return decodeReplies(t, c.out.Bytes())
}

func call1(t *testing.T, s *Server, p Peer, req proto.Request) proto.Reply {
	t.Helper()
	r := call(t, s, p, req)
	if len(r) != 1 {
		t.Fatalf("%d replies", len(r))
	}
	return r[0]
}

func wantCode(t *testing.T, r proto.Reply, code proto.Code) {
	t.Helper()
	if !r.IsError() || r.Code != code {
		t.Fatalf("reply %+v, want error code %d", r, code)
	}
}

func wantOK(t *testing.T, r proto.Reply, op proto.Op) {
	t.Helper()
	if r.Type != proto.ReplyFlag|uint8(op) {
		t.Fatalf("reply %+v, want success for %s", r, op)
	}
}

// tempDirHandle opens a fresh temp directory and returns its handle value in this
// process.
func tempDirHandle(t *testing.T) uint64 {
	t.Helper()
	f, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return uint64(f.Fd())
}

func TestVersion(t *testing.T) {
	s, _ := newTestServer(t, Config{Version: "1.2.3"})
	r := call1(t, s, &localPeer{sid: sidA}, proto.Request{Op: proto.OpVersion})
	wantOK(t, r, proto.OpVersion)
	if r.ProtoVersion != proto.Version || r.BrokerVersion != "1.2.3" {
		t.Fatalf("version reply %+v", r)
	}
}

func TestStartNeedsDataLocation(t *testing.T) {
	s, be := newTestServer(t, Config{})
	wantCode(t, call1(t, s, &localPeer{sid: sidA}, proto.Request{Op: proto.OpStart}), proto.CodeNoDataLocation)
	if be.Starts[sidA] != 0 {
		t.Fatal("backend started without a data location")
	}
}

// TestCallerIsolation: per-user state is keyed by the SID the peer reports, and one
// caller can neither see nor change another caller's VM or settings.
func TestCallerIsolation(t *testing.T) {
	s, be := newTestServer(t, Config{validateDataDir: acceptAll})
	a, b := &localPeer{sid: sidA}, &localPeer{sid: sidB}

	// A's VM lives as long as A's connection (its lease).
	sa := openSession(t, s, a)
	wantOK(t, sa.do(proto.Request{Op: proto.OpSetDataLocation, Handle: tempDirHandle(t)}), proto.OpSetDataLocation)
	wantOK(t, sa.do(proto.Request{Op: proto.OpResize, CPUs: 4}), proto.OpResize)
	started := sa.do(proto.Request{Op: proto.OpStart})
	wantOK(t, started, proto.OpStart)
	if started.State != proto.StateRunning || started.CPUs != 4 || started.VMID == ([16]byte{}) {
		t.Fatalf("A start: %+v", started)
	}

	// B sees none of A's state.
	st := call1(t, s, b, proto.Request{Op: proto.OpStatus})
	if st.State != proto.StateStopped || st.DataLocationSet || st.VMID != ([16]byte{}) || st.CPUs != 2 {
		t.Fatalf("B status leaks A's state: %+v", st)
	}
	// B cannot stop, pause, resize or start A's VM.
	wantCode(t, call1(t, s, b, proto.Request{Op: proto.OpStop, Force: true}), proto.CodeNotRunning)
	wantCode(t, call1(t, s, b, proto.Request{Op: proto.OpPause}), proto.CodeNotRunning)
	wantOK(t, call1(t, s, b, proto.Request{Op: proto.OpResize, CPUs: 1}), proto.OpResize)
	wantCode(t, call1(t, s, b, proto.Request{Op: proto.OpStart}), proto.CodeNoDataLocation)

	st = call1(t, s, a, proto.Request{Op: proto.OpStatus})
	if st.State != proto.StateRunning || st.CPUs != 4 || st.VMID != started.VMID || !st.DataLocationSet {
		t.Fatalf("A status after B's requests: %+v", st)
	}
	if be.Starts[sidB] != 0 || be.Starts[sidA] != 1 {
		t.Fatalf("starts: %v", be.Starts)
	}
	wantOK(t, sa.do(proto.Request{Op: proto.OpStop}), proto.OpStop)
	sa.close()
}

func TestIdentityFromPeerOnce(t *testing.T) {
	s, _ := newTestServer(t, Config{})
	p := &localPeer{sid: sidA}
	r := call(t, s, p, proto.Request{Op: proto.OpStatus}, proto.Request{Op: proto.OpVersion}, proto.Request{Op: proto.OpStatus})
	if len(r) != 3 || p.identified != 1 {
		t.Fatalf("%d replies, identified %d times", len(r), p.identified)
	}
	// No bytes, no identification.
	p = &localPeer{sid: sidA}
	if r := call(t, s, p); len(r) != 0 || p.identified != 0 {
		t.Fatalf("empty connection: %d replies, identified %d times", len(r), p.identified)
	}
}

func TestIdentityRejected(t *testing.T) {
	s, _ := newTestServer(t, Config{AllowedSIDs: []string{sidA}})
	for _, p := range []*localPeer{
		{sid: sidB},
		{sid: ""},
		{sid: sidA, identifyErr: errors.New("anonymous")},
	} {
		c := &conn{in: bytes.NewReader(encode(t, proto.Request{Op: proto.OpStatus}, proto.Request{Op: proto.OpStatus}))}
		if err := s.ServeConn(context.Background(), c, p); err == nil {
			t.Errorf("peer %+v: ServeConn returned nil", p)
		}
		r := decodeReplies(t, c.out.Bytes())
		if len(r) != 1 {
			t.Fatalf("peer %+v: %d replies, want 1 then close", p, len(r))
		}
		wantCode(t, r[0], proto.CodeAccessDenied)
	}
	wantOK(t, call1(t, s, &localPeer{sid: sidA}, proto.Request{Op: proto.OpStatus}), proto.OpStatus)
}

func TestMalformedClosesConnection(t *testing.T) {
	s, _ := newTestServer(t, Config{})
	status := encode(t, proto.Request{Op: proto.OpStatus})
	v2 := append([]byte{}, status...)
	v2[2] = 2
	for _, tt := range []struct {
		name string
		in   []byte
		code proto.Code
	}{
		{"oversize length", append(append([]byte{}, status...), 0xff, 0xff, 1, 4), proto.CodeBadRequest},
		{"length 0", append(append([]byte{}, status...), 0, 0), proto.CodeBadRequest},
		{"unknown op", append(append([]byte{}, status...), 0, 2, 1, 9), proto.CodeBadRequest},
		{"version 2", append(append([]byte{}, status...), v2...), proto.CodeUnsupportedVersion},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := &conn{in: bytes.NewReader(append(tt.in, status...))}
			if err := s.ServeConn(context.Background(), c, &localPeer{sid: sidA}); err == nil {
				t.Fatal("ServeConn returned nil")
			}
			r := decodeReplies(t, c.out.Bytes())
			if len(r) != 2 {
				t.Fatalf("%d replies, want status then error then close", len(r))
			}
			wantOK(t, r[0], proto.OpStatus)
			wantCode(t, r[1], tt.code)
		})
	}
}

func TestResizeLimits(t *testing.T) {
	s, _ := newTestServer(t, Config{MaxCPUs: 4, MinMemoryMB: 1024, MaxMemoryMB: 4096})
	p := &localPeer{sid: sidA}
	wantCode(t, call1(t, s, p, proto.Request{Op: proto.OpResize, CPUs: 5}), proto.CodeOutOfRange)
	wantCode(t, call1(t, s, p, proto.Request{Op: proto.OpResize, MemoryMB: 512}), proto.CodeOutOfRange)
	wantCode(t, call1(t, s, p, proto.Request{Op: proto.OpResize, MemoryMB: 8192}), proto.CodeOutOfRange)
	r := call1(t, s, p, proto.Request{Op: proto.OpResize, CPUs: 4, MemoryMB: 4096})
	wantOK(t, r, proto.OpResize)
	if r.CPUs != 4 || r.MemoryMB != 4096 {
		t.Fatalf("resize: %+v", r)
	}
}

func TestResizeRunningVM(t *testing.T) {
	s, _ := newTestServer(t, Config{validateDataDir: acceptAll})
	ss := openSession(t, s, &localPeer{sid: sidA})
	ss.do(proto.Request{Op: proto.OpSetDataLocation, Handle: tempDirHandle(t)})
	wantOK(t, ss.do(proto.Request{Op: proto.OpStart}), proto.OpStart)
	r := ss.do(proto.Request{Op: proto.OpResize, MemoryMB: 4096})
	if r.State != proto.StateRunning || r.MemoryMB != 4096 {
		t.Fatalf("resize running VM: %+v", r)
	}
}

func TestDataLocationBusyWhileRunning(t *testing.T) {
	s, _ := newTestServer(t, Config{validateDataDir: acceptAll})
	p := &localPeer{sid: sidA}
	ss := openSession(t, s, p)
	ss.do(proto.Request{Op: proto.OpSetDataLocation, Handle: tempDirHandle(t)})
	wantOK(t, ss.do(proto.Request{Op: proto.OpStart}), proto.OpStart)
	wantCode(t, call1(t, s, p, proto.Request{Op: proto.OpSetDataLocation, Handle: tempDirHandle(t)}), proto.CodeBusy)
}

func TestStartRevalidatesDataLocation(t *testing.T) {
	ok := true
	s, be := newTestServer(t, Config{validateDataDir: func(windows.Handle, string) error {
		if ok {
			return nil
		}
		return reject("DACL grants access to another account", nil)
	}})
	p := &localPeer{sid: sidA}
	wantOK(t, call1(t, s, p, proto.Request{Op: proto.OpSetDataLocation, Handle: tempDirHandle(t)}), proto.OpSetDataLocation)
	ok = false
	r := call1(t, s, p, proto.Request{Op: proto.OpStart})
	wantCode(t, r, proto.CodeDataLocationRejected)
	if be.Starts[sidA] != 0 {
		t.Fatal("started with a data location that no longer validates")
	}
}

func TestInvalidHandle(t *testing.T) {
	s, _ := newTestServer(t, Config{validateDataDir: acceptAll})
	wantCode(t, call1(t, s, &localPeer{sid: sidA}, proto.Request{Op: proto.OpSetDataLocation, Handle: 0xFFFFF0}), proto.CodeInvalidHandle)
}

func TestBackendErrorsAreNotEchoed(t *testing.T) {
	be := NewFakeBackend()
	be.Err = errors.New(`HCS failed on C:\Users\secret\vm.vhdx`)
	var logged []string
	s, _ := newTestServer(t, Config{Backend: be, Logf: func(f string, a ...any) { logged = append(logged, f) }})
	r := call1(t, s, &localPeer{sid: sidA}, proto.Request{Op: proto.OpStatus})
	wantCode(t, r, proto.CodeInternal)
	if strings.Contains(r.Message, "secret") || len(logged) == 0 {
		t.Fatalf("message %q, logged %d", r.Message, len(logged))
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	for _, cfg := range []Config{
		{},
		{Backend: NewFakeBackend(), Version: "bad\nversion"},
		{Backend: NewFakeBackend(), MaxCPUs: proto.MaxCPUs + 1},
		{Backend: NewFakeBackend(), MinMemoryMB: 4096, MaxMemoryMB: 1024},
		{Backend: NewFakeBackend(), DefaultCPUs: 16},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) accepted", cfg)
		}
	}
}

// recordingPeer records whether code runs inside Impersonate, and can block or fail
// impersonation.
type recordingPeer struct {
	localPeer
	impersonating bool
	impErr        error
	block         chan struct{}
}

func (p *recordingPeer) Impersonate(fn func() error) error {
	if p.block != nil {
		<-p.block
	}
	if p.impErr != nil {
		return p.impErr
	}
	p.impersonating = true
	defer func() { p.impersonating = false }()
	return fn()
}

// The reopen must run as the caller (the broker is SYSTEM in production, so this is
// what stops a caller laundering a handle it could not open itself), and validation
// must run as the broker.
func TestDataDirReopenImpersonatesValidateDoesNot(t *testing.T) {
	sid := currentSID(t)
	p := &recordingPeer{localPeer: localPeer{sid: sid}}
	var reopened, validated bool
	saved := reopenDirectoryFn
	reopenDirectoryFn = func(h windows.Handle) (windows.Handle, error) {
		reopened = true
		if !p.impersonating {
			t.Error("reopen ran without impersonating the caller")
		}
		return saved(h)
	}
	t.Cleanup(func() { reopenDirectoryFn = saved })
	validate := func(h windows.Handle, s string) error {
		validated = true
		if p.impersonating {
			t.Error("validation ran while impersonating the caller")
		}
		return ValidateDataDir(h, s)
	}
	f, err := os.Open(goodDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d, err := openDataDir(p, uint64(f.Fd()), sid, validate)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	if !reopened || !validated {
		t.Fatalf("reopened %v, validated %v", reopened, validated)
	}

	// If impersonation fails, nothing is opened or validated.
	reopened, validated = false, false
	p.impErr = errors.New("cannot impersonate")
	_, err = openDataDir(p, uint64(f.Fd()), sid, validate)
	wantReject(t, err, proto.CodeDataLocationRejected, "cannot open the handle as a directory")
	if reopened || validated {
		t.Fatalf("after failed impersonation: reopened %v, validated %v", reopened, validated)
	}
}

// A data-location check that never returns (a remote or provider-backed file system
// that stops answering) must not hold the user's lock or the connection forever.
func TestDataDirCheckTimesOut(t *testing.T) {
	s, _ := newTestServer(t, Config{RequestTimeout: 300 * time.Millisecond})
	p := &recordingPeer{localPeer: localPeer{sid: sidA}, block: make(chan struct{})}
	defer close(p.block)

	start := time.Now()
	f, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := call1(t, s, p, proto.Request{Op: proto.OpSetDataLocation, Handle: uint64(f.Fd())})
	wantCode(t, r, proto.CodeInvalidHandle)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
	// The user's lock is free again.
	wantOK(t, call1(t, s, &localPeer{sid: sidA}, proto.Request{Op: proto.OpStatus}), proto.OpStatus)
}

// blockingReader blocks reads until closed, like an idle client.
type blockingReader struct{ ch chan struct{} }

func (b blockingReader) Read([]byte) (int, error) { <-b.ch; return 0, io.EOF }

func TestMaxConnsPerUser(t *testing.T) {
	s, _ := newTestServer(t, Config{MaxConnsPerUser: 1})
	hold := make(chan struct{})
	status := encode(t, proto.Request{Op: proto.OpStatus})
	held := &conn{in: io.MultiReader(bytes.NewReader(status), blockingReader{hold})}
	done := make(chan error, 1)
	go func() { done <- s.ServeConn(context.Background(), held, &localPeer{sid: sidA}) }()
	// Wait until the first connection has been identified.
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		n := s.conns[sidA]
		s.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first connection never identified")
		}
		time.Sleep(time.Millisecond)
	}

	c := &conn{in: bytes.NewReader(status)}
	if err := s.ServeConn(context.Background(), c, &localPeer{sid: sidA}); err == nil {
		t.Fatal("second connection of the same user was served")
	}
	r := decodeReplies(t, c.out.Bytes())
	if len(r) != 1 {
		t.Fatalf("%d replies", len(r))
	}
	wantCode(t, r[0], proto.CodeBusy)
	// Another user is not affected.
	wantOK(t, call1(t, s, &localPeer{sid: sidB}, proto.Request{Op: proto.OpStatus}), proto.OpStatus)

	close(hold)
	<-done
	wantOK(t, call1(t, s, &localPeer{sid: sidA}, proto.Request{Op: proto.OpStatus}), proto.OpStatus)
}

func TestCloseReleasesDataDirs(t *testing.T) {
	s, _ := newTestServer(t, Config{validateDataDir: acceptAll})
	p := &localPeer{sid: sidA}
	call1(t, s, p, proto.Request{Op: proto.OpSetDataLocation, Handle: tempDirHandle(t)})
	s.Close()
	wantCode(t, call1(t, s, p, proto.Request{Op: proto.OpStatus}), proto.CodeBusy)
}

// session is one open connection to the server, served in the background.
type session struct {
	t    *testing.T
	c    net.Conn
	done chan error
}

func openSession(t *testing.T, s *Server, p Peer) *session {
	t.Helper()
	client, server := net.Pipe()
	ss := &session{t: t, c: client, done: make(chan error, 1)}
	go func() { ss.done <- s.ServeConn(context.Background(), server, p) }()
	t.Cleanup(func() { ss.close() })
	return ss
}

func (ss *session) do(req proto.Request) proto.Reply {
	ss.t.Helper()
	b, err := proto.EncodeRequest(req)
	if err != nil {
		ss.t.Fatal(err)
	}
	_ = ss.c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := ss.c.Write(b); err != nil {
		ss.t.Fatalf("write: %v", err)
	}
	var buf [proto.MaxFrameSize]byte
	f, err := proto.ReadFrame(ss.c, &buf)
	if err != nil {
		ss.t.Fatalf("read reply: %v", err)
	}
	r, err := proto.DecodeReply(f)
	if err != nil {
		ss.t.Fatalf("decode reply: %v", err)
	}
	return r
}

// close ends the connection and waits for the server to finish with it.
func (ss *session) close() {
	if ss.c == nil {
		return
	}
	_ = ss.c.Close()
	<-ss.done
	ss.c = nil
}

// The connection that started the VM is its lease (Implementation decision 6): when it
// ends, the VM is terminated; another connection of the same user cannot take it over.
func TestLease(t *testing.T) {
	s, be := newTestServer(t, Config{validateDataDir: acceptAll})
	p := &localPeer{sid: sidA}
	holder := openSession(t, s, p)
	wantOK(t, holder.do(proto.Request{Op: proto.OpSetDataLocation, Handle: tempDirHandle(t)}), proto.OpSetDataLocation)
	first := holder.do(proto.Request{Op: proto.OpStart})
	wantOK(t, first, proto.OpStart)
	// The same connection asking again gets the same VM.
	if again := holder.do(proto.Request{Op: proto.OpStart}); again.VMID != first.VMID || be.Starts[sidA] != 1 {
		t.Fatalf("second start on the lease: %+v, starts %d", again, be.Starts[sidA])
	}
	// Another connection of the owner sees it but cannot start it again.
	other := openSession(t, s, p)
	wantCode(t, other.do(proto.Request{Op: proto.OpStart}), proto.CodeLeaseHeld)
	if st := other.do(proto.Request{Op: proto.OpStatus}); st.State != proto.StateRunning {
		t.Fatalf("status from another connection: %+v", st)
	}
	// Closing the other connection leaves the VM; closing the lease terminates it.
	other.close()
	if be.Stops[sidA] != 0 {
		t.Fatal("a connection without the lease stopped the VM")
	}
	holder.close()
	if be.Stops[sidA] != 1 {
		t.Fatalf("lease closed: %d stops, want 1", be.Stops[sidA])
	}
	wantCode(t, call1(t, s, p, proto.Request{Op: proto.OpStop}), proto.CodeNotRunning)

	// A forced stop ends the lease, and a new start takes it on its own connection.
	next := openSession(t, s, p)
	wantOK(t, next.do(proto.Request{Op: proto.OpStart}), proto.OpStart)
	wantOK(t, next.do(proto.Request{Op: proto.OpStop, Force: true}), proto.OpStop)
	wantOK(t, next.do(proto.Request{Op: proto.OpStart}), proto.OpStart)
	stops := be.Stops[sidA]
	next.close()
	if be.Stops[sidA] != stops+1 {
		t.Fatal("the restarted VM outlived its lease")
	}
}

func TestStartFields(t *testing.T) {
	s, be := newTestServer(t, Config{validateDataDir: acceptAll, MaxCPUs: 4, MaxMemoryMB: 8192})
	ss := openSession(t, s, &localPeer{sid: sidA})
	ss.do(proto.Request{Op: proto.OpSetDataLocation, Handle: tempDirHandle(t)})
	wantCode(t, ss.do(proto.Request{Op: proto.OpStart, CPUs: 5}), proto.CodeOutOfRange)
	wantCode(t, ss.do(proto.Request{Op: proto.OpStart, MemoryMB: 8193}), proto.CodeOutOfRange)
	if be.Starts[sidA] != 0 {
		t.Fatal("started outside the limits")
	}
	r := ss.do(proto.Request{Op: proto.OpStart, CPUs: 3, MemoryMB: 6144,
		Features: proto.FeatureNet | proto.FeatureShare | proto.FeatureBlankDisk,
		NetAddr:  [4]byte{10, 20, 30, 64}, NetPrefix: 26, NetMTU: 1500, NetFlags: proto.NetOffload | proto.NetHostGateway | proto.NetCRCStrict})
	wantOK(t, r, proto.OpStart)
	if r.CPUs != 3 || r.MemoryMB != 6144 {
		t.Fatalf("start reply %+v", r)
	}
	got := be.Specs[sidA]
	want := VMSpec{Owner: sidA, CPUs: 3, MemoryMB: 6144, Share: true, BlankDisk: true,
		Net: &vmtemplate.Net{Subnet: netip.MustParsePrefix("10.20.30.64/26"), MTU: 1500, Offload: true, HostGateway: true, CRC: vmtemplate.CRCStrict}}
	if got.Net == nil || *got.Net != *want.Net {
		t.Fatalf("net %+v, want %+v", got.Net, want.Net)
	}
	got.Net, want.Net = nil, nil
	if got != want {
		t.Fatalf("spec %+v, want %+v", got, want)
	}
	// The values stay for the next start, as a resize's do.
	if st := ss.do(proto.Request{Op: proto.OpStatus}); st.CPUs != 3 || st.MemoryMB != 6144 {
		t.Fatalf("status %+v", st)
	}
}

func TestPause(t *testing.T) {
	s, _ := newTestServer(t, Config{validateDataDir: acceptAll})
	p := &localPeer{sid: sidA}
	wantCode(t, call1(t, s, p, proto.Request{Op: proto.OpPause}), proto.CodeNotRunning)
	ss := openSession(t, s, p)
	ss.do(proto.Request{Op: proto.OpSetDataLocation, Handle: tempDirHandle(t)})
	wantOK(t, ss.do(proto.Request{Op: proto.OpStart}), proto.OpStart)
	if r := ss.do(proto.Request{Op: proto.OpPause}); r.State != proto.StatePaused {
		t.Fatalf("pause: %+v", r)
	}
	if r := call1(t, s, p, proto.Request{Op: proto.OpStatus}); r.State != proto.StatePaused {
		t.Fatalf("status while paused: %+v", r)
	}
	// A paused VM is still the lease's.
	wantCode(t, call1(t, s, p, proto.Request{Op: proto.OpStart}), proto.CodeLeaseHeld)
	if r := ss.do(proto.Request{Op: proto.OpPause, Resume: true}); r.State != proto.StateRunning {
		t.Fatalf("resume: %+v", r)
	}
}

// Image and disk rejections reach the caller as their own codes, without detail.
func TestStartRejections(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code proto.Code
	}{
		{fmt.Errorf("%w: image.json at C:\\secret does not match", ErrImageRejected), proto.CodeImageRejected},
		{fmt.Errorf("%w: C:\\secret\\data.vhdx has 2 links", ErrDiskRejected), proto.CodeDiskRejected},
	} {
		be := &failingStart{FakeBackend: NewFakeBackend(), err: tc.err}
		s, _ := newTestServer(t, Config{Backend: be, validateDataDir: acceptAll})
		ss := openSession(t, s, &localPeer{sid: sidA})
		ss.do(proto.Request{Op: proto.OpSetDataLocation, Handle: tempDirHandle(t)})
		r := ss.do(proto.Request{Op: proto.OpStart})
		wantCode(t, r, tc.code)
		if strings.Contains(r.Message, "secret") {
			t.Fatalf("message %q", r.Message)
		}
	}
}

type failingStart struct {
	*FakeBackend
	err error
}

func (f *failingStart) Start(context.Context, VMSpec) (VMStatus, error) { return VMStatus{}, f.err }
