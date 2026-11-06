package egress

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// A hand-built ClientHello, so that tests can get every field wrong.

type helloSpec struct {
	version  uint16
	session  []byte
	suites   []byte
	methods  []byte
	exts     []byte
	noExts   bool
	trailing []byte // inside the handshake message, after the extensions
}

func u16(v int) []byte { return []byte{byte(v >> 8), byte(v)} }

func ext(typ int, data []byte) []byte {
	return append(append(u16(typ), u16(len(data))...), data...)
}

// sniExt is a server_name extension with one host_name per name.
func sniExt(names ...string) []byte {
	var list []byte
	for _, n := range names {
		list = append(append(append(list, tlsNameTypeHostName), u16(len(n))...), n...)
	}
	return ext(tlsExtServerName, append(u16(len(list)), list...))
}

func (h helloSpec) body() []byte {
	if h.version == 0 {
		h.version = 0x0303
	}
	if h.suites == nil {
		h.suites = []byte{0x13, 0x01}
	}
	if h.methods == nil {
		h.methods = []byte{0}
	}
	b := append(u16(int(h.version)), make([]byte, 32)...)
	b = append(append(b, byte(len(h.session))), h.session...)
	b = append(append(b, u16(len(h.suites))...), h.suites...)
	b = append(append(b, byte(len(h.methods))), h.methods...)
	if !h.noExts {
		b = append(append(b, u16(len(h.exts))...), h.exts...)
	}
	return append(b, h.trailing...)
}

// handshake is the body with its handshake header.
func handshake(body []byte) []byte {
	n := len(body)
	return append([]byte{tlsHandshakeHello, byte(n >> 16), byte(n >> 8), byte(n)}, body...)
}

// records cuts a handshake message into TLS records of at most size bytes each.
func records(hs []byte, size int) []byte {
	var out []byte
	for len(hs) > 0 {
		k := min(size, len(hs))
		out = append(append(out, tlsRecordHandshake, 3, 1, byte(k>>8), byte(k)), hs[:k]...)
		hs = hs[k:]
	}
	return out
}

func hello(exts ...[]byte) []byte {
	var e []byte
	for _, x := range exts {
		e = append(e, x...)
	}
	return records(handshake(helloSpec{exts: e}.body()), 1<<14)
}

// clientConn plays the network for a real crypto/tls client: it keeps what the client
// writes and never answers.
type clientConn struct {
	net.Conn
	mu  sync.Mutex
	buf []byte
	got chan struct{}
}

func (c *clientConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, p...)
	select {
	case c.got <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (c *clientConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *clientConn) Close() error                     { return nil }
func (c *clientConn) SetDeadline(time.Time) error      { return nil }
func (c *clientConn) SetReadDeadline(time.Time) error  { return nil }
func (c *clientConn) SetWriteDeadline(time.Time) error { return nil }

// realHello returns what crypto/tls sends first for cfg.
func realHello(t testing.TB, cfg *tls.Config) []byte {
	t.Helper()
	c := &clientConn{got: make(chan struct{}, 1)}
	// The read fails at once and ends the handshake, after the hello is out.
	_ = tls.Client(c, cfg).HandshakeContext(context.Background())
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.buf) == 0 {
		t.Fatal("crypto/tls wrote nothing")
	}
	return c.buf
}

func TestParseClientHelloRealClients(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *tls.Config
	}{
		{"TLS 1.3", &tls.Config{ServerName: "registry.npmjs.org"}},
		{"TLS 1.2 only", &tls.Config{ServerName: "registry.npmjs.org", MaxVersion: tls.VersionTLS12}},
		{"ALPN", &tls.Config{ServerName: "registry.npmjs.org", NextProtos: []string{"h2", "http/1.1"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := parseClientHello(realHello(t, tc.cfg))
			if r.state != parseDone || r.name != "registry.npmjs.org" {
				t.Fatalf("%+v", r)
			}
		})
	}
	t.Run("no server name", func(t *testing.T) {
		r := parseClientHello(realHello(t, &tls.Config{InsecureSkipVerify: true}))
		if r.state != parseBad || r.kind != KindNoServerName {
			t.Fatalf("%+v", r)
		}
	})
}

