package egress

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/netstack"
)

// A HelloRetryRequest through the stack: the second hello goes out in plaintext, so it
// passes the checks the first one passed before it reaches the server.

// retryCounter counts how often the policy was shown a second hello.
type retryCounter struct {
	*Policy
	retries atomic.Int64
}

func (r *retryCounter) InspectRetry(dst netip.AddrPort, next []byte) netstack.InspectResult {
	r.retries.Add(1)
	return r.Policy.InspectRetry(dst, next)
}

// scriptServer runs handle on each connection on loopback, and keeps what handle read
// through its reader.
type scriptServer struct {
	addr netip.AddrPort
	mu   sync.Mutex
	got  bytes.Buffer
}

func (s *scriptServer) received() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.got.Bytes()...)
}

// keep reads c into the record until it ends, with a deadline so that a test never hangs.
func (s *scriptServer) keep(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 4096)
	for {
		k, err := c.Read(buf)
		s.mu.Lock()
		s.got.Write(buf[:k])
		s.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func newScriptServer(t *testing.T, handle func(*scriptServer, net.Conn)) *scriptServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &scriptServer{addr: netip.MustParseAddrPort(ln.Addr().String())}
	var wg sync.WaitGroup
	conns := make(chan net.Conn, 8)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns <- c
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				handle(s, c)
			}()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		// End the handlers still reading, so that cleanup does not wait out their deadlines.
		for {
			select {
			case c := <-conns:
				c.Close()
				continue
			default:
			}
			break
		}
		wg.Wait()
	})
	return s
}

// retryOnFirstHello reads one ClientHello, answers with a HelloRetryRequest and keeps
// everything else it gets.
func retryOnFirstHello(s *scriptServer, c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	var b []byte
	buf := make([]byte, 4096)
	for parseClientHello(b).state == parseMore {
		k, err := c.Read(buf)
		b = append(b, buf[:k]...)
		if err != nil {
			return
		}
	}
	s.mu.Lock()
	s.got.Write(b)
	s.mu.Unlock()
	if _, err := c.Write(serverHello(tlsRetryRandom, 1<<14)); err != nil {
		return
	}
	s.keep(c)
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: stackName},
		DNSNames:     []string{stackName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

// A real client and a real server that forces a HelloRetryRequest: it wants a curve the
// client lists but sent no key for.
func TestStackHelloRetryRealClientSucceeds(t *testing.T) {
	cfg := &tls.Config{
		Certificates:     []tls.Certificate{selfSigned(t)},
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: []tls.CurveID{tls.CurveP521},
	}
	srv := newScriptServer(t, func(_ *scriptServer, c net.Conn) {
		tc := tls.Server(c, cfg)
		_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
		if tc.Handshake() != nil {
			return
		}
		_, _ = io.Copy(tc, tc)
	})
	var counter retryCounter
	s := newSessionNetTo(t, Dev, sessionOpts{target: srv.addr, wrap: func(p *Policy) netstack.Egress {
		counter.Policy = p
		return &counter
	}}, stackName)
	s.pinForward(t)
	c, err := s.g.dial(t, s.tls)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	tc := tls.Client(c, &tls.Config{
		ServerName:         stackName,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
		CurvePreferences:   []tls.CurveID{tls.X25519, tls.CurveP521},
	})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("handshake through the stack: %v", err)
	}
	if got := tc.ConnectionState().CurveID; got != tls.CurveP521 {
		t.Fatalf("curve %v", got)
	}
	if counter.retries.Load() == 0 {
		t.Fatal("no HelloRetryRequest happened, so this test proved nothing")
	}
	if _, err := tc.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(tc, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("%q %v", buf, err)
	}
	if st := s.n.Stats(); st.TCPDenied != 0 {
		t.Fatalf("%+v", st)
	}
}

// helloThroughRetry connects, sends a first hello for the allowed name, and reads the
// server's HelloRetryRequest as the guest sees it.
func helloThroughRetry(t *testing.T, s *sessionNet) net.Conn {
	t.Helper()
	c, err := s.g.dial(t, s.tls)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := c.Write(hello(sniExt(stackName))); err != nil {
		t.Fatal(err)
	}
	want := serverHello(tlsRetryRandom, 1<<14)
	buf := make([]byte, len(want))
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || !bytes.Equal(buf, want) {
		t.Fatalf("the guest did not get the request: %v", err)
	}
	return c
}

