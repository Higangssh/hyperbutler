package relay

import (
	"bufio"
	"bytes"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
)

// FuzzParseConnect: never panics, never reads more than maxHeaderLen bytes, and an
// accepted target is a canonical host:port that SplitTarget accepts again unchanged.
func FuzzParseConnect(f *testing.F) {
	f.Add([]byte("CONNECT registry-1.docker.io:443 HTTP/1.1\r\nHost: registry-1.docker.io:443\r\n\r\n"))
	f.Add([]byte("CONNECT [::1]:22 HTTP/1.0\n\n"))
	f.Add([]byte("CONNECT 1.2.3:443 HTTP/1.1\r\n\r\n"))
	f.Add([]byte("CONNECT a:443 HTTP/1.1\r\n\r\nTLS bytes follow"))
	f.Fuzz(func(t *testing.T, data []byte) {
		src := bytes.NewReader(data)
		r := bufio.NewReaderSize(src, 16)
		host, port, err := ParseConnect(r)
		if consumed := len(data) - src.Len() - r.Buffered(); consumed > maxHeaderLen+1 {
			t.Fatalf("consumed %d bytes", consumed)
		}
		if err != nil {
			return
		}
		if port == 0 {
			t.Fatal("accepted port 0")
		}
		target := net.JoinHostPort(host, strconv.Itoa(int(port)))
		h2, p2, err := SplitTarget(target)
		if err != nil || h2 != host || p2 != port {
			t.Fatalf("SplitTarget(%q) = %q %d %v", target, h2, p2, err)
		}
		if strings.ContainsAny(host, " \r\n\x00%") {
			t.Fatalf("accepted host %q", host)
		}
		if a, err := netip.ParseAddr(host); err == nil && a.Zone() != "" {
			t.Fatalf("accepted zoned address %q", host)
		}
	})
}

// FuzzHandshake: the client side never panics or reads past the end of the reply
// header, whatever the relay sends.
func FuzzHandshake(f *testing.F) {
	f.Add([]byte("HTTP/1.1 200 Connection Established\r\n\r\nDATA"))
	f.Add([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
	f.Add([]byte("HTTP/1.1 200\r\n"))
	f.Fuzz(func(t *testing.T, reply []byte) {
		rw := &fakeConn{r: bytes.NewReader(reply)}
		err := Handshake(rw, "example.com:443")
		if err != nil {
			return
		}
		end := bytes.Index(reply, []byte("\r\n\r\n"))
		if end < 0 {
			t.Fatal("accepted a reply without a header end")
		}
		if f := strings.Fields(string(reply[:end])); len(f) < 2 || f[1] != "200" {
			t.Fatalf("accepted status line %q", reply[:end])
		}
		if rw.r.Len() != len(reply)-end-4 {
			t.Fatalf("consumed %d bytes past the header", len(reply)-end-4-rw.r.Len())
		}
	})
}

type fakeConn struct {
	r *bytes.Reader
	w bytes.Buffer
}

func (c *fakeConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *fakeConn) Write(p []byte) (int, error) { return c.w.Write(p) }
