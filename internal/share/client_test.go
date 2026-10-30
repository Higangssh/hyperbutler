package share

import (
	"encoding/binary"
	"testing"
)

// tclient builds FUSE requests the way the kernel does and decodes the replies, so the
// server can be tested without a guest.
type tclient struct {
	t      *testing.T
	s      *Server
	unique uint64
	pid    uint32
	uid    uint32
}

func newClient(t *testing.T, s *Server) *tclient {
	c := &tclient{t: t, s: s, pid: 100}
	c.init()
	return c
}

func (c *tclient) frame(op uint32, node uint64, body []byte) []byte {
	c.unique++
	b := make([]byte, inHeaderLen, inHeaderLen+len(body))
	le := binary.LittleEndian
	le.PutUint32(b[0:], uint32(inHeaderLen+len(body)))
	le.PutUint32(b[4:], op)
	le.PutUint64(b[8:], c.unique)
	le.PutUint64(b[16:], node)
	le.PutUint32(b[24:], c.uid)
	le.PutUint32(b[28:], c.uid)
	le.PutUint32(b[32:], c.pid)
	return append(b, body...)
}

// do sends one request and returns errno (positive) and the reply body.
func (c *tclient) do(op uint32, node uint64, body []byte) (int, []byte) {
	c.t.Helper()
	req, err := parseRequest(c.frame(op, node, body))
	if err != nil {
		c.t.Fatalf("parse own request: %v", err)
	}
	rep := c.s.dispatch(&req)
	if rep == nil {
		return 0, nil
	}
	if len(rep) < outHeaderLen || int(binary.LittleEndian.Uint32(rep)) != len(rep) {
		c.t.Fatalf("bad reply framing: %x", rep)
	}
	if binary.LittleEndian.Uint64(rep[8:]) != c.unique {
		c.t.Fatalf("reply unique %d, want %d", binary.LittleEndian.Uint64(rep[8:]), c.unique)
	}
	return -int(int32(binary.LittleEndian.Uint32(rep[4:]))), rep[outHeaderLen:]
}

// capture records every frame the server writes.
type capture struct{ out *[][]byte }

func (c *capture) Read([]byte) (int, error) { select {} }
func (c *capture) Write(p []byte) (int, error) {
	*c.out = append(*c.out, append([]byte(nil), p...))
	return len(p), nil
}

type wbuf struct{ b []byte }

func (w *wbuf) u32(v uint32) *wbuf { w.b = binary.LittleEndian.AppendUint32(w.b, v); return w }
func (w *wbuf) u64(v uint64) *wbuf { w.b = binary.LittleEndian.AppendUint64(w.b, v); return w }
func (w *wbuf) str(s string) *wbuf { w.b = append(append(w.b, s...), 0); return w }
func (w *wbuf) raw(p []byte) *wbuf { w.b = append(w.b, p...); return w }

func (c *tclient) init() {
	body := (&wbuf{}).u32(7).u32(44).u32(1 << 20).u32(0xffffffff).u32(0).raw(make([]byte, 44)).b
	if e, _ := c.do(opInit, 0, body); e != 0 {
		c.t.Fatalf("INIT: errno %d", e)
	}
}

type entry struct {
	node uint64
	attr Attr
}

func decodeAttr(b []byte) Attr {
	le := binary.LittleEndian
	return Attr{
		Ino: le.Uint64(b[0:]), Size: le.Uint64(b[8:]), Blocks: le.Uint64(b[16:]),
		Atime: le.Uint64(b[24:]), Mtime: le.Uint64(b[32:]), Ctime: le.Uint64(b[40:]),
		Mode: le.Uint32(b[60:]), Nlink: le.Uint32(b[64:]), UID: le.Uint32(b[68:]), GID: le.Uint32(b[72:]),
	}
}

func decodeEntry(b []byte) entry {
	return entry{node: binary.LittleEndian.Uint64(b), attr: decodeAttr(b[40:])}
}

