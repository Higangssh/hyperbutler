package share

// The FUSE wire protocol (ABI 7.x) as the Linux kernel speaks it on /dev/fuse. The guest
// relay forwards it unchanged over hvsock, so this file decodes requests that come from
// the guest: every length and offset is checked before it is used. Layouts follow
// include/uapi/linux/fuse.h (used here under its BSD-2-Clause option); only x86-64 and
// arm64 little-endian guests are supported.

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Kernel ABI version this server speaks. Older kernels negotiate down (minor >= 31).
const (
	kernelMajor = 7
	kernelMinor = 31
	// minMinor is the oldest minor version accepted: 7.31 (Linux 5.4) has every
	// structure size used here.
	minMinor = 31
)

// Opcodes.
const (
	opLookup      = 1
	opForget      = 2
	opGetattr     = 3
	opSetattr     = 4
	opReadlink    = 5
	opSymlink     = 6
	opMknod       = 8
	opMkdir       = 9
	opUnlink      = 10
	opRmdir       = 11
	opRename      = 12
	opLink        = 13
	opOpen        = 14
	opRead        = 15
	opWrite       = 16
	opStatfs      = 17
	opRelease     = 18
	opFsync       = 20
	opSetxattr    = 21
	opGetxattr    = 22
	opListxattr   = 23
	opRemovexattr = 24
	opFlush       = 25
	opInit        = 26
	opOpendir     = 27
	opReaddir     = 28
	opReleasedir  = 29
	opFsyncdir    = 30
	opGetlk       = 31
	opSetlk       = 32
	opSetlkw      = 33
	opAccess      = 34
	opCreate      = 35
	opInterrupt   = 36
	opBmap        = 37
	opDestroy     = 38
	opIoctl       = 39
	opPoll        = 40
	opBatchForget = 42
	opFallocate   = 43
	opReaddirplus = 44
	opRename2     = 45
	opLseek       = 46
	opCopyRange   = 47
	opSyncfs      = 50
	opTmpfile     = 51
	opStatx       = 52

	// opHello is not a kernel opcode: the guest relay sends it once, first, to tell
	// the server which guest thread performs injected events (see inject.go).
	opHello = 0x77620001
)

// Notification codes (out header error field, unique 0).
const (
	notifyInvalInode = 2
	notifyInvalEntry = 3
	// notifyInject is not a kernel code: the relay consumes it and replays an event
	// on the mount so that inotify watchers see it.
	notifyInject = 0x7762
)

// INIT flags.
const (
	initAsyncRead       = 1 << 0
	initAtomicOTrunc    = 1 << 3
	initBigWrites       = 1 << 5
	initDontMask        = 1 << 6
	initAutoInvalData   = 1 << 12
	initDoReaddirplus   = 1 << 13
	initReaddirplusAuto = 1 << 14
	initAsyncDIO        = 1 << 15
	initWritebackCache  = 1 << 16
	initNoOpenSupport   = 1 << 17 // the kernel takes ENOSYS from OPEN as "no opens needed"
	initParallelDirops  = 1 << 18
	initMaxPages        = 1 << 22
	initCacheSymlinks   = 1 << 23
)

// open_out flags.
const (
	fopenDirectIO  = 1 << 0
	fopenKeepCache = 1 << 1
	fopenCacheDir  = 1 << 3
	fopenNoFlush   = 1 << 5
)

// setattr valid bits.
const (
	fattrMode     = 1 << 0
	fattrUID      = 1 << 1
	fattrGID      = 1 << 2
	fattrSize     = 1 << 3
	fattrAtime    = 1 << 4
	fattrMtime    = 1 << 5
	fattrFh       = 1 << 6
	fattrAtimeNow = 1 << 7
	fattrMtimeNow = 1 << 8
	fattrCtime    = 1 << 10
)

// rename2 flags.
const (
	renameNoreplace = 1 << 0
	renameExchange  = 1 << 1
	renameWhiteout  = 1 << 2
)

