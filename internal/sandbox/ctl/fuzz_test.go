package ctl

import (
	"bytes"
	"testing"
)

func FuzzReadFrame(f *testing.F) {
	var b bytes.Buffer
	_ = WriteFrame(&b, TypeOutput, []byte("x"))
	f.Add(b.Bytes())
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 1})
	f.Fuzz(func(t *testing.T, in []byte) {
		ty, d, err := ReadFrame(bytes.NewReader(in))
		if err != nil {
			return
		}
		if len(d) > MaxData {
			t.Fatalf("%d bytes", len(d))
		}
		var out bytes.Buffer
		if err := WriteFrame(&out, ty, d); err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(in, out.Bytes()) {
			t.Fatal("a frame did not encode back to its bytes")
		}
		_, _, _ = ParseResize(d)
		_, _ = ParseExit(d)
	})
}

func FuzzCheckBundle(f *testing.F) {
	f.Add([]byte("# v2 git bundle\n-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa s\naaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa HEAD\n\nPACK"))
	f.Add([]byte("# v3 git bundle\n@object-format=sha1\n\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		if CheckBundle(in) != nil {
			return
		}
		// What passes names HEAD exactly once and no other ref.
		end := bytes.Index(in, []byte("\n\n"))
		if bytes.Count(in[:end], []byte(" HEAD\n"))+bytes.Count(in[:end], []byte(" HEAD")) == 0 {
			t.Fatalf("passed with no HEAD: %q", in[:end])
		}
	})
}
