package share

// Host changes reach the guest in two ways.
//
//  1. Invalidation. FUSE_NOTIFY_INVAL_INODE and _INVAL_ENTRY drop what the guest kernel
//     cached, so the next access asks the host again. This keeps long cache lifetimes
//     coherent.
//  2. Replay ("injection"). inotify only reports operations that pass through the guest's
//     VFS, and an invalidation is not one. So the server asks the guest relay to repeat
//     the change on the mount: truncate a modified file to its own size (IN_MODIFY),
//     close it (IN_CLOSE_WRITE), mknod or mkdir a created name (IN_CREATE), unlink a
//     removed one (IN_DELETE), rename (IN_MOVED_FROM/TO). Those requests arrive from the
//     relay's injector thread, which the relay names in its HELLO frame, and the server
//     answers them from the pending record without touching the host: the change has
//     already happened there.
//
// The guest is untrusted, and so is its claim about which thread is the injector. That is
// fine: an injector request can only make the server skip a host operation and report the
// host's current state, which the guest could learn anyway.

import (
	"encoding/binary"
	"time"
)

// echoWindow is how long a change the guest made itself is not replayed when the host
// watcher reports it back.
const echoWindow = 2 * time.Second

// pendingTTL drops replay records the relay never acted on.
const pendingTTL = 5 * time.Second

// Replay kinds in a notifyInject frame.
const (
	injModify = 1
	injCreate = 2
	injDelete = 3
	injRename = 4
)

type injection struct {
	kind int
	// role is what a LOOKUP of this record's key should see: absent (create, rename
	// target) or present (delete, rename source).
	present bool
	attr    Attr
	node    uint64 // existing node for a present name, if any
	// For a rename source: where it goes.
	newParent uint64
	newName   string
	at        time.Time
	seq       uint64 // host change sequence before attr was read (racedLocked)
}

// EventKind is what happened to a host path.
type EventKind int

// Host change kinds.
const (
	Modified EventKind = iota
	Created
	Removed
	Renamed
)

// HostEvent is one change seen on the host, below a share root.
type HostEvent struct {
	Share string
	// Path and, for Renamed, NewPath are Linux name components relative to the share.
	Path, NewPath []string
	Kind          EventKind
}

// injectMsg builds the frame the relay turns into a replayed operation. Paths are
// relative to the FUSE mount point.
func injectMsg(kind int, mode uint32, p1, p2 string) []byte {
	w := newReply(0, notifyInject)
	w.u32(uint32(kind))
	w.u32(mode)
	w.u32(uint32(len(p1)))
	w.b = append(w.b, p1...)
	w.u32(uint32(len(p2)))
	w.b = append(w.b, p2...)
	return w.finish()
}

// InjectFrame is a decoded notifyInject frame (for the guest relay).
type InjectFrame struct {
	Kind    int
	Mode    uint32
	Path    string
	NewPath string
}

// ParseInject decodes a notifyInject frame body (after the 16-byte out header). It is
// used by the guest relay; the frame comes from the host.
func ParseInject(b []byte) (InjectFrame, bool) {
	r := reader{b: b}
	f := InjectFrame{Kind: int(r.u32()), Mode: r.u32()}
	n := r.u32()
	if r.err != nil || n > maxSymlinkLen {
		return f, false
	}
	f.Path = string(r.take(int(n)))
	n = r.u32()
	if r.err != nil || n > maxSymlinkLen {
		return f, false
	}
	f.NewPath = string(r.take(int(n)))
	return f, r.err == nil && f.Kind >= injModify && f.Kind <= injRename
}

// IsInject reports whether an out frame from the host is a replay request.
func IsInject(frame []byte) bool {
	return len(frame) >= outHeaderLen && binary.LittleEndian.Uint64(frame[8:]) == 0 &&
		int32(binary.LittleEndian.Uint32(frame[4:])) == notifyInject
}

func joinPath(share string, p []string) string {
	s := share
	for _, c := range p {
		s += "/" + c
	}
	return s
}

