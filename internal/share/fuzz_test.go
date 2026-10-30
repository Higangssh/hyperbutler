package share

import (
	"encoding/binary"
	"io"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// memFS is a tiny in-memory FS for fuzzing the protocol without a disk.
type memFS struct {
	mu    sync.Mutex
	files map[string][]byte // "a/b" -> data; dirs end with "/"
	ino   uint64
}

func newMemFS() *memFS {
	return &memFS{files: map[string][]byte{"/": nil, "d/": nil, "d/f": []byte("hello"), "top": []byte("x")}}
}

func key(p []string) string { return strings.Join(p, "/") }

func (m *memFS) attr(k string, dir bool, n int) Attr {
	m.ino++
	a := Attr{Ino: m.ino, Size: uint64(n), Nlink: 1, UID: NoID, GID: NoID, Mode: sIFREG | 0o644}
	if dir {
		a.Mode = sIFDIR | 0o755
	}
	return a
}

func (m *memFS) Lstat(p []string) (Attr, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(p)
	if len(p) == 0 {
		return m.attr("/", true, 0), nil
	}
	if _, ok := m.files[k+"/"]; ok {
		return m.attr(k, true, 0), nil
	}
	if d, ok := m.files[k]; ok {
		return m.attr(k, false, len(d)), nil
	}
	return Attr{}, ENOENT
}

func (m *memFS) ReadDir(p []string) ([]DirEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := key(p)
	if prefix != "" {
		if _, ok := m.files[prefix+"/"]; !ok {
			return nil, ENOTDIR
		}
		prefix += "/"
	}
	var out []DirEntry
	for k, d := range m.files {
		if !strings.HasPrefix(k, prefix) || k == prefix || k == "/" {
			continue
		}
		rest := strings.TrimPrefix(k, prefix)
		dir := strings.HasSuffix(rest, "/")
		rest = strings.TrimSuffix(rest, "/")
		if strings.Contains(rest, "/") {
			continue
		}
		out = append(out, DirEntry{Name: rest, Attr: m.attr(k, dir, len(d))})
	}
	return out, nil
}

type memFile struct {
	m *memFS
	k string
}

func (f *memFile) ReadAt(b []byte, off int64) (int, error) {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	d := f.m.files[f.k]
	if off >= int64(len(d)) {
		return 0, io.EOF
	}
	return copy(b, d[off:]), nil
}

func (f *memFile) WriteAt(b []byte, off int64) (int, error) {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	if off > 1<<20 {
		return 0, EFBIG
	}
	d := f.m.files[f.k]
	if n := int(off) + len(b); n > len(d) {
		d = append(d, make([]byte, n-len(d))...)
	}
	copy(d[off:], b)
	f.m.files[f.k] = d
	return len(b), nil
}
func (f *memFile) Sync() error  { return nil }
func (f *memFile) Close() error { return nil }

func (m *memFS) Open(p []string, flags uint32) (File, error) {
	if _, err := m.Lstat(p); err != nil {
		return nil, err
	}
	return &memFile{m, key(p)}, nil
}

func (m *memFS) Create(p []string, flags, mode uint32, o Owner) (File, Attr, error) {
	m.mu.Lock()
	if _, ok := m.files[key(p)]; ok && flags&oEXCL != 0 {
		m.mu.Unlock()
		return nil, Attr{}, EEXIST
	}
	if len(m.files) > 1000 {
		m.mu.Unlock()
		return nil, Attr{}, ENOSPC
	}
	m.files[key(p)] = nil
	m.mu.Unlock()
	a, _ := m.Lstat(p)
	return &memFile{m, key(p)}, a, nil
}

func (m *memFS) Mkdir(p []string, mode uint32, o Owner) (Attr, error) {
	m.mu.Lock()
	m.files[key(p)+"/"] = nil
	m.mu.Unlock()
	return m.Lstat(p)
}
func (m *memFS) Mknod(p []string, mode uint32, o Owner) (Attr, error) { return Attr{}, EPERM }
func (m *memFS) Symlink(p []string, t string, o Owner) (Attr, error) {
	f, a, err := m.Create(p, oEXCL, 0, o)
	if err == nil {
		f.Close()
	}
	return a, err
}
func (m *memFS) Readlink(p []string) (string, error) { return "", EINVAL }
func (m *memFS) Link(o, n []string) (Attr, error)    { return Attr{}, EPERM }
func (m *memFS) Unlink(p []string) error {
	m.mu.Lock()
	delete(m.files, key(p))
	m.mu.Unlock()
	return nil
}
func (m *memFS) Rmdir(p []string) error {
	m.mu.Lock()
	delete(m.files, key(p)+"/")
	m.mu.Unlock()
	return nil
}
func (m *memFS) Rename(o, n []string, noReplace bool) error { return EXDEV }
func (m *memFS) Setattr(p []string, f File, s SetAttr) (Attr, error) {
	return m.Lstat(p)
}
func (m *memFS) Statfs() (Statfs, error) { return Statfs{Bsize: 4096}, nil }
func (m *memFS) Close() error            { return nil }

// FuzzDispatch feeds arbitrary request frames (as a hostile guest would send them)
// through the decoder and every handler. It must never panic or return a reply whose
// framing is wrong.
func FuzzDispatch(f *testing.F) {
	c := &tclient{}
	seed := func(op uint32, node uint64, body []byte) { f.Add(c.frame(op, node, body)) }
	seed(opInit, 0, (&wbuf{}).u32(7).u32(44).u32(1<<20).u32(0xffffffff).b)
	seed(opLookup, 1, (&wbuf{}).str("src").b)
	seed(opLookup, 2, (&wbuf{}).str("..").b)
	seed(opLookup, 2, (&wbuf{}).str("d").b)
	seed(opCreate, 2, (&wbuf{}).u32(oWRONLY|oCREAT).u32(0o644).u32(0).u32(0).str("n").b)
	seed(opMkdir, 2, (&wbuf{}).u32(0o755).u32(0).str("x").b)
	seed(opWrite, 3, (&wbuf{}).u64(1).u64(0).u32(3).u32(0).u64(0).u32(0).u32(0).raw([]byte("abc")).b)
	seed(opRead, 3, (&wbuf{}).u64(1).u64(0).u32(4096).u32(0).u64(0).u32(0).u32(0).b)
	seed(opReaddirplus, 2, (&wbuf{}).u64(1).u64(0).u32(4096).u32(0).u64(0).u32(0).u32(0).b)
	seed(opRename2, 2, (&wbuf{}).u64(2).u32(0).u32(0).str("top").str("t2").b)
	seed(opBatchForget, 0, (&wbuf{}).u32(2).u32(0).u64(2).u64(1).u64(3).u64(1).b)
	seed(opSetattr, 3, make([]byte, 88))
	seed(opSymlink, 2, (&wbuf{}).str("l").str("/etc/passwd").b)
	seed(opHello, 0, (&wbuf{}).u32(7).u32(0).b)
	seed(opMknod, 2, (&wbuf{}).u32(sIFREG|0o644).u32(0).u32(0).u32(0).str("m").b)
	f.Fuzz(func(t *testing.T, frame []byte) {
		s := NewServer(Options{MaxNodes: 256, MaxHandles: 16, NoWatch: true})
		s.AddShare(&Share{Name: "src", FS: newMemFS()})
		cl := &tclient{t: t, s: s, pid: 100}
		// Bring the session to a realistic state first.
		cl.init()
		cl.lookup(rootNode, "src")
		cl.lookup(2, "d")
		cl.open(3, oRDWR)
		// The fuzzed frame may also be a sequence of frames.
		for len(frame) >= inHeaderLen {
			n := int(binary.LittleEndian.Uint32(frame))
			if n < inHeaderLen || n > len(frame) {
				n = len(frame)
				binary.LittleEndian.PutUint32(frame, uint32(n))
			}
			req, err := parseRequest(frame[:n])
			frame = frame[n:]
			if err != nil {
				continue
			}
			rep := s.dispatch(&req)
			if rep != nil && (len(rep) < outHeaderLen || int(binary.LittleEndian.Uint32(rep)) != len(rep)) {
				t.Fatalf("bad reply framing for op %d", req.h.Opcode)
			}
			if len(rep) > MaxWrite+4096 {
				t.Fatalf("reply of %d bytes", len(rep))
			}
		}
		s.HostChanged(HostEvent{Share: "src", Path: []string{"d", "f"}, Kind: Modified})
		s.HostOverflow("src")
	})
}

// FuzzNames checks the Linux <-> NTFS name mapping: toNT never yields a name with a
// separator or an NTFS-reserved character, and fromNT inverts it.
func FuzzNames(f *testing.F) {
	for _, s := range []string{"a", "a:b", `a\b`, "x.", "x ", "..", ".", "a/b", "\x01", "😀", "\xff", "a\uf03ab", "\uf02e", "con"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		nt, err := toNT(s)
		if err != nil {
			return
		}
		for _, r := range nt {
			if r == '\\' || r == '/' || r == 0 || ntReserved(r) {
				t.Fatalf("toNT(%q) = %q contains %q", s, nt, r)
			}
		}
		if l := nt[len(nt)-1]; l == '.' || l == ' ' {
			t.Fatalf("toNT(%q) = %q ends in a dot or space", s, nt)
		}
		if !utf8.ValidString(nt) || utf16Len(nt) > maxNameLen {
			t.Fatalf("toNT(%q) = %q too long or invalid", s, nt)
		}
		if back := fromNT(nt); back != s {
			// Only the drvfs aliasing case may differ: s contained mapped code points.
			if again, err := toNT(back); err != nil || again != nt {
				t.Fatalf("fromNT(toNT(%q)) = %q, which maps to %q", s, back, again)
			}
		}
		if checkName(fromNT(nt)) != nil {
			t.Fatalf("fromNT(%q) is not a valid name", nt)
		}
	})
}

