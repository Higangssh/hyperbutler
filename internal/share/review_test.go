package share

// Regression tests for the adversarial review findings (ADR 0007).

import (
	"bytes"
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A sibling created next to the file of a single-file share must not be named to the
// guest nor become reachable through replay.
func TestOnlyShareIgnoresSiblingEvents(t *testing.T) {
	fs := newMemFS()
	fs.files["config.json"] = []byte("{}")
	s := NewServer(Options{NoWatch: true})
	s.AddShare(&Share{Name: "src", FS: fs, Only: []string{"config.json"}})
	c := newClient(t, s)
	root := c.mustLookup(rootNode, "src")
	c.do(opHello, 0, (&wbuf{}).u32(4242).b)
	sent := &syncCapture{}
	s.cmu.Lock()
	s.conn = &conn{rw: sent}
	s.cmu.Unlock()
	fs.files["secret.txt"] = []byte("private")
	s.HostChanged(HostEvent{Share: "src", Path: []string{"secret.txt"}, Kind: Created})
	for _, m := range sent.frames() {
		if bytes.Contains(m, []byte("secret")) {
			t.Fatalf("sibling name sent to the guest: %q", m)
		}
	}
	c.pid = 4242
	if e, _ := c.do(opMknod, root.node, (&wbuf{}).u32(sIFREG|0o644).u32(0).u32(0).u32(0).str("secret.txt").b); e == 0 {
		t.Fatal("injector created a node for a hidden sibling")
	}
	if e, _ := c.lookup(root.node, "secret.txt"); e == 0 {
		t.Fatal("hidden sibling visible")
	}
}

// countingFile records use after close.
type countingFile struct {
	closed   atomic.Bool
	misuse   atomic.Int32
	inFlight atomic.Int32
}

func (f *countingFile) ReadAt(p []byte, off int64) (int, error) {
	f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	if f.closed.Load() {
		f.misuse.Add(1)
	}
	time.Sleep(50 * time.Microsecond)
	if f.closed.Load() {
		f.misuse.Add(1)
	}
	return 0, io.EOF
}
func (f *countingFile) WriteAt(p []byte, off int64) (int, error) { return f.ReadAt(p, off) }
func (f *countingFile) Sync() error                              { return nil }
func (f *countingFile) Close() error {
	if f.inFlight.Load() != 0 {
		f.misuse.Add(1)
	}
	f.closed.Store(true)
	return nil
}

// RELEASE racing READ on the same fh must not close the file under the read.
func TestReleaseWaitsForInflightIO(t *testing.T) {
	s := NewServer(Options{NoWatch: true})
	for i := 0; i < 200; i++ {
		f := &countingFile{}
		fh, _ := s.addHandle(&handle{node: 5, f: f})
		var wg sync.WaitGroup
		for j := 0; j < 4; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if ff, done, err := s.file(fh); err == nil {
					ff.ReadAt(make([]byte, 1), 0)
					done()
				}
			}()
		}
		wg.Add(1)
		go func() { defer wg.Done(); s.release(fh) }()
		wg.Wait()
		if f.misuse.Load() != 0 {
			t.Fatal("file used after close or closed during I/O")
		}
		if !f.closed.Load() {
			t.Fatal("file never closed")
		}
	}
}

type pipeRW struct {
	r *io.PipeReader
	w io.Writer
}

func (p pipeRW) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p pipeRW) Write(b []byte) (int, error) { return p.w.Write(b) }

func helloBytes(queue uint32) []byte {
	c := &tclient{}
	return c.frame(opHello, 0, (&wbuf{}).u32(1).u32(queue).b)
}

// A stale queue-0 connection ending must not wipe the session that replaced it.
func TestStaleSessionDoesNotResetNew(t *testing.T) {
	s := NewServer(Options{NoWatch: true})
	s.AddShare(&Share{Name: "src", FS: newMemFS()})
	r1, w1 := io.Pipe()
	done1 := make(chan struct{})
	go func() { s.Serve(pipeRW{r1, io.Discard}); close(done1) }()
	w1.Write(helloBytes(0))
	time.Sleep(50 * time.Millisecond)
	r2, w2 := io.Pipe()
	done2 := make(chan struct{})
	go func() { s.Serve(pipeRW{r2, io.Discard}); close(done2) }()
	w2.Write(helloBytes(0))
	time.Sleep(50 * time.Millisecond)
	c := &tclient{t: t, s: s, pid: 1}
	en := c.mustLookup(rootNode, "src")
	w1.Close()
	<-done1
	if e, _ := c.lookup(en.node, "top"); e != 0 {
		t.Fatalf("old connection's end reset the live session: errno %d", e)
	}
	w2.Close()
	<-done2
}

// blockingWriter never returns from Write until released.
type blockingWriter struct{ release chan struct{} }

func (b blockingWriter) Write(p []byte) (int, error) { <-b.release; return len(p), nil }

