package share

// Opens without round trips (FUSE_NO_OPEN_SUPPORT, REQ-FS-002).
//
// An OPEN costs a guest-to-host round trip even when every page of the file is in the
// guest's page cache: a warm `git status` paid 16 of them (and 17 RELEASEs) for .git
// files it only reads from cache. When OPEN answers ENOSYS, the kernel stops sending
// OPEN, RELEASE and (once FLUSH answers ENOSYS too) FLUSH for the whole connection,
// opens every file with FOPEN_KEEP_CACHE, and sends READ, WRITE and FSYNC with fh 0.
//
// The server then finds a host file by node instead of by handle. Host handles are
// opened on the first READ or WRITE that needs one and kept in a small cache (files),
// because the guest no longer says when it closes a file:
//   - every handle shares read, write and delete (shareAll), so Windows programs can
//     edit, rename and delete the file while it is cached;
//   - an idle handle is closed after Options.FileIdle, and at most Options.MaxFiles are
//     kept (least recently used first), so a handle can hold up a Windows program that
//     wants the file exclusively, or a classic (non-POSIX) delete, for that long at most;
//   - a host removal or replacement of the file closes its handle at once (inject.go);
//   - a read never opens the file for writing: a cached read handle does not conflict
//     with Windows programs that share reads only.
//
// What OPEN used to do moves elsewhere:
//   - O_TRUNC: FUSE_ATOMIC_O_TRUNC is not negotiated, so the kernel truncates with a
//     path SETATTR inside open(2);
//   - O_APPEND: the kernel already wrote at its own idea of the end of the file;
//   - permission and sharing errors of the host open (a read-only file opened for
//     writing by root, a file a Windows program holds exclusively) surface at the first
//     read or write instead of at open(2);
//   - POSIX "unlinked but still open": the guest's own unlink or rename-over of a file
//     keeps a host handle to it (pinned) until the kernel forgets the inode, which it
//     does as soon as no process has it open;
//   - CREATE answers fh 0 and caches the file it opened: the kernel sends no RELEASE
//     for a CREATE handle either once OPEN answered ENOSYS (seen on 6.18).
//
// Options.RealOpen, NoCache and Writeback keep the old behaviour (an OPEN and a host
// handle per guest open).

import (
	"errors"
	"time"
)

// nodeFile is a host file kept for requests without a handle.
type nodeFile struct {
	node        uint64
	f           File
	read, write bool // what f was opened for
	refs        int  // requests using f
	used        time.Time
	// pinned: the guest removed or replaced the name; kept until the kernel forgets
	// the node, for processes that still have it open. Not subject to FileIdle.
	pinned bool
	gone   bool // out of Server.files; f is closed when refs drops to 0
}

// keepUnlinker is an FS that can remove a file and keep a handle to it in one host
// operation (NTFS: open with data access and DELETE, then a POSIX delete).
type keepUnlinker interface {
	// UnlinkKeep removes a file like Unlink. For a regular file it also returns an open
	// handle to it, and whether that can write; nil if none could be had.
	UnlinkKeep(path []string) (File, bool, error)
}

// can reports whether e serves a read (or, with write, a write).
func (e *nodeFile) can(write bool) bool {
	if write {
		return e.write
	}
	return e.read
}

// openNodeFile opens a host file for node requests: read-only for reads, read-write
// (or write-only, if the host refuses reads) for writes.
func openNodeFile(sh *Share, path []string, write bool) (f File, rd, wr bool, err error) {
	if !write {
		f, err = sh.FS.Open(path, oRDONLY)
		return f, true, false, err
	}
	f, err = sh.FS.Open(path, oRDWR)
	if errors.Is(err, EACCES) {
		f, err = sh.FS.Open(path, oWRONLY)
		return f, false, true, err
	}
	return f, true, true, err
}

// nodeFileGet returns a host file for a READ, WRITE or FSYNC of node id without a
// handle, with a reference held: call done when finished.
func (s *Server) nodeFileGet(id uint64, write bool) (File, func(), error) {
	s.mu.Lock()
	n := s.nodes[id]
	if n == nil || id == rootNode {
		s.mu.Unlock()
		return nil, nil, ESTALE
	}
	if n.dir {
		s.mu.Unlock()
		return nil, nil, EISDIR
	}
	if e := s.files[id]; e != nil && e.can(write) {
		e.refs++
		e.used = time.Now()
		s.mu.Unlock()
		return e.f, func() { s.putFile(e) }, nil
	}
	path, sh, err := s.pathLocked(id)
	s.mu.Unlock()
	if err != nil {
		if errors.Is(err, ENOENT) {
			// Removed (by the guest without a pin, or on the host): this open file
			// has nothing behind it any more.
			err = ESTALE
		}
		return nil, nil, err
	}
	if write && sh.ReadOnly {
		return nil, nil, EROFS
	}
	f, rd, wr, err := openNodeFile(sh, path, write)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nodes[id] != n || n.orphan {
		// Forgotten or removed meanwhile: serve this request, keep nothing.
		return f, func() { f.Close() }, nil
	}
	if e := s.files[id]; e != nil {
		switch {
		case e.can(write):
			// Another request cached one good enough meanwhile.
			f.Close()
			e.refs++
			e.used = time.Now()
			return e.f, func() { s.putFile(e) }, nil
		case (rd || !e.read) && (wr || !e.write) && !e.pinned:
			s.dropFileLocked(e) // the new handle can do everything it could
		default:
			// A write-only handle and a read-only one: serve this request, keep
			// the cached one.
			return f, func() { f.Close() }, nil
		}
	}
	e := s.cacheFileLocked(id, f, rd, wr, 1)
	return f, func() { s.putFile(e) }, nil
}

