package share

// Tests for opens without round trips (openfile.go) and for host changes that race a
// LOOKUP or listing (inject.go, noteChangeLocked).

import (
	"encoding/binary"
	"sync"
	"testing"
	"time"
)

// hookFS is a memFS with stable inode numbers, counted opens and closes, and a hook
// that runs inside Lstat and ReadDir (after the host was read), to race host events
// against a request.
type hookFS struct {
	*memFS
	imu    sync.Mutex
	inos   map[string]uint64
	opens  int
	closes int
	hook   func(path []string) // called after reading the host
}

func newHookFS() *hookFS { return &hookFS{memFS: newMemFS(), inos: map[string]uint64{}} }

func (h *hookFS) ino(k string) uint64 {
	h.imu.Lock()
	defer h.imu.Unlock()
	if h.inos[k] == 0 {
		h.inos[k] = uint64(len(h.inos) + 100)
	}
	return h.inos[k]
}

func (h *hookFS) Lstat(p []string) (Attr, error) {
	a, err := h.memFS.Lstat(p)
	if err == nil {
		a.Ino = h.ino(key(p))
	}
	if h.hook != nil {
		h.hook(p)
	}
	return a, err
}

func (h *hookFS) ReadDir(p []string) ([]DirEntry, error) {
	es, err := h.memFS.ReadDir(p)
	for i := range es {
		es[i].Attr.Ino = h.ino(key(append(append([]string(nil), p...), es[i].Name)))
	}
	if h.hook != nil {
		h.hook(p)
	}
	return es, err
}

type hookFile struct {
	File
	h *hookFS
}

func (f *hookFile) Close() error {
	f.h.imu.Lock()
	f.h.closes++
	f.h.imu.Unlock()
	return f.File.Close()
}

func (h *hookFS) Open(p []string, flags uint32) (File, error) {
	f, err := h.memFS.Open(p, flags)
	if err != nil {
		return nil, err
	}
	h.imu.Lock()
	h.opens++
	h.imu.Unlock()
	return &hookFile{File: f, h: h}, nil
}

func (h *hookFS) counts() (int, int) {
	h.imu.Lock()
	defer h.imu.Unlock()
	return h.opens, h.closes
}

// syncCapture records frames written from any goroutine.
type syncCapture struct {
	mu  sync.Mutex
	out [][]byte
}

func (c *syncCapture) Read([]byte) (int, error) { select {} }
func (c *syncCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.out = append(c.out, append([]byte(nil), p...))
	c.mu.Unlock()
	return len(p), nil
}
func (c *syncCapture) frames() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.out...)
}

func hookShare(t *testing.T, opt Options) (*Server, *tclient, *hookFS, uint64) {
	t.Helper()
	fs := newHookFS()
	fs.files["f"] = []byte("hello")
	fs.files["g"] = []byte("world")
	opt.NoWatch = true
	s := NewServer(opt)
	if err := s.AddShare(&Share{Name: "src", FS: fs}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := newClient(t, s)
	return s, c, fs, c.mustLookup(rootNode, "src").node
}

func initFlags(t *testing.T, s *Server, kernelFlags uint32) uint32 {
	t.Helper()
	c := &tclient{t: t, s: s, pid: 100}
	body := (&wbuf{}).u32(7).u32(44).u32(1 << 20).u32(kernelFlags).u32(0).raw(make([]byte, 44)).b
	e, b := c.do(opInit, 0, body)
	if e != 0 {
		t.Fatalf("INIT: %d", e)
	}
	return binary.LittleEndian.Uint32(b[12:])
}

func TestNoOpenNegotiation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		opt    Options
		kernel uint32
		noOpen bool
	}{
		{"default", Options{}, 0xffffffff, true},
		{"old kernel", Options{}, 0xffffffff &^ initNoOpenSupport, false},
		{"RealOpen", Options{RealOpen: true}, 0xffffffff, false},
		{"NoCache", Options{NoCache: true}, 0xffffffff, false},
		{"Writeback", Options{Writeback: true}, 0xffffffff, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c, _, root := hookShare(t, tc.opt)
			fl := initFlags(t, s, tc.kernel)
			// O_TRUNC is atomic only when OPEN does it.
			if got := fl&initAtomicOTrunc != 0; got == tc.noOpen {
				t.Fatalf("FUSE_ATOMIC_O_TRUNC %v with no-open %v", got, tc.noOpen)
			}
			f := c.mustLookup(root, "f")
			e, _ := c.do(opOpen, f.node, (&wbuf{}).u32(oRDONLY).u32(0).b)
			if (e == int(ENOSYS)) != tc.noOpen {
				t.Fatalf("OPEN: errno %d", e)
			}
			e, _ = c.do(opFlush, f.node, (&wbuf{}).u64(0).u32(0).u32(0).u64(0).b)
			if (e == int(ENOSYS)) != tc.noOpen {
				t.Fatalf("FLUSH: errno %d", e)
			}
			if !tc.noOpen {
				if e, _ := c.read(f.node, 0, 0, 10); e != int(EBADF) {
					t.Fatalf("READ fh 0 with OPEN in use: %d", e)
				}
			}
		})
	}
}

