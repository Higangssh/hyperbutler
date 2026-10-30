package share

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Options tune a Server. Zero values pick the defaults in brackets.
type Options struct {
	// EntryTTL and AttrTTL are how long the guest kernel may cache names and attributes
	// without asking again [1 h]. Host changes are pushed as invalidations, so these
	// only bound staleness when a change notification is lost.
	EntryTTL, AttrTTL time.Duration
	// NegTTL is how long a "no such file" answer is cached [same as EntryTTL]. Use a
	// negative value to disable negative caching.
	NegTTL time.Duration
	// NoCache turns off all guest caching: zero TTLs, no page cache reuse across opens.
	// It is the coherence baseline, not a mode anyone should run.
	NoCache bool
	// Writeback lets the guest kernel buffer writes (FUSE_WRITEBACK_CACHE).
	Writeback bool
	// UID and GID own every file the guest sees [0, 0].
	UID, GID uint32
	// Workers is how many requests run at once [16].
	Workers int
	// MaxHandles bounds open files and directories [4096]; MaxNodes bounds the inode
	// table [1 << 20]. Past them the guest gets EMFILE and ENFILE.
	MaxHandles, MaxNodes int
	// Trace, if set, is called after each request with its opcode and host time.
	Trace func(opcode uint32, d time.Duration)
	// NoWatch turns off host change notifications (for measuring without them).
	NoWatch bool
	// Logger receives protocol errors [discarded].
	Logger *slog.Logger
}

