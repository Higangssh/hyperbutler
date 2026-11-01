package desktopimport

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeContext(t *testing.T, dir, meta string) {
	t.Helper()
	sum := sha256.Sum256([]byte(DesktopContext))
	d := filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
}

type entry struct {
	name, link, content string
	typ                 byte
	mode                int64
}

func mkTar(t testing.TB, es ...entry) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range es {
		h := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: e.typ, Mode: e.mode, Size: int64(len(e.content))}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

var root = entry{name: "v/", typ: tar.TypeDir, mode: 0o755}

func TestHashArchive(t *testing.T) {
	a := mkTar(t, root, entry{name: "v/a", typ: tar.TypeReg, mode: 0o644, content: "hello"},
		entry{name: "v/l", typ: tar.TypeSymlink, link: "/etc/passwd"}, entry{name: "v/h", typ: tar.TypeLink, link: "v/a"})
	s1, err := hashArchive(bytes.NewReader(a), nil)
	if err != nil {
		t.Fatal(err)
	}
	if s1.entries != 4 || s1.bytes != 5 {
		t.Errorf("entries %d bytes %d", s1.entries, s1.bytes)
	}
	// Passing it on gives an archive that hashes the same.
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	s2, err := hashArchive(bytes.NewReader(a), tw)
	if err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	s3, err := hashArchive(&out, nil)
	if err != nil || s1.Sum() != s2.Sum() || s2.Sum() != s3.Sum() {
		t.Fatalf("%v %v %v %v", s1.Sum(), s2.Sum(), s3, err)
	}
	// Content, mode, name and link target all count.
	for _, b := range [][]byte{
		mkTar(t, root, entry{name: "v/a", typ: tar.TypeReg, mode: 0o644, content: "hellO"}, entry{name: "v/l", typ: tar.TypeSymlink, link: "/etc/passwd"}, entry{name: "v/h", typ: tar.TypeLink, link: "v/a"}),
		mkTar(t, root, entry{name: "v/a", typ: tar.TypeReg, mode: 0o600, content: "hello"}, entry{name: "v/l", typ: tar.TypeSymlink, link: "/etc/passwd"}, entry{name: "v/h", typ: tar.TypeLink, link: "v/a"}),
		mkTar(t, root, entry{name: "v/b", typ: tar.TypeReg, mode: 0o644, content: "hello"}, entry{name: "v/l", typ: tar.TypeSymlink, link: "/etc/passwd"}, entry{name: "v/h", typ: tar.TypeLink, link: "v/b"}),
		mkTar(t, root, entry{name: "v/a", typ: tar.TypeReg, mode: 0o644, content: "hello"}, entry{name: "v/l", typ: tar.TypeSymlink, link: "/etc/shadow"}, entry{name: "v/h", typ: tar.TypeLink, link: "v/a"}),
	} {
		s, err := hashArchive(bytes.NewReader(b), nil)
		if err != nil || s.Sum() == s1.Sum() {
			t.Errorf("a change hashes the same (%v)", err)
		}
	}
}

