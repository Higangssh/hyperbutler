package transport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// testRewriter turns the Windows source C:\src into a guest path and fails on "FAIL".
func testRewriter(body []byte) ([]byte, error) {
	if bytes.Contains(body, []byte("FAIL")) {
		return nil, errors.New("cannot share FAIL")
	}
	return bytes.ReplaceAll(body, []byte(`C:\\src`), []byte(`/mnt/hyperbutler/b-1`)), nil
}

func runFilter(t testing.TB, in string, rw CreateRewriter) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := newReqFilter(strings.NewReader(in), &out, rw).run()
	return out.String(), err
}

const createBody = `{"Image":"alpine","HostConfig":{"Binds":["C:\\src:/data:ro"]}}`

var rewritten = strings.ReplaceAll(createBody, `C:\\src`, `/mnt/hyperbutler/b-1`)

func TestFilterPassesOtherRequestsUnchanged(t *testing.T) {
	in := "HEAD /_ping HTTP/1.1\r\nHost: docker\r\n\r\n" +
		"GET /v1.47/containers/json?all=1 HTTP/1.1\r\nHost: docker\r\n\r\n" +
		"POST /v1.47/build HTTP/1.1\r\nHost: docker\r\nTransfer-Encoding: chunked\r\n\r\n5;x=y\r\nhello\r\n0\r\nX-T: 1\r\n\r\n" +
		"POST /containers/x/start HTTP/1.1\r\nHost: docker\r\nContent-Length: 3\r\n\r\nabc" +
		// Looks like a create, but is the body of the request before it.
		"POST /images/load HTTP/1.1\r\nHost: d\r\nContent-Length: 60\r\n\r\nPOST /containers/create HTTP/1.1\r\nContent-Length: 2\r\n\r\n{}"
	out, err := runFilter(t, in, testRewriter)
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, out, in)
}

func TestFilterRewritesCreate(t *testing.T) {
	for _, path := range []string{"/containers/create", "/v1.47/containers/create?name=web", "/v1.4/containers/create"} {
		in := "HEAD /_ping HTTP/1.1\r\nHost: docker\r\n\r\n" +
			fmt.Sprintf("POST %s HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", path, len(createBody), createBody) +
			"GET /_ping HTTP/1.1\r\nHost: docker\r\n\r\n"
		out, err := runFilter(t, in, testRewriter)
		if err != nil {
			t.Fatal(err)
		}
		want := "HEAD /_ping HTTP/1.1\r\nHost: docker\r\n\r\n" +
			fmt.Sprintf("POST %s HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", path, len(rewritten), rewritten) +
			"GET /_ping HTTP/1.1\r\nHost: docker\r\n\r\n"
		mustEqual(t, out, want)
	}
}

func TestFilterRewritesChunkedCreate(t *testing.T) {
	half := len(createBody) / 2
	in := fmt.Sprintf("POST /containers/create HTTP/1.1\r\nHost: docker\r\nTransfer-Encoding: chunked\r\nExpect: 100-continue\r\n\r\n%x\r\n%s\r\n%x\r\n%s\r\n0\r\n\r\n",
		half, createBody[:half], len(createBody)-half, createBody[half:])
	out, err := runFilter(t, in, testRewriter)
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, out, fmt.Sprintf("POST /containers/create HTTP/1.1\r\nHost: docker\r\nContent-Length: %d\r\n\r\n%s", len(rewritten), rewritten))
}

func TestFilterLeavesHijackedStreamsAlone(t *testing.T) {
	create := fmt.Sprintf("POST /containers/create HTTP/1.1\r\nContent-Length: %d\r\n\r\n%s", len(createBody), createBody)
	for _, head := range []string{
		"POST /v1.47/containers/abc/attach?stream=1&stdin=1 HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n",
		"POST /v1.47/exec/abc/start HTTP/1.1\r\nHost: docker\r\nContent-Length: 2\r\n\r\n{}",
		"POST /containers/abc/attach HTTP/1.1\r\nHost: docker\r\n\r\n",
		"GET /grpc HTTP/1.1\r\nHost: docker\r\nConnection: upgrade\r\nUpgrade: h2c\r\n\r\n",
		"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n",
		"garbage\r\n\r\n",
	} {
		in := head + create // stdin of the hijacked stream that happens to look like a create
		out, err := runFilter(t, in, testRewriter)
		if err != nil {
			t.Fatal(err)
		}
		mustEqual(t, out, in)
	}
}