// resolveLocked walks known nodes from the share root. It returns the node of the
// deepest directory in p[:len(p)-1] only if every component is known; ok is false
// otherwise (nothing below is cached in the guest).
func (s *Server) resolveLocked(share string, p []string) (parent uint64, ok bool) {
	id, found := s.index[nodeKey{rootNode, share}]
	if !found {
		return 0, false
	}
	for _, c := range p[:len(p)-1] {
		id, found = s.index[nodeKey{id, c}]
		if !found {
			return 0, false
		}
	}
	return id, true
}

// allowedLocked applies Share.Only to a name in directory node parent.
func (s *Server) allowedLocked(sh *Share, parent uint64, name string) bool {
	if sh == nil || sh.Only == nil {
		return true
	}
	n := s.nodes[parent]
	return n == nil || n.parent != rootNode || allowed(sh, true, name)
}

func (s *Server) echoLocked(parent uint64, name string) bool {
	k := nodeKey{parent, name}
	cut := time.Now().Add(-echoWindow)
	if t, ok := s.recent[k]; ok && t.After(cut) {
		return true
	}
	if id, ok := s.index[k]; ok && s.nodes[id].touched.After(cut) {
		return true
	}
	return false
}

func (s *Server) fabricated(n *node) Attr {
	a := Attr{Ino: n.ino, Mode: sIFREG | 0o644, Nlink: 1}
	if n.dir {
		a.Mode = sIFDIR | 0o755
	}
	s.fixAttr(&a)
	return a
}

// change is the last host change reported for a name.
type change struct {
	seq uint64
	at  time.Time
}

// Changes are remembered for changedKeep, and at most maxChanged of them; a LOOKUP or
// listing that read the host before what was forgotten counts as raced.
const (
	changedKeep = 10 * time.Second
	maxChanged  = 1 << 14
)

// noteChangeLocked records a host change of name k.
//
// A LOOKUP (or READDIRPLUS, or replayed create) reads the host, then makes the node.
// A change reported in between finds no node to invalidate, and the guest would
// cache what was read before it for an hour: a file created and then written on
// Windows could stay at size 0, a name created during a negative LOOKUP missing.
// Such replies are sent with nothing to cache instead (raced).
func (s *Server) noteChangeLocked(k nodeKey) {
	if len(s.changed) >= maxChanged {
		cut := time.Now().Add(-changedKeep)
		for k2, c := range s.changed {
			if c.at.Before(cut) {
				s.changedFloor = max(s.changedFloor, c.seq)
				delete(s.changed, k2)
			}
		}
		if len(s.changed) >= maxChanged {
			clear(s.changed)
			s.changedFloor = s.hostSeq
		}
	}
	s.hostSeq++
	s.changed[k] = change{seq: s.hostSeq, at: time.Now()}
}

// racedLocked reports whether name k changed on the host after sequence since (taken
// before the host was read).
func (s *Server) racedLocked(k nodeKey, since uint64) bool {
	return since < s.changedFloor || s.changed[k].seq > since
}

func (s *Server) gcPendingLocked() {
	cut := time.Now().Add(-pendingTTL)
	for k, p := range s.pending {
		if p.at.Before(cut) {
			delete(s.pending, k)
		}
	}
}

