//go:build windows

package webview2

import (
	"net/url"
	"testing"
	"unsafe"

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

// TestOptions3 (L7): the options object answers ICoreWebView2EnvironmentOptions3 with a
// second interface of the same COM object (same identity and reference count) whose
// IsCustomCrashReportingEnabled is true, and still refuses every other newer interface.
func TestOptions3(t *testing.T) {
	initVtables()
	o := &envOptions{vtbl: &optionsVtbl, refs: 1}
	o.opt3 = envOptions3{vtbl: &options3Vtbl, outer: o}
	main, third := uintptr(unsafe.Pointer(o)), o.options3Addr()
	regMu.Lock()
	options[main], options3[third] = o, o
	regMu.Unlock()
	defer func() {
		regMu.Lock()
		delete(options, main)
		delete(options3, third)
		regMu.Unlock()
	}()
	var out uintptr
	if hr := optionsQueryInterface(main, uintptr(unsafe.Pointer(iidEnvOptions3)), uintptr(unsafe.Pointer(&out))); hr != sOK || out != third {
		t.Fatalf("QueryInterface(Options3): %#x %#x", hr, out)
	}
	if hr := options3QueryInterface(third, uintptr(unsafe.Pointer(iidIUnknown)), uintptr(unsafe.Pointer(&out))); hr != sOK || out != main {
		t.Fatalf("Options3's IUnknown is not the object's: %#x %#x", hr, out)
	}
	options2 := mustGUID("{FF85C98A-1BA7-4A6B-90C8-2B752C89E9E2}")
	if hr := options3QueryInterface(third, uintptr(unsafe.Pointer(options2)), uintptr(unsafe.Pointer(&out))); hr != eNoInterface || out != 0 {
		t.Fatalf("Options2 answered: %#x", hr)
	}
	if o.refs != 3 {
		t.Fatalf("refs %d, want 3 (one shared count)", o.refs)
	}
	var on int32
	if hr := options3GetCrashReporting(third, uintptr(unsafe.Pointer(&on))); hr != sOK || on != 1 || !o.crashAsked.Load() {
		t.Fatalf("IsCustomCrashReportingEnabled: %#x %d", hr, on)
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