func TestNoOpenReadWriteByNode(t *testing.T) {
	s, c, fs, root := hookShare(t, Options{FileIdle: time.Hour})
	f := c.mustLookup(root, "f")
	for i := 0; i < 5; i++ {
		e, b := c.read(f.node, 0, 0, 100)
		if e != 0 || string(b) != "hello" {
			t.Fatalf("read: %d %q", e, b)
		}
	}
	if o, _ := fs.counts(); o != 1 {
		t.Fatalf("%d host opens for 5 reads", o)
	}
	// A write needs a write handle: the read one is replaced, then reused.
	for i := 0; i < 3; i++ {
		if e := c.write(f.node, 0, 5, []byte("!")); e != 0 {
			t.Fatalf("write: %d", e)
		}
	}
	if e, b := c.read(f.node, 0, 0, 100); e != 0 || string(b) != "hello!" {
		t.Fatalf("read back: %d %q", e, b)
	}
	if o, cl := fs.counts(); o != 2 || cl != 1 {
		t.Fatalf("opens %d closes %d, want 2 and 1", o, cl)
	}
	if e := c.fsync(f.node, 0); e != 0 {
		t.Fatalf("fsync: %d", e)
	}
	if n, _ := s.CachedFiles(); n != 1 {
		t.Fatalf("%d cached files", n)
	}
	// O_TRUNC as the kernel sends it without OPEN: a SETATTR inside open(2).
	if e, _ := c.open(f.node, oWRONLY|oTRUNC); e != 0 {
		t.Fatalf("open O_TRUNC: %d", e)
	}
	// Directories and unknown nodes have no file.
	if e, _ := c.read(root, 0, 0, 10); e != int(EISDIR) {
		t.Fatalf("read of a directory: %d", e)
	}
	if e, _ := c.read(9999, 0, 0, 10); e != int(ESTALE) {
		t.Fatalf("read of an unknown node: %d", e)
	}
	// FORGET closes the handle.
	c.do(opForget, f.node, (&wbuf{}).u64(1).b)
	if n, _ := s.CachedFiles(); n != 0 {
		t.Fatalf("%d cached files after FORGET", n)
	}
	if o, cl := fs.counts(); o != cl {
		t.Fatalf("opens %d closes %d", o, cl)
	}
}