// A guest that stops reading must not block the host side.
func TestNotifyNeverBlocks(t *testing.T) {
	s := NewServer(Options{NoWatch: true})
	s.AddShare(&Share{Name: "src", FS: newMemFS()})
	r, w := io.Pipe()
	bw := blockingWriter{make(chan struct{})}
	done := make(chan struct{})
	go func() { s.Serve(pipeRW{r, bw}); close(done) }()
	w.Write(helloBytes(0))
	time.Sleep(20 * time.Millisecond)
	finished := make(chan struct{})
	go func() {
		for i := 0; i < maxPendingNotes+100; i++ {
			s.notify(notifyInvalInodeMsg(2, 0, 0))
		}
		s.RemoveShare("src")
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("notify blocked on a guest that does not read")
	}
	w.Close()
	close(bw.release)
	<-done
	_ = binary.LittleEndian
}

// A directory the guest just changed something in is reported by the host as modified
// (its times changed). That echo must not invalidate the directory (a GETATTR per
// create, and the guest's readdir cache dropped); a Windows change inside it must.
func TestDirectoryModifiedEchoSuppressed(t *testing.T) {
	fs := newMemFS()
	s := NewServer(Options{NoWatch: true})
	s.AddShare(&Share{Name: "src", FS: fs})
	c := newClient(t, s)
	root := c.mustLookup(rootNode, "src")
	d := c.mustLookup(root.node, "d")
	sent := &syncCapture{}
	s.cmu.Lock()
	s.conn = &conn{rw: sent}
	s.cmu.Unlock()
	if e, _, _ := c.create(d.node, "new.txt", oWRONLY|oCREAT, 0o644); e != 0 {
		t.Fatalf("create: errno %d", e)
	}
	s.HostChanged(HostEvent{Share: "src", Path: []string{"d"}, Kind: Modified})
	if len(sent.frames()) != 0 {
		t.Fatalf("echo of the guest's own create invalidated the directory: %d frames", len(sent.frames()))
	}
	// A Windows-side create in d still invalidates d.
	fs.files["d/win.txt"] = nil
	s.HostChanged(HostEvent{Share: "src", Path: []string{"d", "win.txt"}, Kind: Created})
	if len(sent.frames()) == 0 {
		t.Fatal("a Windows create in the directory sent nothing")
	}
	// Once the echo window has passed, a directory modification is passed on.
	sent.reset()
	s.mu.Lock()
	s.nodes[d.node].inner = time.Now().Add(-2 * echoWindow)
	s.mu.Unlock()
	s.HostChanged(HostEvent{Share: "src", Path: []string{"d"}, Kind: Modified})
	if len(sent.frames()) == 0 {
		t.Fatal("a later directory modification was dropped")
	}
	// The directory's own removal is never taken for an echo of a change inside it.
	sent.reset()
	c.create(d.node, "again.txt", oWRONLY|oCREAT, 0o644)
	s.HostChanged(HostEvent{Share: "src", Path: []string{"d"}, Kind: Removed})
	if len(sent.frames()) == 0 {
		t.Fatal("removal of a directory the guest wrote in was suppressed")
	}
}

// bench/fs-notify: Windows edits of files the guest never looked up (a plain READDIR
// names entries without lookups) must still be replayed, so an inotify watch on the
// directory gets IN_MODIFY. Directories are not replayed.
func TestModifyOfUnlookedFileReplayed(t *testing.T) {
	// Only the frames each event sends at once are checked; a delayed re-invalidation
	// of an earlier event must not land among a later event's frames on a slow machine.
	old := reinvalAfter
	reinvalAfter = []time.Duration{time.Hour, time.Hour}
	defer func() { reinvalAfter = old }()
	fs := newMemFS()
	fs.files["d/sub/"] = nil
	s := NewServer(Options{NoWatch: true})
	s.AddShare(&Share{Name: "src", FS: fs})
	c := newClient(t, s)
	root := c.mustLookup(rootNode, "src")
	c.mustLookup(root.node, "d")
	c.do(opHello, 0, (&wbuf{}).u32(4242).b)
	sent := &syncCapture{}
	s.cmu.Lock()
	s.conn = &conn{rw: sent}
	s.cmu.Unlock()
	s.HostChanged(HostEvent{Share: "src", Path: []string{"d", "f"}, Kind: Modified})
	replays := 0
	for _, m := range sent.frames() {
		if IsInject(m) {
			f, ok := ParseInject(m[outHeaderLen:])
			if !ok || f.Kind != injModify || f.Path != "src/d/f" {
				t.Fatalf("replay %+v %v", f, ok)
			}
			replays++
		}
	}
	if replays != 1 {
		t.Fatalf("%d replays, want 1", replays)
	}
	sent.reset()
	s.HostChanged(HostEvent{Share: "src", Path: []string{"d", "sub"}, Kind: Modified})
	for _, m := range sent.frames() {
		if IsInject(m) {
			t.Fatal("a directory's modification was replayed")
		}
	}
	// A file in a directory the guest never looked up is not replayed either: nothing
	// in the guest can be watching it.
	sent.reset()
	s.HostChanged(HostEvent{Share: "src", Path: []string{"other", "f"}, Kind: Modified})
	if len(sent.frames()) != 0 {
		t.Fatalf("%d frames for an unknown directory", len(sent.frames()))
	}
}
