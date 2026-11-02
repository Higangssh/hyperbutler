//go:build windows

package broker

import (
	"bytes"
	"context"
	"strings"
	"testing"
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

// A per-user name is the SID's hash and a fresh random suffix: another user can compute
// the prefix, not the name.
func TestUserPipeName(t *testing.T) {
	a, err := newUserPipeName(DefaultSharedPipe, sidA)
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := newUserPipeName(DefaultSharedPipe, sidA)
	b, _ := newUserPipeName(DefaultSharedPipe, sidB)
	if a == a2 || !strings.HasPrefix(a, DefaultSharedPipe+"-") || len(a) != len(DefaultSharedPipe)+1+32+1+16 || strings.ToLower(a) != a {
		t.Fatalf("names %q, %q", a, a2)
	}
	if !strings.HasPrefix(a2, a[:len(a)-16]) {
		t.Fatal("the SID part is not stable")
	}
	if !ValidUserPipeName(DefaultSharedPipe, sidA, a) || ValidUserPipeName(DefaultSharedPipe, sidA, b) || ValidUserPipeName(DefaultSharedPipe, sidB, a) {
		t.Fatal("ValidUserPipeName does not tell users apart")
	}
	for _, bad := range []string{
		a[:len(a)-1], a + "0", a[:len(a)-1] + "G", a[:len(a)-16], strings.ToUpper(a),
		DefaultSharedPipe, `\\.\pipe\elsewhere` + a[len(DefaultSharedPipe):],
	} {
		if ValidUserPipeName(DefaultSharedPipe, sidA, bad) {
			t.Errorf("accepted %q", bad)
		}
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
	if !ValidUserPipeName(e.Name(), me, r.PipeName) {
		t.Fatalf("endpoint %q is not of this user's form", r.PipeName)
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
	// Even a name an attacker guessed would be refused by the DACL; with the random part
	// it cannot be guessed at all (TestUserPipeName).
	// The SDDL names exactly the two principals, for distinct SIDs too.
	if got := PipeSDDL("S-1-5-18", sidA); strings.Count(got, "(") != 2 || !strings.Contains(got, ";;;"+sidA+")") ||
		strings.Contains(got, "IU") || !strings.Contains(got, "D:P") {
		t.Fatalf("per-user SDDL %s", got)
	}
}
