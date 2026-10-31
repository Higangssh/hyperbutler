package cpio

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	h    Header
	data string
}

func writeAll(t testing.TB, ents []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := NewWriter(&buf)
	for _, e := range ents {
		h := e.h
		h.Size = uint32(len(e.data))
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func readAll(r io.Reader) ([]entry, error) {
	cr := NewReader(r)
	var out []entry
	for {
		h, err := cr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		d, err := io.ReadAll(cr)
		if err != nil {
			return out, err
		}
		out = append(out, entry{*h, string(d)})
	}
}

func TestRoundTrip(t *testing.T) {
	ents := []entry{
		{Header{Name: "bin", Mode: TypeDir | 0o755}, ""},
		{Header{Name: "bin/a", Mode: TypeRegular | 0o755, Mtime: 7}, "x"},
		{Header{Name: "bin/bb", Mode: TypeRegular | 0o644}, "four"},
		{Header{Name: "bin/sh", Mode: TypeSymlink | 0o777}, "busybox"},
		{Header{Name: "dev/console", Mode: TypeChar | 0o600, RdevMajor: 5, RdevMinor: 1}, ""},
		{Header{Name: "e", Mode: TypeRegular | 0o600}, ""},
	}
	b := writeAll(t, ents)
	if len(b)%512 != 0 {
		t.Fatalf("archive length %d not a multiple of 512", len(b))
	}
	got, err := readAll(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(ents) {
		t.Fatalf("got %d entries, want %d", len(got), len(ents))
	}
	for i, g := range got {
		w := ents[i]
		if g.h.Name != w.h.Name || g.h.Mode != w.h.Mode || g.data != w.data ||
			g.h.RdevMajor != w.h.RdevMajor || g.h.RdevMinor != w.h.RdevMinor || g.h.Mtime != w.h.Mtime {
			t.Errorf("entry %d: got %+v %q, want %+v %q", i, g.h, g.data, w.h, w.data)
		}
		if g.h.Ino != uint32(i+1) {
			t.Errorf("entry %d: ino %d, want sequential %d", i, g.h.Ino, i+1)
		}
	}
}

// TestKnownBytes pins the exact encoding of one entry, so a change in layout or padding
// is caught even if reader and writer drift together.
func TestKnownBytes(t *testing.T) {
	b := writeAll(t, []entry{{Header{Name: "a", Mode: TypeRegular | 0o644}, "hi"}})
	want := "070701" + "00000001" + "000081A4" + "00000000" + "00000000" + "00000001" +
		"00000000" + "00000002" + "00000000" + "00000000" + "00000000" + "00000000" +
		"00000002" + "00000000" + "a\x00" + "hi\x00\x00"
	if !strings.HasPrefix(string(b), want) {
		t.Fatalf("got %q\nwant prefix %q", b[:len(want)], want)
	}
	if string(b[len(want):len(want)+6]) != magic {
		t.Fatalf("trailer header does not follow at a 4-byte boundary")
	}
}

func TestWriterErrors(t *testing.T) {
	w := NewWriter(io.Discard)
	if err := w.WriteHeader(Header{Name: "a", Mode: TypeRegular, Size: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("abcd")); err == nil {
		t.Error("overlong write accepted")
	}
	if err := w.WriteHeader(Header{Name: "b"}); err == nil {
		t.Error("header accepted while data was still owed")
	}
	if err := NewWriter(io.Discard).WriteHeader(Header{Name: "a\x00b"}); err == nil {
		t.Error("NUL in name accepted")
	}
	if err := NewWriter(io.Discard).WriteHeader(Header{}); err == nil {
		t.Error("empty name accepted")
	}
	for _, h := range []Header{
		{Name: "TRAILER!!!", Mode: TypeRegular},
		{Name: "l", Mode: TypeSymlink | 0o777, Size: 0},
		{Name: "l", Mode: TypeSymlink | 0o777, Size: 4096},
	} {
		if err := NewWriter(io.Discard).WriteHeader(h); err == nil {
			t.Errorf("%+v accepted", h)
		}
	}
}

func TestConcatenated(t *testing.T) {
	a := writeAll(t, []entry{{Header{Name: "a", Mode: TypeRegular | 0o644}, "1"}})
	b := writeAll(t, []entry{{Header{Name: "b", Mode: TypeRegular | 0o644}, "22"}})
	both := append(append(append([]byte{}, a...), make([]byte, 16)...), b...)

	r := NewReader(bytes.NewReader(both))
	r.Concatenated = true
	var names []string
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		d, _ := io.ReadAll(r)
		names = append(names, h.Name+"="+string(d))
	}
	if got := strings.Join(names, " "); got != "a=1 b=22" {
		t.Fatalf("got %q", got)
	}
	// Without Concatenated, reading stops at the first trailer.
	got, err := readAll(bytes.NewReader(both))
	if err != nil || len(got) != 1 {
		t.Fatalf("plain reader: %d entries, err %v", len(got), err)
	}
}

func TestReaderRejectsTruncated(t *testing.T) {
	b := writeAll(t, []entry{{Header{Name: "a", Mode: TypeRegular | 0o644}, "hello world"}})
	for _, n := range []int{0, 5, 50, 112, 116} {
		_, err := readAll(bytes.NewReader(b[:n]))
		if !errors.Is(err, ErrFormat) {
			t.Errorf("len %d: err = %v, want ErrFormat", n, err)
		}
	}
}

func FuzzReader(f *testing.F) {
	f.Add(writeAll(f, []entry{{Header{Name: "a", Mode: TypeRegular | 0o644}, "hi"}}))
	f.Add([]byte("070701"))
	f.Fuzz(func(t *testing.T, b []byte) {
		ents, err := readAll(bytes.NewReader(b))
		if err != nil {
			return
		}
		// Whatever parses (and the writer accepts) must parse back identically.
		var buf bytes.Buffer
		w := NewWriter(&buf)
		for _, e := range ents {
			h := e.h
			h.Size = uint32(len(e.data))
			if w.WriteHeader(h) != nil {
				return
			}
			w.Write([]byte(e.data))
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		again, err := readAll(&buf)
		if err != nil {
			t.Fatalf("rewrite does not parse: %v", err)
		}
		if len(again) != len(ents) {
			t.Fatalf("rewrite has %d entries, want %d", len(again), len(ents))
		}
		for i := range ents {
			if again[i].h.Name != ents[i].h.Name || again[i].data != ents[i].data {
				t.Fatalf("entry %d changed on rewrite", i)
			}
		}
	})
}

func TestSpecBuild(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "init"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "mods", "kernel"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"b.ko", "a.ko"} {
		if err := os.WriteFile(filepath.Join(dir, "mods", "kernel", n), []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	spec := `
# test
file /init ${SRC}/init 0755
slink bin/sh busybox
nod dev/console 0600 c 5 1
tree lib/modules ${SRC}/mods 0755 0644
dir var/lib/docker 0710 0 0
`
	s, err := ParseSpec(strings.NewReader(spec), map[string]string{"SRC": dir})
	if err != nil {
		t.Fatal(err)
	}
	build := func() []byte {
		var buf bytes.Buffer
		w := NewWriter(&buf)
		if err := s.Build(w, ".", 42); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	b := build()
	if !bytes.Equal(b, build()) {
		t.Fatal("build is not deterministic")
	}
	got, err := readAll(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range got {
		names = append(names, e.h.Name)
		if e.h.Mtime != 42 {
			t.Errorf("%s: mtime %d", e.h.Name, e.h.Mtime)
		}
	}
	want := "init bin bin/sh dev dev/console lib lib/modules lib/modules/kernel " +
		"lib/modules/kernel/a.ko lib/modules/kernel/b.ko var var/lib var/lib/docker"
	if strings.Join(names, " ") != want {
		t.Fatalf("entries:\n got %s\nwant %s", strings.Join(names, " "), want)
	}
	if got[0].h.Mode != TypeRegular|0o755 || got[0].data != "#!/bin/sh\n" {
		t.Errorf("init: %+v %q", got[0].h, got[0].data)
	}
	if got[2].data != "busybox" {
		t.Errorf("symlink target %q", got[2].data)
	}
	if got[len(got)-1].h.Mode != TypeDir|0o710 {
		t.Errorf("explicit dir mode %o", got[len(got)-1].h.Mode)
	}
}

func TestParseSpecErrors(t *testing.T) {
	for _, s := range []string{
		"bogus a",
		"file a",
		"file a b 0999",
		"file a ${NOPE} 0644",
		"dir .. 0755",
		"dir ../x 0755",
		"nod a 0600 x 1 1",
		"dir a 0755 0",
		"tree a b 0755",
		"dir / 0755",
		`dir etc\ssl 0755`,
	} {
		if _, err := ParseSpec(strings.NewReader(s), nil); err == nil {
			t.Errorf("%q: accepted", s)
		}
	}
}

func TestBuildRejectsConflicts(t *testing.T) {
	for _, spec := range []string{
		"slink a b\nslink a c\n",        // duplicate non-directory
		"slink a b\ndir a 0755\n",       // file, then directory
		"dir a 0755\nslink a b\n",       // directory, then file
		"slink a b\nslink a/c d\n",      // parent is not a directory
		"slink a/b c\ndir a 0700 5 5\n", // implicit parent, then a different explicit one
	} {
		s, err := ParseSpec(strings.NewReader(spec), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Build(NewWriter(io.Discard), ".", 0); err == nil {
			t.Errorf("%q accepted", spec)
		}
	}
	// Repeating an identical directory is fine.
	s, err := ParseSpec(strings.NewReader("dir a 0755\nslink a/b c\ndir a 0755\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Build(NewWriter(io.Discard), ".", 0); err != nil {
		t.Errorf("identical directory rejected: %v", err)
	}
}

func FuzzParseSpec(f *testing.F) {
	f.Add("file /init ${X}/init 0755\nslink bin/sh busybox\nnod dev/null 0666 c 1 3 0 0\n")
	f.Add("tree lib/modules ${X} 0755 0644\ndir a/../b 0700 1 1\n")
	f.Fuzz(func(t *testing.T, s string) {
		spec, err := ParseSpec(strings.NewReader(s), map[string]string{"X": "x"})
		if err != nil {
			return
		}
		for _, l := range spec.Lines {
			if l.Name == "" || l.Name == "." || strings.HasPrefix(l.Name, "/") || strings.Contains(l.Name, `\`) ||
				l.Name == ".." || strings.HasPrefix(l.Name, "../") {
				t.Fatalf("unsafe name %q accepted", l.Name)
			}
			if l.Mode > 0o7777 || l.DirMode > 0o7777 {
				t.Fatalf("mode %o out of range", l.Mode)
			}
		}
	})
}
