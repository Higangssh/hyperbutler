package desktopimport

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"path"
	"strings"
)

// volumeRoot is the first name of every entry in a helper's archive: the archive API
// names entries after the base name of the path it was asked for (/v).
const volumeRoot = "v"

// errBadArchive is an archive entry the importer refuses to pass on.
var errBadArchive = errors.New("unexpected archive entry")

// checkEntryName accepts "v", "v/" and names under "v/" with no "..", "." or empty
// segment and no leading '/': an entry that would land outside the volume is refused
// before anything reaches hyperbutler (dockerd's own extraction refuses them too).
func checkEntryName(name string) error {
	n := strings.TrimSuffix(name, "/")
	if n == volumeRoot {
		return nil
	}
	if !strings.HasPrefix(n, volumeRoot+"/") || strings.ContainsRune(n, 0) || strings.Contains(n, `\`) {
		return fmt.Errorf("%w %q: not under %s/", errBadArchive, clipName(name), volumeRoot)
	}
	for seg := range strings.SplitSeq(n, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%w %q: a %q path segment", errBadArchive, clipName(name), seg)
		}
	}
	if path.Clean(n) != n {
		return fmt.Errorf("%w %q: not a clean path", errBadArchive, clipName(name))
	}
	return nil
}

func clipName(s string) string {
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return strings.ToValidUTF8(s, "?")
}

// tarSum hashes a volume archive: every entry's name, type, permission bits, owner,
// size, link target and device numbers, and its content, in archive order. Times,
// extended attributes and the tar format's own encoding are left out, so the archive
// hyperbutler's dockerd makes of the copied volume hashes like the one Docker Desktop's
// made of the original. Both are made by dockerd's archive code walking the volume in
// lexical order, so equal trees give entries in the same order.
type tarSum struct {
	h       hash.Hash
	entries int64
	bytes   int64 // content bytes
}

func newTarSum() *tarSum { return &tarSum{h: sha256.New()} }

func (s *tarSum) field(b []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(b)))
	s.h.Write(n[:])
	s.h.Write(b)
}

func (s *tarSum) num(v int64) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(v)) //nolint:gosec // G115: the bit pattern is hashed, not used as a number
	s.h.Write(n[:])
}

func (s *tarSum) header(h *tar.Header) {
	s.entries++
	s.field([]byte(strings.TrimSuffix(h.Name, "/")))
	s.num(int64(h.Typeflag))
	s.num(h.Mode & 0o7777)
	s.num(int64(h.Uid))
	s.num(int64(h.Gid))
	s.num(h.Size)
	s.field([]byte(h.Linkname))
	s.num(h.Devmajor)
	s.num(h.Devminor)
}

// Sum is "sha256:" and the hex digest.
func (s *tarSum) Sum() string { return "sha256:" + hex.EncodeToString(s.h.Sum(nil)) }

// allowedType is the entry types a volume archive holds.
func allowedType(t byte) bool {
	switch t {
	case tar.TypeReg, tar.TypeDir, tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		return true
	}
	return false
}

// hashArchive reads a volume archive from r, checks every entry (checkEntryName, the
// type, hard links staying within the volume) and returns its tarSum. With w set, it
// writes each entry to w as it reads it, so one pass both checks, hashes and passes the
// archive on; the caller closes w. Nothing is ever extracted on this machine.
func hashArchive(r io.Reader, w *tar.Writer) (*tarSum, error) {
	sum := newTarSum()
	tr := tar.NewReader(r)
	sawRoot := false
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read the volume archive: %w", err)
		}
		if err := checkEntryName(h.Name); err != nil {
			return nil, err
		}
		if !allowedType(h.Typeflag) {
			return nil, fmt.Errorf("%w %q: type %q", errBadArchive, clipName(h.Name), h.Typeflag)
		}
		if strings.TrimSuffix(h.Name, "/") == volumeRoot {
			if h.Typeflag != tar.TypeDir || sawRoot {
				return nil, fmt.Errorf("%w %q: the volume root is not one directory", errBadArchive, clipName(h.Name))
			}
			sawRoot = true
		} else if !sawRoot {
			return nil, fmt.Errorf("%w %q: before the volume root", errBadArchive, clipName(h.Name))
		}
		if h.Typeflag == tar.TypeLink {
			if err := checkEntryName(h.Linkname); err != nil {
				return nil, fmt.Errorf("hard link %q: %w", clipName(h.Name), err)
			}
		}
		if h.Size < 0 || (h.Typeflag != tar.TypeReg && h.Size != 0) {
			return nil, fmt.Errorf("%w %q: size %d", errBadArchive, clipName(h.Name), h.Size)
		}
		sum.header(h)
		if w != nil {
			if err := w.WriteHeader(h); err != nil {
				return nil, fmt.Errorf("pass the volume archive on: %w", err)
			}
		}
		if h.Typeflag == tar.TypeReg {
			ch := sha256.New()
			var dst io.Writer = ch
			if w != nil {
				dst = io.MultiWriter(ch, w)
			}
			n, err := io.Copy(dst, tr) //nolint:gosec // G110: no decompression; a volume is streamed whole on purpose, never held in memory
			if err != nil {
				return nil, fmt.Errorf("volume archive entry %q: %w", clipName(h.Name), err)
			}
			if n != h.Size {
				return nil, fmt.Errorf("%w %q: %d of %d bytes", errBadArchive, clipName(h.Name), n, h.Size)
			}
			sum.bytes += n
			sum.field(ch.Sum(nil))
		}
	}
	if !sawRoot {
		return nil, fmt.Errorf("%w: the archive has no volume root", errBadArchive)
	}
	return sum, nil
}