// HostChanged tells the guest about one host change: it invalidates what the guest
// cached and, where a watcher could be looking, replays the change.
func (s *Server) HostChanged(ev HostEvent) {
	if len(ev.Path) == 0 {
		return
	}
	var msgs [][]byte
	s.mu.Lock()
	s.gcPendingLocked()
	sh := s.shares[ev.Share]
	parent, ok := s.resolveLocked(ev.Share, ev.Path)
	if sh == nil || !ok {
		s.unlock()
		return
	}
	name := ev.Path[len(ev.Path)-1]
	// A single-file share must not learn about, or get nodes for, its siblings.
	if !s.allowedLocked(sh, parent, name) {
		s.unlock()
		return
	}
	if s.echoLocked(parent, name) {
		s.unlock()
		return
	}
	k := nodeKey{parent, name}
	s.noteChangeLocked(k)
	childID, known := s.index[k]
	inj := s.injectorKnown()
	// The node was made by a reply the guest kernel may not have applied yet; an
	// invalidation that reaches it first finds nothing to drop. Sent again later.
	fresh := known && s.nodes[childID].born.After(time.Now().Add(-bornWindow))
	switch ev.Kind {
	case Modified:
		if n := s.nodes[childID]; known && n.dir && n.inner.After(time.Now().Add(-echoWindow)) {
			// The directory's times changed because the guest changed something in
			// it. Invalidating it would only make the guest ask again for what it
			// already knows (a GETATTR per create) and drop its readdir cache.
			break
		}
		if known {
			msgs = append(msgs, notifyInvalInodeMsg(childID, 0, 0))
			if n := s.nodes[childID]; inj && !n.dir {
				s.pending[k] = &injection{kind: injModify, present: true, node: childID, at: time.Now()}
				msgs = append(msgs, injectMsg(injModify, sIFREG, joinPath(ev.Share, ev.Path), ""))
			}
		} else if inj {
			// A file the guest never looked up, in a directory it has: an inotify watch
			// on that directory still expects IN_MODIFY (a READDIR without PLUS, which
			// the kernel uses past the first chunk, names entries without looking them
			// up). Found by bench/fs-notify: 88 of 100 such edits had no event. The
			// replay looks the name up like any other process would. Regular files
			// only: a directory's own times change with its contents.
			s.unlock()
			a, err := sh.FS.Lstat(ev.Path)
			s.mu.Lock()
			if err == nil && a.Mode&sIFMT == sIFREG {
				s.pending[k] = &injection{kind: injModify, present: true, at: time.Now()}
				msgs = append(msgs, injectMsg(injModify, sIFREG, joinPath(ev.Share, ev.Path), ""))
			}
		}
	case Created:
		msgs = append(msgs, notifyInvalEntryMsg(parent, name), notifyInvalInodeMsg(parent, 0, 0))
		delete(s.negative, k)
		if inj {
			since := s.hostSeq
			s.unlock()
			a, err := sh.FS.Lstat(ev.Path)
			s.mu.Lock()
			if err == nil {
				s.fixAttr(&a)
				s.pending[k] = &injection{kind: injCreate, attr: a, at: time.Now(), seq: since}
				msgs = append(msgs, injectMsg(injCreate, a.Mode&sIFMT, joinPath(ev.Share, ev.Path), ""))
			}
		}
	case Removed:
		msgs = append(msgs, notifyInvalEntryMsg(parent, name), notifyInvalInodeMsg(parent, 0, 0))
		if inj {
			p := &injection{kind: injDelete, present: true, at: time.Now()}
			if known {
				p.node = childID
				p.attr = s.fabricated(s.nodes[childID])
			} else {
				p.attr = Attr{Ino: 0, Mode: sIFREG | 0o644, Nlink: 1}
				s.fixAttr(&p.attr)
			}
			s.pending[k] = p
			msgs = append(msgs, injectMsg(injDelete, p.attr.Mode&sIFMT, joinPath(ev.Share, ev.Path), ""))
		}
		if known {
			s.nodes[childID].orphan = true
			delete(s.index, k)
			// A cached handle would keep a classic (non-POSIX) delete pending, and the
			// name taken, until it closed.
			s.dropNodeFileLocked(childID)
		}
	case Renamed:
		newParent, ok2 := s.resolveLocked(ev.Share, ev.NewPath)
		if len(ev.NewPath) == 0 || !ok2 {
			// Moved somewhere nothing is cached: treat as a removal.
			s.unlock()
			s.HostChanged(HostEvent{Share: ev.Share, Path: ev.Path, Kind: Removed})
			return
		}
		newName := ev.NewPath[len(ev.NewPath)-1]
		if !s.allowedLocked(sh, newParent, newName) {
			// Renamed to a name the share hides: to the guest it is gone.
			s.unlock()
			s.HostChanged(HostEvent{Share: ev.Share, Path: ev.Path, Kind: Removed})
			return
		}
		nk := nodeKey{newParent, newName}
		s.noteChangeLocked(nk)
		if id, ok := s.index[nk]; ok && id != childID {
			s.dropNodeFileLocked(id) // replaced on the host
			fresh = fresh || s.nodes[id].born.After(time.Now().Add(-bornWindow))
		}
		msgs = append(msgs, notifyInvalEntryMsg(parent, name), notifyInvalEntryMsg(newParent, newName),
			notifyInvalInodeMsg(parent, 0, 0))
		if newParent != parent {
			msgs = append(msgs, notifyInvalInodeMsg(newParent, 0, 0))
		}
		if inj && known {
			n := s.nodes[childID]
			s.pending[k] = &injection{kind: injRename, present: true, node: childID, attr: s.fabricated(n), newParent: newParent, newName: newName, at: time.Now()}
			s.pending[nk] = &injection{kind: injRename, at: time.Now()}
			msgs = append(msgs, injectMsg(injRename, 0, joinPath(ev.Share, ev.Path), joinPath(ev.Share, ev.NewPath)))
		} else if known {
			s.moveLocked(parent, name, newParent, newName)
		}
	}
	s.unlock()
	for _, m := range msgs {
		s.notify(m)
	}
	if fresh {
		s.invalidateLater(msgs)
	}
}

