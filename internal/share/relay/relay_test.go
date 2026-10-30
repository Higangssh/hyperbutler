package relay

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/Higangssh/winbox/internal/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

func TestSafeJoin(t *testing.T) {
	for _, bad := range []string{"", "/etc", "a/../b", "a//b", ".", "a/./b", "..", "a\x00b"} {
		if _, err := safeJoin("/mnt/winbox", bad); err == nil {
			t.Errorf("safeJoin accepted %q", bad)
		}
	}
	if p, err := safeJoin("/mnt/winbox", "src/a b/c"); err != nil || p != "/mnt/winbox/src/a b/c" {
		t.Fatalf("%q %v", p, err)
	}
}

// FuzzReadFrame decodes host frames; a frame must never exceed its buffer.
func FuzzReadFrame(f *testing.F) {
	ok := make([]byte, 20)
	binary.LittleEndian.PutUint32(ok, 20)
	f.Add(ok)
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, b []byte) {
		buf := make([]byte, maxHostFrame)
		r := bytes.NewReader(b)
		for {
			fr, err := readFrame(r, buf)
			if err != nil {
				return
			}
			if len(fr) < outHeaderLen || int(binary.LittleEndian.Uint32(fr)) != len(fr) {
				t.Fatal("bad frame accepted")
			}
		}
	})
}

// FuzzParseInject decodes replay requests from the host.
func FuzzParseInject(f *testing.F) {
	f.Add([]byte{4, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 'a', 1, 0, 0, 0, 'b'})
	f.Add([]byte{1, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0x7f})
	f.Fuzz(func(t *testing.T, b []byte) {
		fr, ok := parseInject(b)
		if ok && (len(fr.path) > 4096 || len(fr.newPath) > 4096 || len(fr.path)+len(fr.newPath)+16 > len(b)) {
			t.Fatal("parseInject accepted more than the frame holds")
		}
	})
}

// FuzzSafeJoin: an accepted path never escapes the mount point.
func FuzzSafeJoin(f *testing.F) {
	f.Add("src/a")
	f.Add("../x")
	f.Fuzz(func(t *testing.T, p string) {
		j, err := safeJoin("/mnt/w", p)
		if err != nil {
			return
		}
		for _, c := range strings.Split(strings.TrimPrefix(j, "/mnt/w/"), "/") {
			if c == ".." || c == "." || c == "" {
				t.Fatalf("safeJoin(%q) = %q", p, j)
			}
		}
	})
}
