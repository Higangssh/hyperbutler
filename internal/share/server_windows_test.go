//go:build windows

package share

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// testDir returns an empty directory under %TEMP%\hb-dev\share\unit.
func testDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(os.TempDir(), "hb-dev", "share", "unit", strings.NewReplacer("/", "_", "\\", "_").Replace(t.Name()))
	os.RemoveAll(d)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func newTestServer(t *testing.T, dir string, opt Options, mod func(*Share)) (*Server, *tclient, uint64) {
	t.Helper()
	fs, err := OpenNTFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(opt)
	sh := &Share{Name: "src", FS: fs}
	if mod != nil {
		mod(sh)
	}
	if err := s.AddShare(sh); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.RemoveShare("src") })
	c := newClient(t, s)
	root := c.mustLookup(rootNode, "src")
	return s, c, root.node
}

// makeJunction creates a directory junction at link pointing to target (no privilege
// needed, unlike an NTFS symlink).
func makeJunction(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Mkdir(link, 0o755); err != nil {
		t.Fatal(err)
	}
	p, _ := windows.UTF16PtrFromString(link)
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	sub := utf16.Encode([]rune(`\??\` + target))
	pr := utf16.Encode([]rune(target))
	var pb []byte
	for _, c := range sub {
		pb = binary.LittleEndian.AppendUint16(pb, c)
	}
	pb = append(pb, 0, 0)
	for _, c := range pr {
		pb = binary.LittleEndian.AppendUint16(pb, c)
	}
	pb = append(pb, 0, 0)
	d := make([]byte, 8)
	binary.LittleEndian.PutUint16(d[0:], 0)
	binary.LittleEndian.PutUint16(d[2:], uint16(2*len(sub)))
	binary.LittleEndian.PutUint16(d[4:], uint16(2*len(sub)+2))
	binary.LittleEndian.PutUint16(d[6:], uint16(2*len(pr)))
	d = append(d, pb...)
	if err := setReparse(h, tagMountPoint, d); err != nil {
		t.Fatalf("set junction: %v", err)
	}
}

func TestBasicFileOps(t *testing.T) {
	dir := testDir(t)
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello from windows"), 0o644)
	_, c, root := newTestServer(t, dir, Options{}, nil)

	en := c.mustLookup(root, "hello.txt")
	if en.attr.Mode&sIFMT != sIFREG || en.attr.Size != 18 {
		t.Fatalf("attr %+v", en.attr)
	}
	e, fh := c.open(en.node, oRDONLY)
	if e != 0 {
		t.Fatalf("open: %d", e)
	}
	e, data := c.read(en.node, fh, 0, 4096)
	if e != 0 || string(data) != "hello from windows" {
		t.Fatalf("read: %d %q", e, data)
	}
	c.release(en.node, fh)

	e, ne, nfh := c.create(root, "new.txt", oWRONLY|oCREAT|oEXCL, 0o640)
	if e != 0 {
		t.Fatalf("create: %d", e)
	}
	if e := c.write(ne.node, nfh, 0, []byte("abc")); e != 0 {
		t.Fatalf("write: %d", e)
	}
	c.release(ne.node, nfh)
	if b, _ := os.ReadFile(filepath.Join(dir, "new.txt")); string(b) != "abc" {
		t.Fatalf("host sees %q", b)
	}
	if ne.attr.Mode&0o777 != 0o640 || ne.attr.UID != 0 {
		t.Fatalf("created mode %o uid %d", ne.attr.Mode, ne.attr.UID)
	}
	if e, _, _ := c.create(root, "new.txt", oWRONLY|oCREAT|oEXCL, 0o644); e != int(EEXIST) {
		t.Fatalf("O_EXCL on existing: %d", e)
	}

	e, de := c.mkdir(root, "sub")
	if e != 0 || de.attr.Mode&sIFMT != sIFDIR {
		t.Fatalf("mkdir: %d %o", e, de.attr.Mode)
	}
	// rename new.txt -> sub/moved.txt
	body := (&wbuf{}).u64(de.node).u32(0).u32(0).str("new.txt").str("moved.txt").b
	if e, _ := c.do(opRename2, root, body); e != 0 {
		t.Fatalf("rename: %d", e)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "moved.txt")); err != nil {
		t.Fatal(err)
	}
	e, list := c.readdir(de.node, true)
	if e != 0 || list["moved.txt"].node == 0 {
		t.Fatalf("readdirplus: %d %v", e, list)
	}
	if _, ok := list["."]; !ok {
		t.Fatal("no . entry")
	}
	if e, _ := c.do(opUnlink, de.node, (&wbuf{}).str("moved.txt").b); e != 0 {
		t.Fatalf("unlink: %d", e)
	}
	if e, _ := c.do(opRmdir, root, (&wbuf{}).str("sub").b); e != 0 {
		t.Fatalf("rmdir: %d", e)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub")); !os.IsNotExist(err) {
		t.Fatalf("sub still there: %v", err)
	}
}

func TestSymlinkAndMetadata(t *testing.T) {
	dir := testDir(t)
	_, c, root := newTestServer(t, dir, Options{}, nil)
	c.uid = 1000
	e, b := c.do(opSymlink, root, (&wbuf{}).str("link").str("../target with space").b)
	if e != 0 {
		t.Fatalf("symlink: %d", e)
	}
	en := decodeEntry(b)
	if en.attr.Mode&sIFMT != sIFLNK || en.attr.UID != 1000 {
		t.Fatalf("symlink attr %o uid %d", en.attr.Mode, en.attr.UID)
	}
	e, tb := c.do(opReadlink, en.node, nil)
	if e != 0 || string(tb) != "../target with space" {
		t.Fatalf("readlink: %d %q", e, tb)
	}
	// chmod 0755 and chown 1000:1000 through SETATTR persist in the WSL EAs.
	e, fe, fh := c.create(root, "script.sh", oWRONLY|oCREAT, 0o644)
	if e != 0 {
		t.Fatal(e)
	}
	c.release(fe.node, fh)
	sa := (&wbuf{}).u32(fattrMode).u32(0).u64(0).u64(0).u64(0).u64(0).u64(0).u64(0).u32(0).u32(0).u32(0).u32(0o755).u32(0).u32(0).u32(0).u32(0).b
	e, ab := c.do(opSetattr, fe.node, sa)
	if e != 0 {
		t.Fatalf("setattr: %d", e)
	}
	if a := decodeAttr(ab[16:]); a.Mode != sIFREG|0o755 {
		t.Fatalf("mode after chmod %o", a.Mode)
	}
	if e, en := c.lookup(root, "script.sh"); e != 0 || en.attr.Mode != sIFREG|0o755 || en.attr.UID != 1000 {
		t.Fatalf("lookup after chmod: %d %o uid %d", e, en.attr.Mode, en.attr.UID)
	}
	// A file created on Windows has no metadata: 0777, owner from Options.
	os.WriteFile(filepath.Join(dir, "win.txt"), nil, 0o644)
	if _, en := c.lookup(root, "win.txt"); en.attr.Mode != sIFREG|0o777 || en.attr.UID != 0 {
		t.Fatalf("windows file attr %o uid %d", en.attr.Mode, en.attr.UID)
	}
}

func TestNameMappingOnHost(t *testing.T) {
	dir := testDir(t)
	_, c, root := newTestServer(t, dir, Options{}, nil)
	for _, name := range []string{"a:b", `back\slash`, "trail.", "q?", "CON", "nul.txt", "유니코드 file"} {
		e, en, fh := c.create(root, name, oWRONLY|oCREAT|oEXCL, 0o644)
		if e != 0 {
			t.Fatalf("create %q: %d", name, e)
		}
		c.release(en.node, fh)
	}
	es, _ := os.ReadDir(dir)
	var got []string
	for _, e := range es {
		got = append(got, e.Name())
	}
	for _, want := range []string{"ab", "backslash", "trail", "q", "CON", "nul.txt", "유니코드 file"} {
		found := false
		for _, g := range got {
			if g == want {
				found = true
			}
		}
		if !found {
			t.Errorf("host has %q, want %q among them", got, want)
		}
	}
	_, list := c.readdir(root, false)
	for _, name := range []string{"a:b", `back\slash`, "trail.", "q?"} {
		if _, ok := list[name]; !ok {
			t.Errorf("guest listing lacks %q: %v", name, list)
		}
	}
	// "\" is a name character, never a separator: no traversal.
	os.Mkdir(filepath.Join(dir, "d"), 0o755)
	os.WriteFile(filepath.Join(dir, "d", "x"), nil, 0o644)
	if e, _ := c.lookup(root, `d\x`); e != int(ENOENT) {
		t.Fatalf(`lookup "d\x": %d, want ENOENT`, e)
	}
	for _, bad := range []string{"..", ".", "a/b", ""} {
		if e, _ := c.lookup(root, bad); e != int(EINVAL) {
			t.Errorf("lookup %q: %d, want EINVAL", bad, e)
		}
	}
}

func TestJunctionIsNotFollowed(t *testing.T) {
	dir := testDir(t)
	outside := testDir(t) + "-outside"
	os.MkdirAll(outside, 0o755)
	defer os.RemoveAll(outside)
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("host secret"), 0o644)
	makeJunction(t, filepath.Join(dir, "j"), outside)

	s, c, root := newTestServer(t, dir, Options{}, nil)
	en := c.mustLookup(root, "j")
	if en.attr.Mode&sIFMT != sIFLNK {
		t.Fatalf("junction shown as %o, want a symlink", en.attr.Mode)
	}
	e, tb := c.do(opReadlink, en.node, nil)
	if e != 0 || !strings.Contains(string(tb), "outside") {
		t.Fatalf("readlink junction: %d %q", e, tb)
	}
	// A hostile guest uses the junction's node as a directory anyway.
	if e, _ := c.lookup(en.node, "secret.txt"); e == 0 {
		t.Fatal("lookup through a junction succeeded")
	}
	if e, _, _ := c.create(en.node, "planted.txt", oWRONLY|oCREAT, 0o644); e == 0 {
		t.Fatal("create through a junction succeeded")
	}
	if e, _ := c.readdir(en.node, true); e == 0 {
		t.Fatal("readdir of a junction succeeded")
	}
	if e, _ := c.mkdir(en.node, "x"); e == 0 {
		t.Fatal("mkdir through a junction succeeded")
	}
	// Rename and link into the junction.
	_, fe, fh := c.create(root, "f", oWRONLY|oCREAT, 0o644)
	c.release(fe.node, fh)
	if e, _ := c.do(opRename2, root, (&wbuf{}).u64(en.node).u32(0).u32(0).str("f").str("moved").b); e == 0 {
		t.Fatal("rename into a junction succeeded")
	}
	if e, _ := c.do(opLink, en.node, (&wbuf{}).u64(fe.node).str("hard").b); e == 0 {
		t.Fatal("link into a junction succeeded")
	}
	// The host path itself: only the share root handle and NT relative opens.
	fs := s.shares["src"].FS
	if _, err := fs.Lstat([]string{"j", "secret.txt"}); err == nil {
		t.Fatal("NTFS.Lstat followed the junction")
	}
	if _, err := fs.ReadDir([]string{"j"}); err == nil {
		t.Fatal("NTFS.ReadDir followed the junction")
	}
	if _, err := fs.Open([]string{"j", "secret.txt"}, oRDONLY); err == nil {
		t.Fatal("NTFS.Open followed the junction")
	}
	es, _ := os.ReadDir(outside)
	if len(es) != 1 {
		t.Fatalf("outside dir changed: %v", es)
	}
	// Removing the junction removes the link, not the target.
	if e, _ := c.do(opUnlink, root, (&wbuf{}).str("j").b); e != 0 {
		t.Fatalf("unlink junction: %d", e)
	}
	if _, err := os.Stat(filepath.Join(outside, "secret.txt")); err != nil {
		t.Fatalf("target damaged: %v", err)
	}
}

func TestSingleFileShare(t *testing.T) {
	dir := testDir(t)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "sibling.txt"), []byte("private"), 0o644)
	_, c, root := newTestServer(t, dir, Options{}, func(sh *Share) { sh.Only = []string{"config.json"} })
	if e, _ := c.lookup(root, "sibling.txt"); e != int(ENOENT) {
		t.Fatalf("sibling visible: %d", e)
	}
	_, list := c.readdir(root, true)
	if len(list) != 3 { // . .. config.json
		t.Fatalf("listing %v", list)
	}
	if e, _, _ := c.create(root, "new", oWRONLY|oCREAT, 0o644); e != int(EACCES) {
		t.Fatalf("create in single-file share: %d", e)
	}
	en := c.mustLookup(root, "config.json")
	e, fh := c.open(en.node, oWRONLY)
	if e != 0 || c.write(en.node, fh, 0, []byte(`{"a":1}`)) != 0 {
		t.Fatalf("write to the shared file failed: %d", e)
	}
	c.release(en.node, fh)
}

func TestReadOnlyShare(t *testing.T) {
	dir := testDir(t)
	os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644)
	_, c, root := newTestServer(t, dir, Options{}, func(sh *Share) { sh.ReadOnly = true })
	en := c.mustLookup(root, "f")
	if en.attr.Mode&0o222 != 0 {
		t.Fatalf("read-only share shows write bits: %o", en.attr.Mode)
	}
	// Without OPEN the refusal comes with the first write (openfile.go).
	if e, fh := c.open(en.node, oWRONLY); e != 0 || c.write(en.node, fh, 0, []byte("y")) != int(EROFS) {
		t.Fatalf("open for write: %d", e)
	}
	if e, _ := c.open(en.node, oWRONLY|oTRUNC); e != int(EROFS) {
		t.Fatalf("open with O_TRUNC: %d", e)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "f")); string(b) != "x" {
		t.Fatalf("read-only share changed: %q", b)
	}
	if e, _, _ := c.create(root, "n", oWRONLY|oCREAT, 0o644); e != int(EROFS) {
		t.Fatalf("create: %d", e)
	}
}

func TestHandleLimit(t *testing.T) {
	dir := testDir(t)
	os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644)
	_, c, root := newTestServer(t, dir, Options{MaxHandles: 4, RealOpen: true}, nil)
	en := c.mustLookup(root, "f")
	for i := 0; i < 4; i++ {
		if e, _ := c.open(en.node, oRDONLY); e != 0 {
			t.Fatalf("open %d: %d", i, e)
		}
	}
	if e, _ := c.open(en.node, oRDONLY); e != int(EMFILE) {
		t.Fatalf("open past the limit: %d", e)
	}
}

func TestInjectedModifyDoesNotTouchHost(t *testing.T) {
	dir := testDir(t)
	os.WriteFile(filepath.Join(dir, "f"), []byte("original"), 0o644)
	s, c, root := newTestServer(t, dir, Options{NoWatch: true}, nil)
	en := c.mustLookup(root, "f")
	// The relay names its injector thread.
	c.do(opHello, 0, (&wbuf{}).u32(4242).b)
	var sent [][]byte
	s.cmu.Lock()
	s.conn = &conn{rw: &capture{out: &sent}}
	s.cmu.Unlock()
	os.WriteFile(filepath.Join(dir, "f"), []byte("changed on windows"), 0o644)
	s.HostChanged(HostEvent{Share: "src", Path: []string{"f"}, Kind: Modified})
	var inject *InjectFrame
	for _, m := range sent {
		if IsInject(m) {
			f, ok := ParseInject(m[outHeaderLen:])
			if !ok {
				t.Fatal("bad inject frame")
			}
			inject = &f
		}
	}
	if inject == nil || inject.Kind != injModify || inject.Path != "src/f" {
		t.Fatalf("inject frame %+v", inject)
	}
	// The injector opens for write and truncates to the current size: nothing reaches
	// the host file.
	c.pid = 4242
	e, fh := c.open(en.node, oWRONLY)
	if e != 0 {
		t.Fatalf("injector open: %d", e)
	}
	sa := (&wbuf{}).u32(fattrSize | fattrFh).u32(0).u64(fh).u64(0).u64(0).u64(0).u64(0).u64(0).u32(0).u32(0).u32(0).u32(0).u32(0).u32(0).u32(0).u32(0).b
	if e, _ := c.do(opSetattr, en.node, sa); e != 0 {
		t.Fatalf("injector setattr: %d", e)
	}
	c.release(en.node, fh)
	if b, _ := os.ReadFile(filepath.Join(dir, "f")); string(b) != "changed on windows" {
		t.Fatalf("injector changed the host file: %q", b)
	}
	// A normal guest truncate does reach the host.
	c.pid = 100
	if e, _ := c.do(opSetattr, en.node, sa); e != 0 {
		t.Fatal(e)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "f")); len(b) != 0 {
		t.Fatalf("normal truncate did not apply: %q", b)
	}
}

func TestInjectedCreateAndDelete(t *testing.T) {
	dir := testDir(t)
	s, c, root := newTestServer(t, dir, Options{NoWatch: true}, nil)
	c.do(opHello, 0, (&wbuf{}).u32(4242).b)
	var sent [][]byte
	s.cmu.Lock()
	s.conn = &conn{rw: &capture{out: &sent}}
	s.cmu.Unlock()
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x"), 0o644)
	s.HostChanged(HostEvent{Share: "src", Path: []string{"new.txt"}, Kind: Created})
	c.pid = 4242
	if e, _ := c.lookup(root, "new.txt"); e != int(ENOENT) {
		t.Fatalf("injector lookup of a created name: %d, want ENOENT", e)
	}
	e, b := c.do(opMknod, root, (&wbuf{}).u32(sIFREG|0o644).u32(0).u32(0).u32(0).str("new.txt").b)
	if e != 0 {
		t.Fatalf("injector mknod: %d", e)
	}
	created := decodeEntry(b)
	if created.attr.Size != 1 {
		t.Fatalf("injected create attrs %+v", created.attr)
	}
	// A second mknod is refused: the record is used up.
	if e, _ := c.do(opMknod, root, (&wbuf{}).u32(sIFREG|0o644).u32(0).u32(0).u32(0).str("new.txt").b); e != int(EPERM) {
		t.Fatalf("second injected mknod: %d", e)
	}
	// Delete on Windows, then the injector unlinks: the host is not asked.
	os.Remove(filepath.Join(dir, "new.txt"))
	s.HostChanged(HostEvent{Share: "src", Path: []string{"new.txt"}, Kind: Removed})
	if e, en := c.lookup(root, "new.txt"); e != 0 || en.node != created.node {
		t.Fatalf("injector lookup of removed name: %d node %d want %d", e, en.node, created.node)
	}
	if e, _ := c.do(opUnlink, root, (&wbuf{}).str("new.txt").b); e != 0 {
		t.Fatalf("injector unlink: %d", e)
	}
	// Without a record the injector cannot remove anything on the host.
	os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("x"), 0o644)
	c.pid = 100
	c.mustLookup(root, "keep.txt")
	c.pid = 4242
	if e, _ := c.do(opUnlink, root, (&wbuf{}).str("keep.txt").b); e != int(EPERM) {
		t.Fatalf("unrecorded injector unlink: %d", e)
	}
	if _, err := os.Stat(filepath.Join(dir, "keep.txt")); err != nil {
		t.Fatal("injector removed a host file")
	}
}