const (
	inHeaderLen  = 40
	outHeaderLen = 16
	attrLen      = 88
	entryOutLen  = 40 + attrLen
	attrOutLen   = 16 + attrLen
	direntLen    = 24
	// MaxWrite is the largest WRITE payload the server accepts (and advertises).
	MaxWrite = 1 << 20
	// MaxRequest bounds one request frame: a maximal WRITE plus its headers, or two
	// maximal names.
	MaxRequest = MaxWrite + 4096
	// maxNameLen is NAME_MAX.
	maxNameLen = 255
	// maxSymlinkLen bounds a symlink target (PATH_MAX).
	maxSymlinkLen = 4096
	// maxBatchForget bounds BATCH_FORGET entries in one request.
	maxBatchForget = (MaxRequest - inHeaderLen - 8) / 16
)

var errShort = errors.New("share: short request")

// inHeader is struct fuse_in_header.
type inHeader struct {
	Len     uint32
	Opcode  uint32
	Unique  uint64
	NodeID  uint64
	UID     uint32
	GID     uint32
	PID     uint32
	ExtLen  uint16 // total_extlen, in 8-byte units
	Padding uint16
}

// request is one decoded frame: the header and the body after it, with any request
// extensions (security context, supplementary groups) cut off.
type request struct {
	h    inHeader
	body []byte
}

// parseRequest decodes one frame. b must be exactly one frame (its length is h.Len).
func parseRequest(b []byte) (request, error) {
	if len(b) < inHeaderLen {
		return request{}, errShort
	}
	le := binary.LittleEndian
	h := inHeader{
		Len:     le.Uint32(b[0:]),
		Opcode:  le.Uint32(b[4:]),
		Unique:  le.Uint64(b[8:]),
		NodeID:  le.Uint64(b[16:]),
		UID:     le.Uint32(b[24:]),
		GID:     le.Uint32(b[28:]),
		PID:     le.Uint32(b[32:]),
		ExtLen:  le.Uint16(b[36:]),
		Padding: le.Uint16(b[38:]),
	}
	if int(h.Len) != len(b) {
		return request{}, fmt.Errorf("share: frame length %d, header says %d", len(b), h.Len)
	}
	body := b[inHeaderLen:]
	ext := int(h.ExtLen) * 8
	if ext > len(body) {
		return request{}, errors.New("share: request extensions exceed the frame")
	}
	return request{h: h, body: body[:len(body)-ext]}, nil
}

// reader walks a request body. Every accessor fails, rather than panics, on a short
// body; check err once at the end.
type reader struct {
	b   []byte
	err error
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || n > len(r.b) {
		r.err = errShort
		return nil
	}
	p := r.b[:n]
	r.b = r.b[n:]
	return p
}

func (r *reader) u32() uint32 {
	p := r.take(4)
	if p == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(p)
}

func (r *reader) u64() uint64 {
	p := r.take(8)
	if p == nil {
		return 0
	}
	return binary.LittleEndian.Uint64(p)
}

// name reads a NUL-terminated name and checks it as a single path component.
func (r *reader) name() string {
	if r.err != nil {
		return ""
	}
	for i, c := range r.b {
		if c == 0 {
			s := string(r.b[:i])
			r.b = r.b[i+1:]
			if err := checkName(s); err != nil {
				r.err = err
				return ""
			}
			return s
		}
	}
	r.err = errors.New("share: name is not NUL-terminated")
	return ""
}

// cstring reads a NUL-terminated string of at most max bytes without name checks (a
// symlink target or an xattr name).
func (r *reader) cstring(max int) string {
	if r.err != nil {
		return ""
	}
	for i, c := range r.b {
		if c == 0 {
			if i > max {
				r.err = errors.New("share: string too long")
				return ""
			}
			s := string(r.b[:i])
			r.b = r.b[i+1:]
			return s
		}
	}
	r.err = errors.New("share: string is not NUL-terminated")
	return ""
}

