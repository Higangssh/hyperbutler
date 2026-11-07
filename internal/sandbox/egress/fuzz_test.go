package egress

import (
	"crypto/tls"
	"net/netip"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/netstack"
)

// maxFuzzPrefixes bounds the input whose every prefix a fuzz run walks.
const maxFuzzPrefixes = 2048

// checkPrefixes: whatever is decided on a prefix stays decided on anything longer, so
// the verdict does not depend on how the bytes were cut into reads. parse is called on
// every prefix of b and on b.
func checkPrefixes(t *testing.T, b []byte, parse func([]byte) parsed) parsed {
	t.Helper()
	whole := parse(b)
	if len(b) > maxFuzzPrefixes {
		return whole
	}
	for k := range len(b) {
		if r := parse(b[:k]); r.state != parseMore && (r.state != whole.state || r.name != whole.name) {
			t.Fatalf("after %d of %d bytes: %+v, but whole: %+v", k, len(b), r, whole)
		}
	}
	return whole
}

// FuzzClientHello: arbitrary bytes on 443 from the guest. The parser never panics, a
// decided prefix stays decided, what it allows has exactly one server name, and crypto/tls
// reads the same name from any hello it can read and this parser accepted.
func FuzzClientHello(f *testing.F) {
	good := sniExt("a.example")
	hs := handshake(helloSpec{exts: good}.body())
	f.Add(hello(good))
	f.Add(records(hs, 1))
	f.Add(records(hs, 7))
	f.Add(hello(good, ext(tlsExtECH, []byte{1})))
	f.Add(hello(sniExt("a.example", "b.example")))
	f.Add(hello(good, good))
	f.Add(hello(ext(0x0a0a, nil)))
	f.Add(hello(sniExt("A.EXAMPLE.")))
	f.Add(hello(sniExt("a\x00.example")))
	f.Add([]byte{0x80, 0x2e, 0x01, 0x00, 0x02})
	f.Add([]byte("GET / HTTP/1.1\r\n\r\n"))
	f.Add(append(hello(good), 20, 3, 3, 0, 1, 1))
	f.Add(realHello(f, &tls.Config{ServerName: "a.example"}))
	f.Add(realHello(f, &tls.Config{ServerName: "a.example", MaxVersion: tls.VersionTLS12}))
	dst := netip.MustParseAddrPort("8.8.4.4:443")
	f.Fuzz(func(t *testing.T, b []byte) {
		checkPrefixes(t, b, parseRetryHello)
		r := checkPrefixes(t, b, parseClientHello)
		if r.state == parseDone {
			if r.name == "" {
				t.Fatal("accepted without a name")
			}
			if r.end <= 0 || r.end > len(b) {
				t.Fatalf("hello ends at %d of %d", r.end, len(b))
			}
			if got, seen := stdlibServerName(b); seen && !strings.EqualFold(strings.TrimSuffix(r.name, "."), got) {
				t.Fatalf("this parser read %q, crypto/tls read %q", r.name, got)
			}
		}
		if len(b) > maxHelloBytes && r.state == parseMore {
			t.Fatal("still waiting past the bound")
		}
		// Through the policy: only the pinned name gets through, and the log stays clean.
		p, err := New(Dev, []Entry{{Name: "a.example"}}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		p.Answer("a.example", []netip.Addr{dst.Addr()})
		v := p.Inspect(dst, b)
		if v == netstack.InspectAllowTLS {
			if c, ok := canonName(r.name); !ok || c != "a.example" {
				t.Fatalf("allowed %q", r.name)
			}
		}
		if (v == netstack.InspectMore) != (r.state == parseMore) {
			t.Fatalf("verdict %d for %+v", v, r)
		}
		checkLog(t, p)
	})
}

// FuzzServerReply: arbitrary bytes from a server. The parser never panics, a decided prefix
// stays decided, and whatever way a HelloRetryRequest's bytes come after its 5-byte
// record headers, it is one exactly when the handshake bytes say so.
func FuzzServerReply(f *testing.F) {
	var random [32]byte
	f.Add(serverHello(tlsRetryRandom, 1<<14))
	f.Add(serverHello(tlsRetryRandom, 1))
	f.Add(serverHello(tlsRetryRandom, 7))
	f.Add(serverHello(random, 1<<14))
	f.Add([]byte("220 smtp ready\r\n"))
	f.Add([]byte{21, 3, 3, 0, 2, 2, 40})
	f.Add([]byte{22, 3, 3, 0, 0})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		whole := parseServerReply(b)
		if len(b) <= maxFuzzPrefixes {
			for k := range len(b) {
				if r := parseServerReply(b[:k]); r != replyMore && r != whole {
					t.Fatalf("after %d of %d bytes: %d, but whole: %d", k, len(b), r, whole)
				}
			}
		}
		// A real one is seen whatever follows it, and however it was cut.
		size := 1 + len(b)%64
		if parseServerReply(append(serverHello(tlsRetryRandom, size), b...)) != replyRetry {
			t.Fatalf("missed in records of %d", size)
		}
		var other [32]byte
		copy(other[:], b)
		if other != tlsRetryRandom && parseServerReply(append(serverHello(other, size), b...)) != replyPass {
			t.Fatalf("another random taken for a retry, in records of %d", size)
		}
		// Through the policy, a retry is the only way to more inspection.
		p, err := New(Locked, nil, Options{})
		if err != nil {
			t.Fatal(err)
		}
		v := p.InspectReply(netip.MustParseAddrPort("8.8.4.4:443"), b)
		if (v == netstack.ReplyRetry) != (whole == replyRetry) || (v == netstack.ReplyMore) != (whole == replyMore) {
			t.Fatalf("verdict %d for %d", v, whole)
		}
	})
}