func TestNoOpenIdleHandlesClose(t *testing.T) {
	s, c, fs, root := hookShare(t, Options{FileIdle: 20 * time.Millisecond})
	f := c.mustLookup(root, "f")
	if e, _ := c.read(f.node, 0, 0, 100); e != 0 {
		t.Fatal(e)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if n, _ := s.CachedFiles(); n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle handle never closed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if o, cl := fs.counts(); o != 1 || cl != 1 {
		t.Fatalf("opens %d closes %d", o, cl)
	}
	// The timer stops when nothing is cached.
	s.mu.Lock()
	armed := s.sweep != nil
	s.mu.Unlock()
	if armed {
		t.Fatal("sweep still armed with nothing cached")
	}
}

func TestNoOpenMaxFiles(t *testing.T) {
	s, c, fs, root := hookShare(t, Options{FileIdle: time.Hour, MaxFiles: 2})
	fs.files["h"] = []byte("x")
	var nodes []uint64
	for _, n := range []string{"f", "g", "h"} {
		en := c.mustLookup(root, n)
		nodes = append(nodes, en.node)
		if e, _ := c.read(en.node, 0, 0, 10); e != 0 {
			t.Fatal(e)
		}
		time.Sleep(time.Millisecond) // distinct use times
	}
	if n, _ := s.CachedFiles(); n != 2 {
		t.Fatalf("%d cached, want 2", n)
	}
	s.mu.Lock()
	_, first := s.files[nodes[0]]
	s.mu.Unlock()
	if first {
		t.Fatal("the least recently used handle was kept")
	}
	if _, cl := fs.counts(); cl != 1 {
		t.Fatalf("%d closes", cl)
	}
}

// A host removal closes the cached handle at once, but not under a request using it.
func TestNoOpenHostRemoveWaitsForInflight(t *testing.T) {
	s, c, _, root := hookShare(t, Options{FileIdle: time.Hour})
	f := c.mustLookup(root, "f")
	if e, _ := c.read(f.node, 0, 0, 10); e != 0 {
		t.Fatal(e)
	}
	file, done, err := s.nodeFileGet(f.node, false) // a READ in flight
	if err != nil {
		t.Fatal(err)
	}
	cf := file.(*hookFile)
	s.HostChanged(HostEvent{Share: "src", Path: []string{"f"}, Kind: Removed})
	if n, _ := s.CachedFiles(); n != 0 {
		t.Fatalf("%d cached after removal", n)
	}
	if _, cl := cf.h.counts(); cl != 0 {
		t.Fatal("closed under a request")
	}
	done()
	if _, cl := cf.h.counts(); cl != 1 {
		t.Fatal("not closed after the request")
	}
	// The removed node now has nothing behind it.
	if e, _ := c.read(f.node, 0, 0, 10); e != int(ESTALE) {
		t.Fatalf("read after host removal: %d", e)
	}
}

// Concurrent READs and WRITEs on nodes, with host removals and FORGETs: every host
// file is closed exactly once, never while in use, and nothing is left open.
func TestNoOpenConcurrent(t *testing.T) {
	s, c, fs, root := hookShare(t, Options{FileIdle: time.Millisecond, MaxFiles: 2})
	_ = c
	f := c.mustLookup(root, "f")
	g := c.mustLookup(root, "g")
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			cl := &tclient{t: t, s: s, pid: uint32(200 + w)}
			for i := 0; i < 300; i++ {
				n := f.node
				if (i+w)%2 == 0 {
					n = g.node
				}
				if i%3 == 0 {
					cl.write(n, 0, 0, []byte("h"))
				} else {
					cl.read(n, 0, 0, 10)
				}
				if w == 0 && i%50 == 0 {
					s.HostOverflow("src")
				}
			}
		}(w)
	}
	wg.Wait()
	s.Close()
	if o, cl := fs.counts(); o != cl {
		t.Fatalf("opens %d closes %d", o, cl)
	}
}

