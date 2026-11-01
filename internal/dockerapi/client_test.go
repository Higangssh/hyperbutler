package dockerapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	c := New(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	})
	t.Cleanup(c.Close)
	return c
}

func TestListRunning(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"Id":"aaaaaaaaaaaaaaaa","Names":["/web"],"Image":"nginx"},{"Id":"bbbbbbbbbbbbbbbb","Names":[],"Image":"busybox"}]`))
	})
	mux.HandleFunc("/containers/aaaaaaaaaaaaaaaa/json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"HostConfig":{"RestartPolicy":{"Name":"always"}}}`))
	})
	mux.HandleFunc("/containers/bbbbbbbbbbbbbbbb/json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"HostConfig":{"RestartPolicy":{"Name":"on-failure"}}}`))
	})
	got, err := ListRunning(context.Background(), testClient(t, mux))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "web" || !got[0].ComesBack || got[1].Name != "bbbbbbbbbbbb" || got[1].ComesBack || !got[1].MayComeBack() {
		t.Fatalf("got %+v", got)
	}
}

func TestAPIError(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"No such container: x"}`))
	}))
	err := c.GetJSON(context.Background(), "/containers/x/json", nil, &struct{}{})
	var ae *Error
	if !errors.As(err, &ae) || ae.Message != "No such container: x" || !IsNotFound(err) {
		t.Fatalf("err = %v", err)
	}
	// Error() names the request and the status, then dockerd's message, cleaned.
	if s := err.Error(); !strings.HasPrefix(s, "GET /containers/x/json: Docker API 404 Not Found") {
		t.Fatalf("Error() = %q", s)
	}
}

func TestRequestError(t *testing.T) {
	c := New(func(context.Context) (net.Conn, error) { return nil, errors.New("no pipe") })
	defer c.Close()
	err := c.GetJSON(context.Background(), "/info", nil, &struct{}{})
	var re *RequestError
	if !errors.As(err, &re) || re.Op != "GET /info" {
		t.Fatalf("err = %v", err)
	}
}

func TestValidNameAndIsName(t *testing.T) {
	for _, ok := range []string{"web", "a1", "my_app.v2-1", strings.Repeat("a", 255)} {
		if err := ValidName("container", ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "-x", "a/b", "../x", "a b", "a?b", "a%2fb", strings.Repeat("a", 256), "x\n"} {
		if ValidName("container", bad) == nil || IsName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCleanAndLogText(t *testing.T) {
	if got := CleanText("a\x1b[2Jb\u202ec\r\nd", true); got != "a�[2Jb�c\nd" {
		t.Errorf("CleanText = %q", got)
	}
	text, n, cut := LogText([]byte("1\n2\n3\n"), 2, 1<<10)
	if text != "2\n3" || n != 2 || !cut {
		t.Errorf("LogText = %q %d %v", text, n, cut)
	}
	out, ok := Demux([]byte{1, 0, 0, 0, 0, 0, 0, 2, 'h', 'i'})
	if !ok || string(out) != "hi" {
		t.Errorf("Demux = %q %v", out, ok)
	}
	if ComposeLabel("my app") != "" || ComposeLabel("shop-api") != "shop-api" {
		t.Error("ComposeLabel")
	}
}

// FuzzDemux: the log parsers never panic, and their output is never longer than the input.
func FuzzDemux(f *testing.F) {
	f.Add([]byte{1, 0, 0, 0, 0, 0, 0, 2, 'h', 'i'})
	f.Add([]byte("plain text\n"))
	f.Add([]byte{2, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 'x'})
	f.Fuzz(func(t *testing.T, b []byte) {
		if out, _ := Demux(b); len(out) > len(b) {
			t.Fatal("Demux grew")
		}
		if r := Resync(b); r != nil && !frames(r) {
			t.Fatal("Resync returned no frames")
		}
		text, n, _ := LogText(b, 10, 256)
		if len(text) > 256+4 || n > 10 {
			t.Fatalf("LogText out of bounds: %d bytes, %d lines", len(text), n)
		}
	})
}

func TestResync(t *testing.T) {
	frame := func(s string) []byte { return append([]byte{1, 0, 0, 0, 0, 0, 0, byte(len(s))}, s...) }
	b := append(frame("aaaa"), frame("bbbb")...)
	if out, ok := Demux(Resync(b[3:])); !ok || string(out) != "bbbb" {
		t.Fatalf("resync from mid-frame: %q %v", out, ok)
	}
	if Resync([]byte("no frames here")) != nil {
		t.Fatal("found frames in text")
	}
}
