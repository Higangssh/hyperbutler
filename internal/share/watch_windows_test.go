//go:build windows

package share

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatcherDeliversReplays(t *testing.T) {
	dir := testDir(t)
	os.WriteFile(filepath.Join(dir, "edit.txt"), []byte("v0"), 0o644)
	s, c, root := newTestServer(t, dir, Options{}, nil)
	c.mustLookup(root, "edit.txt")
	c.do(opHello, 0, (&wbuf{}).u32(4242).b)
	got := make(chan []byte, 256)
	s.cmu.Lock()
	s.conn = &conn{rw: chanWriter(got)}
	s.cmu.Unlock()
	wait := func(kind int, path string) time.Duration {
		t.Helper()
		t0 := time.Now()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case m := <-got:
				if IsInject(m) {
					if f, ok := ParseInject(m[outHeaderLen:]); ok && f.Kind == kind && f.Path == path {
						return time.Since(t0)
					}
				}
			case <-deadline:
				t.Fatalf("no replay of kind %d for %s", kind, path)
			}
		}
	}
	os.WriteFile(filepath.Join(dir, "edit.txt"), []byte("v1"), 0o644)
	t.Logf("modify replay after %v", wait(injModify, "src/edit.txt"))
	os.WriteFile(filepath.Join(dir, "created.txt"), []byte("x"), 0o644)
	wait(injCreate, "src/created.txt")
	os.Remove(filepath.Join(dir, "created.txt"))
	wait(injDelete, "src/created.txt")
	os.Rename(filepath.Join(dir, "edit.txt"), filepath.Join(dir, "renamed.txt"))
	wait(injRename, "src/edit.txt")
}

func TestParseNotifyPairsRenames(t *testing.T) {
	var got []string
	rec := func(k EventKind, p, np []string) {
		got = append(got, string(rune('0'+k))+":"+joinPath("", p)+">"+joinPath("", np))
	}
	b := notifyRecord(4, `a\old`, false)
	b = append(b, notifyRecord(5, `a\new`, false)...)
	b = append(b, notifyRecord(3, `x`, false)...)
	b = append(b, notifyRecord(3, `x`, true)...)
	parseNotify(b, rec)
	want := []string{"3:/a/old>/a/new", "0:/x>"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %q want %q", got, want)
	}
}

func notifyRecord(action uint32, name string, last bool) []byte {
	u := []uint16{}
	for _, r := range name {
		u = append(u, uint16(r))
	}
	n := 12 + 2*len(u)
	n += (4 - n%4) % 4
	b := make([]byte, n)
	if !last {
		putU32(b[0:], uint32(n))
	}
	putU32(b[4:], action)
	putU32(b[8:], uint32(2*len(u)))
	for i, c := range u {
		b[12+2*i] = byte(c)
		b[13+2*i] = byte(c >> 8)
	}
	return b
}

func putU32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

type chanWriter chan []byte

func (c chanWriter) Read([]byte) (int, error) { select {} }
func (c chanWriter) Write(p []byte) (int, error) {
	select {
	case c <- bytes.Clone(p):
	default:
	}
	return len(p), nil
}