func TestHashArchiveRefuses(t *testing.T) {
	for name, a := range map[string][]byte{
		"traversal":      mkTar(t, root, entry{name: "v/../etc/passwd", typ: tar.TypeReg, content: "x"}),
		"absolute":       mkTar(t, root, entry{name: "/v/a", typ: tar.TypeReg}),
		"outside":        mkTar(t, root, entry{name: "etc/passwd", typ: tar.TypeReg}),
		"dot":            mkTar(t, root, entry{name: "v/./a", typ: tar.TypeReg}),
		"backslash":      mkTar(t, root, entry{name: `v/..\..\a`, typ: tar.TypeReg}),
		"hardlink out":   mkTar(t, root, entry{name: "v/h", typ: tar.TypeLink, link: "etc/shadow"}),
		"hardlink up":    mkTar(t, root, entry{name: "v/h", typ: tar.TypeLink, link: "v/../../x"}),
		"no root":        mkTar(t, entry{name: "v/a", typ: tar.TypeReg}),
		"root is a file": mkTar(t, entry{name: "v", typ: tar.TypeReg}),
		"two roots":      mkTar(t, root, root),
		"empty":          mkTar(t),
		"not a tar":      []byte(strings.Repeat("garbage!", 200)),
		"cut short":      mkTar(t, root, entry{name: "v/a", typ: tar.TypeReg, content: strings.Repeat("x", 2000)})[:1500],
		"type":           mkTar(t, root, entry{name: "v/x", typ: 'Z'}),
	} {
		if _, err := hashArchive(bytes.NewReader(a), nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := checkEntryName("v/../x"); !errors.Is(err, errBadArchive) {
		t.Errorf("%v", err)
	}
}

func FuzzHashArchive(f *testing.F) {
	f.Add(mkTar(f, root, entry{name: "v/a", typ: tar.TypeReg, mode: 0o644, content: "hello"}))
	f.Add(mkTar(f, root, entry{name: "v/../a", typ: tar.TypeReg}))
	f.Add(mkTar(f, root, entry{name: "v/h", typ: tar.TypeLink, link: "v/a"}))
	f.Add([]byte("not a tar"))
	f.Fuzz(func(t *testing.T, b []byte) {
		s1, err := hashArchive(bytes.NewReader(b), nil)
		if err != nil {
			return
		}
		// An accepted archive passed on hashes the same and is accepted again.
		var out bytes.Buffer
		tw := tar.NewWriter(&out)
		s2, err := hashArchive(bytes.NewReader(b), tw)
		if err != nil {
			t.Fatalf("second pass: %v", err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		s3, err := hashArchive(&out, nil)
		if err != nil {
			t.Fatalf("passed-on archive refused: %v", err)
		}
		if s1.Sum() != s2.Sum() || s2.Sum() != s3.Sum() {
			t.Fatalf("hashes differ: %s %s %s", s1.Sum(), s2.Sum(), s3.Sum())
		}
	})
}

func TestParseLoadAnswer(t *testing.T) {
	got, err := parseLoadAnswer(strings.NewReader(`{"stream":"Loaded image: busybox:latest\n"}` + "\n" + `{"stream":"Loaded image ID: sha256:abc\n"}`))
	if err != nil || len(got) != 2 || got[0] != "busybox:latest" || got[1] != "sha256:abc" {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{
		`{"errorDetail":{"message":"unexpected EOF"},"error":"unexpected EOF"}`,
		`{"error":"no space left on device"}`,
		`{"stream":"Loaded`,
		`<html>`,
		`{"stream":"` + strings.Repeat("x", loadAnswerMax+10) + `"}`,
	} {
		if _, err := parseLoadAnswer(strings.NewReader(bad)); err == nil {
			t.Errorf("accepted %.40q", bad)
		}
	}
	if _, err := parseLoadAnswer(strings.NewReader("")); err != nil {
		t.Errorf("empty answer: %v", err)
	}
}

func FuzzParseLoadAnswer(f *testing.F) {
	f.Add(`{"stream":"Loaded image: busybox:latest\n"}`)
	f.Add(`{"errorDetail":{"message":"x\u001b[2J"}}`)
	f.Add(`{"stream":"Loaded image ID: sha256:abc\nLoaded image: a:b\n"}{"stream":""}`)
	f.Fuzz(func(t *testing.T, s string) {
		got, err := parseLoadAnswer(strings.NewReader(s))
		for _, g := range got {
			if strings.ContainsAny(g, "\n\r\x1b") || len(g) > 256 {
				t.Fatalf("unclean reference %q", g)
			}
		}
		if err != nil && strings.ContainsAny(err.Error(), "\x1b\r") {
			t.Fatalf("unclean error %q", err)
		}
	})
}

func FuzzPipeFromHost(f *testing.F) {
	f.Add("npipe:////./pipe/dockerDesktopLinuxEngine")
	f.Add("npipe:////server/pipe/x")
	f.Add("tcp://127.0.0.1:2375")
	f.Fuzz(func(t *testing.T, host string) {
		p, err := pipeFromHost(host)
		if err != nil {
			return
		}
		if !strings.HasPrefix(p, `\\.\pipe\`) || strings.Contains(p, "/") {
			t.Fatalf("%q gave %q", host, p)
		}
	})
}