// FuzzHTTPHead: arbitrary bytes on 80 from the guest. As FuzzClientHello, with net/http as
// the second opinion: a head this parser accepts and net/http can read has the same Host.
func FuzzHTTPHead(f *testing.F) {
	for _, s := range []string{
		"GET / HTTP/1.1\r\nHost: a.example\r\n\r\n",
		"GET /x HTTP/1.0\r\nhost:a.example:80\r\nAccept: */*\r\n\r\nbody",
		"GET / HTTP/1.1\r\nHost: a.example\r\nHost: b.example\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: a.example\r\n b.example\r\n\r\n",
		"GET / HTTP/1.1\nHost: a.example\n\n",
		"GET http://b.example/ HTTP/1.1\r\nHost: a.example\r\n\r\n",
		"CONNECT a.example:443 HTTP/1.1\r\nHost: a.example\r\n\r\n",
		"OPTIONS * HTTP/1.1\r\nHost: [::1]:80\r\n\r\n",
		"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n",
		"\x16\x03\x01\x00\x01\x01",
		"GET / HTTP/1.1\r\nHost : a.example\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: a\x00.example\r\n\r\n",
	} {
		f.Add([]byte(s))
	}
	dst := netip.MustParseAddrPort("8.8.4.4:80")
	f.Fuzz(func(t *testing.T, b []byte) {
		r := checkPrefixes(t, b, parseHTTPHead)
		if r.state == parseDone {
			if r.name == "" || strings.ContainsAny(r.name, ":\r\n\t ") {
				t.Fatalf("name %q", r.name)
			}
			if host, ok := stdlibHost(b); ok {
				if i := strings.LastIndexByte(host, ':'); i >= 0 {
					host = host[:i]
				}
				if host != r.name {
					t.Fatalf("this parser read %q, net/http read %q", r.name, host)
				}
			}
		}
		if len(b) > maxHeadBytes && r.state == parseMore {
			t.Fatal("still waiting past the bound")
		}
		p, err := New(Dev, []Entry{{Name: "a.example"}}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		p.Answer("a.example", []netip.Addr{dst.Addr()})
		if p.Inspect(dst, b) == netstack.InspectAllow {
			if c, ok := canonName(r.name); !ok || c != "a.example" {
				t.Fatalf("allowed %q", r.name)
			}
		}
		checkLog(t, p)
	})
}

// FuzzParseEntry: an accepted entry is canonical, valid, reads back the same and makes a
// policy whose names are ASCII; a wildcard never covers a whole top-level domain. Whatever
// the input, an error shows it escaped.
func FuzzParseEntry(f *testing.F) {
	for _, s := range []string{
		"example.com", "EXAMPLE.com.", "example.com:22", "*.example.com", "lan:nas", "lan:22", "lan.:22", "*.com", "*", "lan:*.a.b",
		"127.0.0.1", "0x7f.1", "[::1]:22", "exaKple.com", "bücher.example", "a..b", "-a.b", "a:b:c", "a\x00b", ":", "lan:",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		e, err := ParseEntry(s)
		if err != nil {
			if !isPrintableASCII(err.Error()) {
				t.Fatalf("error with raw bytes: %q", err.Error())
			}
			return
		}
		if err := e.validate(); err != nil {
			t.Fatalf("%q: %+v: %v", s, e, err)
		}
		if !isPrintableASCII(e.String()) || strings.ToLower(e.Name) != e.Name || strings.HasSuffix(e.Name, ".") {
			t.Fatalf("%q: %+v", s, e)
		}
		back, err := ParseEntry(e.String())
		if err != nil || back != e {
			t.Fatalf("%q: %q reads back as %+v, %v", s, e, back, err)
		}
		if _, ok := netip.ParseAddr(e.Name); ok == nil {
			t.Fatalf("%q: an address as a name", s)
		}
		p, err := New(Locked, []Entry{e}, Options{})
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if got := p.AllowQuery(e.Name, 1); got == e.Wildcard {
			t.Fatalf("%q: AllowQuery(%q) = %v", s, e.Name, got)
		}
		if p.AllowQuery("other-"+e.Name+".invalid", 1) {
			t.Fatalf("%q allowed another name", s)
		}
	})
}

func checkLog(t *testing.T, p *Policy) {
	t.Helper()
	for _, b := range p.Blocked() {
		if !isPrintableASCII(b.Name) {
			t.Fatalf("raw bytes in the log: %q", b.Name)
		}
	}
}