func TestParseClientHelloTable(t *testing.T) {
	good := sniExt("a.example")
	grease := ext(0x0a0a, nil)
	long := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name  string
		b     []byte
		state parseState
		kind  string
	}{
		{"plain", hello(good), parseDone, ""},
		{"with others", hello(grease, ext(0x0010, []byte{0, 3, 2, 'h', '2'}), good, ext(0x002b, []byte{2, 3, 4})), parseDone, ""},
		{"empty", nil, parseMore, ""},
		{"HTTP", []byte("GET / HTTP/1.1\r\n"), parseBad, KindNotTLS},
		{"SSLv2", []byte{0x80, 0x2e, 0x01, 0x00, 0x02}, parseBad, KindNotTLS},
		{"alert record", []byte{21, 3, 1, 0, 2, 1, 0}, parseBad, KindNotTLS},
		{"record version 3.0", []byte{22, 3, 0, 0, 5}, parseBad, KindNotTLS},
		{"record version 4.1", []byte{22, 4, 1}, parseBad, KindNotTLS},
		{"zero-length record", []byte{22, 3, 1, 0, 0}, parseBad, KindBadHello},
		{"oversized record", []byte{22, 3, 1, 0x40, 0x01}, parseBad, KindBadHello},
		{"not a hello", []byte{22, 3, 1, 0, 4, 2, 0, 0, 0}, parseBad, KindBadHello},
		{"hello longer than the bound", []byte{22, 3, 1, 0, 4, 1, 0xff, 0xff, 0xff}, parseBad, KindBadHello},
		{"no extensions", records(handshake(helloSpec{noExts: true}.body()), 1<<14), parseBad, KindNoServerName},
		{"no server name", hello(grease), parseBad, KindNoServerName},
		{"ECH", hello(good, ext(tlsExtECH, []byte{0})), parseBad, KindECH},
		{"ECH before the name", hello(ext(tlsExtECH, nil), good), parseBad, KindECH},
		{"old ESNI", hello(good, ext(tlsExtESNI, nil)), parseBad, KindECH},
		{"ECH draft", hello(good, ext(0xfe0a, nil)), parseBad, KindECH},
		{"two names", hello(sniExt("a.example", "b.example")), parseBad, KindBadHello},
		{"two name extensions", hello(sniExt("a.example"), sniExt("b.example")), parseBad, KindBadHello},
		{"repeated other extension", hello(good, grease, grease), parseBad, KindBadHello},
		{"empty name", hello(sniExt("")), parseBad, KindBadHello},
		{"empty list", hello(ext(tlsExtServerName, u16(0))), parseBad, KindBadHello},
		{"name type 1", hello(ext(tlsExtServerName, append(u16(4), 1, 0, 1, 'a'))), parseBad, KindBadHello},
		{"list longer than the extension", hello(ext(tlsExtServerName, append(u16(40), 0, 0, 1, 'a'))), parseBad, KindBadHello},
		{"bytes after the list", hello(ext(tlsExtServerName, append(u16(4), 0, 0, 1, 'a', 9))), parseBad, KindBadHello},
		{"session ID too long", records(handshake(helloSpec{exts: good, session: []byte(long)}.body()), 1<<14), parseBad, KindBadHello},
		{"odd suites", records(handshake(helloSpec{exts: good, suites: []byte{1, 2, 3}}.body()), 1<<14), parseBad, KindBadHello},
		{"no suites", records(handshake(helloSpec{exts: good, suites: []byte{}}.body()), 1<<14), parseBad, KindBadHello},
		{"no compression", records(handshake(helloSpec{exts: good, methods: []byte{}}.body()), 1<<14), parseBad, KindBadHello},
		{"old body version", records(handshake(helloSpec{exts: good, version: 0x0300}.body()), 1<<14), parseBad, KindBadHello},
		{"bytes after the extensions", records(handshake(helloSpec{exts: good, trailing: []byte{0}}.body()), 1<<14), parseBad, KindBadHello},
		{"extension past the end", hello(append(u16(0), 0xff, 0xff)), parseBad, KindBadHello},
		{"cut extension header", hello([]byte{0}), parseBad, KindBadHello},
		{"body too short", records(handshake([]byte{3, 3, 0}), 1<<14), parseBad, KindBadHello},
		{"empty body", records(handshake(nil), 1<<14), parseBad, KindBadHello},
		{"second message in the record", records(append(handshake(helloSpec{exts: good}.body()), 1, 0, 0, 0), 1<<14), parseBad, KindBadHello},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := parseClientHello(tc.b)
			if r.state != tc.state || r.kind != tc.kind {
				t.Fatalf("state %d kind %q, want %d %q", r.state, r.kind, tc.state, tc.kind)
			}
			if tc.state == parseDone && r.name != "a.example" {
				t.Fatalf("name %q", r.name)
			}
		})
	}
}

