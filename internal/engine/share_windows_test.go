//go:build windows

package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/share"
	"github.com/Higangssh/hyperbutler/internal/vmsecret"
	"github.com/Higangssh/hyperbutler/internal/vmtemplate"
)

// fakeRelay plays the guest's share relay: a listener the share service dials, as it
// dials the relay's vsock port.
type fakeRelay struct {
	ln net.Listener
	// fail makes that many dials fail first.
	mu    sync.Mutex
	fail  int
	dials int
}

func newFakeRelay(t *testing.T) *fakeRelay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return &fakeRelay{ln: ln}
}

func (r *fakeRelay) dial(ctx context.Context) (net.Conn, error) {
	r.mu.Lock()
	r.dials++
	fail := r.fail > 0
	if fail {
		r.fail--
	}
	r.mu.Unlock()
	if fail {
		return nil, errors.New("connect: the guest is not listening yet")
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", r.ln.Addr().String())
}

func (r *fakeRelay) dialCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dials
}

// accept takes the next connection from the share service and checks, as the guest
// relay does, that it begins with the VM secret (vmsecret.Verify).
func (r *fakeRelay) accept(t *testing.T) net.Conn {
	t.Helper()
	c, err := r.ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if err := vmsecret.Verify(c, testVMSecret); err != nil {
		_ = c.Close()
		t.Fatalf("the share service did not send the VM secret first: %v", err)
	}
	return c
}