// Review finding 1: a detached exec start is a plain request on a pooled connection; a
// create after it must still be rewritten.
func TestFilterFollowsDetachedExecStart(t *testing.T) {
	exec := "POST /v1.47/exec/abc/start HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\nContent-Length: 29\r\n\r\n{\"Detach\":true,\"Tty\":false}\r\n"
	in := exec + fmt.Sprintf("POST /containers/create HTTP/1.1\r\nContent-Length: %d\r\n\r\n%s", len(createBody), createBody)
	var out bytes.Buffer
	f := newReqFilter(strings.NewReader(in), &out, testRewriter)
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if f.wentRaw {
		t.Fatal("went raw after a detached exec start")
	}
	mustEqual(t, out.String(), exec+fmt.Sprintf("POST /containers/create HTTP/1.1\r\nContent-Length: %d\r\n\r\n%s", len(rewritten), rewritten))
}

func TestFilterRefusals(t *testing.T) {
	for _, tc := range []struct {
		in     string
		status int
	}{
		{fmt.Sprintf("POST /containers/create HTTP/1.1\r\nContent-Length: %d\r\n\r\n", MaxCreateBody+1), http.StatusRequestEntityTooLarge},
		{fmt.Sprintf("POST /containers/create HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n0\r\n\r\n", MaxCreateBody+1, strings.Repeat("x", MaxCreateBody+1)), http.StatusRequestEntityTooLarge},
		{"POST /containers/create HTTP/1.1\r\nContent-Length: 6\r\n\r\n\"FAIL\"", http.StatusBadRequest},
		{"POST /containers/create HTTP/1.1\r\nX-A: 1\r\n  2\r\nContent-Length: 2\r\n\r\n{}", http.StatusBadRequest},
		{"POST /containers/create HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}XX0\r\n\r\n", http.StatusBadRequest},
	} {
		out, err := runFilter(t, "GET /_ping HTTP/1.1\r\n\r\n"+tc.in, testRewriter)
		var rej *rejection
		if !errors.As(err, &rej) || rej.status != tc.status {
			t.Fatalf("%.60q: err = %v, want a %d refusal", tc.in, err, tc.status)
		}
		mustEqual(t, out, "GET /_ping HTTP/1.1\r\n\r\n") // nothing of the create went upstream
	}
	// The exact limit is accepted.
	body := `"` + strings.Repeat("x", MaxCreateBody-2) + `"`
	out, err := runFilter(t, fmt.Sprintf("POST /containers/create HTTP/1.1\r\nContent-Length: %d\r\n\r\n%s", len(body), body), testRewriter)
	if err != nil || !strings.HasSuffix(out, body) {
		t.Fatalf("body at the limit: err = %v", err)
	}
}

// createDockerd answers each create with the Content-Length it saw and the body, and
// /_ping with OK, on keep-alive connections.
func createDockerd(t *testing.T) Dialer {
	t.Helper()
	ln := tcpListener(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					req, err := http.ReadRequest(br)
					if err != nil {
						return
					}
					body, _ := io.ReadAll(req.Body)
					if strings.HasSuffix(req.URL.Path, "/containers/create") {
						msg := fmt.Sprintf("%d %s", req.ContentLength, body)
						fmt.Fprintf(c, "HTTP/1.1 201 Created\r\nContent-Length: %d\r\n\r\n%s", len(msg), msg)
					} else {
						io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nOK")
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { ln.Close(); <-done })
	return TCPDialer{Address: ln.Addr().String()}
}

func TestProxyRewritesCreateEndToEnd(t *testing.T) {
	ln := tcpListener(t)
	runProxy(t, &Proxy{Upstream: createDockerd(t), RewriteCreate: testRewriter}, ln)
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var nd net.Dialer
			return nd.DialContext(ctx, "tcp", ln.Addr().String())
		},
	}}
	defer client.CloseIdleConnections()
	for i := 0; i < 3; i++ { // keep-alive: ping, create, ping, create on one connection
		resp, err := client.Get("http://docker/_ping")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		resp, err = client.Post("http://docker/v1.47/containers/create", "application/json", strings.NewReader(createBody))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		mustEqual(t, string(b), fmt.Sprintf("%d %s", len(rewritten), rewritten))
	}
}