// bornWindow is how long after a node was made an invalidation of it is sent twice.
const bornWindow = 2 * time.Second

// reinvalAfter are the delays of the repeated invalidations.
var reinvalAfter = []time.Duration{50 * time.Millisecond, time.Second}

// invalidateLater sends the invalidations among msgs again, for a node the guest
// kernel may have been making when they first arrived (it applies a LOOKUP reply after
// waking the process that asked, so an invalidation can overtake it). Invalidating
// twice only costs the guest a lookup.
func (s *Server) invalidateLater(msgs [][]byte) {
	var inval [][]byte
	for _, m := range msgs {
		if !IsInject(m) {
			inval = append(inval, m)
		}
	}
	if len(inval) == 0 {
		return
	}
	// One batch and one timer per delay, bounded: a Windows checkout touching
	// thousands of names the guest just looked up must not make thousands of timers
	// or flood the notification queue (past the bound the repeat is skipped; the first
	// invalidation was sent).
	s.lmu.Lock()
	defer s.lmu.Unlock()
	for i, d := range reinvalAfter {
		if i >= len(s.later) {
			break
		}
		b := &s.later[i]
		if len(b.msgs)+len(inval) > maxReinval {
			continue
		}
		b.msgs = append(b.msgs, inval...)
		if b.timer == nil {
			i := i
			b.timer = time.AfterFunc(d, func() { s.flushLater(i) })
		}
	}
}

// maxReinval bounds the invalidations waiting to be sent again, per delay.
const maxReinval = 4096

// laterBatch is the invalidations waiting for one of reinvalAfter.
type laterBatch struct {
	msgs  [][]byte
	timer *time.Timer
}

func (s *Server) flushLater(i int) {
	s.lmu.Lock()
	msgs := s.later[i].msgs
	s.later[i] = laterBatch{}
	s.lmu.Unlock()
	for _, m := range msgs {
		s.notify(m)
	}
}

// HostOverflow handles a lost change notification for a share: everything the guest
// may have cached below it is invalidated.
func (s *Server) HostOverflow(share string) {
	var msgs [][]byte
	s.mu.Lock()
	sh := s.shares[share]
	if sh != nil {
		s.dropAllFilesLocked(sh, false) // a lost change may have replaced any of them
	}
	for k, id := range s.index {
		n := s.nodes[id]
		if n == nil || n.share != sh || sh == nil {
			continue
		}
		msgs = append(msgs, notifyInvalInodeMsg(id, 0, 0))
		if k.parent != rootNode {
			msgs = append(msgs, notifyInvalEntryMsg(k.parent, k.name))
		}
	}
	for k := range s.negative {
		if n := s.nodes[k.parent]; n != nil && n.share == sh && sh != nil {
			msgs = append(msgs, notifyInvalEntryMsg(k.parent, k.name))
			delete(s.negative, k)
		}
	}
	s.unlock()
	for _, m := range msgs {
		s.notify(m)
	}
}

func (s *Server) injectorKnown() bool {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	return s.injector != 0
}

// Injector request handlers. None of them touches the host except to read state.

