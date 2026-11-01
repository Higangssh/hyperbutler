//go:build windows

package app

import (
	"net/url"
	"testing"
	"time"

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

// TestSingleInstance (L1): a second start of the same profile is not first; with no
// window of that instance to bring forward it says found false (the window then shows a
// message instead of exiting silently). Another profile is an instance of its own.
func TestSingleInstance(t *testing.T) {
	instanceWait = 200 * time.Millisecond
	key := `c:\users\x\appdata\local\temp\` + t.Name()
	release, first, _ := singleInstance(key)
	defer release()
	if !first {
		t.Fatal("the first start was not first")
	}
	if r2, first, found := singleInstance(key); first || found {
		r2()
		t.Fatalf("second start: first %v found %v", first, found)
	}
	r3, first, _ := singleInstance(key + "-other")
	defer r3()
	if !first {
		t.Fatal("another profile shares the instance")
	}
	if windowClass(key) == windowClass(key+"-other") || windowClass(key) == "hyperbutlerGUI" {
		t.Fatal("window classes are not per instance")
	}
}

func TestWindowsLanguages(t *testing.T) {
	for _, l := range WindowsLanguages() {
		if l == "" || len(l) > 85 {
			t.Errorf("language %q", l)
		}
	}
}