// errBadName is returned for a name that cannot be a single Linux path component.
var errBadName = errors.New("share: bad name")

// checkName accepts one Linux path component: not empty, not "." or "..", no '/' or
// NUL, at most NAME_MAX bytes. Host-specific mapping happens later (names_windows.go).
func checkName(s string) error {
	if s == "" || s == "." || s == ".." || len(s) > maxNameLen {
		return errBadName
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '/' || s[i] == 0 {
			return errBadName
		}
	}
	return nil
}

// Decoded request bodies.

type initIn struct {
	Major, Minor, MaxReadahead, Flags, Flags2 uint32
}

func parseInit(b []byte) (initIn, error) {
	r := reader{b: b}
	in := initIn{Major: r.u32(), Minor: r.u32(), MaxReadahead: r.u32(), Flags: r.u32()}
	if r.err == nil && len(r.b) >= 4 {
		in.Flags2 = r.u32()
	}
	return in, r.err
}

type setattrIn struct {
	Valid                           uint32
	Fh, Size, LockOwner             uint64
	Atime, Mtime, Ctime             uint64
	AtimeNsec, MtimeNsec, CtimeNsec uint32
	Mode, UID, GID                  uint32
}

func parseSetattr(b []byte) (setattrIn, error) {
	r := reader{b: b}
	var s setattrIn
	s.Valid = r.u32()
	r.u32()
	s.Fh, s.Size, s.LockOwner = r.u64(), r.u64(), r.u64()
	s.Atime, s.Mtime, s.Ctime = r.u64(), r.u64(), r.u64()
	s.AtimeNsec, s.MtimeNsec, s.CtimeNsec = r.u32(), r.u32(), r.u32()
	s.Mode = r.u32()
	r.u32()
	s.UID, s.GID = r.u32(), r.u32()
	r.u32()
	if s.AtimeNsec >= 1e9 || s.MtimeNsec >= 1e9 || s.CtimeNsec >= 1e9 {
		return s, errors.New("share: bad nanoseconds")
	}
	return s, r.err
}

type mknodIn struct {
	Mode, Rdev, Umask uint32
	Name              string
}

func parseMknod(b []byte) (mknodIn, error) {
	r := reader{b: b}
	m := mknodIn{Mode: r.u32(), Rdev: r.u32(), Umask: r.u32()}
	r.u32()
	m.Name = r.name()
	return m, r.err
}

type mkdirIn struct {
	Mode, Umask uint32
	Name        string
}

func parseMkdir(b []byte) (mkdirIn, error) {
	r := reader{b: b}
	m := mkdirIn{Mode: r.u32(), Umask: r.u32()}
	m.Name = r.name()
	return m, r.err
}

type renameIn struct {
	NewDir           uint64
	Flags            uint32
	OldName, NewName string
}

func parseRename(b []byte, v2 bool) (renameIn, error) {
	r := reader{b: b}
	m := renameIn{NewDir: r.u64()}
	if v2 {
		m.Flags = r.u32()
		r.u32()
	}
	m.OldName = r.name()
	m.NewName = r.name()
	return m, r.err
}

type linkIn struct {
	OldNode uint64
	Name    string
}

func parseLink(b []byte) (linkIn, error) {
	r := reader{b: b}
	m := linkIn{OldNode: r.u64()}
	m.Name = r.name()
	return m, r.err
}

type symlinkIn struct {
	Name, Target string
}

func parseSymlink(b []byte) (symlinkIn, error) {
	r := reader{b: b}
	m := symlinkIn{}
	m.Name = r.name()
	m.Target = r.cstring(maxSymlinkLen)
	if r.err == nil && m.Target == "" {
		return m, errors.New("share: empty symlink target")
	}
	return m, r.err
}

type openIn struct{ Flags, OpenFlags uint32 }

func parseOpen(b []byte) (openIn, error) {
	r := reader{b: b}
	o := openIn{Flags: r.u32(), OpenFlags: r.u32()}
	return o, r.err
}