// TestProxyRefusalAfterPipelinedRequest: the refusal comes after the response dockerd
// owes for an earlier request, and the connection then closes.
func TestProxyRefusalAfterPipelinedRequest(t *testing.T) {
	ln := tcpListener(t)
	runProxy(t, &Proxy{Upstream: createDockerd(t), RewriteCreate: testRewriter}, ln)
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	// A refused body far larger than the pipe buffers: the client is still writing it
	// when the refusal comes.
	big := strings.Repeat("x", MaxCreateBody+(1<<20))
	go fmt.Fprintf(c, "GET /_ping HTTP/1.1\r\nHost: d\r\n\r\nPOST /containers/create HTTP/1.1\r\nHost: d\r\nContent-Length: %d\r\n\r\n%s", len(big), big)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	mustEqual(t, string(b), "OK")
	resp, err = http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(string(b), `"message":"hyperbutler: container create body exceeds 4 MiB"`) {
		t.Fatalf("got %d %s", resp.StatusCode, b)
	}
	if _, err := br.ReadByte(); err != io.EOF {
		t.Fatalf("after the refusal: %v, want EOF", err)
	}
}

func TestProxyRewriteAttachStillHalfCloses(t *testing.T) {
	d := newFakeDockerd(t)
	ln := tcpListener(t)
	runProxy(t, &Proxy{Upstream: d.dialer(), RewriteCreate: testRewriter}, ln)
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	mustEqual(t, attachRoundTrip(t, c, "hello world"), "HELLO WORLD\nstdin closed\n")
}

// --- fuzzing --------------------------------------------------------------------------

// reqView is what dockerd's net/http server makes of one request.
type reqView struct {
	method, uri, host, proto string
	header                   http.Header
	body                     string
	create                   bool
}

// dockerdView parses b as dockerd would: requests until a parse error or a hijack.
// rest is what follows the last complete request, which dockerd then handles as raw
// bytes (or refuses).
func dockerdView(b []byte) (views []reqView, rest string) {
	br := bufio.NewReader(bytes.NewReader(b))
	remaining := func() string {
		r, _ := io.ReadAll(br)
		return string(r)
	}
	for {
		req, err := http.ReadRequest(br)
		if err != nil || req.ProtoMajor != 1 {
			return views, ""
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return views, ""
		}
		h := req.Header.Clone()
		for _, k := range []string{"Content-Length", "Transfer-Encoding", "Expect"} {
			h.Del(k)
		}
		views = append(views, reqView{req.Method, req.RequestURI, req.Host, req.Proto, h, string(body),
			req.Method == http.MethodPost && createPath.MatchString(req.URL.Path)})
		if hijacks(req) && !oracleDetached(req, body) {
			return views, remaining()
		}
	}
}

// oracleDetached is dockerd's view of a detached exec start that keeps the connection,
// within the bounds the filter follows (detachedExec).
func oracleDetached(req *http.Request, body []byte) bool {
	var start struct{ Detach bool }
	return req.Method == http.MethodPost && execStartPath.MatchString(req.URL.Path) && !upgrades(req) &&
		len(req.TransferEncoding) == 0 && req.ContentLength > 0 && req.ContentLength <= maxExecStartBody &&
		json.Unmarshal(body, &start) == nil && start.Detach
}

func sameView(a, b reqView) bool {
	return a.method == b.method && a.uri == b.uri && a.host == b.host && a.proto == b.proto &&
		a.body == b.body && fmt.Sprint(a.header) == fmt.Sprint(b.header)
}

