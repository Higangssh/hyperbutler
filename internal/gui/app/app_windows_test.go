//go:build windows

package app

import (
	"net/url"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

// TestMain fails the package's tests if any goroutine is still running after
// they finish (ADR 0003, guardrail 1).
func TestMain(m *testing.M) {
	leaktest.VerifyTestMain(m)
}
func TestServe(t *testing.T) {
	for _, c := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/", 200}, {"GET", "/app.js", 200}, {"POST", "/", 405}, {"GET", "/nope", 404}, {"GET", "/../go.mod", 404},
	} {
		u, _ := url.Parse("https://hb.localhost" + c.path)
		r := serve(c.method, u)
		if r.Status != c.status || r.Headers["Content-Security-Policy"] == "" {
			t.Errorf("%s %s: %d", c.method, c.path, r.Status)
		}
	}
	u, _ := url.Parse("https://hb.localhost/app.js?x=1")
	if r := serve("GET", u); r.Status != 405 {
		t.Errorf("query: %d", r.Status)
	}
}

func TestWindowsLanguages(t *testing.T) {
	for _, l := range WindowsLanguages() {
		if l == "" || len(l) > 85 {
			t.Errorf("language %q", l)
		}
	}
}