type createIn struct {
	Flags, Mode, Umask, OpenFlags uint32
	Name                          string
}

func parseCreate(b []byte) (createIn, error) {
	r := reader{b: b}
	c := createIn{Flags: r.u32(), Mode: r.u32(), Umask: r.u32(), OpenFlags: r.u32()}
	c.Name = r.name()
	return c, r.err
}

type readIn struct {
	Fh, Offset uint64
	Size       uint32
}

func parseRead(b []byte) (readIn, error) {
	r := reader{b: b}
	m := readIn{Fh: r.u64(), Offset: r.u64(), Size: r.u32()}
	return m, r.err
}

type writeIn struct {
	Fh, Offset uint64
	Data       []byte
}

func parseWrite(b []byte) (writeIn, error) {
	r := reader{b: b}
	m := writeIn{Fh: r.u64(), Offset: r.u64()}
	size := r.u32()
	r.u32() // write_flags
	r.u64() // lock_owner
	r.u32() // flags
	r.u32() // padding
	if r.err != nil {
		return m, r.err
	}
	if size > MaxWrite || int(size) != len(r.b) {
		return m, fmt.Errorf("share: write of %d bytes with %d bytes of data", size, len(r.b))
	}
	m.Data = r.b
	return m, nil
}

// fhIn reads the fh that RELEASE, RELEASEDIR, FLUSH, FSYNC and FSYNCDIR start with.
func parseFh(b []byte) (uint64, error) {
	r := reader{b: b}
	fh := r.u64()
	return fh, r.err
}

type forgetOne struct{ Node, N uint64 }

func parseForget(b []byte) (uint64, error) {
	r := reader{b: b}
	n := r.u64()
	return n, r.err
}

func parseBatchForget(b []byte) ([]forgetOne, error) {
	r := reader{b: b}
	count := r.u32()
	r.u32()
	if r.err != nil {
		return nil, r.err
	}
	if count > maxBatchForget || int(count)*16 > len(r.b) {
		return nil, errors.New("share: batch forget count exceeds the frame")
	}
	out := make([]forgetOne, count)
	for i := range out {
		out[i] = forgetOne{Node: r.u64(), N: r.u64()}
	}
	return out, r.err
}

// helloIn is the relay's first frame on each connection: the injector thread id and
// the connection's queue number (0 opens the session).
type helloIn struct {
	InjectorPID uint32
	Queue       uint32
}

func parseHello(b []byte) (helloIn, error) {
	r := reader{b: b}
	h := helloIn{InjectorPID: r.u32()}
	if r.err == nil && len(r.b) >= 4 {
		h.Queue = r.u32()
	}
	return h, r.err
}

// Reply encoding.

// Attr is struct fuse_attr in host-neutral form.
type Attr struct {
	Ino                             uint64
	Size, Blocks                    uint64
	Atime, Mtime, Ctime             uint64 // seconds since the Unix epoch
	AtimeNsec, MtimeNsec, CtimeNsec uint32
	Mode, Nlink, UID, GID, Rdev     uint32
}

// Linux file type bits.
const (
	sIFMT   = 0o170000
	sIFSOCK = 0o140000
	sIFLNK  = 0o120000
	sIFREG  = 0o100000
	sIFDIR  = 0o040000
	sIFIFO  = 0o010000
)

func (a *Attr) isDir() bool { return a.Mode&sIFMT == sIFDIR }

// dtype is the readdir d_type for a mode.
func dtype(mode uint32) uint32 { return (mode & sIFMT) >> 12 }

type writer struct{ b []byte }

func (w *writer) u16(v uint16) { w.b = binary.LittleEndian.AppendUint16(w.b, v) }
func (w *writer) u32(v uint32) { w.b = binary.LittleEndian.AppendUint32(w.b, v) }
func (w *writer) u64(v uint64) { w.b = binary.LittleEndian.AppendUint64(w.b, v) }
func (w *writer) zero(n int) {
	for i := 0; i < n; i++ {
		w.b = append(w.b, 0)
	}
}