func TestStackSecondHelloOtherNameRefused(t *testing.T) {
	ccs := []byte{20, 3, 3, 0, 1, 1}
	evil := hello(sniExt("evil.example"))
	for _, tc := range []struct {
		name string
		b    []byte
	}{
		{"other name", evil},
		{"other name behind a change cipher spec", append(append([]byte{}, ccs...), evil...)},
		{"another allowed name, not pinned here", hello(sniExt("other.example"))},
		{"no server name", hello(ext(0x0a0a, nil))},
		{"ECH", hello(sniExt(stackName), ext(tlsExtECH, []byte{1}))},
		{"two change cipher specs", append(append([]byte{}, ccs...), ccs...)},
		{"application data", []byte{23, 3, 3, 0, 1, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newScriptServer(t, retryOnFirstHello)
			s := newSessionNetTo(t, Dev, sessionOpts{target: srv.addr}, stackName, "other.example")
			s.pinForward(t)
			first := hello(sniExt(stackName))
			c := helloThroughRetry(t, s)
			if _, err := c.Write(tc.b); err != nil {
				t.Fatal(err)
			}
			_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
			if _, err := c.Read(make([]byte, 1)); err == nil {
				t.Fatal("the connection went on")
			} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatal("the connection stayed open")
			}
			if got := srv.received(); !bytes.Equal(got, first) {
				t.Fatalf("the server got %d bytes beyond the first hello: %q", len(got)-len(first), got[min(len(first), len(got)):])
			}
			if st := s.n.Stats(); st.TCPDenied != 1 || st.TCPActive != 0 {
				t.Fatalf("%+v", st)
			}
		})
	}
}

func TestStackSecondHelloSameNameGoesOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		ccs  []byte
	}{{"plain", nil}, {"behind a change cipher spec", []byte{20, 3, 3, 0, 1, 1}}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newScriptServer(t, retryOnFirstHello)
			s := newSessionNetTo(t, Dev, sessionOpts{target: srv.addr}, stackName)
			s.pinForward(t)
			c := helloThroughRetry(t, s)
			second := append(append([]byte{}, tc.ccs...), hello(sniExt(stackName))...)
			// Byte by byte, then the relay carries what follows unread.
			for _, b := range second {
				if _, err := c.Write([]byte{b}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := c.Write([]byte("after")); err != nil {
				t.Fatal(err)
			}
			want := append(append(hello(sniExt(stackName)), second...), "after"...)
			deadline := time.Now().Add(5 * time.Second)
			for len(srv.received()) < len(want) && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if !bytes.Equal(srv.received(), want) {
				t.Fatalf("the server got %d bytes, want %d", len(srv.received()), len(want))
			}
		})
	}
}

// A guest does not get to send the second hello right behind the first, in one write, to
// have it at the server before the request comes back.
func TestStackPipelinedSecondHelloRefused(t *testing.T) {
	srv := newScriptServer(t, retryOnFirstHello)
	s := newSessionNetTo(t, Dev, sessionOpts{target: srv.addr}, stackName)
	s.pinForward(t)
	c, err := s.g.dial(t, s.tls)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(append(hello(sniExt(stackName)), hello(sniExt("evil.example"))...)); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("the connection went on")
	}
	if len(srv.received()) != 0 {
		t.Fatalf("the server got %d bytes", len(srv.received()))
	}
	if blockedKinds(s.p)[KindBadHello] == "" {
		t.Fatalf("%+v", s.p.Blocked())
	}
}

