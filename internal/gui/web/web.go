// Package web is the GUI's page (ADR 0008, direction "B"), served from memory on
// https://hb.localhost/ by internal/gui/app: an HTML document, its style sheet and
// script, the icons, and the brand images from assets/. There is no other file and no
// network: Lookup answers every path the page may ask for, and nothing else.
package web

import (
	"embed"
	"path"
	"strings"

	"github.com/Higangssh/hyperbutler/assets"
)

//go:embed index.html app.css app.js icons/*.svg
var files embed.FS

// CSP is the page's Content-Security-Policy. Only this origin's own script, style and
// images load; no inline script or style, no eval, no frames, forms, workers, fonts,
// or connections (the bridge is window.chrome.webview, which CSP does not govern);
// and Trusted Types with no policy, so assigning a string as HTML (innerHTML,
// outerHTML, insertAdjacentHTML, document.write) throws instead of parsing text that
// a container wrote.
const CSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; " +
	"connect-src 'none'; font-src 'none'; media-src 'none'; object-src 'none'; frame-src 'none'; " +
	"child-src 'none'; worker-src 'none'; manifest-src 'none'; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'; require-trusted-types-for 'script'; trusted-types 'none'"

// Headers are sent with every file.
var Headers = map[string]string{
	"Content-Security-Policy":      CSP,
	"X-Content-Type-Options":       "nosniff",
	"Referrer-Policy":              "no-referrer",
	"Cache-Control":                "no-store",
	"Cross-Origin-Opener-Policy":   "same-origin",
	"Cross-Origin-Resource-Policy": "same-origin",
	"Permissions-Policy":           "camera=(), microphone=(), geolocation=(), clipboard-read=(), usb=(), serial=(), hid=(), bluetooth=()",
}

var types = map[string]string{
	".html": "text/html; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".svg":  "image/svg+xml",
	".png":  "image/png",
}

// Lookup returns the file at p (a URL path such as "/app.js") and its content type.
// "/" is the document. ok is false for any other path.
func Lookup(p string) (body []byte, contentType string, ok bool) {
	switch p {
	case "/", "/index.html":
		p = "/index.html"
	case "/mascot.png":
		return assets.Mascot, types[".png"], true
	case "/icon.png":
		return assets.Icon, types[".png"], true
	}
	if !strings.HasPrefix(p, "/") || strings.Contains(p, "..") || strings.Contains(p, "\\") {
		return nil, "", false
	}
	name := strings.TrimPrefix(path.Clean(p), "/")
	ct, known := types[path.Ext(name)]
	if !known {
		return nil, "", false
	}
	b, err := files.ReadFile(name)
	if err != nil {
		return nil, "", false
	}
	return b, ct, true
}