// The guest unlinks a file it may have open: later reads still reach the file (here a
// memFS, whose files are gone with their name: the pin is checked, not the data).
func TestNoOpenUnlinkPins(t *testing.T) {
	s, c, fs, root := hookShare(t, Options{FileIdle: time.Millisecond})
	f := c.mustLookup(root, "f")
	if e, _ := c.do(opUnlink, root, (&wbuf{}).str("f").b); e != 0 {
		t.Fatal(e)
	}
	if n, p := s.CachedFiles(); n != 1 || p != 1 {
		t.Fatalf("cached %d pinned %d", n, p)
	}
	time.Sleep(20 * time.Millisecond) // pins are not idle handles
	if n, _ := s.CachedFiles(); n != 1 {
		t.Fatal("pin closed as idle")
	}
	if e, _ := c.read(f.node, 0, 0, 10); e != 0 {
		t.Fatalf("read of the unlinked file: %d", e)
	}
	c.do(opForget, f.node, (&wbuf{}).u64(1).b)
	if n, p := s.CachedFiles(); n != 0 || p != 0 {
		t.Fatalf("after FORGET: cached %d pinned %d", n, p)
	}
	if o, cl := fs.counts(); o != cl {
		t.Fatalf("opens %d closes %d", o, cl)
	}
	// A name the guest never looked up needs no pin.
	fs.files["u"] = []byte("x")
	if e, _ := c.do(opUnlink, root, (&wbuf{}).str("u").b); e != 0 {
		t.Fatal(e)
	}
	if o, _ := fs.counts(); o != 1 {
		t.Fatalf("%d opens, want no pin for an unknown name", o)
	}
}

// A LOOKUP that read the host before a change was reported must not let the guest
// cache what it read (the size-0 report: a file created and then written on Windows).
func TestLookupRacingHostChange(t *testing.T) {
	s, c, fs, root := hookShare(t, Options{})
	fs.hook = func(p []string) {
		if key(p) == "f" {
			fs.hook = nil
			s.HostChanged(HostEvent{Share: "src", Path: []string{"f"}, Kind: Modified})
		}
	}
	e, b := c.do(opLookup, root, (&wbuf{}).str("f").b)
	if e != 0 {
		t.Fatal(e)
	}
	if en := decodeEntry(b); en.node == 0 {
		t.Fatal("no node")
	}
	if ev, av := binary.LittleEndian.Uint64(b[16:]), binary.LittleEndian.Uint64(b[24:]); ev != 0 || av != 0 {
		t.Fatalf("raced LOOKUP cached for %d/%d s", ev, av)
	}
	// An unraced one is cached as usual.
	_, b = c.do(opLookup, root, (&wbuf{}).str("g").b)
	if av := binary.LittleEndian.Uint64(b[24:]); av == 0 {
		t.Fatal("unraced LOOKUP not cached")
	}
}

// A negative LOOKUP racing a host create must not be cached either.
func TestNegativeLookupRacingHostCreate(t *testing.T) {
	s, c, fs, root := hookShare(t, Options{})
	fs.hook = func(p []string) {
		if key(p) == "new" {
			fs.hook = nil
			fs.memFS.mu.Lock()
			fs.files["new"] = []byte("data")
			fs.memFS.mu.Unlock()
			s.HostChanged(HostEvent{Share: "src", Path: []string{"new"}, Kind: Created})
		}
	}
	e, b := c.do(opLookup, root, (&wbuf{}).str("new").b)
	if e != 0 || decodeEntry(b).node != 0 {
		t.Fatalf("lookup: %d", e)
	}
	if ev := binary.LittleEndian.Uint64(b[16:]); ev != 0 {
		t.Fatalf("raced negative LOOKUP cached for %d s", ev)
	}
	s.mu.Lock()
	_, neg := s.negative[nodeKey{root, "new"}]
	s.mu.Unlock()
	if neg {
		t.Fatal("raced negative entry recorded")
	}
}

// READDIRPLUS hands out no attributes for a name that changed after the listing was
// read; the others keep theirs.
func TestReaddirplusRacingHostChange(t *testing.T) {
	s, c, fs, root := hookShare(t, Options{})
	fs.hook = func(p []string) {
		if len(p) == 0 {
			fs.hook = nil
			s.HostChanged(HostEvent{Share: "src", Path: []string{"f"}, Kind: Modified})
		}
	}
	e, list := c.readdir(root, true)
	if e != 0 {
		t.Fatal(e)
	}
	if list["f"].node != 0 {
		t.Fatal("raced entry has a node")
	}
	if list["g"].node == 0 {
		t.Fatal("unraced entry has no node")
	}
}

