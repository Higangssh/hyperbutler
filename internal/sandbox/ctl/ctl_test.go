package ctl

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var b bytes.Buffer
	if err := WriteFrame(&b, TypeOutput, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&b, TypeExit, Exit(-7)); err != nil {
		t.Fatal(err)
	}
	ty, d, err := ReadFrame(&b)
	if err != nil || ty != TypeOutput || string(d) != "hello" {
		t.Fatalf("got %v %q %v", ty, d, err)
	}
	ty, d, err = ReadFrame(&b)
	if err != nil || ty != TypeExit {
		t.Fatalf("got %v %v", ty, err)
	}
	if c, err := ParseExit(d); err != nil || c != -7 {
		t.Fatalf("exit %d %v", c, err)
	}
	if _, _, err := ReadFrame(&b); !errors.Is(err, io.EOF) {
		t.Fatalf("at the end: %v", err)
	}
}

func TestFrameBounds(t *testing.T) {
	if err := WriteFrame(io.Discard, TypeInput, make([]byte, MaxData+1)); !errors.Is(err, ErrFrame) {
		t.Fatalf("write over the limit: %v", err)
	}
	for _, n := range []uint32{0, MaxBody + 1, 1 << 31} {
		h := binary.BigEndian.AppendUint32(nil, n)
		if _, _, err := ReadFrame(bytes.NewReader(append(h, 1))); !errors.Is(err, ErrFrame) {
			t.Errorf("length %d: %v", n, err)
		}
	}
	h := binary.BigEndian.AppendUint32(nil, 10)
	if _, _, err := ReadFrame(bytes.NewReader(append(h, 1, 2))); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("cut frame: %v", err)
	}
}

func TestResize(t *testing.T) {
	c, r, err := ParseResize(Resize(120, 40))
	if err != nil || c != 120 || r != 40 {
		t.Fatalf("%d %d %v", c, r, err)
	}
	if _, _, err := ParseResize([]byte{1}); err == nil {
		t.Fatal("a short resize was taken")
	}
}

func TestBundleCommit(t *testing.T) {
	sha1 := strings.Repeat("a", 40)
	if got := BundleCommit([]byte("# v2 git bundle\n-" + strings.Repeat("b", 40) + " HEAD\n" + sha1 + " HEAD\n\nPACK")); got != sha1 {
		t.Fatalf("%q", got)
	}
	if got := BundleCommit([]byte("no end")); got != "" {
		t.Fatalf("%q", got)
	}
}

func TestCheckBundle(t *testing.T) {
	sha1 := strings.Repeat("a", 40)
	sha256 := strings.Repeat("b", 64)
	ok := []string{
		"# v2 git bundle\n" + sha1 + " HEAD\n\nPACK",
		"# v2 git bundle\n-" + sha1 + " the subject, any text\n" + sha1 + " HEAD\n\nPACK",
		"# v3 git bundle\n@object-format=sha256\n" + sha256 + " HEAD\n\nPACK",
	}
	for _, s := range ok {
		if err := CheckBundle([]byte(s)); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	bad := map[string]string{
		"empty":        "",
		"no end":       "# v2 git bundle\n" + sha1 + " HEAD\n",
		"not a bundle": "PACK\n\n",
		"v4":           "# v4 git bundle\n" + sha1 + " HEAD\n\n",
		"no ref":       "# v2 git bundle\n-" + sha1 + " x\n\n",
		"other ref":    "# v2 git bundle\n" + sha1 + " refs/heads/main\n\n",
		"two refs":     "# v2 git bundle\n" + sha1 + " HEAD\n" + sha1 + " HEAD\n\n",
		"short oid":    "# v2 git bundle\nabc HEAD\n\n",
		"upper oid":    "# v2 git bundle\n" + strings.Repeat("A", 40) + " HEAD\n\n",
		"filter":       "# v3 git bundle\n@filter=blob:none\n" + sha1 + " HEAD\n\n",
		"bad prereq":   "# v2 git bundle\n-xyz s\n" + sha1 + " HEAD\n\n",
		"option":       "# v2 git bundle\n--upload-pack=x HEAD\n\n",
	}
	for name, s := range bad {
		if err := CheckBundle([]byte(s)); !errors.Is(err, ErrBundle) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