// FuzzInject checks the replay frame decoder the guest relay relies on.
func FuzzInject(f *testing.F) {
	f.Add(injectMsg(injRename, 0, "src/a", "src/b")[outHeaderLen:])
	f.Add([]byte{1, 0, 0, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		fr, ok := ParseInject(b)
		if ok && (len(fr.Path) > maxSymlinkLen || len(fr.NewPath) > maxSymlinkLen) {
			t.Fatal("oversized path accepted")
		}
	})
}

// FuzzBind checks the docker bind rewriting: no panic, and every Windows source that
// is rewritten goes through the resolver.
func FuzzBind(f *testing.F) {
	f.Add([]byte(`{"HostConfig":{"Binds":["C:\\src:/src:ro","vol:/v","/x:/y"],"Mounts":[{"Type":"bind","Source":"D:/d","Target":"/d"}]}}`))
	f.Add([]byte(`{"HostConfig":{"Binds":["C:\\:/c"]}}`))
	f.Add([]byte(`{"HostConfig":{"Binds":[1,null,"\\\\?\\C:\\x:/x"]}}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		var seen []string
		out, err := RewriteCreate(body, func(p string) (string, error) {
			if !WindowsPath(p) {
				t.Fatalf("resolver got %q", p)
			}
			seen = append(seen, p)
			return "/mnt/winbox/b-x", nil
		})
		if err != nil {
			return
		}
		if len(seen) == 0 && string(out) != string(body) {
			t.Fatal("body changed without a resolved bind")
		}
	})
}

func FuzzSplitBind(f *testing.F) {
	for _, s := range []string{`C:\src:/src`, `C:\src:/src:ro`, `C:/a b:/x`, `\\?\C:\x:/y`, `c:`, `C:\x`, `C:\x:rel`, `C:\x:/y:ro:z`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		src, target, _, ok, err := splitBind(s)
		if ok && err == nil {
			if !WindowsPath(src) || !strings.HasPrefix(target, "/") || strings.Contains(target, ":") {
				t.Fatalf("splitBind(%q) = %q %q", s, src, target)
			}
		}
	})
}