func (w *writer) attr(a *Attr) {
	w.u64(a.Ino)
	w.u64(a.Size)
	w.u64(a.Blocks)
	w.u64(a.Atime)
	w.u64(a.Mtime)
	w.u64(a.Ctime)
	w.u32(a.AtimeNsec)
	w.u32(a.MtimeNsec)
	w.u32(a.CtimeNsec)
	w.u32(a.Mode)
	w.u32(a.Nlink)
	w.u32(a.UID)
	w.u32(a.GID)
	w.u32(a.Rdev)
	w.u32(4096) // blksize
	w.u32(0)    // flags
}

func splitTTL(ns uint64) (uint64, uint32) { return ns / 1e9, uint32(ns % 1e9) }

// entryOut appends struct fuse_entry_out.
func (w *writer) entryOut(node, gen uint64, entryTTL, attrTTL uint64, a *Attr) {
	w.u64(node)
	w.u64(gen)
	es, ens := splitTTL(entryTTL)
	as, ans := splitTTL(attrTTL)
	w.u64(es)
	w.u64(as)
	w.u32(ens)
	w.u32(ans)
	w.attr(a)
}

func (w *writer) attrOut(attrTTL uint64, a *Attr) {
	s, ns := splitTTL(attrTTL)
	w.u64(s)
	w.u32(ns)
	w.u32(0)
	w.attr(a)
}

// len32 is the length field of something this package built: a reply, a name, a path or
// a buffer it allocated. They are bounded far below 4 GiB (MaxWrite, maxSymlinkLen, the
// NTFS name limit), whatever the guest sends.
func len32(n int) uint32 {
	return uint32(n) //nolint:gosec // G115: n is a length (never negative) far below 4 GiB, see above
}

// header writes an out header; the length is patched by finish.
func newReply(unique uint64, errno int32) *writer {
	w := &writer{b: make([]byte, 0, 256)}
	w.u32(0)
	w.u32(uint32(errno)) //nolint:gosec // G115: the error field is an int32 on the wire; u32 writes its two's complement
	w.u64(unique)
	return w
}

func (w *writer) finish() []byte {
	binary.LittleEndian.PutUint32(w.b[0:], len32(len(w.b)))
	return w.b
}

// errReply is a reply carrying only -errno.
func errReply(unique uint64, errno int) []byte {
	return newReply(unique, -int32(errno)).finish() //nolint:gosec // G115: errno is a Linux errno (below 4096)
}

// dirent appends a fuse_dirent padded to 8 bytes; it returns false (and appends
// nothing) if it would not fit in max bytes of body.
func (w *writer) dirent(bodyStart, max int, ino, off uint64, typ uint32, name string) bool {
	n := direntLen + len(name)
	pad := (8 - n%8) % 8
	if len(w.b)-bodyStart+n+pad > max {
		return false
	}
	w.u64(ino)
	w.u64(off)
	w.u32(len32(len(name)))
	w.u32(typ)
	w.b = append(w.b, name...)
	w.zero(pad)
	return true
}

func direntSize(name string, plus bool) int {
	n := direntLen + len(name)
	n += (8 - n%8) % 8
	if plus {
		n += entryOutLen
	}
	return n
}

// Notifications (server to guest).

func notifyInvalInodeMsg(node uint64, off, length int64) []byte {
	w := newReply(0, notifyInvalInode)
	w.u64(node)
	w.u64(uint64(off))    //nolint:gosec // G115: fuse_notify_inval_inode_out has s64 fields; u64 writes the same bits
	w.u64(uint64(length)) //nolint:gosec // G115: as above (a negative length means to the end)
	return w.finish()
}

func notifyInvalEntryMsg(parent uint64, name string) []byte {
	w := newReply(0, notifyInvalEntry)
	w.u64(parent)
	w.u32(len32(len(name)))
	w.u32(0)
	w.b = append(w.b, name...)
	w.b = append(w.b, 0)
	return w.finish()
}
