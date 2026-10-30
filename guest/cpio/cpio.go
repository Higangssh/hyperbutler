// Package cpio reads and writes the "newc" (SVR4, magic 070701) cpio format that the
// Linux kernel unpacks as an initramfs.
//
// It exists so that a guest initramfs can be built on Windows without WSL or any Linux
// tool. The writer is deterministic: the caller controls every header field, and
// nothing is read from the host file system's metadata. See docs/adr/0005-guest-image.md.
//
// Format reference: the kernel's Documentation/driver-api/early-userspace/buffer-format.rst.
// This is an independent implementation; no code was taken from gen_init_cpio or GNU cpio.
package cpio

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// File type bits of Header.Mode, as in <sys/stat.h>.
const (
	TypeMask    = 0o170000
	TypeSocket  = 0o140000
	TypeSymlink = 0o120000
	TypeRegular = 0o100000
	TypeBlock   = 0o060000
	TypeDir     = 0o040000
	TypeChar    = 0o020000
	TypeFIFO    = 0o010000
)

const (
	magic      = "070701"
	headerLen  = 110
	trailer    = "TRAILER!!!"
	maxNameLen = 4096 // PATH_MAX; the kernel rejects longer names too.
)

// Header is one cpio entry. Size is the length of the data that follows: file contents
// for regular files, the link target for symlinks, and 0 for everything else.
type Header struct {
	Name      string
	Ino       uint32
	Mode      uint32 // permission bits | Type* bits
	UID       uint32
	GID       uint32
	Nlink     uint32
	Mtime     uint32
	Size      uint32
	DevMajor  uint32
	DevMinor  uint32
	RdevMajor uint32
	RdevMinor uint32
}

// ErrFormat is returned (wrapped) for malformed archives.
var ErrFormat = errors.New("cpio: malformed archive")

func pad4(n int64) int64 { return (4 - n%4) % 4 }

// Writer writes a newc archive. Call Close to write the trailer.
type Writer struct {
	w       io.Writer
	off     int64
	remain  int64 // data bytes still owed for the current entry
	nextIno uint32
	closed  bool
}

// NewWriter returns a Writer that writes to w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w, nextIno: 1} }

func (w *Writer) write(p []byte) error {
	n, err := w.w.Write(p)
	w.off += int64(n)
	return err
}

func (w *Writer) finishEntry() error {
	if w.remain != 0 {
		return fmt.Errorf("cpio: %d bytes of entry data missing", w.remain)
	}
	return w.write(make([]byte, pad4(w.off)))
}

// WriteHeader starts a new entry. If h.Ino is 0, a sequential inode number is assigned,
// so identical input always gives identical output. Exactly h.Size bytes must then be
// written with Write.
func (w *Writer) WriteHeader(h Header) error {
	if h.Name == trailer {
		// The kernel would stop unpacking here and choke on the rest.
		return errors.New("cpio: reserved name " + trailer)
	}
	return w.writeHeader(h)
}

func (w *Writer) writeHeader(h Header) error {
	if w.closed {
		return errors.New("cpio: write after close")
	}
	if err := w.finishEntry(); err != nil {
		return err
	}
	if h.Name == "" || len(h.Name) >= maxNameLen {
		return fmt.Errorf("cpio: invalid name length %d", len(h.Name))
	}
	for i := 0; i < len(h.Name); i++ {
		if h.Name[i] == 0 {
			return errors.New("cpio: NUL in name")
		}
	}
	if h.Mode&TypeMask == TypeSymlink && (h.Size == 0 || h.Size >= maxNameLen) {
		// The kernel ignores symlinks with a target of PATH_MAX or more.
		return fmt.Errorf("cpio: symlink target length %d out of range", h.Size)
	}
	if h.Ino == 0 {
		h.Ino = w.nextIno
		w.nextIno++
	}
	if h.Nlink == 0 {
		h.Nlink = 1
		if h.Mode&TypeMask == TypeDir {
			h.Nlink = 2
		}
	}
	fields := []uint32{h.Ino, h.Mode, h.UID, h.GID, h.Nlink, h.Mtime, h.Size,
		h.DevMajor, h.DevMinor, h.RdevMajor, h.RdevMinor, uint32(len(h.Name) + 1), 0}
	buf := make([]byte, 0, headerLen+len(h.Name)+1)
	buf = append(buf, magic...)
	for _, f := range fields {
		buf = append(buf, fmt.Sprintf("%08X", f)...)
	}
	buf = append(buf, h.Name...)
	buf = append(buf, 0)
	if err := w.write(buf); err != nil {
		return err
	}
	if err := w.write(make([]byte, pad4(w.off))); err != nil {
		return err
	}
	w.remain = int64(h.Size)
	return nil
}

// Write writes entry data. It fails if more than Header.Size bytes are written.
func (w *Writer) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remain {
		return 0, errors.New("cpio: write exceeds header size")
	}
	n, err := w.w.Write(p)
	w.off += int64(n)
	w.remain -= int64(n)
	return n, err
}

