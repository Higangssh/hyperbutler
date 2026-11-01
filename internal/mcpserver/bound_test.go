package mcpserver

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

// TestMain fails the package's tests if any goroutine is still running after
// they finish (ADR 0003, guardrail 1).
func TestMain(m *testing.M) {
	leaktest.VerifyTestMain(m)
}

func TestDemux(t *testing.T) {
	in := append(append(frame(1, "out\n"), frame(2, "err\n")...), frame(1, "cut sh")...)
	in = in[:len(in)-2] // the last frame cut short
	out, ok := demux(in)
	if !ok || string(out) != "out\nerr\ncut " {
		t.Errorf("demux = %q, %v", out, ok)
	}
	out, ok = demux(append(frame(1, "a"), []byte("plain text, not a stream")...))
	if ok || string(out) != "a" {
		t.Errorf("bad header: %q, %v", out, ok)
	}
	if out, ok := demux(nil); !ok || len(out) != 0 {
		t.Errorf("empty: %q, %v", out, ok)
	}
}

func FuzzDemux(f *testing.F) {
	f.Add(append(frame(1, "hello\n"), frame(2, "world\n")...))
	f.Add(frame(0, ""))
	f.Add([]byte("\x01\x00\x00\x00\xff\xff\xff\xffshort"))
	f.Add([]byte("not a stream"))
	f.Fuzz(func(t *testing.T, b []byte) {
		out, ok := demux(b)
		if len(out) > len(b) {
			t.Fatalf("%d bytes out of %d", len(out), len(b))
		}
		if ok && len(b) >= 8 && b[0] <= 2 && len(out) > len(b)-8 {
			t.Fatalf("payload %d of a %d-byte stream with a header", len(out), len(b))
		}
		// What demux returns is redacted, cleaned and bounded like any log.
		text, n, _ := logText(out, maxTail, maxLogBytes)
		if len(text) > maxLogBytes || n > maxTail || !utf8.ValidString(text) {
			t.Fatalf("logText: %d bytes, %d lines, valid %v", len(text), n, utf8.ValidString(text))
		}
	})
}

func TestLogText(t *testing.T) {
	text, n, cut := logText([]byte("a\nb\nc\n"), 2, 100)
	if text != "b\nc" || n != 2 || !cut {
		t.Errorf("tail 2: %q %d %v", text, n, cut)
	}
	text, n, cut = logText([]byte("a\nb\n"), 5, 100)
	if text != "a\nb" || n != 2 || cut {
		t.Errorf("all: %q %d %v", text, n, cut)
	}
	text, n, cut = logText([]byte("aaaa\nbbbb\ncccc\n"), 10, 10)
	if text != "bbbb\ncccc" || n != 2 || !cut {
		t.Errorf("bytes: %q %d %v", text, n, cut)
	}
	text, n, cut = logText([]byte(strings.Repeat("é", 100)), 10, 11)
	if !utf8.ValidString(text) || len(text) > 11 || n != 1 || !cut {
		t.Errorf("one long line: %q %d %v", text, n, cut)
	}
	// Redaction happens before the cut: a secret on the cut line is gone, not halved.
	text, _, _ = logText([]byte("x password=hunter2hunter2\n"), 1, 12)
	if strings.Contains(text, "hunter") {
		t.Errorf("cut secret: %q", text)
	}
	if text, n, cut := logText(nil, 10, 10); text != "" || n != 0 || cut {
		t.Errorf("empty: %q %d %v", text, n, cut)
	}
}

func TestCleanText(t *testing.T) {
	rlo, lri, bad := string(rune(0x202e)), string(rune(0x2066)), string(utf8.RuneError)
	in := "ok\ttab\r\n\x1b[31mred\x07 " + rlo + "evil" + lri + " \xff\u0085"
	if got := cleanText(in, true); got != "ok\ttab\n"+bad+"[31mred"+bad+" "+bad+"evil"+bad+" "+bad+bad {
		t.Errorf("multiline: %q", got)
	}
	if got := cleanText("a\nb\tc", false); got != "a"+bad+"b"+bad+"c" {
		t.Errorf("single line: %q", got)
	}
}

func TestNameChecks(t *testing.T) {
	for _, ok := range []string{"web", "a", "my-app_1.2", strings.Repeat("a", 64), "0123456789ab"} {
		if err := validName("container", ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "-x", "_x", "a/b", `a\b`, "a b", "a?b", "a%2f", strings.Repeat("a", 256)} {
		if validName("container", bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, ok := range []string{"busybox", "busybox:1.37", "library/busybox", "ghcr.io/o/n:tag", "localhost:5000/x", "x@sha256:" + strings.Repeat("a", 64), "sha256:abc"} {
		if err := validImageRef(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "..", "a/../b", "a/./b", "a//b", "a/", "/a", "-a", "a b", "a?b", "a#b", `a\b`} {
		if validImageRef(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for in, want := range map[string][2]string{
		"busybox":                {"busybox", "latest"},
		"busybox:1.37":           {"busybox", "1.37"},
		"localhost:5000/x":       {"localhost:5000/x", "latest"},
		"localhost:5000/x:2":     {"localhost:5000/x", "2"},
		"x@sha256:abc":           {"x", "sha256:abc"},
		"ghcr.io/o/n:t@sha256:a": {"ghcr.io/o/n:t", "sha256:a"},
	} {
		if n, tag := splitImageRef(in); n != want[0] || tag != want[1] {
			t.Errorf("%s: %s %s, want %v", in, n, tag, want)
		}
	}
}

func TestRedactedJSON(t *testing.T) {
	b, err := redactedJSON(map[string]any{"password": "x", "url": "https://u:p@h/", "note": "Bearer abcdefgh"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(`"x"`)) || bytes.Contains(b, []byte("u:p@")) || bytes.Contains(b, []byte("abcdefgh")) || bytes.Contains(b, []byte("\n")) {
		t.Errorf("redactedJSON = %s", b)
	}
}
