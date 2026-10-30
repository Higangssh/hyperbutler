package share

import (
	"encoding/binary"
	"fmt"
	"testing"
)

func readdirShare(t *testing.T, opt Options, files int) (*Server, *tclient, *memFS, uint64) {
	t.Helper()
	m := newMemFS()
	for _, d := range []string{"a", "b", "big"} {
		m.files[d+"/"] = nil
	}
	for i := 0; i < files; i++ {
		m.files[fmt.Sprintf("a/file-with-a-longish-name-%04d", i)] = nil
		m.files[fmt.Sprintf("b/file-with-a-longish-name-%04d", i)] = nil
	}
	for i := 0; i < 2*files; i++ {
		m.files[fmt.Sprintf("big/f%04d", i)] = nil
	}
	opt.NoWatch = true
	s := NewServer(opt)
	if err := s.AddShare(&Share{Name: "s", FS: m}); err != nil {
		t.Fatal(err)
	}
	c := newClient(t, s)
	return s, c, m, c.mustLookup(rootNode, "s").node
}

// readChunk reads one READDIR (fh 0) at off and returns the names and the next offset.
func (c *tclient) readChunk(node, off uint64) (int, []string, uint64) {
	e, b := c.do(opReaddir, node, (&wbuf{}).u64(0).u64(off).u32(4096).u32(0).u64(0).u32(0).u32(0).b)
	if e != 0 {
		return e, nil, off
	}
	var names []string
	le := binary.LittleEndian
	for len(b) > 0 {
		nameLen := int(le.Uint32(b[16:]))
		off = le.Uint64(b[8:])
		names = append(names, string(b[24:24+nameLen]))
		n := 24 + nameLen
		n += (8 - n%8) % 8
		b = b[n:]
	}
	return 0, names, off
}

func TestReaddirWithoutHandles(t *testing.T) {
	s, c, _, sh := readdirShare(t, Options{}, 300)
	if e, _ := c.do(opOpendir, sh, (&wbuf{}).u32(0).u32(0).b); e != int(ENOSYS) {
		t.Fatalf("OPENDIR: errno %d, want ENOSYS (no directory handles)", e)
	}
	a := c.mustLookup(sh, "a").node
	for _, plus := range []bool{false, true} {
		e, list := c.readdir(a, plus)
		if e != 0 || len(list) != 302 {
			t.Fatalf("plus=%v: errno %d, %d entries, want 300 + . and ..", plus, e, len(list))
		}
	}
	s.mu.Lock()
	held, listed := s.dirEntries, s.nodes[a].listed
	s.mu.Unlock()
	if held != 0 || listed {
		t.Fatalf("a finished listing is still held: %d entries, listed %v", held, listed)
	}
	f := c.mustLookup(a, "file-with-a-longish-name-0000").node
	if e, _, _ := c.readChunk(f, 0); e != int(ENOTDIR) {
		t.Fatalf("READDIR of a file: errno %d, want ENOTDIR", e)
	}
	// The root is listed without a handle too.
	if e, list := c.readdir(rootNode, false); e != 0 || len(list) != 3 {
		t.Fatalf("root: errno %d, %v", e, list)
	}
}

// A READDIR that continues a listing reads the snapshot its offset-0 READDIR took, as a
// directory handle did; the next listing sees the change.
func TestReaddirContinuesSnapshot(t *testing.T) {
	_, c, m, sh := readdirShare(t, Options{}, 300)
	a := c.mustLookup(sh, "a").node
	e, first, off := c.readChunk(a, 0)
	if e != 0 || len(first) == 0 || len(first) >= 302 {
		t.Fatalf("first chunk: errno %d, %d names", e, len(first))
	}
	m.mu.Lock()
	m.files["a/zz-added"] = nil
	m.mu.Unlock()
	seen := map[string]bool{}
	for _, n := range first {
		seen[n] = true
	}
	for {
		e, names, next := c.readChunk(a, off)
		if e != 0 {
			t.Fatalf("errno %d", e)
		}
		if len(names) == 0 {
			break
		}
		for _, n := range names {
			if seen[n] {
				t.Fatalf("%q listed twice", n)
			}
			seen[n] = true
		}
		off = next
	}
	if len(seen) != 302 || seen["zz-added"] {
		t.Fatalf("%d names, added name seen %v: the continuation did not use the snapshot", len(seen), seen["zz-added"])
	}
	if _, list := c.readdir(a, false); len(list) != 303 {
		t.Fatalf("a new listing has %d entries, want 303", len(list))
	}
}

// Snapshots of listings nobody finished are dropped when the budget runs out, and a
// READDIR that continues one reads the directory again.
func TestReaddirBudget(t *testing.T) {
	old := maxDirBudget
	maxDirBudget = 500
	defer func() { maxDirBudget = old }()
	s, c, _, sh := readdirShare(t, Options{}, 300)
	a := c.mustLookup(sh, "a").node
	b := c.mustLookup(sh, "b").node
	_, _, offA := c.readChunk(a, 0)
	if e, _, _ := c.readChunk(b, 0); e != 0 {
		t.Fatalf("b: errno %d", e)
	}
	s.mu.Lock()
	aListed, held := s.nodes[a].listed, s.dirEntries
	s.mu.Unlock()
	if aListed || held != 300 {
		t.Fatalf("a's abandoned snapshot kept (listed %v, %d entries held)", aListed, held)
	}
	if e, names, _ := c.readChunk(a, offA); e != 0 || len(names) == 0 {
		t.Fatalf("continuing a after eviction: errno %d, %d names", e, len(names))
	}
	big := c.mustLookup(sh, "big").node
	if e, _, _ := c.readChunk(big, 0); e != int(ENOMEM) {
		t.Fatalf("a listing over the budget: errno %d, want ENOMEM", e)
	}
}

// Options.NoCache keeps directory handles (and so no guest readdir cache).
func TestReaddirNoCacheUsesHandles(t *testing.T) {
	_, c, _, sh := readdirShare(t, Options{NoCache: true}, 10)
	e, b := c.do(opOpendir, sh, (&wbuf{}).u32(0).u32(0).b)
	if e != 0 || binary.LittleEndian.Uint64(b) == 0 || binary.LittleEndian.Uint32(b[8:])&fopenCacheDir != 0 {
		t.Fatalf("OPENDIR with NoCache: errno %d, %x", e, b)
	}
	if e, list := c.readdir(c.mustLookup(sh, "a").node, true); e != 0 || len(list) != 12 {
		t.Fatalf("errno %d, %d entries", e, len(list))
	}
}
