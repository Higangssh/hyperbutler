//go:build windows

package broker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/broker/proto"
)

// peerSID is the SID a test peer reports, without calling Identify (some tests count
// those calls).
func peerSID(p Peer) string {
	switch p := p.(type) {
	case *localPeer:
		return p.sid
	case *recordingPeer:
		return p.sid
	}
	return ""
}

// callShared sends reqs on one connection to the shared endpoint.
func callShared(t *testing.T, s *Server, p Peer, reqs ...proto.Request) []proto.Reply {
	t.Helper()
	c := &conn{in: bytes.NewReader(encode(t, reqs...))}
	if err := s.ServeConn(context.Background(), c, p, ""); err != nil {
		t.Fatalf("ServeConn: %v", err)
	}
	return decodeReplies(t, c.out.Bytes())
}

// serveEndpoints serves a shared endpoint for the current user, with per-user
// endpoints, on fresh pipe names.
func serveEndpoints(t *testing.T) (*Endpoints, *Server, *FakeBackend) {
	t.Helper()
	me := currentSID(t)
	s, be := newTestServer(t, Config{AllowedSIDs: []string{me}, Logf: t.Logf})
	e, err := ListenEndpoints(s, EndpointsConfig{Name: testPipeName(t), Instances: 2, UserInstances: 2})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.Serve() }()
	t.Cleanup(func() {
		e.Close()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return e, s, be
}

func TestUserPipeName(t *testing.T) {
	a, b := UserPipeName(DefaultSharedPipe, sidA), UserPipeName(DefaultSharedPipe, sidB)
	if a == b || !strings.HasPrefix(a, DefaultSharedPipe+"-") || len(a) != len(DefaultSharedPipe)+1+32 {
		t.Fatalf("names %q, %q", a, b)
	}
	if a != UserPipeName(DefaultSharedPipe, sidA) {
		t.Fatal("not deterministic")
	}
	if strings.ToLower(a) != a {
		t.Fatal("not lowercase hex")
	}
}

// The shared endpoint answers version and open-endpoint and refuses every VM verb, so
// no VM state and no secret ever travels on it; a per-user endpoint refuses
// open-endpoint.
func TestSharedEndpointServesOnlyOpen(t *testing.T) {
	s, be := newTestServer(t, Config{validateDataDir: acceptAll})
	var opened []string
	s.opener = func(sid string) (string, error) {
		opened = append(opened, sid)
		return `\\.\pipe\own-` + sid, nil
	}
	// A's VM runs (started on A's own endpoint), so status would have a secret to give.
	sa := openSession(t, s, &localPeer{sid: sidA})
	wantOK(t, sa.do(proto.Request{Op: proto.OpSetDataLocation, Handle: tempDirHandle(t)}), proto.OpSetDataLocation)
	started := sa.do(proto.Request{Op: proto.OpStart})
	if started.Secret.IsZero() {
		t.Fatal("the owner's endpoint gave no secret")
	}

	vmVerbs := []proto.Request{
		{Op: proto.OpStatus}, {Op: proto.OpStart}, {Op: proto.OpStop, Force: true}, {Op: proto.OpResize, CPUs: 1},
		{Op: proto.OpSetDataLocation, Handle: tempDirHandle(t)}, {Op: proto.OpPause}, {Op: proto.OpPause, Resume: true},
	}
	reqs := append([]proto.Request{{Op: proto.OpVersion}, {Op: proto.OpOpenEndpoint}}, vmVerbs...)
	r := callShared(t, s, &localPeer{sid: sidA}, reqs...)
	if len(r) != len(reqs) {
		t.Fatalf("%d replies", len(r))
	}
	wantOK(t, r[0], proto.OpVersion)
	wantOK(t, r[1], proto.OpOpenEndpoint)
	if r[1].PipeName != `\\.\pipe\own-`+sidA || len(opened) != 1 || opened[0] != sidA {
		t.Fatalf("open: %+v, opened %v", r[1], opened)
	}
	for i, rep := range r {
		if !rep.Secret.IsZero() || rep.VMID != ([16]byte{}) {
			t.Fatalf("reply %d on the shared endpoint carries VM state: %+v", i, rep)
		}
		if i >= 2 {
			wantCode(t, rep, proto.CodeWrongEndpoint)
		}
	}
	if be.StopCount(sidA) != 0 || be.StartCount(sidA) != 1 {
		t.Fatal("a VM verb on the shared endpoint reached the backend")
	}
	// The owner's own endpoint refuses open-endpoint, and still reports the secret.
	wantCode(t, sa.do(proto.Request{Op: proto.OpOpenEndpoint}), proto.CodeWrongEndpoint)
	if st := sa.do(proto.Request{Op: proto.OpStatus}); st.Secret != started.Secret {
		t.Fatal("status on the owner's endpoint lost the secret")
	}
}

// A second user's (simulated) peer on A's endpoint is refused before any request runs,
// even for the version verb, and cannot see or touch A's VM; the SID check holds even
// with no AllowedSIDs.
func TestOwnEndpointRefusesOtherUser(t *testing.T) {
	s, be := newTestServer(t, Config{validateDataDir: acceptAll})
	sa := openSession(t, s, &localPeer{sid: sidA})
	wantOK(t, sa.do(proto.Request{Op: proto.OpSetDataLocation, Handle: tempDirHandle(t)}), proto.OpSetDataLocation)
	wantOK(t, sa.do(proto.Request{Op: proto.OpStart}), proto.OpStart)
	for _, req := range []proto.Request{{Op: proto.OpVersion}, {Op: proto.OpStatus}, {Op: proto.OpStop, Force: true}} {
		c := &conn{in: bytes.NewReader(encode(t, req, proto.Request{Op: proto.OpStatus}))}
		if err := s.ServeConn(context.Background(), c, &localPeer{sid: sidB}, sidA); err == nil {
			t.Fatal("B was served on A's endpoint")
		}
		r := decodeReplies(t, c.out.Bytes())
		if len(r) != 1 {
			t.Fatalf("%d replies, want one refusal", len(r))
		}
		wantCode(t, r[0], proto.CodeAccessDenied)
		if !r[0].Secret.IsZero() {
			t.Fatal("a refusal carries the secret")
		}
	}
	if be.StopCount(sidA) != 0 || be.StartCount(sidB) != 0 {
		t.Fatal("B reached the backend through A's endpoint")
	}
}

// Every VM verb that reports a running VM carries its secret on the owner's endpoint;
// a new start has a new secret.
func TestSecretOnOwnEndpoint(t *testing.T) {
	s, _ := newTestServer(t, Config{validateDataDir: acceptAll})
	ss := openSession(t, s, &localPeer{sid: sidA})
	if st := ss.do(proto.Request{Op: proto.OpStatus}); !st.Secret.IsZero() {
		t.Fatal("a secret without a VM")
	}
	ss.do(proto.Request{Op: proto.OpSetDataLocation, Handle: tempDirHandle(t)})
	first := ss.do(proto.Request{Op: proto.OpStart})
	for _, r := range []proto.Reply{
		ss.do(proto.Request{Op: proto.OpStatus}),
		ss.do(proto.Request{Op: proto.OpPause}),
		ss.do(proto.Request{Op: proto.OpPause, Resume: true}),
		ss.do(proto.Request{Op: proto.OpResize, CPUs: 1}),
	} {
		if r.Secret.IsZero() || r.Secret != first.Secret {
			t.Fatalf("reply %+v without the VM's secret", r)
		}
	}
	wantOK(t, ss.do(proto.Request{Op: proto.OpStop, Force: true}), proto.OpStop)
	if st := ss.do(proto.Request{Op: proto.OpStatus}); !st.Secret.IsZero() {
		t.Fatal("a stopped VM's secret was sent")
	}
	if again := ss.do(proto.Request{Op: proto.OpStart}); again.Secret.IsZero() || again.Secret == first.Secret {
		t.Fatal("a restart reused the secret")
	}
}

// The per-user endpoint is created on request with its own protected DACL: the broker's
// account full access, the user read and write without FILE_CREATE_PIPE_INSTANCE, and
// nobody else. Asking again returns the same endpoint.
func TestUserEndpointCreatedWithDACL(t *testing.T) {
	e, _, _ := serveEndpoints(t)
	me := currentSID(t)
	c := dial(t, e.Name())
	r := roundTrip(t, c, proto.Request{Op: proto.OpOpenEndpoint})
	wantOK(t, r, proto.OpOpenEndpoint)
	if r.PipeName != UserPipeName(e.Name(), me) {
		t.Fatalf("endpoint %q, want %q", r.PipeName, UserPipeName(e.Name(), me))
	}
	if again := roundTrip(t, c, proto.Request{Op: proto.OpOpenEndpoint}); again.PipeName != r.PipeName {
		t.Fatalf("second open: %+v", again)
	}
	// VM verbs are refused on the shared endpoint.
	wantCode(t, roundTrip(t, c, proto.Request{Op: proto.OpStatus}), proto.CodeWrongEndpoint)

	e.mu.Lock()
	l := e.users[me]
	e.mu.Unlock()
	if l == nil || l.Name() != r.PipeName {
		t.Fatal("no listener for the endpoint")
	}
	sd, err := windows.GetSecurityInfo(l.insts[0].h, windows.SE_KERNEL_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	ctl, _, err := sd.Control()
	if err != nil || ctl&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("DACL of %s is not protected (%v)", sd, err)
	}
	meSID, err := windows.StringToSid(me)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(meSID) {
		t.Fatalf("owner of %s is not the broker's account", sd)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 2 {
		t.Fatalf("pipe SD %s: want 2 ACEs", sd)
	}
	var clientACE, serverACE bool
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		// The broker and the client are the same user here, so compare masks: one ACE is
		// the client's (no FILE_CREATE_PIPE_INSTANCE), the other the broker's.
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !sid.Equals(meSID) {
			t.Fatalf("pipe SD %s: ACE %d is for someone else", sd, i)
		}
		switch {
		case uint32(ace.Mask) == clientPipeAccess:
			clientACE = true
		case ace.Mask&0x4 != 0:
			serverACE = true
		}
	}
	if !clientACE || !serverACE {
		t.Fatalf("pipe SD %s: client ACE %v, server ACE %v", sd, clientACE, serverACE)
	}
	// The SDDL names exactly the two principals, for distinct SIDs too.
	if got := PipeSDDL("S-1-5-18", sidA); strings.Count(got, "(") != 2 || !strings.Contains(got, ";;;"+sidA+")") ||
		strings.Contains(got, "IU") || !strings.Contains(got, "D:P") {
		t.Fatalf("per-user SDDL %s", got)
	}
}

// A per-user name that is already taken is reported with its own code, nothing is
// served under another name, and the next request tries again.
func TestUserEndpointSquatted(t *testing.T) {
	me := currentSID(t)
	s, _ := newTestServer(t, Config{AllowedSIDs: []string{me}, Logf: t.Logf})
	name := testPipeName(t)
	n16, _ := windows.UTF16PtrFromString(UserPipeName(name, me))
	sd, err := windows.SecurityDescriptorFromString("D:(A;;GA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateNamedPipe(n16, windows.PIPE_ACCESS_DUPLEX, windows.PIPE_TYPE_BYTE,
		windows.PIPE_UNLIMITED_INSTANCES, 512, 512, 0, &windows.SecurityAttributes{SecurityDescriptor: sd})
	if err != nil {
		t.Fatal(err)
	}
	e, err := ListenEndpoints(s, EndpointsConfig{Name: name, Instances: 2})
	if err != nil {
		windows.CloseHandle(h)
		t.Fatal(err)
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
	// Once the squatter is gone, the endpoint is created.
	windows.CloseHandle(h)
	if r := roundTrip(t, c, proto.Request{Op: proto.OpOpenEndpoint}); r.PipeName != UserPipeName(name, me) {
		t.Fatalf("after the squatter left: %+v", r)
	}
}

func TestEndpointsLimits(t *testing.T) {
	me := currentSID(t)
	s, _ := newTestServer(t, Config{})
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
	if _, err := e.open(me); err != nil {
		t.Fatal(err)
	}
	if _, err := e.open(sidB); !errors.Is(err, errTooManyEndpoints) {
		t.Fatalf("over the limit: %v", err)
	}
	e.Close()
	if _, err := e.open(me); err == nil {
		t.Fatal("opened after Close")
	}
	s2, _ := newTestServer(t, Config{})
	if _, err := ListenEndpoints(s2, EndpointsConfig{Name: `\\.\pipe\` + strings.Repeat("x", proto.MaxPipeNameLen)}); err == nil {
		t.Fatal("accepted a shared name too long for per-user names")
	}
}

// The client goes through the shared endpoint to its own, and refuses an endpoint name
// it did not derive itself.
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

	// A shared endpoint that names another pipe is not followed.
	s.mu.Lock()
	s.opener = func(string) (string, error) { return e.Name(), nil }
	s.mu.Unlock()
	if _, err := Dial(ctx, e.Name()); err == nil || !strings.Contains(err.Error(), "want") {
		t.Fatalf("followed a foreign endpoint name: %v", err)
	}
	// A VM verb sent to the shared endpoint itself is refused.
	sc, err := dialPipe(ctx, e.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	if _, err := sc.Status(); !IsCode(err, proto.CodeWrongEndpoint) {
		t.Fatalf("status on the shared endpoint: %v", err)
	}
}