// FuzzRequestFilter checks the filter against dockerd's own parser: dockerd must see the
// same requests through the filter as without it (create bodies transformed when
// transform is set), and the raw tail after a hijack must be unchanged.
func FuzzRequestFilter(f *testing.F) {
	f.Add([]byte("HEAD /_ping HTTP/1.1\r\nHost: d\r\n\r\nPOST /v1.47/containers/create HTTP/1.1\r\nContent-Length: 2\r\n\r\n{}GET / HTTP/1.1\r\n\r\n"), true)
	f.Add([]byte("POST /containers/create HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n2;e\r\n{}\r\n0\r\nT: x\r\n\r\nPOST /containers/create HTTP/1.1\r\n\r\n"), true)
	f.Add([]byte("POST /build HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n0\r\n\r\nPOST /containers/create HTTP/1.1\r\nContent-Length: 1\r\n\r\nx"), false)
	f.Add([]byte("POST /exec/1/start HTTP/1.1\r\nContent-Length: 2\r\n\r\n{}POST /containers/create HTTP/1.1\r\nContent-Length: 1\r\n\r\nx"), true)
	f.Add([]byte("POST /containers/create HTTP/1.1\nContent-Length: 1\nContent-Length: 1\n\nxGET / HTTP/1.0\n\n"), true)
	f.Add([]byte("GET / HTTP/1.1\r\nUpgrade: tcp\r\n\r\nPOST /containers/create HTTP/1.1\r\n\r\n"), true)
	f.Add([]byte("POST /exec/1/start HTTP/1.1\r\nContent-Length: 15\r\n\r\n{\"Detach\":true}POST /containers/create HTTP/1.1\r\nContent-Length: 2\r\n\r\n{}"), true)
	f.Fuzz(func(t *testing.T, in []byte, transform bool) {
		rw := func(b []byte) ([]byte, error) { return b, nil }
		if transform {
			rw = func(b []byte) ([]byte, error) {
				if bytes.Contains(b, []byte("FAIL")) {
					return nil, errors.New("FAIL")
				}
				return append([]byte("<"), append(b, '>')...), nil
			}
		}
		var out bytes.Buffer
		filter := newReqFilter(bytes.NewReader(in), &out, rw)
		err := filter.run()
		var rej *rejection
		complete := err == nil
		if err != nil && !errors.As(err, &rej) && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			t.Fatalf("unexpected error %v", err)
		}
		if rej != nil {
			resp, rerr := http.ReadResponse(bufio.NewReader(bytes.NewReader(rej.response())), nil)
			if rerr != nil || resp.StatusCode != rej.status {
				t.Fatalf("bad refusal response %q: %v", rej.response(), rerr)
			}
		}
		if !transform && complete && !bytes.Contains(in, []byte("containers/create")) && !bytes.Equal(out.Bytes(), in) {
			t.Fatalf("changed bytes without any create:\nin  %q\nout %q", in, out.Bytes())
		}
		want, wantRest := dockerdView(in)
		got, gotRest := dockerdView(out.Bytes())
		if !complete {
			// Refused or cut short: dockerd saw a prefix of the requests.
			if len(got) > len(want) {
				t.Fatalf("%d requests went upstream, input has %d", len(got), len(want))
			}
			want = want[:len(got)]
		} else if len(got) != len(want) {
			t.Fatalf("dockerd sees %d requests, want %d:\nin  %q\nout %q", len(got), len(want), in, out.Bytes())
		}
		for i := range got {
			w := want[i]
			// After the filter went raw (input it would not follow, though dockerd
			// may), creates pass untranslated: that fails safe at dockerd.
			if w.create && transform && (!filter.wentRaw || i < filter.handled) {
				w.body = "<" + w.body + ">"
			}
			if !sameView(got[i], w) {
				t.Fatalf("request %d: dockerd sees %+v, want %+v\nin  %q\nout %q", i, got[i], w, in, out.Bytes())
			}
		}
		if complete && gotRest != wantRest {
			t.Fatalf("raw tail changed: %q, want %q", gotRest, wantRest)
		}
	})
}

// FuzzScanChunked: the chunk scanner accepts what net/http accepts with the same
// payload, and forwards verbatim exactly the bytes it consumed.
func FuzzScanChunked(f *testing.F) {
	f.Add([]byte("5\r\nhello\r\n0\r\n\r\n"))
	f.Add([]byte("5;ext=1\r\nhello\r\n000\r\nTrailer: x\r\n\r\nrest"))
	f.Add([]byte("ffffffffffffffff\r\n"))
	f.Add([]byte("1 \t\r\nx\r\n0\n\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		var framing, payload, verbatim bytes.Buffer
		br := bufio.NewReader(bytes.NewReader(in))
		err := scanChunked(br, &framing, &payload)
		rest, _ := io.ReadAll(br)
		used := len(in) - len(rest)
		br2 := bufio.NewReader(bytes.NewReader(in))
		err2 := scanChunked(br2, &verbatim, nil)
		if (err == nil) != (err2 == nil) || !bytes.Equal(verbatim.Bytes(), in[:verbatim.Len()]) {
			t.Fatalf("verbatim mode differs: %v / %v", err, err2)
		}
		if err == nil && verbatim.Len() != used {
			t.Fatalf("forwarded %d bytes, consumed %d", verbatim.Len(), used)
		}
		// net/http's decoder, through the trailer as the server reads it.
		req, rerr := http.ReadRequest(bufio.NewReader(io.MultiReader(strings.NewReader("POST / HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n"), bytes.NewReader(in))))
		if rerr != nil {
			t.Fatal(rerr)
		}
		want, herr := io.ReadAll(req.Body)
		if err == nil && herr != nil {
			t.Fatalf("scanner accepted %q, net/http: %v", in, herr)
		}
		if err == nil && !bytes.Equal(payload.Bytes(), want) {
			t.Fatalf("payload %q, net/http %q", payload.Bytes(), want)
		}
	})
}