// cacheFileLocked caches f as the file of node id, which has none, with refs requests
// using it already (so the trim below cannot close it).
func (s *Server) cacheFileLocked(id uint64, f File, rd, wr bool, refs int) *nodeFile {
	e := &nodeFile{node: id, f: f, read: rd, write: wr, refs: refs, used: time.Now()}
	s.files[id] = e
	s.trimFilesLocked()
	s.armSweepLocked()
	return e
}

// keepCreated caches the file a CREATE opened, answered with fh 0: the kernel sends
// READ, WRITE and FSYNC for it with fh 0 like for any other file, and, once OPEN
// answered ENOSYS, no RELEASE (seen on 6.18: every CREATE handle stayed open until the
// handle table was full).
func (s *Server) keepCreated(id uint64, f File, rd, wr bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodes[id]
	if n == nil || n.orphan || n.dir {
		f.Close()
		return
	}
	if e := s.files[id]; e != nil {
		s.dropFileLocked(e)
	}
	s.cacheFileLocked(id, f, rd, wr, 0)
}

// putFile drops a request's reference.
func (s *Server) putFile(e *nodeFile) {
	s.mu.Lock()
	e.refs--
	e.used = time.Now()
	if e.gone && e.refs == 0 {
		e.f.Close()
	}
	s.mu.Unlock()
}

// dropFileLocked takes e out of the cache; its file closes when no request uses it.
func (s *Server) dropFileLocked(e *nodeFile) {
	if e.gone {
		return
	}
	e.gone = true
	if s.files[e.node] == e {
		delete(s.files, e.node)
	}
	if e.pinned {
		s.pins--
	}
	if e.refs == 0 {
		e.f.Close()
	}
	if len(s.files) == 0 && s.sweep != nil {
		s.sweep.Stop()
		s.sweep = nil
	}
}

// dropNodeFileLocked drops the cached file of node id, if any.
func (s *Server) dropNodeFileLocked(id uint64) {
	if e := s.files[id]; e != nil {
		s.dropFileLocked(e)
	}
}

// dropAllFilesLocked drops every cached file (of share sh, or all if sh is nil), and
// the pins too if pins.
func (s *Server) dropAllFilesLocked(sh *Share, pins bool) {
	for id, e := range s.files {
		if e.pinned && !pins {
			continue
		}
		if n := s.nodes[id]; sh == nil || n == nil || n.share == sh {
			s.dropFileLocked(e)
		}
	}
}

// pinLocked keeps f (may be nil; w if it can write) as the file of node id, which the
// guest just removed or replaced, until the kernel forgets the node. An already cached
// handle is kept instead if it can do at least as much.
func (s *Server) pinLocked(id uint64, f File, w bool) {
	e := s.files[id]
	if e != nil && (f == nil || e.read && (e.write || !w)) {
		if f != nil {
			f.Close()
		}
		if !e.pinned {
			e.pinned = true
			s.pins++
		}
		return
	}
	if f == nil {
		return
	}
	if e != nil {
		s.dropFileLocked(e)
	}
	if s.pins >= s.opt.MaxHandles {
		f.Close() // bounded: past this, removed files that are still open read as ESTALE
		return
	}
	s.files[id] = &nodeFile{node: id, f: f, read: true, write: w, used: time.Now(), pinned: true}
	s.pins++
}

// mayBeOpen reports whether name k is a file the guest knows, so that a process may
// have it open without the server knowing (only without OPEN), and whether a cached
// read-write handle already holds it (then that one is pinned, at no host cost).
func (s *Server) mayBeOpen(k nodeKey) (may, cached bool) {
	if !s.noOpen.Load() {
		return false, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.index[k]
	if !ok || s.nodes[id].dir {
		return false, false
	}
	e := s.files[id]
	return true, e != nil && e.read && e.write
}

// openPin opens the file at path for a pin: read-write if the host allows, else
// read-only, else nothing.
func openPin(sh *Share, path []string) (File, bool) {
	if f, err := sh.FS.Open(path, oRDWR); err == nil {
		return f, true
	}
	if f, err := sh.FS.Open(path, oRDONLY); err == nil {
		return f, false
	}
	return nil, false
}

// trimFilesLocked closes the least recently used idle handles past Options.MaxFiles.
func (s *Server) trimFilesLocked() {
	for len(s.files)-s.pins > s.opt.MaxFiles {
		var old *nodeFile
		for _, e := range s.files {
			if !e.pinned && e.refs == 0 && (old == nil || e.used.Before(old.used)) {
				old = e
			}
		}
		if old == nil {
			return // all in use; they are dropped as they go idle
		}
		s.dropFileLocked(old)
	}
}

// armSweepLocked makes sure idle handles get closed.
func (s *Server) armSweepLocked() {
	if s.sweep == nil {
		s.sweepGen++
		g := s.sweepGen
		s.sweep = time.AfterFunc(max(s.opt.FileIdle/4, time.Millisecond), func() { s.sweepFiles(g) })
	}
}

// sweepFiles closes handles idle for Options.FileIdle. g tells a timer that was
// stopped too late (it had already fired) from the current one.
func (s *Server) sweepFiles(g uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g != s.sweepGen || s.sweep == nil {
		return
	}
	s.sweep = nil
	cut := time.Now().Add(-s.opt.FileIdle)
	live := false
	for _, e := range s.files {
		if e.pinned {
			continue
		}
		if e.refs == 0 && !e.used.After(cut) {
			s.dropFileLocked(e)
			continue
		}
		live = true
	}
	if live {
		s.armSweepLocked()
	}
}

// CachedFiles reports how many host files are held for guest opens without handles,
// and how many of them are pinned (for tests and diagnostics).
func (s *Server) CachedFiles() (open, pinned int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.files), s.pins
}
