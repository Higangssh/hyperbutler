package review

import (
	"bytes"
	"strings"
	"testing"
	"unicode"
)

func TestTextDiff(t *testing.T) {
	d := TextDiff([]byte("a\nb\nc\nd\ne\nf\ng\n"), []byte("a\nb\nc\nX\ne\nf\ng\nh\n"))
	if d.Added != 2 || d.Removed != 1 || d.Binary {
		t.Fatalf("%+v", d)
	}
	for _, want := range []string{"-d\n", "+X\n", "+h\n", " c\n"} {
		if !strings.Contains(d.Text, want) {
			t.Errorf("no %q in\n%s", want, d.Text)
		}
	}
}

func TestTextDiffSame(t *testing.T) {
	if d := TextDiff([]byte("a\n"), []byte("a\n")); d.Text != "" || d.Added != 0 || d.Removed != 0 {
		t.Fatalf("%+v", d)
	}
	if d := TextDiff(nil, nil); d.Text != "" {
		t.Fatalf("%+v", d)
	}
}

func TestTextDiffNewAndGone(t *testing.T) {
	if d := TextDiff(nil, []byte("x\ny")); d.Added != 2 || d.Removed != 0 {
		t.Fatalf("%+v", d)
	}
	if d := TextDiff([]byte("x\n"), nil); d.Added != 0 || d.Removed != 1 {
		t.Fatalf("%+v", d)
	}
}

// TestTextDiffFinalNewline: bytes that differ always show as a difference.
func TestTextDiffFinalNewline(t *testing.T) {
	for _, c := range [][2]string{{"a\n", "a"}, {"a", "a\n"}} {
		d := TextDiff([]byte(c[0]), []byte(c[1]))
		if d.Text == "" || d.Added != 1 || d.Removed != 1 || !strings.Contains(d.Text, "No newline") {
			t.Errorf("%q to %q: %+v", c[0], c[1], d)
		}
	}
}

func TestTextDiffBinary(t *testing.T) {
	if d := TextDiff([]byte("a"), []byte("a\x00b")); !d.Binary || d.Text != "" {
		t.Fatalf("%+v", d)
	}
}

// TestTextDiffEscapes: a line cannot hide itself or write over the review (REQ-SBX-009).
func TestTextDiffEscapes(t *testing.T) {
	d := TextDiff(nil, []byte("safe\x1b[2K\rlooks harmless ‮ evil\n"))
	for _, r := range d.Text {
		if r != '\n' && (unicode.IsControl(r) || r == '‮') {
			t.Fatalf("%q reached the text: %q", r, d.Text)
		}
	}
	if strings.Count(d.Text, "\n") != 1 {
		t.Fatalf("one line became several: %q", d.Text)
	}
}

func TestTextDiffBounds(t *testing.T) {
	long := strings.Repeat("x", maxDiffLine*2)
	if d := TextDiff(nil, []byte(long)); len(d.Text) > maxDiffLine*5 || !strings.Contains(d.Text, "cut") || !d.Cut {
		t.Fatalf("a long line: %d bytes, cut %v", len(d.Text), d.Cut)
	}
	many := strings.Repeat("l\n", maxDiffLines+1)
	if d := TextDiff(nil, []byte(many)); !strings.Contains(d.Text, "too long") || !d.Cut {
		t.Fatalf("many lines: %.80q", d.Text)
	}
}

func FuzzTextDiff(f *testing.F) {
	f.Add([]byte("a\nb\n"), []byte("a\nc\n"))
	f.Add([]byte{0}, []byte("x"))
	f.Add([]byte("\x1b]0;t\x07\r\n"), []byte("‮\n"))
	f.Fuzz(func(t *testing.T, a, b []byte) {
		d := TextDiff(a, b)
		for _, r := range d.Text {
			if r != '\n' && unicode.IsControl(r) {
				t.Fatalf("control %q in %q", r, d.Text)
			}
		}
		if !d.Binary && !bytes.Equal(a, b) && d.Text == "" {
			t.Fatalf("%q and %q differ and show nothing", a, b)
		}
		if len(d.Text) > maxDiffText+maxDiffLine*6+64 {
			t.Fatalf("%d bytes of text", len(d.Text))
		}
	})
}