// A replayed create whose file was written on the host before the node existed.
func TestInjectedCreateRacingHostWrite(t *testing.T) {
	s, c, fs, root := hookShare(t, Options{})
	c.do(opHello, 0, (&wbuf{}).u32(4242).b)
	cap := &syncCapture{}
	s.cmu.Lock()
	s.conn = &conn{rw: cap}
	s.cmu.Unlock()
	fs.files["n"] = nil
	s.HostChanged(HostEvent{Share: "src", Path: []string{"n"}, Kind: Created})
	// A change of the name after the attributes were read and before the relay's
	// mknod (any reported event replaces the record too; this is the backstop).
	s.mu.Lock()
	s.noteChangeLocked(nodeKey{root, "n"})
	s.mu.Unlock()
	c.pid = 4242
	e, b := c.do(opMknod, root, (&wbuf{}).u32(sIFREG|0o644).u32(0).u32(0).u32(0).str("n").b)
	if e != 0 {
		t.Fatalf("injector mknod: %d", e)
	}
	if av := binary.LittleEndian.Uint64(b[24:]); av != 0 {
		t.Fatalf("raced replayed create cached for %d s", av)
	}
}

// The kernel sends no RELEASE for CREATE handles once OPEN answered ENOSYS: creates
// must not use up the handle table, and their files are cached like any other.
func TestNoOpenCreatesNeedNoRelease(t *testing.T) {
	s, c, _, root := hookShare(t, Options{MaxHandles: 8, MaxFiles: 4, FileIdle: time.Hour})
	for i := 0; i < 50; i++ {
		e, en, fh := c.create(root, "c"+itoa(i), oWRONLY|oCREAT|oEXCL, 0o644)
		if e != 0 || fh != 0 {
			t.Fatalf("create %d: errno %d fh %d", i, e, fh)
		}
		if e := c.write(en.node, 0, 0, []byte("data")); e != 0 {
			t.Fatalf("write %d: %d", i, e)
		}
		// Opened O_WRONLY by the guest, cached read-write: a later open for reading
		// needs no host open.
		if e, b := c.read(en.node, 0, 0, 10); e != 0 || string(b) != "data" {
			t.Fatalf("read %d: %d %q", i, e, b)
		}
	}
	if n, _ := s.CachedFiles(); n > 4 {
		t.Fatalf("%d cached files, want at most 4", n)
	}
	s.mu.Lock()
	nh := len(s.handles)
	s.mu.Unlock()
	if nh != 0 {
		t.Fatalf("%d handles", nh)
	}
}

// Invalidations of a node made moments ago are sent again later: the kernel may still
// be applying the reply that made it.
func TestFreshNodeInvalidatedAgain(t *testing.T) {
	old := reinvalAfter
	reinvalAfter = []time.Duration{time.Millisecond, 5 * time.Millisecond}
	defer func() { reinvalAfter = old }()
	s, c, _, root := hookShare(t, Options{})
	cap := &syncCapture{}
	s.cmu.Lock()
	s.conn = &conn{rw: cap}
	s.cmu.Unlock()
	f := c.mustLookup(root, "f")
	s.HostChanged(HostEvent{Share: "src", Path: []string{"f"}, Kind: Modified})
	count := func() int {
		n := 0
		for _, m := range cap.frames() {
			if int32(binary.LittleEndian.Uint32(m[4:])) == notifyInvalInode && binary.LittleEndian.Uint64(m[16:]) == f.node {
				n++
			}
		}
		return n
	}
	deadline := time.Now().Add(5 * time.Second)
	for count() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("%d invalidations of a fresh node, want 3", count())
		}
		time.Sleep(time.Millisecond)
	}
	// An old node is invalidated once.
	s.mu.Lock()
	s.nodes[f.node].born = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	before := count()
	s.HostChanged(HostEvent{Share: "src", Path: []string{"f"}, Kind: Modified})
	time.Sleep(20 * time.Millisecond)
	if n := count() - before; n != 1 {
		t.Fatalf("%d invalidations of an old node", n)
	}
}