// Close writes the trailer entry and pads the archive to a 512-byte boundary, as GNU
// cpio does. It does not close the underlying writer.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	if err := w.writeHeader(Header{Name: trailer, Ino: 0xFFFFFFFF, Nlink: 1}); err != nil {
		return err
	}
	w.closed = true
	if err := w.write(make([]byte, pad4(w.off))); err != nil {
		return err
	}
	if r := w.off % 512; r != 0 {
		return w.write(make([]byte, 512-r))
	}
	return nil
}

// Reader reads a newc archive.
type Reader struct {
	r      io.Reader
	off    int64
	remain int64
	done   bool
	// Concatenated makes Next continue past a trailer when another archive follows,
	// after NUL padding, as the kernel does for a concatenated initramfs.
	Concatenated bool
	pending      bool // first holds the first byte of the next header
	first        byte
}

// NewReader returns a Reader that reads from r.
func NewReader(r io.Reader) *Reader { return &Reader{r: r} }

// skipPadding consumes NUL bytes after a trailer. It reports whether a non-NUL byte
// follows, leaving that byte as the first of the next header.
func (r *Reader) skipPadding(first *byte) (bool, error) {
	var b [1]byte
	for {
		n, err := r.r.Read(b[:])
		if n == 1 {
			r.off++
			if b[0] != 0 {
				*first = b[0]
				return true, nil
			}
			continue
		}
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

func (r *Reader) skip(n int64) error {
	m, err := io.CopyN(io.Discard, r.r, n)
	r.off += m
	if err != nil {
		return fmt.Errorf("%w: truncated: %v", ErrFormat, err)
	}
	return nil
}

func (r *Reader) readFull(p []byte) error {
	n, err := io.ReadFull(r.r, p)
	r.off += int64(n)
	return err
}

// Next advances to the next entry and returns its header. It returns io.EOF after the
// trailer. Unread data of the previous entry is skipped.
func (r *Reader) Next() (*Header, error) {
	if r.done {
		return nil, io.EOF
	}
	if err := r.skip(r.remain); err != nil {
		return nil, err
	}
	r.remain = 0
	if err := r.skip(pad4(r.off)); err != nil {
		return nil, err
	}
	var hb [headerLen]byte
	if r.pending {
		hb[0] = r.first
		r.pending = false
		r.off++
		if err := r.readFull(hb[1:]); err != nil {
			return nil, fmt.Errorf("%w: short header: %v", ErrFormat, err)
		}
	} else if err := r.readFull(hb[:]); err != nil {
		return nil, fmt.Errorf("%w: short header: %v", ErrFormat, err)
	}
	if string(hb[:6]) != magic {
		return nil, fmt.Errorf("%w: bad magic %q", ErrFormat, hb[:6])
	}
	var f [13]uint32
	for i := range f {
		v, err := strconv.ParseUint(string(hb[6+8*i:14+8*i]), 16, 32)
		if err != nil {
			return nil, fmt.Errorf("%w: bad header field %d", ErrFormat, i)
		}
		f[i] = uint32(v)
	}
	nameSize := f[11]
	if nameSize < 2 || nameSize > maxNameLen {
		return nil, fmt.Errorf("%w: bad name size %d", ErrFormat, nameSize)
	}
	name := make([]byte, nameSize)
	if err := r.readFull(name); err != nil {
		return nil, fmt.Errorf("%w: short name: %v", ErrFormat, err)
	}
	if bytes.IndexByte(name, 0) != int(nameSize-1) {
		return nil, fmt.Errorf("%w: name not NUL-terminated or contains NUL", ErrFormat)
	}
	h := &Header{
		Name: string(name[:nameSize-1]), Ino: f[0], Mode: f[1], UID: f[2], GID: f[3],
		Nlink: f[4], Mtime: f[5], Size: f[6], DevMajor: f[7], DevMinor: f[8],
		RdevMajor: f[9], RdevMinor: f[10],
	}
	if err := r.skip(pad4(r.off)); err != nil {
		return nil, err
	}
	if h.Name == trailer {
		if r.Concatenated {
			more, err := r.skipPadding(&r.first)
			if err != nil {
				return nil, err
			}
			if more {
				// Alignment restarts with the next archive: count offsets from its
				// first byte, which skipPadding has already consumed.
				r.off = 0
				r.pending = true
				return r.Next()
			}
		}
		r.done = true
		return nil, io.EOF
	}
	r.remain = int64(h.Size)
	return h, nil
}

// Read reads the data of the current entry.
func (r *Reader) Read(p []byte) (int, error) {
	if r.remain <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remain {
		p = p[:r.remain]
	}
	n, err := r.r.Read(p)
	r.off += int64(n)
	r.remain -= int64(n)
	if err == io.EOF && r.remain > 0 {
		err = fmt.Errorf("%w: truncated data", ErrFormat)
	}
	return n, err
}
