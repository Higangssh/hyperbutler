package dockerenv

import (
	"bytes"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// FuzzProxyURL: ParseProxyURL never panics; what it accepts round-trips through
// RewriteLoopback, and the redacted form is valid and never contains the password.
func FuzzProxyURL(f *testing.F) {
	for _, s := range []string{
		"http://proxy:3128", "http://user:secret@proxy:3128", "socks5://[::1]:1080", "https://u@h",
		"http://127.0.0.1:3128", "http://a%40b:c%3Ad@x:1/", "ftp://x", "http://x:0", "http://%zz",
	} {
		f.Add(s)
	}
	gw := netip.MustParseAddr("192.168.127.254")
	f.Fuzz(func(t *testing.T, v string) {
		u, err := ParseProxyURL(v)
		if err != nil {
			// Longer than any phrase of the messages, so a match is the input itself.
			if len(v) > 64 && strings.Contains(err.Error(), v) {
				t.Fatalf("the error quotes the input: %v", err)
			}
			if Redact(v) != "(invalid proxy URL, not shown)" && v != "" {
				t.Fatal("an invalid URL is shown")
			}
			return
		}
		r := Redact(v)
		if pw, ok := u.User.Password(); ok && len(pw) >= 3 && strings.Contains(r, pw) && !strings.Contains(u.Host, pw) && !strings.Contains(u.User.Username(), pw) && !strings.Contains(u.Scheme, pw) {
			t.Fatalf("Redact(%q) = %q shows the password", v, r)
		}
		if strings.Contains(r, "@") && !strings.Contains(r, "***@") {
			t.Fatalf("Redact(%q) = %q keeps user info", v, r)
		}
		if err := ValidateProxyURL(r); err != nil {
			t.Fatalf("Redact(%q) = %q is not a valid URL: %v", v, r, err)
		}
		w, _ := RewriteLoopback(v, gw)
		if err := ValidateProxyURL(w); err != nil {
			t.Fatalf("RewriteLoopback(%q) = %q is invalid: %v", v, w, err)
		}
	})
}

// FuzzDecode: Decode never panics, and what it accepts is canonical and valid.
func FuzzDecode(f *testing.F) {
	var s CertSet
	s.Now = time.Now()
	s.Add(validCA(f, "corp"), true)
	good, _ := (&Doc{HTTPProxy: "http://u:p@proxy:3128", NoProxy: "localhost", CACerts: s.PEM()}).Encode()
	f.Add(good)
	for _, v := range []string{"", "{}", `{"httpProxy":"http://p:1"}`, `{"noProxy":"a,b"}`, `{"caCerts":""}`, "null", `{"httpProxy":"http://p:1","x":2}`} {
		f.Add([]byte(v))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		orig := bytes.Clone(b)
		d, err := Decode(b)
		if !bytes.Equal(orig, b) {
			t.Fatal("Decode changed its input")
		}
		if err != nil {
			return
		}
		if err := d.Validate(); err != nil {
			t.Fatalf("accepted an invalid doc: %v", err)
		}
		re, err := d.Encode()
		if err != nil || !bytes.Equal(re, b) {
			t.Fatalf("not canonical: %q vs %q (%v)", re, b, err)
		}
		for _, kv := range d.Environ("/x") {
			if strings.ContainsAny(kv, "\n\r\x00") {
				t.Fatalf("environment entry %q has a line break", kv)
			}
		}
	})
}

// FuzzNoProxy: ValidateNoProxy never panics, and an accepted list has no characters
// that could break the init's environment file.
func FuzzNoProxy(f *testing.F) {
	for _, s := range []string{"localhost", "*.corp,10.0.0.0/8", "a:1", "[::1]:2", "a,,b", " x "} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		if ValidateNoProxy(v) != nil {
			return
		}
		n := NormalizeNoProxy(v)
		if strings.ContainsAny(n, "\n\r\x00=\"'$`\\ ") {
			t.Fatalf("accepted %q", v)
		}
		if ValidateNoProxy(n) != nil || NormalizeNoProxy(n) != n {
			t.Fatalf("normal form %q of %q is not stable", n, v)
		}
	})
}