func waitMounted(t *testing.T, s *shareService, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !s.srv.Mounted() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A dial that fails (the guest still booting) is retried, and the relay is served once
// it listens.
func TestShareServiceRetriesDial(t *testing.T) {
	s := setupShares(ShareConfig{}, slog.New(slog.DiscardHandler))
	defer s.close()
	r := newFakeRelay(t)
	r.fail = 3
	s.run(r.dial, testVMSecret)
	c := r.accept(t)
	defer c.Close()
	if _, err := c.Write(helloFrame(0)); err != nil {
		t.Fatal(err)
	}
	waitMounted(t, s, "the relay was never served after failed dials")
}

// The share service opens shareQueues connections, and opens them again once the
// guest's session has ended (all of them closed).
func TestShareServiceRedialsAfterSession(t *testing.T) {
	s := setupShares(ShareConfig{}, slog.New(slog.DiscardHandler))
	defer s.close()
	r := newFakeRelay(t)
	s.run(r.dial, testVMSecret)
	for round := range 2 {
		var conns []net.Conn
		for range shareQueues {
			conns = append(conns, r.accept(t))
		}
		if round == 0 {
			if _, err := conns[0].Write(helloFrame(0)); err != nil {
				t.Fatal(err)
			}
			waitMounted(t, s, "the first session never started")
		}
		for _, c := range conns {
			_ = c.Close()
		}
	}
	if got := r.dialCount(); got < 2*shareQueues {
		t.Fatalf("%d dials, want at least %d", got, 2*shareQueues)
	}
}

func TestSetupSharesOff(t *testing.T) {
	if s := setupShares(ShareConfig{Off: true}, slog.Default()); s != nil {
		t.Fatalf("off: got %v", s)
	}
	s := setupShares(ShareConfig{}, slog.Default())
	defer s.close()
	c, err := vmtemplate.Build(&vmtemplate.Spec{UserSID: "S-1-5-21-1", Secret: testVMSecret, MemoryMB: 1024, CPUs: 1, Kernel: `C:\k`, Initrd: `C:\i`, Share: true})
	if err != nil {
		t.Fatal(err)
	}
	// The host dials the guest's relay, so the owner may connect to the share's
	// service, and nobody may listen on it.
	svcs := c.Services[2:]
	if len(svcs) != 1 || svcs[0].ID != s.svc || svcs[0].ConnectSDDL != "D:P(A;;FA;;;S-1-5-21-1)" || svcs[0].BindSDDL != "" || !strings.HasSuffix(c.KernelCmdLine, " hyperbutler.share=2385") {
		t.Fatalf("got %+v %q", svcs, c.KernelCmdLine)
	}
}

// ADR 0004 (Two-account run): every connection the share service opens begins with
// the VM secret, the guest's proof that the peer is its own host's owner; without a
// secret the service closes the connection having written nothing, and nothing mounts.
func TestShareServiceSendsSecretFirst(t *testing.T) {
	s := setupShares(ShareConfig{}, slog.New(slog.DiscardHandler))
	defer s.close()
	r := newFakeRelay(t)
	s.run(r.dial, vmsecret.Secret{})
	c, err := r.ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if b, err := io.ReadAll(c); len(b) != 0 {
		t.Fatalf("a service without a secret wrote %d byte(s) (%v)", len(b), err)
	}
	if s.srv.Mounted() {
		t.Fatal("mounted without a secret")
	}

	s2 := setupShares(ShareConfig{}, slog.New(slog.DiscardHandler))
	defer s2.close()
	r2 := newFakeRelay(t)
	s2.run(r2.dial, testVMSecret)
	c2 := r2.accept(t)
	defer c2.Close()
	if _, err := c2.Write(helloFrame(0)); err != nil {
		t.Fatal(err)
	}
	waitMounted(t, s2, "the session after the secret never started")
}

// A guest that refuses the service's secret closes the connection without a HELLO,
// and the service dials again: the first such connection is a warning with a hint at
// the cause, the later ones are debug.
func TestShareServiceHintsAtRefusedSecret(t *testing.T) {
	h := &recordHandler{}
	s := setupShares(ShareConfig{}, slog.New(h))
	defer s.close()
	r := newFakeRelay(t)
	s.run(r.dial, testVMSecret)
	for range 3 {
		c := r.accept(t)
		_ = c.Close()
	}
	h.waitSilent(t, "before the guest's HELLO", 3)
}

// helloFrame is the relay's first frame on a connection (relay.helloFrame).
func helloFrame(queue uint32) []byte {
	b := make([]byte, 48)
	binary.LittleEndian.PutUint32(b[0:], 48)
	binary.LittleEndian.PutUint32(b[4:], 0x77620001)
	binary.LittleEndian.PutUint32(b[44:], queue)
	return b
}

// TestShareServiceCloseEndsConnections: close stops dialing, closes live guest
// connections (an idle session and a half-sent frame) and leaves no goroutine behind
// (checked by TestMain).
func TestShareServiceCloseEndsConnections(t *testing.T) {
	s := setupShares(ShareConfig{}, slog.Default())
	r := newFakeRelay(t)
	s.run(r.dial, testVMSecret)
	if _, err := s.rewriteCreate([]byte(`{"HostConfig":{"Binds":["C:\\x:/x"]}}`)); err == nil || !strings.Contains(err.Error(), "not mounted") {
		t.Fatalf("rewrite before the guest mounted: %v", err)
	}
	var clients []net.Conn
	for i, first := range [][]byte{helloFrame(0), helloFrame(0)[:20]} {
		c := r.accept(t)
		defer c.Close()
		if _, err := c.Write(first); err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
		if i == 0 {
			waitMounted(t, s, "the session never started")
		}
	}
	// Names are passed through untouched without Windows sources.
	body := []byte(`{"HostConfig":{"Binds":["vol:/x"]}}`)
	if out, err := s.rewriteCreate(body); err != nil || string(out) != string(body) {
		t.Fatalf("named volume: %s %v", out, err)
	}
	done := make(chan struct{})
	go func() { s.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("close did not return")
	}
	for _, c := range clients {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		// EOF or a reset (Windows resets a socket closed with unread data); a timeout
		// means the host never closed it.
		var ne net.Error
		if _, err := io.ReadAll(c); errors.As(err, &ne) && ne.Timeout() {
			t.Fatalf("connection not closed by the host: %v", err)
		}
	}
	if s.srv.Mounted() {
		t.Fatal("still mounted after close")
	}
	// A late run after close dials nothing.
	r2 := newFakeRelay(t)
	s.run(r2.dial, testVMSecret)
	time.Sleep(50 * time.Millisecond)
	if n := r2.dialCount(); n != 0 {
		t.Fatalf("%d dials after close", n)
	}
}

// Integration step 5 (ADR 0007): after a container is destroyed, the shares no
// container lists any more are removed; those still listed stay.
func TestShareLifetimePrunesAfterDestroy(t *testing.T) {
	s := setupShares(ShareConfig{Options: share.Options{NoWatch: true}}, slog.New(slog.DiscardHandler))
	defer s.close()
	s.grace, s.debounce = 0, 10*time.Millisecond
	base := t.TempDir()
	keep, err := s.reg.Resolve(filepath.Join(base, "keep"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.reg.Resolve(filepath.Join(base, "drop")); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	listed := `[{"Id":"x","Mounts":[{"Type":"bind","Source":"` + keep + `"}]}]`
	destroy := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-destroy:
			fmt.Fprintln(w, `{"Type":"container","Action":"destroy","Actor":{"ID":"x"}}`)
			w.(http.Flusher).Flush()
		case <-r.Context().Done():
			return
		}
		<-r.Context().Done()
	})
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		io.WriteString(w, listed)
	})
	hs := httptest.NewServer(mux)
	defer hs.Close()
	u, _ := url.Parse(hs.URL)
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", u.Host)
		},
		DisableKeepAlives: true,
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.lifetime(ctx, client); close(done) }()
	defer func() { cancel(); <-done }()
	wait := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for s.reg.Len() != n {
			if time.Now().After(deadline) {
				t.Fatalf("%d shares, want %d", s.reg.Len(), n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	// The prune when the stream opens removes the share no container lists.
	wait(1)
	mu.Lock()
	listed = `[]`
	mu.Unlock()
	close(destroy)
	wait(0)
	if got := s.srv.Shares(); len(got) != 0 {
		t.Fatalf("server still serves %v", got)
	}
}

// A guest that closes every connection at once (it refuses the secret, or its image
// wants something else) is not redialed in a tight loop: sessions back off.
func TestShareServiceBacksOffRefusedSessions(t *testing.T) {
	s := setupShares(ShareConfig{}, slog.New(slog.DiscardHandler))
	defer s.close()
	r := newFakeRelay(t)
	s.run(r.dial, testVMSecret)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := r.ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	time.Sleep(2 * time.Second)
	close(stop)
	_ = r.ln.Close()
	<-done
	// 250 ms, 500 ms, 1 s between sessions of 4: about 16 dials in 2 s, against
	// hundreds without the backoff.
	if n := r.dialCount(); n > 24 {
		t.Fatalf("%d dials in 2 s to a guest that refuses every session", n)
	}
}

// A guest relay that never connects is a warning, once, after neverWarn; before that
// the failed dials are debug only.
func TestShareServiceWarnsWhenNeverConnected(t *testing.T) {
	h := &recordHandler{}
	s := setupShares(ShareConfig{}, slog.New(h))
	defer s.close()
	s.neverWarn = 200 * time.Millisecond
	r := newFakeRelay(t)
	r.fail = 1 << 30
	s.run(r.dial, testVMSecret)
	time.Sleep(1 * time.Second)
	h.mu.Lock()
	defer h.mu.Unlock()
	warns := 0
	for _, rec := range h.recs {
		if strings.Contains(rec.msg, "has not connected") {
			if rec.level != slog.LevelWarn {
				t.Fatalf("record %+v", rec)
			}
			warns++
		}
	}
	if warns != 1 {
		t.Fatalf("%d warnings, want 1: %+v", warns, h.recs)
	}
}
