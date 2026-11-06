package egress

import (
	"bufio"
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestParseHTTPHeadTable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		head  string
		state parseState
		host  string
	}{
		{"plain", "GET / HTTP/1.1\r\nHost: a.example\r\n\r\n", parseDone, "a.example"},
		{"port stripped", "GET /x HTTP/1.1\r\nHost: a.example:8080\r\n\r\n", parseDone, "a.example"},
		{"any case, odd spaces", "GET / HTTP/1.1\r\nhOsT:\t a.example \r\nAccept: */*\r\n\r\n", parseDone, "a.example"},
		{"HTTP/1.0 with Host", "POST /y HTTP/1.0\r\nHost: a.example\r\n\r\n", parseDone, "a.example"},
		{"OPTIONS *", "OPTIONS * HTTP/1.1\r\nHost: a.example\r\n\r\n", parseDone, "a.example"},
		{"body follows", "POST / HTTP/1.1\r\nHost: a.example\r\nContent-Length: 2\r\n\r\nhi", parseDone, "a.example"},
		{"high bytes in another header", "GET / HTTP/1.1\r\nX-A: \xc3\xa9\r\nHost: a.example\r\n\r\n", parseDone, "a.example"},

		{"nothing yet", "", parseMore, ""},
		{"line unfinished", "GET / HT", parseMore, ""},
		{"head unfinished", "GET / HTTP/1.1\r\nHost: a.example\r\n", parseMore, ""},
		{"CR last", "GET / HTTP/1.1\r", parseMore, ""},

		{"TLS", "\x16\x03\x01\x02\x00", parseBad, ""},
		{"no Host", "GET / HTTP/1.1\r\n\r\n", parseBad, ""},
		{"HTTP/1.0 without Host", "GET / HTTP/1.0\r\n\r\n", parseBad, ""},
		{"two Hosts", "GET / HTTP/1.1\r\nHost: a.example\r\nHost: b.example\r\n", parseBad, ""},
		{"two Hosts, one spelled in capitals", "GET / HTTP/1.1\r\nHost: a.example\r\nHOST: a.example\r\n\r\n", parseBad, ""},
		{"empty Host", "GET / HTTP/1.1\r\nHost:\r\n\r\n", parseBad, ""},
		{"only a port", "GET / HTTP/1.1\r\nHost: :80\r\n\r\n", parseBad, ""},
		{"space inside", "GET / HTTP/1.1\r\nHost: a b.example\r\n\r\n", parseBad, ""},
		{"space before the port", "GET / HTTP/1.1\r\nHost: a.example :80\r\n\r\n", parseBad, ""},
		{"IPv6", "GET / HTTP/1.1\r\nHost: [::1]:80\r\n\r\n", parseBad, ""},
		{"bad port", "GET / HTTP/1.1\r\nHost: a.example:x\r\n\r\n", parseBad, ""},
		{"port too large", "GET / HTTP/1.1\r\nHost: a.example:65536\r\n\r\n", parseBad, ""},
		{"empty port", "GET / HTTP/1.1\r\nHost: a.example:\r\n\r\n", parseBad, ""},
		{"space before the colon", "GET / HTTP/1.1\r\nHost : b.example\r\nHost: a.example\r\n\r\n", parseBad, ""},
		{"folded line", "GET / HTTP/1.1\r\nHost: a.example\r\n b.example\r\n\r\n", parseBad, ""},
		{"bare LF", "GET / HTTP/1.1\nHost: a.example\n\n", parseBad, ""},
		{"bare LF hiding a Host", "GET / HTTP/1.1\r\nX: y\nHost: b.example\r\nHost: a.example\r\n\r\n", parseBad, ""},
		{"bare CR", "GET / HTTP/1.1\r\nX: y\rHost: b.example\r\nHost: a.example\r\n\r\n", parseBad, ""},
		{"NUL", "GET / HTTP/1.1\r\nHost: a\x00.example\r\n\r\n", parseBad, ""},
		{"blank line first", "\r\nGET / HTTP/1.1\r\nHost: a.example\r\n\r\n", parseBad, ""},
		{"absolute target", "GET http://b.example/ HTTP/1.1\r\nHost: a.example\r\n\r\n", parseBad, ""},
		{"CONNECT", "CONNECT b.example:443 HTTP/1.1\r\nHost: a.example\r\n\r\n", parseBad, ""},
		{"HTTP/2 preface", "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n", parseBad, ""},
		{"HTTP/1.2", "GET / HTTP/1.2\r\nHost: a.example\r\n\r\n", parseBad, ""},
		{"HTTP/0.9", "GET /\r\n", parseBad, ""},
		{"extra space", "GET  / HTTP/1.1\r\nHost: a.example\r\n\r\n", parseBad, ""},
		{"star for GET", "GET * HTTP/1.1\r\nHost: a.example\r\n\r\n", parseBad, ""},
		{"no colon", "GET / HTTP/1.1\r\nHost a.example\r\n\r\n", parseBad, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := parseHTTPHead([]byte(tc.head))
			if r.state != tc.state || r.name != tc.host {
				t.Fatalf("%+v, want state %d host %q", r, tc.state, tc.host)
			}
		})
	}
}

func TestParseHTTPHeadBound(t *testing.T) {
	head := "GET / HTTP/1.1\r\nHost: a.example\r\nX: " + strings.Repeat("y", 1000) + "\r\n\r\n"
	if r := parseHTTPHead([]byte(head)); r.state != parseDone {
		t.Fatalf("%+v", r)
	}
	long := "GET / HTTP/1.1\r\nHost: a.example\r\nX: " + strings.Repeat("y", maxHeadBytes)
	if r := parseHTTPHead([]byte(long)); r.state != parseBad {
		t.Fatalf("over the bound: %+v", r)
	}
}

func TestParseHTTPHeadRealClient(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://A.Example:8080/x?y=1", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "*/*")
	var b bytes.Buffer
	if err := req.Write(&b); err != nil {
		t.Fatal(err)
	}
	r := parseHTTPHead(b.Bytes())
	if r.state != parseDone {
		t.Fatalf("%q: %+v", b.String(), r)
	}
	if c, ok := canonName(r.name); !ok || c != "a.example" {
		t.Fatalf("name %q", r.name)
	}
}

func TestParseHTTPHeadSplit(t *testing.T) {
	b := []byte("GET / HTTP/1.1\r\nHost: a.example\r\nAccept: */*\r\n\r\n")
	for k := range len(b) {
		if r := parseHTTPHead(b[:k]); r.state != parseMore {
			t.Fatalf("after %d bytes: %+v", k, r)
		}
	}
	if r := parseHTTPHead(b); r.state != parseDone || r.name != "a.example" {
		t.Fatalf("%+v", r)
	}
}

// stdlibHost returns the Host net/http reads from b, without the port.
func stdlibHost(b []byte) (string, bool) {
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(b)))
	if err != nil {
		return "", false
	}
	return req.Host, true
}
