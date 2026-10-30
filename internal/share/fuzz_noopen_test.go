package share

import (
	"testing"
	"time"
)

// Rename moves a file (memFS has none).
func (h *hookFS) Rename(o, n []string, noReplace bool) error {
	h.memFS.mu.Lock()
	defer h.memFS.mu.Unlock()
	d, ok := h.memFS.files[key(o)]
	if !ok {
		return ENOENT
	}
	if _, exists := h.memFS.files[key(n)]; exists && noReplace {
		return EEXIST
	}
	h.memFS.files[key(n)] = d
	delete(h.memFS.files, key(o))
	h.imu.Lock()
	h.inos[key(n)] = h.inos[key(o)]
	delete(h.inos, key(o))
	h.imu.Unlock()
	return nil
}

// FuzzNoOpen drives the handle cache for opens without OPEN (openfile.go) with
// arbitrary sequences of guest requests and host events, as a hostile guest and a busy
// Windows side could produce them. Invariants: no host file is used after it was
// closed or closed twice, the cache stays within its bounds, and after Close every
// host file opened is closed.
func FuzzNoOpen(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12})
	f.Add([]byte{1, 0x11, 0x21, 6, 0x16, 3, 0x13, 9, 4, 0x14, 7, 0x17})
	f.Add([]byte{2, 2, 2, 5, 1, 1, 8, 0, 0, 10, 11, 11, 12, 3})
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 512 {
			ops = ops[:512]
		}
		fs := newHookFS()
		names := []string{"a", "b", "c", "d"}
		for _, n := range names {
			fs.files[n] = []byte("data-" + n)
		}
		s := NewServer(Options{NoWatch: true, MaxFiles: 2, MaxHandles: 3, FileIdle: time.Millisecond})
		s.AddShare(&Share{Name: "src", FS: fs})
		c := &tclient{t: t, s: s, pid: 100}
		c.init()
		_, re := c.lookup(rootNode, "src")
		root := re.node
		node := func(b byte) (uint64, string) {
			n := names[int(b>>4)%len(names)]
			_, en := c.lookup(root, n)
			return en.node, n
		}
		sleeps := 0
		for _, op := range ops {
			id, name := node(op)
			switch op & 0xf {
			case 0:
				c.read(id, 0, 0, 16)
			case 1:
				c.write(id, 0, 0, []byte("w"))
			case 2:
				c.fsync(id, 0)
			case 3:
				c.do(opUnlink, root, (&wbuf{}).str(name).b)
			case 4:
				other := names[(int(op>>4)+1)%len(names)]
				c.do(opRename2, root, (&wbuf{}).u64(root).u32(0).u32(0).str(other).str(name).b)
			case 5:
				if id != 0 {
					c.do(opForget, id, (&wbuf{}).u64(1 + uint64(op>>6)).b)
				}
			case 6:
				s.HostChanged(HostEvent{Share: "src", Path: []string{name}, Kind: Removed})
			case 7:
				s.HostChanged(HostEvent{Share: "src", Path: []string{name}, Kind: Modified})
			case 8:
				s.HostOverflow("src")
			case 9:
				if sleeps < 3 { // Windows sleeps are 1-16 ms; keep inputs fast
					sleeps++
					time.Sleep(2 * time.Millisecond) // let the sweep run
				} else {
					s.mu.Lock()
					g := s.sweepGen
					s.mu.Unlock()
					s.sweepFiles(g) // the sweep, run inline
				}
			case 10:
				e, en, _ := c.create(root, name, oWRONLY|oCREAT, 0o644)
				if e == 0 {
					c.write(en.node, 0, 0, []byte("new"))
				}
			case 11:
				c.open(id, oWRONLY|oTRUNC)
			case 12:
				fs.memFS.mu.Lock()
				fs.files[name] = []byte("host")
				fs.memFS.mu.Unlock()
				s.HostChanged(HostEvent{Share: "src", Path: []string{name}, Kind: Created})
			default:
				c.do(opFlush, id, (&wbuf{}).u64(0).u32(0).u32(0).u64(0).b)
			}
			s.mu.Lock()
			cached, pins := len(s.files), s.pins
			np := 0
			for _, e := range s.files {
				if e.pinned {
					np++
				}
				if e.gone {
					t.Fatal("a dropped file is still cached")
				}
			}
			s.mu.Unlock()
			if np != pins || pins > 3 || cached-pins > 2 {
				t.Fatalf("cache %d files, %d pins (counted %d)", cached, pins, np)
			}
		}
		s.Close()
		fs.imu.Lock()
		defer fs.imu.Unlock()
		if fs.misuse != 0 {
			t.Fatalf("%d uses after close or double closes", fs.misuse)
		}
		if fs.opens != fs.closes {
			t.Fatalf("%d host opens, %d closes", fs.opens, fs.closes)
		}
	})
}
