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

// flakyListener fails its first Accept with a transient error.
type flakyListener struct {
	net.Listener
	failed bool
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if !l.failed {
		l.failed = true
		return nil, errors.New("an existing connection was forcibly closed")
	}
	return l.Listener.Accept()
}

// Review finding 4: one failed Accept must not end the share listener.
func TestShareServiceSurvivesAcceptError(t *testing.T) {
	s := setupShares(ShareConfig{}, slog.New(slog.DiscardHandler))
	defer s.close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.serve(&flakyListener{Listener: ln}, testVMSecret)
	c := dialShare(t, ln.Addr().String())
	defer c.Close()
	if _, err := c.Write(helloFrame(0)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !s.srv.Mounted() {
		if time.Now().After(deadline) {
			t.Fatal("the connection after a failed accept was never served")
		}
		time.Sleep(5 * time.Millisecond)
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
	svcs := c.Services[2:]
	if len(svcs) != 1 || svcs[0].ID != s.svc || svcs[0].BindSDDL != "D:P(A;;FA;;;S-1-5-21-1)" || svcs[0].ConnectSDDL != "" || !strings.HasSuffix(c.KernelCmdLine, " hyperbutler.share=2385") {
		t.Fatalf("got %+v %q", svcs, c.KernelCmdLine)
	}
}

// dialShare connects to the share server at addr as the guest relay does: it checks
// that the server sends the VM secret first (vmsecret.Verify) before it writes.
func dialShare(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := vmsecret.Verify(c, testVMSecret); err != nil {
		_ = c.Close()
		t.Fatalf("the share server did not send the VM secret first: %v", err)
	}
	return c
}

// ADR 0004 (Two-account run): every connection the share server accepts begins with
// the VM secret, the guest's proof that the listener is its own host's; without a
// secret the server closes the connection having written nothing, and nothing mounts.
func TestShareServiceSendsSecretFirst(t *testing.T) {
	s := setupShares(ShareConfig{}, slog.New(slog.DiscardHandler))
	defer s.close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.serve(ln, vmsecret.Secret{})
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if b, err := io.ReadAll(c); len(b) != 0 {
		t.Fatalf("a server without a secret wrote %d byte(s) (%v)", len(b), err)
	}
	if s.srv.Mounted() {
		t.Fatal("mounted without a secret")
	}

	s2 := setupShares(ShareConfig{}, slog.New(slog.DiscardHandler))
	defer s2.close()
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s2.serve(ln2, testVMSecret)
	c2 := dialShare(t, ln2.Addr().String())
	defer c2.Close()
	if _, err := c2.Write(helloFrame(0)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !s2.srv.Mounted() {
		if time.Now().After(deadline) {
			t.Fatal("the session after the secret never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A guest that refuses the server's secret closes the connection without a HELLO and
// redials every few seconds: the first such connection is a warning with a hint at the
// cause, the later ones are debug.
func TestShareServiceHintsAtRefusedSecret(t *testing.T) {
	h := &recordHandler{}
	s := setupShares(ShareConfig{}, slog.New(h))
	defer s.close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.serve(ln, testVMSecret)
	for range 3 {
		c := dialShare(t, ln.Addr().String())
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

// TestShareServiceCloseEndsConnections: close stops accepting, closes live guest
// connections (an idle session and a half-sent frame) and leaves no goroutine behind
// (checked by TestMain).
func TestShareServiceCloseEndsConnections(t *testing.T) {
	s := setupShares(ShareConfig{}, slog.Default())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.serve(ln, testVMSecret)
	if _, err := s.rewriteCreate([]byte(`{"HostConfig":{"Binds":["C:\\x:/x"]}}`)); err == nil || !strings.Contains(err.Error(), "not mounted") {
		t.Fatalf("rewrite before the guest mounted: %v", err)
	}
	var clients []net.Conn
	for i, first := range [][]byte{helloFrame(0), helloFrame(0)[:20]} {
		c := dialShare(t, ln.Addr().String())
		defer c.Close()
		if _, err := c.Write(first); err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
		if i == 0 {
			deadline := time.Now().Add(5 * time.Second)
			for !s.srv.Mounted() {
				if time.Now().After(deadline) {
					t.Fatal("the session never started")
				}
				time.Sleep(5 * time.Millisecond)
			}
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
	// A late serve after close is refused at once.
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.serve(ln2, testVMSecret)
	if _, err := ln2.Accept(); err == nil {
		t.Fatal("listener still open")
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
