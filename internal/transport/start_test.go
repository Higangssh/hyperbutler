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
	"sync"
	"testing"
	"time"
)

const portsMsg = "Ports are not available: exposing port TCP 127.0.0.1:18080 -> 0.0.0.0:0: listen tcp 127.0.0.1:18080: bind: in use"

// refuseNamed refuses starts of the container "taken" and records every name asked.
type refuseNamed struct {
	mu    sync.Mutex
	asked []string
}

func (r *refuseNamed) check(_ context.Context, name string) error {
	r.mu.Lock()
	r.asked = append(r.asked, name)
	r.mu.Unlock()
	if name == "taken" {
		return errors.New(portsMsg)
	}
	return nil
}

func TestStartRef(t *testing.T) {
	for _, tc := range []struct{ method, path, want string }{
		{"POST", "/containers/web/start", "web"},
		{"POST", "/v1.47/containers/0123abcd/start", "0123abcd"},
		{"GET", "/containers/web/start", ""},
		{"POST", "/containers/web/stop", ""},
		{"POST", "/containers/start", ""},
		{"POST", "/containers/a/b/start", ""},
		{"POST", "/v1.47/exec/x/start", ""},
		{"POST", "/containers/" + strings.Repeat("a", maxStartRef+1) + "/start", ""},
	} {
		req, err := http.NewRequest(tc.method, "http://docker"+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := startRef(req); got != tc.want {
			t.Errorf("%s %s: %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestFilterChecksStart(t *testing.T) {
	r := &refuseNamed{}
	ok := "POST /v1.47/containers/web/start HTTP/1.1\r\nHost: docker\r\nContent-Length: 2\r\n\r\n{}"
	in := ok + "POST /v1.47/containers/taken/start HTTP/1.1\r\nHost: docker\r\n\r\n" +
		"GET /_ping HTTP/1.1\r\nHost: docker\r\n\r\n"
	var out bytes.Buffer
	f := newReqFilter(strings.NewReader(in), &out, nil)
	f.checkStart = r.check
	err := f.run()
	var rej *rejection
	if !errors.As(err, &rej) {
		t.Fatalf("run: %v, want a rejection", err)
	}
	if rej.status != http.StatusInternalServerError || rej.msg != portsMsg || !rej.plain {
		t.Errorf("rejection %+v", rej)
	}
	// The allowed start went on unchanged; nothing of the refused one, or after it, did.
	mustEqual(t, out.String(), ok)
	if fmt.Sprint(r.asked) != "[web taken]" {
		t.Errorf("asked %q", r.asked)
	}
	_, body, _ := strings.Cut(string(rej.response()), "\r\n\r\n")
	if m := dockerMessage(t, []byte(body)); m != portsMsg {
		t.Errorf("message %q: want dockerd's wording without a prefix", m)
	}
}

// Without a rewriter, creates pass unchanged while starts are still checked.
func TestFilterStartCheckWithoutRewriter(t *testing.T) {
	r := &refuseNamed{}
	in := fmt.Sprintf("POST /containers/create HTTP/1.1\r\nContent-Length: %d\r\n\r\n%s", len(createBody), createBody) +
		"POST /containers/web/start HTTP/1.1\r\n\r\n"
	var out bytes.Buffer
	f := newReqFilter(strings.NewReader(in), &out, nil)
	f.checkStart = r.check
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	mustEqual(t, out.String(), in)
	if fmt.Sprint(r.asked) != "[web]" {
		t.Errorf("asked %q", r.asked)
	}
}

// TestProxyRefusesStart: the docker CLI sees dockerd's error format with the checker's
// message, and dockerd never gets the start.
func TestProxyRefusesStart(t *testing.T) {
	r := &refuseNamed{}
	var mu sync.Mutex
	var seen []string
	ln := tcpListener(t)
	up := tcpListener(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := up.Accept()
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
					_, _ = io.Copy(io.Discard, req.Body)
					mu.Lock()
					seen = append(seen, req.URL.Path)
					mu.Unlock()
					_, _ = io.WriteString(c, "HTTP/1.1 204 No Content\r\n\r\n")
				}
			}()
		}
	}()
	t.Cleanup(func() { up.Close(); <-done })
	runProxy(t, &Proxy{Upstream: TCPDialer{Address: up.Addr().String()}, CheckStart: r.check}, ln)

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var nd net.Dialer
			return nd.DialContext(ctx, "tcp", ln.Addr().String())
		},
	}, Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Post("http://docker/v1.47/containers/web/start", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("allowed start: %s", resp.Status)
	}
	resp, err = client.Post("http://docker/v1.47/containers/taken/start", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError || dockerMessage(t, b) != portsMsg {
		t.Fatalf("refused start: %d %s", resp.StatusCode, b)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(seen) != "[/v1.47/containers/web/start]" {
		t.Errorf("dockerd got %q", seen)
	}
}

// dockerMessage decodes dockerd's error body.
func dockerMessage(t *testing.T, b []byte) string {
	t.Helper()
	var m struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("error body %q: %v", b, err)
	}
	return m.Message
}

// Review L4: the start check runs under the connection's context, which ends when the
// client disconnects during the check, not only when the proxy stops.
func TestStartCheckEndsWithClient(t *testing.T) {
	ln := tcpListener(t)
	defer ln.Close()
	cApp, cProxy := tcpPair(t, ln)
	uProxy, uApp := tcpPair(t, ln)
	defer cApp.Close()
	defer uApp.Close()
	fp := &fakeProber{TCPConn: cProxy}
	checking := make(chan struct{})
	ended := make(chan error, 1)
	check := func(ctx context.Context, _ string) error {
		close(checking)
		select {
		case <-ctx.Done():
			ended <- ctx.Err()
		case <-time.After(10 * time.Second):
			ended <- errors.New("not canceled")
		}
		return nil
	}
	done := make(chan struct{})
	go func() {
		forward(context.Background(), fp, uProxy, 10*time.Millisecond, discardLogger, &filterHooks{checkStart: check})
		close(done)
	}()
	if _, err := io.WriteString(cApp, "POST /containers/web/start HTTP/1.1\r\nHost: docker\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-checking:
	case <-time.After(5 * time.Second):
		t.Fatal("the start was not checked")
	}
	fp.gone.Store(true)
	if err := <-ended; !errors.Is(err, context.Canceled) {
		t.Fatalf("the check's context after the client left: %v", err)
	}
	_ = cApp.Close()
	_ = uApp.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("forward did not end")
	}
}
