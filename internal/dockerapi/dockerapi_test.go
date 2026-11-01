package dockerapi

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDoAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte(`{"Version":"29.0.0"}`))
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"no such thing\u001b[2J"}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("plain text"))
		}
	}))
	defer srv.Close()
	c := New(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", srv.Listener.Addr().String())
	})
	defer c.Close()
	ctx := context.Background()
	var v struct{ Version string }
	if err := c.JSON(ctx, http.MethodGet, "/ok", nil, nil, &v); err != nil || v.Version != "29.0.0" {
		t.Fatalf("%v %v", v, err)
	}
	err := c.JSON(ctx, http.MethodGet, "/missing", nil, nil, nil)
	if !IsNotFound(err) || strings.Contains(err.Error(), "\x1b") || !strings.Contains(err.Error(), "no such thing") {
		t.Fatalf("%q", err)
	}
	err = c.JSON(ctx, http.MethodGet, "/other", nil, nil, nil)
	if StatusOf(err) != 500 || !strings.Contains(err.Error(), "plain text") {
		t.Fatalf("%q", err)
	}
}

func TestValidNames(t *testing.T) {
	for _, s := range []string{"pg", "my_vol.1", "abc123"} {
		if ValidName("volume", s) != nil {
			t.Errorf("%q refused", s)
		}
	}
	for _, s := range []string{"", ".", "..", "-v", "a/b", "a b", "a?b", strings.Repeat("a", 300)} {
		if ValidName("volume", s) == nil {
			t.Errorf("%q accepted", s)
		}
	}
	for _, s := range []string{"busybox", "busybox:1.37", "ghcr.io/o/app:1", "sha256:abc", "localhost:5000/x@sha256:abc"} {
		if ValidImageRef(s) != nil {
			t.Errorf("%q refused", s)
		}
	}
	for _, s := range []string{"", "../x", "a//b", "a/./b", "a?b", "-x", "a b"} {
		if ValidImageRef(s) == nil {
			t.Errorf("%q accepted", s)
		}
	}
	if got := Clean("a\x1b[2J\u202eb", 100); strings.ContainsAny(got, "\x1b\u202e") {
		t.Errorf("%q", got)
	}
	if got := Clean("héllo", 2); got != "h" {
		t.Errorf("cut %q", got)
	}
}