// The same bytes in a later read wait for the server's answer, and a request sends them
// through the checks all the same.
func TestStackLaterSecondHelloWaits(t *testing.T) {
	answer := make(chan struct{})
	srv := newScriptServer(t, func(s *scriptServer, c net.Conn) {
		b := make([]byte, 4096)
		k, err := c.Read(b)
		s.mu.Lock()
		s.got.Write(b[:k])
		s.mu.Unlock()
		if err != nil {
			return
		}
		<-answer
		if _, err := c.Write(serverHello(tlsRetryRandom, 1<<14)); err != nil {
			return
		}
		s.keep(c)
	})
	s := newSessionNetTo(t, Dev, sessionOpts{target: srv.addr}, stackName)
	s.pinForward(t)
	c, err := s.g.dial(t, s.tls)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	first := hello(sniExt(stackName))
	if _, err := c.Write(first); err != nil {
		close(answer)
		t.Fatal(err)
	}
	waitFor(t, srv, first)
	if _, err := c.Write(hello(sniExt("evil.example"))); err != nil {
		close(answer)
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	got := srv.received()
	close(answer)
	if !bytes.Equal(got, first) {
		t.Fatalf("the second hello went out before the server answered: %d bytes", len(got))
	}
	// The guest gets the request, then the stack refuses what was waiting.
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.ReadAll(c)
	if got := srv.received(); !bytes.Equal(got, first) {
		t.Fatalf("the server got %d bytes, not just the first hello's %d", len(got), len(first))
	}
	if blockedKinds(s.p)[KindServerName] != "evil.example" {
		t.Fatalf("%+v", s.p.Blocked())
	}
}

// waitFor waits until the server has received want, so that what the test sends next
// arrives in a later read.
func waitFor(t *testing.T, srv *scriptServer, want []byte) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !bytes.Equal(srv.received(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("the server got %q, want %q", srv.received(), want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// A server that says nothing ends the connection at the deadline; what the guest sends in
// the meantime does not reach it.
func TestStackSlowServerClosed(t *testing.T) {
	srv := newScriptServer(t, func(s *scriptServer, c net.Conn) { s.keep(c) })
	s := newSessionNetTo(t, Dev, sessionOpts{target: srv.addr, dial: 500 * time.Millisecond}, stackName)
	s.pinForward(t)
	c, err := s.g.dial(t, s.tls)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	first := hello(sniExt(stackName))
	start := time.Now()
	if _, err := c.Write(first); err != nil {
		t.Fatal(err)
	}
	waitFor(t, srv, first)
	if _, err := c.Write([]byte("later")); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, err = c.Read(make([]byte, 1))
	if ne, ok := err.(net.Error); err == nil || (ok && ne.Timeout()) {
		t.Fatalf("the connection stayed open: %v", err)
	}
	if e := time.Since(start); e < 400*time.Millisecond || e > 5*time.Second {
		t.Fatalf("closed after %v", e)
	}
	if got := srv.received(); !bytes.Equal(got, first) {
		t.Fatalf("the server got %q", got)
	}
}

// What is not a hello on its way passes at once: nothing waits for the deadline.
func TestStackOtherRepliesDoNotHang(t *testing.T) {
	var random [32]byte
	random[3] = 9
	for _, tc := range []struct {
		name  string
		reply []byte
	}{
		{"server speaks first", []byte("220 ready\r\n")},
		{"alert", []byte{21, 3, 3, 0, 2, 2, 40}},
		{"ordinary server hello", serverHello(random, 1<<14)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newScriptServer(t, func(s *scriptServer, c net.Conn) {
				if _, err := c.Write(tc.reply); err != nil {
					return
				}
				s.keep(c)
			})
			s := newSessionNetTo(t, Dev, sessionOpts{target: srv.addr}, stackName)
			s.pinForward(t)
			c, err := s.g.dial(t, s.tls)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			first := hello(sniExt(stackName))
			start := time.Now()
			if _, err := c.Write(first); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, len(tc.reply))
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.ReadFull(c, buf); err != nil || !bytes.Equal(buf, tc.reply) {
				t.Fatalf("%q %v", buf, err)
			}
			if e := time.Since(start); e > 3*time.Second {
				t.Fatalf("took %v", e)
			}
			// The relay runs: later bytes reach the server uninspected, as they should.
			if _, err := c.Write([]byte("after")); err != nil {
				t.Fatal(err)
			}
			want := string(first) + "after"
			deadline := time.Now().Add(5 * time.Second)
			for len(srv.received()) < len(want) && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if string(srv.received()) != want {
				t.Fatalf("the server got %q", srv.received())
			}
		})
	}
}

// A server that closes without a word is the end of the connection, not a hang.
func TestStackServerClosesWithoutReply(t *testing.T) {
	srv := newScriptServer(t, func(_ *scriptServer, c net.Conn) {
		_, _ = c.Read(make([]byte, 16))
	})
	s := newSessionNetTo(t, Dev, sessionOpts{target: srv.addr}, stackName)
	s.pinForward(t)
	c, err := s.g.dial(t, s.tls)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(hello(sniExt(stackName))); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = c.Read(make([]byte, 1))
	if ne, ok := err.(net.Error); err == nil || (ok && ne.Timeout()) {
		t.Fatalf("not closed: %v", err)
	}
}

// Port 80 is not watched: a server that stays silent still gets the rest of the request.
func TestStackPort80NotWatched(t *testing.T) {
	s := newSessionNet(t, Dev, stackName)
	s.pinForward(t)
	c, err := s.g.dial(t, s.web)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	head := "POST / HTTP/1.1\r\nHost: " + stackName + "\r\nContent-Length: 4\r\n\r\n"
	if _, err := c.Write([]byte(head)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("body")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(s.sink.received()) < len(head)+4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := string(s.sink.received()); !strings.HasSuffix(got, "body") {
		t.Fatalf("%q", got)
	}
}