func (c *tclient) lookup(parent uint64, name string) (int, entry) {
	e, b := c.do(opLookup, parent, (&wbuf{}).str(name).b)
	if e != 0 {
		return e, entry{}
	}
	en := decodeEntry(b)
	if en.node == 0 {
		return int(ENOENT), en
	}
	return 0, en
}

func (c *tclient) mustLookup(parent uint64, name string) entry {
	c.t.Helper()
	e, en := c.lookup(parent, name)
	if e != 0 {
		c.t.Fatalf("lookup %q: errno %d", name, e)
	}
	return en
}

func (c *tclient) create(parent uint64, name string, flags, mode uint32) (int, entry, uint64) {
	body := (&wbuf{}).u32(flags).u32(mode).u32(0o022).u32(0).str(name).b
	e, b := c.do(opCreate, parent, body)
	if e != 0 {
		return e, entry{}, 0
	}
	return 0, decodeEntry(b), binary.LittleEndian.Uint64(b[entryOutLen:])
}

func (c *tclient) mkdir(parent uint64, name string) (int, entry) {
	e, b := c.do(opMkdir, parent, (&wbuf{}).u32(0o755).u32(0o022).str(name).b)
	if e != 0 {
		return e, entry{}
	}
	return 0, decodeEntry(b)
}

func (c *tclient) open(node uint64, flags uint32) (int, uint64) {
	e, b := c.do(opOpen, node, (&wbuf{}).u32(flags).u32(0).b)
	if e != 0 {
		return e, 0
	}
	return 0, binary.LittleEndian.Uint64(b)
}

func (c *tclient) write(node, fh, off uint64, data []byte) int {
	body := (&wbuf{}).u64(fh).u64(off).u32(uint32(len(data))).u32(0).u64(0).u32(0).u32(0).raw(data).b
	e, _ := c.do(opWrite, node, body)
	return e
}

func (c *tclient) read(node, fh, off uint64, size uint32) (int, []byte) {
	body := (&wbuf{}).u64(fh).u64(off).u32(size).u32(0).u64(0).u32(0).u32(0).b
	return c.do(opRead, node, body)
}

func (c *tclient) release(node, fh uint64) {
	c.do(opRelease, node, (&wbuf{}).u64(fh).u32(0).u32(0).u64(0).b)
}

// readdirplus returns the names and node ids in a directory.
func (c *tclient) readdir(node uint64, plus bool) (int, map[string]entry) {
	// Like the kernel: an OPENDIR answered ENOSYS means no handles (fh 0, no RELEASEDIR).
	var fh uint64
	e, b := c.do(opOpendir, node, (&wbuf{}).u32(0).u32(0).b)
	switch {
	case e == int(ENOSYS):
	case e != 0:
		return e, nil
	default:
		fh = binary.LittleEndian.Uint64(b)
		defer c.do(opReleasedir, node, (&wbuf{}).u64(fh).u32(0).u32(0).u64(0).b)
	}
	out := map[string]entry{}
	op := uint32(opReaddir)
	if plus {
		op = opReaddirplus
	}
	off := uint64(0)
	for {
		e, b := c.do(op, node, (&wbuf{}).u64(fh).u64(off).u32(4096).u32(0).u64(0).u32(0).u32(0).b)
		if e != 0 {
			return e, nil
		}
		if len(b) == 0 {
			return 0, out
		}
		le := binary.LittleEndian
		for len(b) > 0 {
			var en entry
			if plus {
				en = decodeEntry(b)
				b = b[entryOutLen:]
			}
			nameLen := int(le.Uint32(b[16:]))
			off = le.Uint64(b[8:])
			name := string(b[24 : 24+nameLen])
			if !plus {
				en.attr.Ino = le.Uint64(b)
				en.attr.Mode = le.Uint32(b[20:]) << 12
			}
			out[name] = en
			n := 24 + nameLen
			n += (8 - n%8) % 8
			b = b[n:]
		}
	}
}