func (s *Server) injectLookup(h *inHeader, name string) []byte {
	k := nodeKey{h.NodeID, name}
	s.mu.Lock()
	p := s.pending[k]
	if p == nil || p.kind == injModify {
		s.unlock()
		// Not a replayed name: a normal lookup (the injector also walks the
		// directories above the file it replays on).
		return s.lookupBody(h, name)
	}
	if !p.present {
		s.unlock()
		return errReply(h.Unique, int(ENOENT))
	}
	var n *node
	if p.node != 0 {
		n = s.nodes[p.node]
	}
	if n == nil {
		n = &node{id: s.nextNode, parent: h.NodeID, name: name, ino: p.attr.Ino, dir: p.attr.isDir(), orphan: true}
		s.nextNode++
		s.nodes[n.id] = n
		p.node = n.id
	}
	n.nlookup++
	a := p.attr
	s.unlock()
	// A short TTL: this entry describes something the host no longer has.
	w := newReply(h.Unique, 0)
	w.entryOut(n.id, 0, 0, 0, &a)
	return w.finish()
}

func (s *Server) injectCreate(h *inHeader, name string) []byte {
	k := nodeKey{h.NodeID, name}
	s.mu.Lock()
	p := s.pending[k]
	if pn := s.nodes[h.NodeID]; p == nil || p.kind != injCreate || pn == nil || !s.allowedLocked(pn.share, h.NodeID, name) {
		s.unlock()
		return errReply(h.Unique, int(EPERM))
	}
	delete(s.pending, k)
	parent := s.nodes[h.NodeID]
	if parent == nil || parent.share == nil {
		s.unlock()
		return errReply(h.Unique, int(ENOENT))
	}
	a := p.attr
	n, err := s.childLocked(h.NodeID, name, parent.share, &a)
	raced := s.racedLocked(k, p.seq)
	s.unlock()
	if err != nil {
		return errReply(h.Unique, toErrno(err))
	}
	if raced {
		// Written (or replaced) on the host since attr was read, before this node
		// existed to be invalidated: nothing to cache.
		w := newReply(h.Unique, 0)
		w.entryOut(n.id, 0, 0, 0, &a)
		return w.finish()
	}
	return s.entryReply(h.Unique, n, &a)
}

func (s *Server) injectRemove(h *inHeader, name string) []byte {
	k := nodeKey{h.NodeID, name}
	s.mu.Lock()
	defer s.unlock()
	p := s.pending[k]
	if p == nil || p.kind != injDelete {
		return errReply(h.Unique, int(EPERM))
	}
	delete(s.pending, k)
	if n := s.nodes[p.node]; n != nil {
		n.orphan = true
	}
	if id, ok := s.index[k]; ok && id == p.node {
		delete(s.index, k)
	}
	return newReply(h.Unique, 0).finish()
}

func (s *Server) injectRename(h *inHeader, in renameIn) []byte {
	k := nodeKey{h.NodeID, in.OldName}
	nk := nodeKey{in.NewDir, in.NewName}
	s.mu.Lock()
	defer s.unlock()
	p := s.pending[k]
	if p == nil || p.kind != injRename || !p.present || p.newParent != in.NewDir || p.newName != in.NewName {
		return errReply(h.Unique, int(EPERM))
	}
	delete(s.pending, k)
	delete(s.pending, nk)
	if id, ok := s.index[nk]; ok {
		s.nodes[id].orphan = true
		delete(s.index, nk)
	}
	if n := s.nodes[p.node]; n != nil {
		if s.index[nodeKey{n.parent, n.name}] == n.id {
			delete(s.index, nodeKey{n.parent, n.name})
		}
		n.parent, n.name, n.orphan = in.NewDir, in.NewName, false
		s.index[nk] = n.id
	}
	return newReply(h.Unique, 0).finish()
}

func (s *Server) injectSetattr(h *inHeader) []byte {
	// The modify replay's truncate: its record is done (without OPEN there is no
	// RELEASE to say so).
	s.injectDone(h.NodeID)
	a, err := s.getattr(h.NodeID)
	if err != nil {
		return errReply(h.Unique, toErrno(err))
	}
	w := newReply(h.Unique, 0)
	w.attrOut(s.ttl(s.opt.AttrTTL), &a)
	return w.finish()
}

func (s *Server) injectDone(id uint64) {
	s.mu.Lock()
	defer s.unlock()
	for k, p := range s.pending {
		if p.kind == injModify && p.node == id {
			delete(s.pending, k)
		}
	}
}
