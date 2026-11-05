// Package workspace is the guest side of a sandbox session's project copy (ADR 0009): it
// unpacks the project the host sends into the workspace, records what it unpacked (the
// baseline), and answers the host's requests for what changed since (internal/sandbox/
// wire). It runs inside the session VM, which the agent controls, so it protects
// nothing on the host: the host bounds and checks everything it reads from here. It is
// portable Go so that it is tested on the host too.
package workspace

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"

	"github.com/Higangssh/hyperbutler/internal/sandbox/wire"
)

// File is one copied-in file as the baseline records it.
type File struct {
	Sum  [sha256.Size]byte `json:"sum"`
	Size int64             `json:"size"`
	Exec bool              `json:"exec,omitempty"`
	Link string            `json:"link,omitempty"`
}

// Baseline maps a workspace-relative '/' path to what was copied in.
type Baseline map[string]File

// Extract unpacks the tar stream r into root and returns the baseline. Names must be
// clean relative paths; the host sends only regular files, directories and symbolic
// links.
func Extract(root *os.Root, r io.Reader) (Baseline, error) {
	base := Baseline{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return base, nil
		}
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(h.Name, "/")
		if name == "" || path.Clean(name) != name || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return nil, fmt.Errorf("tar: bad name %q", h.Name)
		}
		if dir := path.Dir(name); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return nil, err
			}
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return nil, err
			}
		case tar.TypeSymlink:
			if err := root.Symlink(h.Linkname, name); err != nil {
				return nil, err
			}
			base[name] = File{Link: h.Linkname}
		case tar.TypeReg:
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return nil, err
			}
			d := sha256.New()
			n, err := io.Copy(io.MultiWriter(f, d), tr) //nolint:gosec // G110: the host's own project stream, bounded by what it sends
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return nil, err
			}
			var s [sha256.Size]byte
			copy(s[:], d.Sum(nil))
			base[name] = File{Sum: s, Size: n, Exec: mode&0o111 != 0}
		default:
			return nil, fmt.Errorf("tar: %q has type %q", h.Name, h.Typeflag)
		}
	}
}

// Save writes the baseline to name; Load reads it back.
func (b Baseline) Save(name string) error {
	j, err := json.Marshal(b)
	if err != nil {
		return err
	}
	return os.WriteFile(name, j, 0o600)
}

// Load reads a baseline Save wrote.
func Load(name string) (Baseline, error) {
	j, err := os.ReadFile(name) //nolint:gosec // G304: the agent's own baseline file
	if err != nil {
		return nil, err
	}
	b := Baseline{}
	return b, json.Unmarshal(j, &b)
}

// Changes lists what differs in root from base, sorted by path. .git is left out: the
// agent's commits come out as a bundle.
func Changes(root *os.Root, base Baseline) ([]wire.Msg, error) {
	var out []wire.Msg
	seen := map[string]bool{}
	err := fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		cur, ok, err := stat(root, p, d)
		if err != nil || !ok {
			return err
		}
		seen[p] = true
		old, had := base[p]
		switch {
		case !had:
			out = append(out, entry(wire.Added, p, cur))
		case old != cur:
			out = append(out, entry(wire.Modified, p, cur))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for p := range base {
		if !seen[p] {
			out = append(out, wire.Msg{Type: wire.TypeEntry, Kind: wire.Deleted, Path: p})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if len(out) > wire.MaxEntries {
		return nil, fmt.Errorf("%d changed files (at most %d)", len(out), wire.MaxEntries)
	}
	return out, nil
}

// stat describes p as the baseline would; ok is false for files that are neither
// regular nor links (a socket, a FIFO), which are not listed.
func stat(root *os.Root, p string, d fs.DirEntry) (File, bool, error) {
	if d.Type()&fs.ModeSymlink != 0 {
		t, err := root.Readlink(p)
		return File{Link: t}, err == nil, err
	}
	if !d.Type().IsRegular() {
		return File{}, false, nil
	}
	f, err := root.Open(p)
	if err != nil {
		return File{}, false, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return File{}, false, err
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return File{}, false, err
	}
	var s [sha256.Size]byte
	copy(s[:], h.Sum(nil))
	return File{Sum: s, Size: n, Exec: fi.Mode()&0o111 != 0}, true, nil
}

func entry(k wire.Kind, p string, f File) wire.Msg {
	m := wire.Msg{Type: wire.TypeEntry, Kind: k, Path: p, Size: uint64(max(f.Size, 0))}
	if f.Exec {
		m.Flags |= wire.FlagExec
	}
	if f.Link != "" {
		m.Flags |= wire.FlagLink
		m.Link = f.Link
	}
	return m
}

// Serve answers the host's requests on c until it closes.
func Serve(c io.ReadWriter, root *os.Root, base Baseline) error {
	buf := make([]byte, wire.MaxBody)
	for {
		body, err := wire.ReadFrame(c, buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		req, err := wire.Decode(body, false)
		if err != nil {
			return err
		}
		for _, m := range answer(root, base, req) {
			m.Seq = req.Seq
			b, err := wire.Encode(m)
			if err != nil {
				b, _ = wire.Encode(failure(req.Seq, err))
			}
			if _, err := c.Write(b); err != nil {
				return err
			}
		}
	}
}

func answer(root *os.Root, base Baseline, req wire.Msg) []wire.Msg {
	switch req.Type {
	case wire.TypeList:
		ch, err := Changes(root, base)
		if err != nil {
			return []wire.Msg{failure(0, err)}
		}
		//nolint:gosec // G115: at most wire.MaxEntries
		return append(ch, wire.Msg{Type: wire.TypeEnd, Count: uint32(len(ch))})
	case wire.TypeRead:
		f, err := root.Open(req.Path)
		if err != nil {
			return []wire.Msg{failure(0, err)}
		}
		defer func() { _ = f.Close() }()
		b := make([]byte, req.Len)
		n, err := f.ReadAt(b, int64(min(req.Off, 1<<62)))
		if err != nil && !errors.Is(err, io.EOF) {
			return []wire.Msg{failure(0, err)}
		}
		return []wire.Msg{{Type: wire.TypeData, Data: b[:n], EOF: errors.Is(err, io.EOF) || n < len(b)}}
	}
	return []wire.Msg{failure(0, errors.New("unknown request"))}
}

// Linux errnos, sent whatever the platform the package is built for.
const (
	linuxENOENT = 2
	linuxEIO    = 5
)

func failure(seq uint32, err error) wire.Msg {
	errno := uint16(linuxEIO)
	var e syscall.Errno
	if errors.As(err, &e) && e > 0 && e <= 4095 {
		errno = uint16(e)
	} else if errors.Is(err, fs.ErrNotExist) {
		errno = linuxENOENT
	}
	text := err.Error()
	if len(text) > wire.MaxMsg {
		text = text[:wire.MaxMsg]
	}
	return wire.Msg{Type: wire.TypeError, Seq: seq, Errno: errno, Text: text}
}
