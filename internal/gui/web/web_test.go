package web

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

// TestMain fails the package's tests if any goroutine is still running after
// they finish (ADR 0003, guardrail 1).
func TestMain(m *testing.M) {
	leaktest.VerifyTestMain(m)
}

func TestLookup(t *testing.T) {
	for _, p := range []string{"/", "/index.html", "/app.js", "/app.css", "/mascot.png", "/icon.png", "/icons/mem.svg"} {
		if b, ct, ok := Lookup(p); !ok || len(b) == 0 || ct == "" {
			t.Errorf("%s: not served", p)
		}
	}
	for _, p := range []string{"", "app.js", "/guard.js", "/probe.js", "/../go.mod", "/icons/../web.go", "/web.go", "/icons", "/icons/", "/x.html", "/%2e%2e/web.go", "/app.js\\..\\web.go"} {
		if _, _, ok := Lookup(p); ok {
			t.Errorf("%s: served", p)
		}
	}
}

// TestNoNetworkOrHTML checks the page's own rules: no URL of another origin, no inline
// script or style, and no way to put a string into the document as HTML.
func TestNoNetworkOrHTML(t *testing.T) {
	for _, f := range []string{"index.html", "app.js", "app.css"} {
		b, err := files.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "fetch(", "XMLHttpRequest", "WebSocket", "http://", "https://", "fonts.googleapis", "import(", "localStorage", "window.open"} {
			if bytes.Contains(b, []byte(bad)) {
				t.Errorf("%s uses %q", f, bad)
			}
		}
	}
	// The page never uses WebRTC (guard.js removes it) or frames (guard.js refuses them).
	for _, f := range []string{"index.html", "app.js"} {
		b, _ := files.ReadFile(f)
		for _, bad := range []string{"RTC", "iframe", "<frame", "<object", "<embed"} {
			if bytes.Contains(b, []byte(bad)) {
				t.Errorf("%s uses %q", f, bad)
			}
		}
	}
	for _, want := range []string{"RTCPeerConnection", "configurable: false", "createElement", "iframe"} {
		if !strings.Contains(Guard, want) {
			t.Errorf("guard.js lacks %q", want)
		}
	}
	// Text the user reads is whole catalog sentences with placeholders, and lists are
	// joined by Intl.ListFormat (REQ-UX-013): no sentence pieces glued in the script.
	js, _ := files.ReadFile("app.js")
	for _, bad := range []string{`+ " · " +`, `"● " +`, `.join(", ")`, `.join("; ")`, `+ " " + t(`, `t("error.HB-IMP-004") +`} {
		if bytes.Contains(js, []byte(bad)) {
			t.Errorf("app.js glues text together with %q; use a catalog sentence", bad)
		}
	}
	// Nor any text in a language in the script: language names come from the catalogs
	// too (language.name), so no letter outside ASCII is written in it, but for the
	// typographic signs it draws (marks, the missing-key brackets).
	for i, r := range string(js) {
		if r > 0x7e && !strings.ContainsRune("⟦⟧✓×", r) {
			t.Errorf("app.js has %q at byte %d; put the text in the catalogs", r, i)
			break
		}
	}
	html, _ := files.ReadFile("index.html")
	if regexp.MustCompile(`<script>|<style|\son[a-z]+=|style="`).Match(html) {
		t.Error("index.html has an inline script, style or event handler, which the CSP refuses")
	}
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "require-trusted-types-for 'script'", "trusted-types 'none'", "connect-src 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(CSP, want) {
			t.Errorf("CSP lacks %q", want)
		}
	}
	if Headers["Content-Security-Policy"] != CSP || Headers["X-Content-Type-Options"] != "nosniff" {
		t.Error("headers")
	}
}