func (o *Options) defaults() {
	if o.EntryTTL == 0 {
		o.EntryTTL = time.Hour
	}
	if o.AttrTTL == 0 {
		o.AttrTTL = time.Hour
	}
	if o.NegTTL == 0 {
		o.NegTTL = o.EntryTTL
	}
	if o.NoCache {
		o.EntryTTL, o.AttrTTL, o.NegTTL = 0, 0, -1
		o.Writeback = false
	}
	if o.Workers <= 0 {
		o.Workers = 16
	}
	if o.MaxHandles <= 0 {
		o.MaxHandles = 4096
	}
	if o.MaxNodes <= 0 {
		o.MaxNodes = 1 << 20
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
}

// Share is one exported host directory.
type Share struct {
	// Name is the share's entry in the FUSE root (and so its guest path under the
	// mount point). It must pass checkName.
	Name string
	FS   FS
	// ReadOnly refuses every change.
	ReadOnly bool
	// Only, if set, limits the share root to these names: a single-file bind mount
	// shares the file's directory but shows only the file.
	Only []string
}

const rootNode = 1

type nodeKey struct {
	parent uint64
	name   string
}

type node struct {
	id      uint64
	parent  uint64
	name    string
	share   *Share
	nlookup uint64
	ino     uint64
	dir     bool
	orphan  bool      // unlinked or replaced; its path is gone
	touched time.Time // last change made by the guest itself (inject.go)
	// inner is the last change the guest made inside this directory: the host then
	// reports the directory itself as modified (its times changed), which is an echo
	// too (inject.go).
	inner time.Time
	// list is a directory's listing from its last READDIR at offset 0, for the
	// READDIRs that continue it (nodeListing). Counted in Server.dirEntries.
	list   []DirEntry
	listed bool
}

type handle struct {
	node   uint64
	f      File
	isDir  bool
	inject bool
	// refs counts requests using f; the file is closed when the handle is released
	// and refs is zero, so a RELEASE racing a READ or WRITE on the same fh can never
	// let the I/O land on a recycled Windows handle value. All under Server.mu.
	refs   int
	closed bool

	// A directory is listed on its first READDIR, not at OPENDIR: with the guest's
	// readdir cache the kernel often opens a directory without reading it.
	dmu    sync.Mutex
	loaded bool
	dir    []DirEntry
}

// maxDirBudget bounds directory entries held in listing snapshots (nodes and directory
// handles) together.
var maxDirBudget = 4 << 20

// closeLocked marks hd released and closes its file if no request uses it.
func (s *Server) closeLocked(hd *handle) {
	if hd.closed {
		return
	}
	hd.closed = true
	if hd.node != rootNode {
		s.dirEntries -= len(hd.dir)
	}
	if hd.refs == 0 && hd.f != nil {
		hd.f.Close()
	}
}

// acquire returns the file of fh with a reference held; call done when finished.
func (s *Server) acquire(fh uint64) (*handle, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hd := s.handles[fh]
	if hd == nil || hd.isDir || (hd.f == nil && !hd.inject) {
		return nil, nil, EBADF
	}
	hd.refs++
	return hd, func() {
		s.mu.Lock()
		hd.refs--
		if hd.closed && hd.refs == 0 && hd.f != nil {
			hd.f.Close()
		}
		s.mu.Unlock()
	}, nil
}

// Server serves a set of shares to one guest connection at a time.
type Server struct {
	opt Options

	mu       sync.Mutex
	shares   map[string]*Share
	nodes    map[uint64]*node
	index    map[nodeKey]uint64
	nextNode uint64
	handles  map[uint64]*handle
	nextFh   uint64
	negative map[nodeKey]time.Time // names answered "missing" (for full invalidation)
	recent   map[nodeKey]time.Time // names the guest itself changed (echo suppression)
	watchers map[string]Closer

	// The live connection, for notifications.
	cmu        sync.Mutex
	conn       *conn
	nconns     int    // live connections, bounded by 2*maxQueues
	injector   uint32 // guest thread id that replays host events
	pending    map[nodeKey]*injection
	dirEntries int // entries held by listing snapshots (maxDirBudget)
}

// NewServer returns a server with no shares.
func NewServer(opt Options) *Server {
	opt.defaults()
	s := &Server{
		opt:      opt,
		shares:   map[string]*Share{},
		nextNode: rootNode + 1,
		nextFh:   1,
		negative: map[nodeKey]time.Time{},
		recent:   map[nodeKey]time.Time{},
		pending:  map[nodeKey]*injection{},
		watchers: map[string]Closer{},
	}
	s.resetNodes()
	return s
}

func (s *Server) resetNodes() {
	s.nodes = map[uint64]*node{rootNode: {id: rootNode, dir: true, nlookup: 1, ino: rootNode}}
	s.index = map[nodeKey]uint64{}
	for _, h := range s.handles {
		s.closeLocked(h)
	}
	s.handles = map[uint64]*handle{}
	s.dirEntries = 0 // every node and handle that held a listing is gone
}

// AddShare registers sh. The guest sees it at once.
func (s *Server) AddShare(sh *Share) error {
	if err := checkName(sh.Name); err != nil {
		return fmt.Errorf("share: bad share name %q", sh.Name)
	}
	for _, n := range sh.Only {
		if err := checkName(n); err != nil {
			return fmt.Errorf("share: bad name %q in Only", n)
		}
	}
	s.mu.Lock()
	if _, ok := s.shares[sh.Name]; ok {
		s.mu.Unlock()
		return fmt.Errorf("share: %q is already shared", sh.Name)
	}
	s.shares[sh.Name] = sh
	s.mu.Unlock()
	if w, ok := sh.FS.(watchable); ok && !s.opt.NoWatch {
		name := sh.Name
		c, err := w.Watch(func(k EventKind, p, np []string) {
			s.HostChanged(HostEvent{Share: name, Path: p, NewPath: np, Kind: k})
		}, func() { s.HostOverflow(name) })
		if err != nil {
			s.opt.Logger.Warn("share: no change notifications", "share", name, "err", err)
		} else {
			s.mu.Lock()
			s.watchers[name] = c
			s.mu.Unlock()
		}
	}
	s.notify(notifyInvalEntryMsg(rootNode, sh.Name))
	s.notify(notifyInvalInodeMsg(rootNode, 0, 0))
	return nil
}

// RemoveShare unregisters a share and closes its FS. Guest processes that still hold
// it see ENOENT or EIO.
func (s *Server) RemoveShare(name string) error {
	s.mu.Lock()
	w := s.watchers[name]
	delete(s.watchers, name)
	s.mu.Unlock()
	if w != nil {
		w.Close()
	}
	s.mu.Lock()
	sh, ok := s.shares[name]
	if ok {
		delete(s.shares, name)
		for _, n := range s.nodes {
			if n.share == sh {
				n.orphan = true
				delete(s.index, nodeKey{n.parent, n.name})
			}
		}
		for fh, h := range s.handles {
			if n := s.nodes[h.node]; n != nil && n.share == sh {
				if h.f != nil {
					h.f.Close()
				}
				delete(s.handles, fh)
			}
		}
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("share: %q is not shared", name)
	}
	s.notify(notifyInvalEntryMsg(rootNode, name))
	s.notify(notifyInvalInodeMsg(rootNode, 0, 0))
	return sh.FS.Close()
}

// watchable is an FS that reports host changes (NTFS on Windows).
type watchable interface {
	Watch(fn func(kind EventKind, path, newPath []string), overflow func()) (Closer, error)
}

// Closer stops a watcher.
type Closer interface{ Close() error }

// Close removes every share.
func (s *Server) Close() error {
	for _, n := range s.Shares() {
		s.RemoveShare(n)
	}
	return nil
}

// Mounted reports whether a guest session is live: a queue-0 connection is being
// served, so the guest has (or is about to have) the file system mounted.
func (s *Server) Mounted() bool {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	return s.conn != nil
}

// Shares returns the registered share names.
func (s *Server) Shares() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for n := range s.shares {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// conn is one guest connection.
type conn struct {
	rw  io.ReadWriter
	wmu sync.Mutex
	err error
	// Notifications (queue 0 only) go through notes to a writer goroutine, so a guest
	// that stops reading can never block the host watcher, RemoveShare or Close.
	notes   chan []byte
	done    chan struct{}
	dropped atomic.Int64
}

// maxPendingNotes bounds queued notifications per session. Past it they are dropped:
// the guest may serve stale data until its cache times out (Options.AttrTTL).
const maxPendingNotes = 1 << 16

func (c *conn) noteWriter() {
	for {
		select {
		case m := <-c.notes:
			if c.write(m) != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

// queueNote queues a notification without blocking.
func (c *conn) queueNote(m []byte) {
	if c.notes == nil {
		c.write(m) // tests drive a bare conn
		return
	}
	select {
	case c.notes <- m:
	default:
		c.dropped.Add(1)
	}
}

func (c *conn) write(b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.err != nil {
		return c.err
	}
	_, err := c.rw.Write(b)
	if err != nil {
		c.err = err
	}
	return err
}

// readFrame reads one request frame from rw.
func readFrame(rw io.Reader, hdr []byte) ([]byte, error) {
	if _, err := io.ReadFull(rw, hdr); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(hdr)
	if n < inHeaderLen || n > MaxRequest {
		return nil, fmt.Errorf("share: request length %d", n)
	}
	b := make([]byte, n)
	copy(b, hdr)
	if _, err := io.ReadFull(rw, b[inHeaderLen:]); err != nil {
		return nil, err
	}
	return b, nil
}

// Serve speaks FUSE over rw until it fails or the guest unmounts.
//
// A guest mount may use several connections (queues) for throughput: hvsock bandwidth
// grows almost linearly with parallel connections. Each one starts with a HELLO frame
// naming its queue number. Queue 0 starts a session: the inode table starts over, as
// the kernel's does after a remount, and notifications go out on it. Other queues join
// the live session. A connection without HELLO is queue 0.
func (s *Server) Serve(rw io.ReadWriter) error {
	c := &conn{rw: rw}
	hdr := make([]byte, inHeaderLen)
	first, err := readFrame(rw, hdr)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	var hello helloIn
	isHello := binary.LittleEndian.Uint32(first[4:]) == opHello
	if isHello {
		req, err := parseRequest(first)
		if err == nil {
			hello, err = parseHello(req.body)
		}
		if err != nil {
			return fmt.Errorf("share: bad HELLO: %w", err)
		}
	}
	s.cmu.Lock()
	if s.nconns >= 2*maxQueues {
		s.cmu.Unlock()
		return errors.New("share: too many connections")
	}
	s.nconns++
	s.cmu.Unlock()
	defer func() {
		s.cmu.Lock()
		s.nconns--
		s.cmu.Unlock()
	}()
	if hello.Queue == 0 {
		// A new queue 0 takes over (a guest reboot or relay restart); the old session's
		// connections then work on a fresh table and fail with ENOENT, as after a
		// remount. Only the current session's end resets the table.
		s.mu.Lock()
		s.resetNodes()
		s.mu.Unlock()
		c.notes = make(chan []byte, maxPendingNotes)
		c.done = make(chan struct{})
		go c.noteWriter()
		s.cmu.Lock()
		s.conn = c
		s.injector = hello.InjectorPID
		s.cmu.Unlock()
		defer func() {
			close(c.done)
			s.cmu.Lock()
			current := s.conn == c
			if current {
				s.conn = nil
				s.injector = 0
			}
			s.cmu.Unlock()
			if current {
				s.mu.Lock()
				s.resetNodes()
				s.mu.Unlock()
			}
		}()
	} else {
		// Queue 0 may still be on its way: the connections are accepted in any order.
		live := false
		for deadline := time.Now().Add(5 * time.Second); hello.Queue < maxQueues && time.Now().Before(deadline); {
			s.cmu.Lock()
			live = s.conn != nil
			s.cmu.Unlock()
			if live {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !live {
			return fmt.Errorf("share: queue %d without a session", hello.Queue)
		}
	}

	work := make(chan []byte)
	var wg sync.WaitGroup
	for i := 0; i < s.opt.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range work {
				s.handle(c, b)
			}
		}()
	}
	defer func() {
		close(work)
		wg.Wait()
	}()
	if !isHello {
		work <- first
	}
	for {
		b, err := readFrame(rw, hdr)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if op := binary.LittleEndian.Uint32(b[4:]); op == opDestroy {
			return nil
		}
		work <- b
	}
}

// maxQueues bounds the connections one guest mount may use. With Workers frames in
// flight per queue, memory for requests stays below maxQueues*(Workers+1)*MaxRequest.
const maxQueues = 8

// notify sends a notification on the live connection, if any. It never blocks a
// request: the guest relay writes notifications to /dev/fuse from its own queue.
func (s *Server) notify(msg []byte) {
	s.cmu.Lock()
	c := s.conn
	s.cmu.Unlock()
	if c != nil {
		c.queueNote(msg)
	}
}

func (s *Server) handle(c *conn, b []byte) {
	req, err := parseRequest(b)
	if err != nil {
		s.opt.Logger.Debug("share: bad frame", "err", err)
		if len(b) >= inHeaderLen {
			c.write(errReply(binary.LittleEndian.Uint64(b[8:]), int(EINVAL)))
		}
		return
	}
	t0 := time.Now()
	reply := s.dispatch(&req)
	if s.opt.Trace != nil {
		s.opt.Trace(req.h.Opcode, time.Since(t0))
	}
	if reply != nil {
		c.write(reply)
	}
}

func (s *Server) ttl(d time.Duration) uint64 {
	if d <= 0 {
		return 0
	}
	return uint64(d.Nanoseconds())
}

func (s *Server) isInjector(h *inHeader) bool {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	return s.injector != 0 && h.PID == s.injector
}

// dispatch runs one request and returns its reply (nil for requests without one).
func (s *Server) dispatch(r *request) []byte {
	h := &r.h
	u := h.Unique
	fail := func(err error) []byte { return errReply(u, toErrno(err)) }
	switch h.Opcode {
	case opInit:
		return s.doInit(r)
	case opHello:
		in, err := parseHello(r.body)
		if err != nil {
			return nil
		}
		s.cmu.Lock()
		s.injector = in.InjectorPID
		s.cmu.Unlock()
		return nil
	case opForget:
		n, err := parseForget(r.body)
		if err == nil {
			s.forget(h.NodeID, n)
		}
		return nil
	case opBatchForget:
		list, err := parseBatchForget(r.body)
		if err == nil {
			for _, f := range list {
				s.forget(f.Node, f.N)
			}
		}
		return nil
	case opInterrupt:
		return nil
	case opLookup:
		name, err := oneName(r.body)
		if err != nil {
			return fail(err)
		}
		return s.doLookup(h, name)
	case opGetattr:
		a, err := s.getattr(h.NodeID)
		if err != nil {
			return fail(err)
		}
		w := newReply(u, 0)
		w.attrOut(s.ttl(s.opt.AttrTTL), &a)
		return w.finish()
	case opSetattr:
		in, err := parseSetattr(r.body)
		if err != nil {
			return fail(EINVAL)
		}
		return s.doSetattr(h, in)
	case opReadlink:
		path, sh, err := s.pathOf(h.NodeID)
		if err != nil {
			return fail(err)
		}
		t, err := sh.FS.Readlink(path)
		if err != nil {
			return fail(err)
		}
		w := newReply(u, 0)
		w.b = append(w.b, t...)
		return w.finish()
	case opMknod:
		in, err := parseMknod(r.body)
		if err != nil {
			return fail(EINVAL)
		}
		return s.doCreateNode(h, in.Name, func(sh *Share, p []string) (Attr, error) {
			switch in.Mode & sIFMT {
			case sIFREG:
				f, a, err := sh.FS.Create(p, oWRONLY|oCREAT|oEXCL, in.Mode&^in.Umask, owner(h))
				if err == nil {
					f.Close()
				}
				return a, err
			case sIFIFO, sIFSOCK:
				return sh.FS.Mknod(p, in.Mode&^in.Umask, owner(h))
			}
			return Attr{}, EPERM
		})
	case opMkdir:
		in, err := parseMkdir(r.body)
		if err != nil {
			return fail(EINVAL)
		}
		return s.doCreateNode(h, in.Name, func(sh *Share, p []string) (Attr, error) {
			return sh.FS.Mkdir(p, in.Mode&^in.Umask, owner(h))
		})
	case opSymlink:
		in, err := parseSymlink(r.body)
		if err != nil {
			return fail(EINVAL)
		}
		return s.doCreateNode(h, in.Name, func(sh *Share, p []string) (Attr, error) {
			return sh.FS.Symlink(p, in.Target, owner(h))
		})
	case opLink:
		in, err := parseLink(r.body)
		if err != nil {
			return fail(EINVAL)
		}
		oldPath, oldShare, err := s.pathOf(in.OldNode)
		if err != nil {
			return fail(err)
		}
		return s.doCreateNode(h, in.Name, func(sh *Share, p []string) (Attr, error) {
			if sh != oldShare {
				return Attr{}, EXDEV
			}
			return sh.FS.Link(oldPath, p)
		})
	case opUnlink, opRmdir:
		name, err := oneName(r.body)
		if err != nil {
			return fail(err)
		}
		return s.doRemove(h, name, h.Opcode == opRmdir)
	case opRename, opRename2:
		in, err := parseRename(r.body, h.Opcode == opRename2)
		if err != nil {
			return fail(EINVAL)
		}
		return s.doRename(h, in)
	case opOpen, opOpendir:
		in, err := parseOpen(r.body)
		if err != nil {
			return fail(EINVAL)
		}
		return s.doOpen(h, in.Flags, h.Opcode == opOpendir)
	case opCreate:
		in, err := parseCreate(r.body)
		if err != nil {
			return fail(EINVAL)
		}
		return s.doCreate(h, in)
	case opRead:
		in, err := parseRead(r.body)
		if err != nil || in.Size > MaxWrite || int64(in.Offset) < 0 {
			// An offset past 2^63 would reach ReadAt as a negative int64 (found by
			// FuzzDispatch).
			return fail(EINVAL)
		}
		f, done, err := s.file(in.Fh)
		if err != nil {
			return fail(err)
		}
		defer done()
		w := newReply(u, 0)
		start := len(w.b)
		w.b = append(w.b, make([]byte, in.Size)...)
		n, err := f.ReadAt(w.b[start:], int64(in.Offset))
		if err != nil && !errors.Is(err, io.EOF) && n == 0 {
			return fail(err)
		}
		w.b = w.b[:start+n]
		return w.finish()
	case opWrite:
		in, err := parseWrite(r.body)
		if err != nil {
			return fail(EINVAL)
		}
		if int64(in.Offset) < 0 {
			return fail(EFBIG)
		}
		f, done, err := s.writableFile(in.Fh)
		if err != nil {
			return fail(err)
		}
		defer done()
		s.touch(h.NodeID)
		n, err := f.WriteAt(in.Data, int64(in.Offset))
		if err != nil && n == 0 {
			return fail(err)
		}
		s.touch(h.NodeID)
		w := newReply(u, 0)
		w.u32(uint32(n))
		w.u32(0)
		return w.finish()
	case opStatfs:
		st, err := s.statfs(h.NodeID)
		if err != nil {
			return fail(err)
		}
		w := newReply(u, 0)
		w.u64(st.Blocks)
		w.u64(st.Bfree)
		w.u64(st.Bavail)
		w.u64(st.Files)
		w.u64(st.Ffree)
		w.u32(st.Bsize)
		w.u32(st.NameLen)
		w.u32(st.Frsize)
		w.u32(0)
		w.zero(24)
		return w.finish()
	case opRelease, opReleasedir:
		fh, err := parseFh(r.body)
		if err != nil {
			return fail(EINVAL)
		}
		s.release(fh)
		return newReply(u, 0).finish()
	case opFlush:
		return newReply(u, 0).finish()
	case opFsync:
		fh, err := parseFh(r.body)
		if err != nil {
			return fail(EINVAL)
		}
		f, done, err := s.file(fh)
		if err != nil {
			return fail(err)
		}
		defer done()
		if err := f.Sync(); err != nil {
			return fail(err)
		}
		return newReply(u, 0).finish()
	case opFsyncdir:
		return newReply(u, 0).finish()
	case opReaddir, opReaddirplus:
		in, err := parseRead(r.body)
		if err != nil || in.Size > MaxWrite {
			return fail(EINVAL)
		}
		return s.doReaddir(h, in, h.Opcode == opReaddirplus)
	case opAccess:
		return newReply(u, 0).finish()
	case opDestroy:
		return nil
	}
	// Extended attributes, locks, ioctl, poll, bmap, fallocate, lseek, copy_file_range,
	// statx, syncfs and anything newer: the kernel falls back or reports EOPNOTSUPP.
	return errReply(u, int(ENOSYS))
}

// Linux open flags used here (x86-64 and arm64 share them).
const (
	oACCMODE = 0o3
	oRDONLY  = 0o0
	oWRONLY  = 0o1
	oRDWR    = 0o2
	oCREAT   = 0o100
	oEXCL    = 0o200
	oTRUNC   = 0o1000
	oAPPEND  = 0o2000
)

func oneName(b []byte) (string, error) {
	r := reader{b: b}
	n := r.name()
	if r.err != nil {
		if errors.Is(r.err, errBadName) {
			return "", EINVAL
		}
		return "", EINVAL
	}
	return n, nil
}

func (s *Server) doInit(r *request) []byte {
	in, err := parseInit(r.body)
	if err != nil || in.Major != kernelMajor || in.Minor < minMinor {
		// Major mismatch or a kernel older than 5.4: refuse.
		return errReply(r.h.Unique, int(EPROTO))
	}
	want := uint32(initAsyncRead | initAtomicOTrunc | initBigWrites | initDoReaddirplus |
		initReaddirplusAuto | initParallelDirops | initMaxPages | initCacheSymlinks | initAutoInvalData | initAsyncDIO)
	if s.opt.Writeback {
		want |= initWritebackCache
	}
	if s.opt.NoCache {
		want &^= initCacheSymlinks
	}
	w := newReply(r.h.Unique, 0)
	w.u32(kernelMajor)
	w.u32(min(in.Minor, kernelMinor))
	w.u32(in.MaxReadahead)
	w.u32(in.Flags & want)
	w.u16(64) // max_background
	w.u16(48) // congestion_threshold
	w.u32(MaxWrite)
	w.u32(100) // time_gran: NTFS keeps 100 ns
	w.u16(MaxWrite / 4096)
	w.u16(0)
	w.zero(64 - 32) // flags2, max_stack_depth, request_timeout, unused
	s.cmu.Lock()
	s.cmu.Unlock()
	return w.finish()
}

// EPROTO is not in fs.go's list because only INIT returns it.
const EPROTO Errno = 71

// node helpers. All of them take s.mu.

// pathOf returns the share-relative path of a node.
func (s *Server) pathOf(id uint64) ([]string, *Share, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pathLocked(id)
}

func (s *Server) pathLocked(id uint64) ([]string, *Share, error) {
	var rev []string
	for depth := 0; ; depth++ {
		n := s.nodes[id]
		if n == nil || n.orphan {
			return nil, nil, ENOENT
		}
		if id == rootNode {
			return nil, nil, EPERM // the virtual root has no host path
		}
		if n.parent == rootNode {
			// A share root.
			out := make([]string, len(rev))
			for i := range rev {
				out[i] = rev[len(rev)-1-i]
			}
			return out, n.share, nil
		}
		rev = append(rev, n.name)
		id = n.parent
		if depth > 4096 {
			return nil, nil, ELOOP
		}
	}
}

// child returns the node for (parent, name), creating it, and counts one lookup.
func (s *Server) childLocked(parent uint64, name string, sh *Share, a *Attr) (*node, error) {
	k := nodeKey{parent, name}
	if id, ok := s.index[k]; ok {
		n := s.nodes[id]
		if n.ino == a.Ino && n.dir == a.isDir() {
			n.nlookup++
			return n, nil
		}
		// Replaced on the host by a different file: the old node becomes an orphan.
		n.orphan = true
		delete(s.index, k)
	}
	if len(s.nodes) >= s.opt.MaxNodes {
		return nil, ENFILE
	}
	n := &node{id: s.nextNode, parent: parent, name: name, share: sh, nlookup: 1, ino: a.Ino, dir: a.isDir()}
	s.nextNode++
	s.nodes[n.id] = n
	s.index[k] = n.id
	delete(s.negative, k)
	return n, nil
}

func (s *Server) forget(id, n uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	nd := s.nodes[id]
	if nd == nil || id == rootNode {
		return
	}
	if n >= nd.nlookup {
		s.dropListLocked(nd)
		delete(s.nodes, id)
		if !nd.orphan && s.index[nodeKey{nd.parent, nd.name}] == id {
			delete(s.index, nodeKey{nd.parent, nd.name})
		}
		return
	}
	nd.nlookup -= n
}

func (s *Server) rootAttr() Attr {
	return Attr{Ino: rootNode, Mode: sIFDIR | 0o555, Nlink: 2, UID: s.opt.UID, GID: s.opt.GID}
}

func owner(h *inHeader) Owner { return Owner{UID: h.UID, GID: h.GID} }

func (s *Server) fixAttr(a *Attr) {
	if a.UID == NoID {
		a.UID = s.opt.UID
	}
	if a.GID == NoID {
		a.GID = s.opt.GID
	}
	if a.Nlink == 0 {
		a.Nlink = 1
	}
}

func (s *Server) getattr(id uint64) (Attr, error) {
	if id == rootNode {
		return s.rootAttr(), nil
	}
	path, sh, err := s.pathOf(id)
	if err != nil {
		return Attr{}, err
	}
	a, err := sh.FS.Lstat(path)
	if err != nil {
		return Attr{}, err
	}
	if sh.ReadOnly {
		a.Mode &^= 0o222
	}
	s.fixAttr(&a)
	return a, nil
}

func (s *Server) statfs(id uint64) (Statfs, error) {
	if id == rootNode {
		return Statfs{Bsize: 4096, Frsize: 4096, NameLen: maxNameLen}, nil
	}
	_, sh, err := s.pathOf(id)
	if err != nil {
		return Statfs{}, err
	}
	return sh.FS.Statfs()
}

// allowed applies Share.Only at the share root.
func allowed(sh *Share, parentIsShareRoot bool, name string) bool {
	if sh.Only == nil || !parentIsShareRoot {
		return true
	}
	for _, n := range sh.Only {
		if n == name {
			return true
		}
	}
	return false
}

func (s *Server) entryReply(u uint64, n *node, a *Attr) []byte {
	w := newReply(u, 0)
	w.entryOut(n.id, 0, s.ttl(s.opt.EntryTTL), s.ttl(s.opt.AttrTTL), a)
	return w.finish()
}

func (s *Server) doLookup(h *inHeader, name string) []byte {
	u := h.Unique
	if h.NodeID == rootNode {
		s.mu.Lock()
		sh := s.shares[name]
		s.mu.Unlock()
		if sh == nil {
			return errReply(u, int(ENOENT))
		}
		a, err := sh.FS.Lstat(nil)
		if err != nil {
			return errReply(u, toErrno(err))
		}
		if !a.isDir() {
			return errReply(u, int(ENOTDIR))
		}
		if sh.ReadOnly {
			a.Mode &^= 0o222
		}
		s.fixAttr(&a)
		s.mu.Lock()
		n, err := s.childLocked(rootNode, name, sh, &a)
		s.mu.Unlock()
		if err != nil {
			return errReply(u, toErrno(err))
		}
		return s.entryReply(u, n, &a)
	}
	if s.isInjector(h) {
		return s.injectLookup(h, name)
	}
	return s.lookupBody(h, name)
}

// lookupBody is LOOKUP below a share root.
func (s *Server) lookupBody(h *inHeader, name string) []byte {
	u := h.Unique
	s.mu.Lock()
	parent, sh, err := s.pathLocked(h.NodeID)
	if err == nil && !s.nodes[h.NodeID].dir {
		err = ENOTDIR
	}
	isShareRoot := err == nil && len(parent) == 0
	s.mu.Unlock()
	if err != nil {
		return errReply(u, toErrno(err))
	}
	if !allowed(sh, isShareRoot, name) {
		return errReply(u, int(ENOENT))
	}
	path := append(parent, name)
	a, err := sh.FS.Lstat(path)
	if err != nil {
		if errors.Is(err, ENOENT) && s.opt.NegTTL > 0 {
			s.mu.Lock()
			if len(s.negative) < s.opt.MaxNodes {
				s.negative[nodeKey{h.NodeID, name}] = time.Now()
			}
			s.mu.Unlock()
			w := newReply(u, 0)
			w.entryOut(0, 0, s.ttl(s.opt.NegTTL), 0, &Attr{})
			return w.finish()
		}
		return errReply(u, toErrno(err))
	}
	if sh.ReadOnly {
		a.Mode &^= 0o222
	}
	s.fixAttr(&a)
	s.mu.Lock()
	n, err := s.childLocked(h.NodeID, name, sh, &a)
	s.mu.Unlock()
	if err != nil {
		return errReply(u, toErrno(err))
	}
	return s.entryReply(u, n, &a)
}

// parentFor resolves the directory a create, remove or rename works in and checks that
// the share takes changes there.
func (s *Server) parentFor(id uint64, name string) ([]string, *Share, error) {
	if id == rootNode {
		return nil, nil, EACCES
	}
	s.mu.Lock()
	path, sh, err := s.pathLocked(id)
	if err == nil && !s.nodes[id].dir {
		// The kernel never sends this; a hostile guest could, to make the host walk
		// through something that is not a directory to it (such as a junction).
		err = ENOTDIR
	}
	s.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	if sh.ReadOnly {
		return nil, nil, EROFS
	}
	if sh.Only != nil && len(path) == 0 {
		// A single-file share: the file may change, its directory may not.
		return nil, nil, EACCES
	}
	return append(path, name), sh, nil
}

func (s *Server) doCreateNode(h *inHeader, name string, mk func(*Share, []string) (Attr, error)) []byte {
	u := h.Unique
	if s.isInjector(h) {
		return s.injectCreate(h, name)
	}
	path, sh, err := s.parentFor(h.NodeID, name)
	if err != nil {
		return errReply(u, toErrno(err))
	}
	s.markRecent(h.NodeID, name)
	a, err := mk(sh, path)
	if err != nil {
		return errReply(u, toErrno(err))
	}
	s.fixAttr(&a)
	s.mu.Lock()
	n, err := s.childLocked(h.NodeID, name, sh, &a)
	s.markRecentLocked(h.NodeID, name)
	if n != nil {
		n.touched = time.Now()
	}
	s.mu.Unlock()
	if err != nil {
		return errReply(u, toErrno(err))
	}
	return s.entryReply(u, n, &a)
}

func (s *Server) doRemove(h *inHeader, name string, dir bool) []byte {
	u := h.Unique
	if s.isInjector(h) {
		return s.injectRemove(h, name)
	}
	path, sh, err := s.parentFor(h.NodeID, name)
	if err != nil {
		return errReply(u, toErrno(err))
	}
	s.markRecent(h.NodeID, name)
	if dir {
		err = sh.FS.Rmdir(path)
	} else {
		err = sh.FS.Unlink(path)
	}
	if err != nil {
		return errReply(u, toErrno(err))
	}
	s.mu.Lock()
	k := nodeKey{h.NodeID, name}
	if id, ok := s.index[k]; ok {
		s.nodes[id].orphan = true
		delete(s.index, k)
	}
	s.markRecentLocked(h.NodeID, name)
	s.mu.Unlock()
	return newReply(u, 0).finish()
}

func (s *Server) doRename(h *inHeader, in renameIn) []byte {
	u := h.Unique
	if in.Flags&(renameExchange|renameWhiteout) != 0 {
		return errReply(u, int(EINVAL))
	}
	if s.isInjector(h) {
		return s.injectRename(h, in)
	}
	oldPath, sh, err := s.parentFor(h.NodeID, in.OldName)
	if err != nil {
		return errReply(u, toErrno(err))
	}
	newPath, sh2, err := s.parentFor(in.NewDir, in.NewName)
	if err != nil {
		return errReply(u, toErrno(err))
	}
	if sh != sh2 {
		return errReply(u, int(EXDEV))
	}
	s.markRecent(h.NodeID, in.OldName)
	s.markRecent(in.NewDir, in.NewName)
	if err := sh.FS.Rename(oldPath, newPath, in.Flags&renameNoreplace != 0); err != nil {
		return errReply(u, toErrno(err))
	}
	s.mu.Lock()
	s.moveLocked(h.NodeID, in.OldName, in.NewDir, in.NewName)
	s.markRecentLocked(h.NodeID, in.OldName)
	s.markRecentLocked(in.NewDir, in.NewName)
	s.mu.Unlock()
	return newReply(u, 0).finish()
}

// moveLocked updates the table after a rename: the node at the target is replaced.
func (s *Server) moveLocked(oldDir uint64, oldName string, newDir uint64, newName string) {
	nk := nodeKey{newDir, newName}
	if id, ok := s.index[nk]; ok {
		s.nodes[id].orphan = true
		delete(s.index, nk)
	}
	ok := nodeKey{oldDir, oldName}
	if id, found := s.index[ok]; found {
		delete(s.index, ok)
		n := s.nodes[id]
		n.parent, n.name = newDir, newName
		s.index[nk] = id
	}
}

func (s *Server) doSetattr(h *inHeader, in setattrIn) []byte {
	u := h.Unique
	if s.isInjector(h) {
		return s.injectSetattr(h)
	}
	if h.NodeID == rootNode {
		return errReply(u, int(EPERM))
	}
	path, sh, err := s.pathOf(h.NodeID)
	if err != nil {
		return errReply(u, toErrno(err))
	}
	if sh.ReadOnly {
		return errReply(u, int(EROFS))
	}
	sa := SetAttr{Valid: in.Valid}
	if in.Valid&fattrSize != 0 {
		if int64(in.Size) < 0 {
			return errReply(u, int(EFBIG))
		}
		sa.SetSize, sa.Size = true, in.Size
	}
	if in.Valid&fattrMode != 0 {
		sa.SetMode, sa.Mode = true, in.Mode
	}
	if in.Valid&(fattrUID|fattrGID) != 0 {
		sa.SetOwner, sa.UID, sa.GID = true, in.UID, in.GID
	}
	ts := func(sec uint64, nsec uint32, now bool) int64 {
		if now {
			return -1
		}
		if sec > 1<<40 {
			return 1 << 62
		}
		return int64(sec)*1e9 + int64(nsec)
	}
	if in.Valid&(fattrAtime|fattrAtimeNow) != 0 {
		sa.SetAtime, sa.Atime = true, ts(in.Atime, in.AtimeNsec, in.Valid&fattrAtimeNow != 0)
	}
	if in.Valid&(fattrMtime|fattrMtimeNow) != 0 {
		sa.SetMtime, sa.Mtime = true, ts(in.Mtime, in.MtimeNsec, in.Valid&fattrMtimeNow != 0)
	}
	// The fh the kernel may send is not used: setattr opens the path itself, so no
	// handle shared with READ/WRITE is touched here.
	s.touch(h.NodeID)
	a, err := sh.FS.Setattr(path, nil, sa)
	if err != nil {
		return errReply(u, toErrno(err))
	}
	s.touch(h.NodeID)
	s.fixAttr(&a)
	w := newReply(u, 0)
	w.attrOut(s.ttl(s.opt.AttrTTL), &a)
	return w.finish()
}

func (s *Server) addHandle(hd *handle) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.handles) >= s.opt.MaxHandles {
		return 0, EMFILE
	}
	fh := s.nextFh
	s.nextFh++
	s.handles[fh] = hd
	return fh, nil
}

func (s *Server) openReply(u, fh uint64, dir bool) []byte {
	w := newReply(u, 0)
	w.u64(fh)
	fl := uint32(fopenNoFlush)
	if !s.opt.NoCache {
		fl |= fopenKeepCache
		if dir {
			fl |= fopenCacheDir
		}
	} else if !dir {
		fl |= fopenDirectIO
	}
	w.u32(fl)
	w.u32(0)
	return w.finish()
}

func (s *Server) doOpen(h *inHeader, flags uint32, dir bool) []byte {
	u := h.Unique
	if !dir && s.isInjector(h) {
		fh, err := s.addHandle(&handle{node: h.NodeID, inject: true})
		if err != nil {
			return errReply(u, toErrno(err))
		}
		return s.openReply(u, fh, false)
	}
	if dir && !s.opt.NoCache {
		// No directory handles: the kernel then stops sending OPENDIR and RELEASEDIR
		// (two round trips per directory a walk opens) and opens every directory with
		// FOPEN_KEEP_CACHE | FOPEN_CACHE_DIR, so a warm walk is answered from its
		// readdir cache. READDIR comes with fh 0 (nodeListing). Host changes reach
		// that cache as FUSE_NOTIFY_INVAL_INODE of the directory, which drops it.
		return errReply(u, int(ENOSYS))
	}
	if h.NodeID == rootNode {
		if !dir {
			return errReply(u, int(EISDIR))
		}
		s.mu.Lock()
		var es []DirEntry
		for name, sh := range s.shares {
			_ = sh
			es = append(es, DirEntry{Name: name, Attr: Attr{Mode: sIFDIR | 0o755}})
		}
		s.mu.Unlock()
		sort.Slice(es, func(i, j int) bool { return es[i].Name < es[j].Name })
		fh, err := s.addHandle(&handle{node: rootNode, isDir: true, dir: es, loaded: true})
		if err != nil {
			return errReply(u, toErrno(err))
		}
		// The root changes when shares are added; never cache its listing.
		w := newReply(u, 0)
		w.u64(fh)
		w.u32(0)
		w.u32(0)
		return w.finish()
	}
	s.mu.Lock()
	path, sh, err := s.pathLocked(h.NodeID)
	s.mu.Unlock()
	if err != nil {
		return errReply(u, toErrno(err))
	}
	if dir {
		s.mu.Lock()
		isDir := s.nodes[h.NodeID] != nil && s.nodes[h.NodeID].dir
		s.mu.Unlock()
		if !isDir {
			return errReply(u, int(ENOTDIR))
		}
		_ = path
		fh, err := s.addHandle(&handle{node: h.NodeID, isDir: true})
		if err != nil {
			return errReply(u, toErrno(err))
		}
		return s.openReply(u, fh, true)
	}
	if flags&oACCMODE != oRDONLY || flags&oTRUNC != 0 {
		if sh.ReadOnly {
			return errReply(u, int(EROFS))
		}
	}
	if flags&oTRUNC != 0 {
		s.touch(h.NodeID)
	}
	f, err := sh.FS.Open(path, flags)
	if err != nil {
		return errReply(u, toErrno(err))
	}
	fh, err := s.addHandle(&handle{node: h.NodeID, f: f})
	if err != nil {
		f.Close()
		return errReply(u, toErrno(err))
	}
	return s.openReply(u, fh, false)
}

func (s *Server) doCreate(h *inHeader, in createIn) []byte {
	u := h.Unique
	if s.isInjector(h) {
		return errReply(u, int(EPERM))
	}
	path, sh, err := s.parentFor(h.NodeID, in.Name)
	if err != nil {
		return errReply(u, toErrno(err))
	}
	s.markRecent(h.NodeID, in.Name)
	f, a, err := sh.FS.Create(path, in.Flags|oCREAT, in.Mode&^in.Umask, owner(h))
	if err != nil {
		return errReply(u, toErrno(err))
	}
	s.fixAttr(&a)
	s.mu.Lock()
	n, err := s.childLocked(h.NodeID, in.Name, sh, &a)
	if n != nil {
		n.touched = time.Now()
	}
	s.markRecentLocked(h.NodeID, in.Name)
	s.mu.Unlock()
	if err != nil {
		f.Close()
		return errReply(u, toErrno(err))
	}
	fh, err := s.addHandle(&handle{node: n.id, f: f})
	if err != nil {
		f.Close()
		s.forget(n.id, 1)
		return errReply(u, toErrno(err))
	}
	w := newReply(u, 0)
	w.entryOut(n.id, 0, s.ttl(s.opt.EntryTTL), s.ttl(s.opt.AttrTTL), &a)
	w.u64(fh)
	fl := uint32(fopenNoFlush)
	if s.opt.NoCache {
		fl = fopenDirectIO
	}
	w.u32(fl)
	w.u32(0)
	return w.finish()
}

// file returns fh's file with a reference held (see acquire).
func (s *Server) file(fh uint64) (File, func(), error) {
	hd, done, err := s.acquire(fh)
	if err != nil {
		return nil, nil, err
	}
	if hd.inject {
		return nopFile{}, done, nil
	}
	return hd.f, done, nil
}

func (s *Server) writableFile(fh uint64) (File, func(), error) {
	f, done, err := s.file(fh)
	if err != nil {
		return nil, nil, err
	}
	if _, ok := f.(nopFile); ok {
		done()
		return nil, nil, EBADF
	}
	return f, done, nil
}

func (s *Server) release(fh uint64) {
	s.mu.Lock()
	hd := s.handles[fh]
	delete(s.handles, fh)
	if hd != nil {
		s.closeLocked(hd)
	}
	s.mu.Unlock()
	if hd != nil && hd.inject {
		s.injectDone(hd.node)
	}
}

// nopFile stands in for the injector's opens: it never touches the host.
type nopFile struct{}

func (nopFile) ReadAt([]byte, int64) (int, error)  { return 0, io.EOF }
func (nopFile) WriteAt([]byte, int64) (int, error) { return 0, EBADF }
func (nopFile) Sync() error                        { return nil }
func (nopFile) Close() error                       { return nil }

// loadDir lists a share directory, applying Share.Only at the share root.
func loadDir(sh *Share, path []string) ([]DirEntry, error) {
	list, err := sh.FS.ReadDir(path)
	if err != nil {
		return nil, err
	}
	if sh.Only != nil && len(path) == 0 {
		kept := list[:0]
		for _, e := range list {
			if allowed(sh, true, e.Name) {
				kept = append(kept, e)
			}
		}
		list = kept
	}
	return list, nil
}

// rootListing lists the shares (the virtual root).
func (s *Server) rootListing() []DirEntry {
	s.mu.Lock()
	es := make([]DirEntry, 0, len(s.shares))
	for name := range s.shares {
		es = append(es, DirEntry{Name: name, Attr: Attr{Mode: sIFDIR | 0o755}})
	}
	s.mu.Unlock()
	sort.Slice(es, func(i, j int) bool { return es[i].Name < es[j].Name })
	return es
}

// nodeListing returns the listing of directory node id for a READDIR at offset off
// without a handle. Offset 0 starts a listing: the directory is read from the host and
// kept on the node for the READDIRs that continue it, as a directory handle would keep
// it. (Whether the guest needs to ask at all is the guest's readdir cache's business:
// it is kept coherent by host invalidations, like every other cache.)
func (s *Server) nodeListing(id, off uint64) ([]DirEntry, *Share, error) {
	if id == rootNode {
		return s.rootListing(), nil, nil
	}
	s.mu.Lock()
	path, sh, err := s.pathLocked(id)
	n := s.nodes[id]
	if err == nil && !n.dir {
		err = ENOTDIR
	}
	if err != nil {
		s.mu.Unlock()
		return nil, nil, err
	}
	if off != 0 && n.listed {
		es := n.list
		s.mu.Unlock()
		return es, sh, nil
	}
	s.mu.Unlock()
	list, err := loadDir(sh, path)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nodes[id] != n {
		return list, sh, nil // forgotten meanwhile: answer, keep nothing
	}
	s.dropListLocked(n)
	if s.dirEntries+len(list) > maxDirBudget {
		// Snapshots of listings nobody finished reading: drop them all. A READDIR
		// that continues one reads its directory again.
		for _, o := range s.nodes {
			s.dropListLocked(o)
		}
	}
	if s.dirEntries+len(list) > maxDirBudget {
		return nil, nil, ENOMEM
	}
	n.list, n.listed = list, true
	s.dirEntries += len(list)
	return list, sh, nil
}

// dropListLocked forgets the listing snapshot of n (which may be nil).
func (s *Server) dropListLocked(n *node) {
	if n != nil && n.listed {
		s.dirEntries -= len(n.list)
		n.list, n.listed = nil, false
	}
}

// handleListing returns the listing of an OPENDIR handle (Options.NoCache), read from
// the host on the handle's first READDIR.
func (s *Server) handleListing(id, fh uint64) ([]DirEntry, *Share, error) {
	s.mu.Lock()
	hd := s.handles[fh]
	ok := hd != nil && hd.isDir && hd.node == id && !hd.closed
	s.mu.Unlock()
	if !ok {
		return nil, nil, EBADF
	}
	path, sh, err := s.pathOf(id)
	if id != rootNode && err != nil {
		return nil, nil, err
	}
	hd.dmu.Lock()
	defer hd.dmu.Unlock()
	if !hd.loaded && id != rootNode {
		list, err := loadDir(sh, path)
		if err != nil {
			return nil, nil, err
		}
		s.mu.Lock()
		if hd.closed || s.dirEntries+len(list) > maxDirBudget {
			s.mu.Unlock()
			return nil, nil, ENOMEM
		}
		s.dirEntries += len(list)
		hd.dir, hd.loaded = list, true
		s.mu.Unlock()
	}
	return hd.dir, sh, nil
}

func (s *Server) doReaddir(h *inHeader, in readIn, plus bool) []byte {
	u := h.Unique
	var es []DirEntry
	var sh *Share
	if in.Fh == 0 {
		// No directory handle (OPENDIR answers ENOSYS, see doOpen).
		var err error
		es, sh, err = s.nodeListing(h.NodeID, in.Offset)
		if err != nil {
			return errReply(u, toErrno(err))
		}
		if in.Offset > uint64(len(es))+1 {
			// The end: the kernel asks once more and gets nothing. The snapshot is no
			// longer needed.
			s.mu.Lock()
			s.dropListLocked(s.nodes[h.NodeID])
			s.mu.Unlock()
		}
	} else {
		var err error
		es, sh, err = s.handleListing(h.NodeID, in.Fh)
		if err != nil {
			return errReply(u, toErrno(err))
		}
	}
	w := newReply(u, 0)
	start := len(w.b)
	max := int(in.Size)
	// Offsets: 1 is ".", 2 is "..", 3+i is es[i].
	for off := in.Offset; ; off++ {
		var name string
		var a Attr
		var dot bool
		switch {
		case off == 0:
			name, a, dot = ".", Attr{Ino: h.NodeID, Mode: sIFDIR}, true
		case off == 1:
			name, a, dot = "..", Attr{Ino: 1, Mode: sIFDIR}, true
		case off-2 < uint64(len(es)):
			e := es[off-2]
			name, a = e.Name, e.Attr
		default:
			return w.finish()
		}
		if off > uint64(len(es))+2 {
			return w.finish()
		}
		if !plus {
			if !w.dirent(start, max, a.Ino, off+1, dtype(a.Mode), name) {
				return w.finish()
			}
			continue
		}
		if len(w.b)-start+direntSize(name, true) > max {
			return w.finish()
		}
		var id uint64
		if !dot && sh != nil {
			s.fixAttr(&a)
			if sh.ReadOnly {
				a.Mode &^= 0o222
			}
			s.mu.Lock()
			n, err := s.childLocked(h.NodeID, name, sh, &a)
			s.mu.Unlock()
			if err == nil {
				id = n.id
			}
		}
		if id != 0 {
			w.entryOut(id, 0, s.ttl(s.opt.EntryTTL), s.ttl(s.opt.AttrTTL), &a)
		} else {
			w.entryOut(0, 0, 0, 0, &Attr{})
		}
		w.dirent(start, 1<<30, a.Ino, off+1, dtype(a.Mode), name)
	}
}

func (s *Server) touch(id uint64) {
	s.mu.Lock()
	if n := s.nodes[id]; n != nil {
		n.touched = time.Now()
	}
	s.mu.Unlock()
}

// markRecent is markRecentLocked for callers without s.mu. Callers mark before the host
// operation: the watcher can report the change before the operation returns.
func (s *Server) markRecent(parent uint64, name string) {
	s.mu.Lock()
	s.markRecentLocked(parent, name)
	s.mu.Unlock()
}

// markRecentLocked records a name the guest changed, so its echo from the host
// watcher is not replayed (inject.go).
func (s *Server) markRecentLocked(parent uint64, name string) {
	if len(s.recent) > 65536 {
		cut := time.Now().Add(-echoWindow)
		for k, t := range s.recent {
			if t.Before(cut) {
				delete(s.recent, k)
			}
		}
	}
	now := time.Now()
	s.recent[nodeKey{parent, name}] = now
	if n := s.nodes[parent]; n != nil {
		n.inner = now
	}
}
