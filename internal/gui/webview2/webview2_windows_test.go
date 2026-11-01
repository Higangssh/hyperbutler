//go:build windows

package webview2

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
func TestTakeStringBounds(t *testing.T) {
	if _, ok := takeString(0, 10); ok {
		t.Fatal("nil string accepted")
	}
	p := coTaskString("hello")
	if s, ok := takeString(p, 10); !ok || s != "hello" {
		t.Fatalf("got %q %v", s, ok)
	}
	p = coTaskString("too long")
	if _, ok := takeString(p, 3); ok {
		t.Fatal("over-long string accepted")
	}
}

func TestHRESULT(t *testing.T) {
	if !failed(0x80004002) || failed(0) || failed(1) {
		t.Fatal("failed()")
	}
	// The upper half of a 64-bit register is ignored.
	if failed(0xFFFFFFFF00000000) {
		t.Fatal("upper bits taken")
	}
}

func TestSameOrigin(t *testing.T) {
	o, _ := url.Parse("https://hb.localhost")
	v := &View{origin: o}
	for _, u := range []string{"https://hb.localhost/", "https://hb.localhost/app.js", "HTTPS://HB.LOCALHOST/x"} {
		if _, ok := v.sameOrigin(u); !ok {
			t.Errorf("%s refused", u)
		}
	}
	for _, u := range []string{"https://hb.localhost@evil.example/", "https://evil.example/", "http://hb.localhost/",
		"https://hb.localhost:444/", "https://hb.localhost.evil.example/", "blob:https://hb.localhost/x", "data:text/html,x",
		"javascript:alert(1)", "file:///C:/Windows", "https://user@hb.localhost/"} {
		if _, ok := v.sameOrigin(u); ok {
			t.Errorf("%s accepted", u)
		}
	}
}