// The verdict arrives only with the last byte of the hello, however it was cut.
func TestParseClientHelloSplit(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    []byte
	}{
		{"one record", hello(sniExt("a.example"))},
		{"a record per byte", records(handshake(helloSpec{exts: sniExt("a.example")}.body()), 1)},
		{"records of 7", records(handshake(helloSpec{exts: sniExt("a.example")}.body()), 7)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k := range len(tc.b) {
				if r := parseClientHello(tc.b[:k]); r.state != parseMore {
					t.Fatalf("after %d of %d bytes: %+v", k, len(tc.b), r)
				}
			}
			if r := parseClientHello(tc.b); r.state != parseDone || r.name != "a.example" {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func TestParseClientHelloBadRecordInside(t *testing.T) {
	hs := handshake(helloSpec{exts: sniExt("a.example")}.body())
	b := records(hs[:10], 1<<14)
	b = append(b, 21, 3, 1, 0, 2, 1, 0) // an alert between the pieces
	b = append(b, records(hs[10:], 1<<14)...)
	if r := parseClientHello(b); r.state != parseBad {
		t.Fatalf("%+v", r)
	}
}

func TestParseClientHelloBounds(t *testing.T) {
	// A hello that fits the bound only without its many record headers.
	pad := ext(0x0015, make([]byte, 10000))
	hs := handshake(helloSpec{exts: append(sniExt("a.example"), pad...)}.body())
	if r := parseClientHello(records(hs, 1<<14)); r.state != parseDone {
		t.Fatalf("10 kB hello: %+v", r)
	}
	if r := parseClientHello(records(hs, 4)); r.state != parseBad {
		t.Fatalf("over the bound in small records: %+v", r)
	}
	// Endless small records.
	var endless []byte
	for len(endless) < maxHelloBytes {
		endless = append(endless, 22, 3, 1, 0, 1, 1)
	}
	if r := parseClientHello(endless); r.state != parseBad {
		t.Fatalf("endless records: %+v", r)
	}
}

// crypto/tls's server parser is a second opinion: when it reads a hello that this parser
// accepts, the two agree on the name.
var errStopHandshake = errors.New("seen")

type serverConn struct {
	net.Conn
	in []byte
}

func (c *serverConn) Read(p []byte) (int, error) {
	if len(c.in) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.in)
	c.in = c.in[n:]
	return n, nil
}
func (c *serverConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *serverConn) Close() error                     { return nil }
func (c *serverConn) SetDeadline(time.Time) error      { return nil }
func (c *serverConn) SetReadDeadline(time.Time) error  { return nil }
func (c *serverConn) SetWriteDeadline(time.Time) error { return nil }

// stdlibServerName returns the name crypto/tls's server read from b, and whether it got
// as far as a parsed hello.
func stdlibServerName(b []byte) (string, bool) {
	name, seen := "", false
	cfg := &tls.Config{GetConfigForClient: func(ch *tls.ClientHelloInfo) (*tls.Config, error) {
		name, seen = ch.ServerName, true
		return nil, errStopHandshake
	}}
	_ = tls.Server(&serverConn{in: append([]byte(nil), b...)}, cfg).HandshakeContext(context.Background())
	return name, seen
}

func TestParseClientHelloAgreesWithStdlib(t *testing.T) {
	b := realHello(t, &tls.Config{ServerName: "a.example"})
	if name, seen := stdlibServerName(b); !seen || name != "a.example" {
		t.Fatalf("stdlib: %q %v", name, seen)
	}
	if name, seen := stdlibServerName(records(handshake(helloSpec{exts: sniExt("a.example")}.body()), 3)); !seen || name != "a.example" {
		t.Fatalf("stdlib on split records: %q %v", name, seen)
	}
}
