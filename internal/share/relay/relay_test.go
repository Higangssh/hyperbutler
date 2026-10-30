package relay

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

func TestSafeJoin(t *testing.T) {
	for _, bad := range []string{"", "/etc", "a/../b", "a//b", ".", "a/./b", "..", "a\x00b"} {
		if _, err := safeJoin("/mnt/hyperbutler", bad); err == nil {
			t.Errorf("safeJoin accepted %q", bad)
		}
	}
	if p, err := safeJoin("/mnt/hyperbutler", "src/a b/c"); err != nil || p != "/mnt/hyperbutler/src/a b/c" {
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

func TestMountDev(t *testing.T) {
	info := "22 1 0:21 / /proc rw - proc proc rw\n" +
		"40 22 0:28 / /mnt/hyperbutler rw,nosuid - fuse.hyperbutler hyperbutler rw\n" +
		"41 22 0:31 / /mnt/a\\040b rw - fuse x rw\n" +
		"42 22 0:300 / /mnt/hyperbutler rw - fuse.hyperbutler hyperbutler rw\n"
	if maj, min, ok := mountDev([]byte(info), "/mnt/hyperbutler"); !ok || maj != 0 || min != 300 {
		t.Fatalf("%d:%d %v, want the last mount 0:300", maj, min, ok)
	}
	if _, min, ok := mountDev([]byte(info), "/mnt/a b"); !ok || min != 31 {
		t.Fatalf("escaped mount point: %d %v", min, ok)
	}
	if _, _, ok := mountDev([]byte(info), "/nope"); ok {
		t.Fatal("found a mount that is not there")
	}
}

// FuzzMountDev parses mountinfo text; it must never panic.
func FuzzMountDev(f *testing.F) {
	f.Add([]byte("40 22 0:28 / /mnt/hyperbutler rw - fuse x rw\n"), "/mnt/hyperbutler")
	f.Add([]byte("1 2 3 4 /x\\04"), "/x\\04")
	f.Fuzz(func(t *testing.T, b []byte, mp string) {
		mountDev(b, mp)
		unescapeMount(mp)
	})
}
