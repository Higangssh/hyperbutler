package cpio

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Spec is a parsed initramfs description. Its line format is close to the kernel's
// gen_init_cpio, with optional ownership and a recursive "tree" directive:
//
//	# comment
//	dir   <name> <mode> [<uid> <gid>]
//	file  <name> <src> <mode> [<uid> <gid>]
//	slink <name> <target> [<mode> [<uid> <gid>]]
//	nod   <name> <mode> <c|b> <major> <minor> [<uid> <gid>]
//	tree  <name> <srcdir> <dirmode> <filemode>
//
// Modes are octal permission bits. Names are archive paths; a leading "/" is dropped.
// ${VAR} in src fields is replaced from the variables passed to ParseSpec. Relative src
// paths are relative to the base directory given to Build.
type Spec struct {
	Lines []SpecLine
}

// SpecLine is one directive.
type SpecLine struct {
	LineNo   int
	Kind     string // dir, file, slink, nod, tree
	Name     string
	Src      string // file/tree source, or slink target
	Mode     uint32 // permission bits; for tree, the file mode
	DirMode  uint32 // tree only
	UID, GID uint32
	DevType  byte // 'c' or 'b'
	Major    uint32
	Minor    uint32
}

func parseOctal(s string) (uint32, error) {
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil || v > 0o7777 {
		return 0, fmt.Errorf("bad mode %q", s)
	}
	return uint32(v), nil
}

func parseUint(s string) (uint32, error) {
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("bad number %q", s)
	}
	return uint32(v), nil
}

func cleanName(n string) (string, error) {
	n = strings.TrimLeft(n, "/")
	if n == "" {
		return "", fmt.Errorf("empty name")
	}
	if strings.Contains(n, `\`) {
		// Almost always a Windows path pasted into a spec; the kernel would create one
		// entry with a literal backslash in its name.
		return "", fmt.Errorf("name %q contains a backslash; use /", n)
	}
	c := path.Clean(n)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("name %q escapes the archive root", n)
	}
	return c, nil
}

func expand(s string, vars map[string]string) (string, error) {
	var missing string
	out := os.Expand(s, func(k string) string {
		v, ok := vars[k]
		if !ok && missing == "" {
			missing = k
		}
		return v
	})
	if missing != "" {
		return "", fmt.Errorf("undefined variable %q", missing)
	}
	return out, nil
}

// ParseSpec parses a spec. vars supplies ${VAR} values for src fields.
func ParseSpec(r io.Reader, vars map[string]string) (*Spec, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	s := &Spec{}
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		l, err := parseLine(strings.Fields(line), vars)
		if err != nil {
			return nil, fmt.Errorf("spec line %d: %w", n, err)
		}
		l.LineNo = n
		s.Lines = append(s.Lines, l)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return s, nil
}

func parseOwner(f []string) (uid, gid uint32, err error) {
	switch len(f) {
	case 0:
		return 0, 0, nil
	case 2:
		if uid, err = parseUint(f[0]); err != nil {
			return
		}
		gid, err = parseUint(f[1])
		return
	}
	return 0, 0, fmt.Errorf("expected <uid> <gid>")
}

func parseLine(f []string, vars map[string]string) (SpecLine, error) {
	var l SpecLine
	var err error
	if len(f) < 2 {
		return l, fmt.Errorf("too few fields")
	}
	l.Kind = f[0]
	if l.Name, err = cleanName(f[1]); err != nil {
		return l, err
	}
	switch l.Kind {
	case "dir":
		if len(f) < 3 {
			return l, fmt.Errorf("dir: too few fields")
		}
		if l.Mode, err = parseOctal(f[2]); err != nil {
			return l, err
		}
		l.UID, l.GID, err = parseOwner(f[3:])
	case "file":
		if len(f) < 4 {
			return l, fmt.Errorf("file: too few fields")
		}
		if l.Src, err = expand(f[2], vars); err != nil {
			return l, err
		}
		if l.Mode, err = parseOctal(f[3]); err != nil {
			return l, err
		}
		l.UID, l.GID, err = parseOwner(f[4:])
	case "slink":
		if len(f) < 3 {
			return l, fmt.Errorf("slink: too few fields")
		}
		l.Src = f[2]
		l.Mode = 0o777
		if len(f) > 3 {
			if l.Mode, err = parseOctal(f[3]); err != nil {
				return l, err
			}
			l.UID, l.GID, err = parseOwner(f[4:])
		}
	case "nod":
		if len(f) < 6 {
			return l, fmt.Errorf("nod: too few fields")
		}
		if l.Mode, err = parseOctal(f[2]); err != nil {
			return l, err
		}
		if f[3] != "c" && f[3] != "b" {
			return l, fmt.Errorf("nod: type must be c or b")
		}
		l.DevType = f[3][0]
		if l.Major, err = parseUint(f[4]); err != nil {
			return l, err
		}
		if l.Minor, err = parseUint(f[5]); err != nil {
			return l, err
		}
		l.UID, l.GID, err = parseOwner(f[6:])
	case "tree":
		if len(f) != 5 {
			return l, fmt.Errorf("tree: expected <name> <srcdir> <dirmode> <filemode>")
		}
		if l.Src, err = expand(f[2], vars); err != nil {
			return l, err
		}
		if l.DirMode, err = parseOctal(f[3]); err != nil {
			return l, err
		}
		l.Mode, err = parseOctal(f[4])
	default:
		return l, fmt.Errorf("unknown directive %q", l.Kind)
	}
	return l, err
}

// Build writes the archive described by s to w. Relative sources resolve against base.
// Missing parent directories are emitted as root-owned 0755 directories, because the
// kernel does not create them. All entries get mtime. Symlinks inside tree sources are
// rejected rather than followed, so the output depends only on file contents.
func (s *Spec) Build(w *Writer, base string, mtime uint32) error {
	b := &builder{w: w, mtime: mtime, seen: map[string]Header{}}
	for _, l := range s.Lines {
		if err := b.line(l, base); err != nil {
			return fmt.Errorf("spec line %d (%s %s): %w", l.LineNo, l.Kind, l.Name, err)
		}
	}
	return nil
}

type builder struct {
	w     *Writer
	mtime uint32
	seen  map[string]Header // every name written so far
}

func (b *builder) parents(name string) error {
	d := path.Dir(name)
	if d == "." {
		return nil
	}
	if prev, ok := b.seen[d]; ok {
		if prev.Mode&TypeMask != TypeDir {
			return fmt.Errorf("parent %q is not a directory", d)
		}
		return nil
	}
	if err := b.parents(d); err != nil {
		return err
	}
	return b.emit(Header{Name: d, Mode: TypeDir | 0o755}, "")
}

func (b *builder) emit(h Header, src string) error {
	if prev, ok := b.seen[h.Name]; ok {
		isDir := h.Mode&TypeMask == TypeDir
		switch {
		case !isDir || prev.Mode&TypeMask != TypeDir:
			return fmt.Errorf("duplicate entry %q", h.Name)
		case prev.Mode != h.Mode || prev.UID != h.UID || prev.GID != h.GID:
			// The kernel keeps the first one, so a later "dir" would be silently ignored.
			return fmt.Errorf("directory %q was already emitted as %o %d:%d (declare it before its contents)",
				h.Name, prev.Mode&0o7777, prev.UID, prev.GID)
		}
		return nil // identical directory repeated, e.g. by a tree directive
	}
	if err := b.parents(h.Name); err != nil {
		return err
	}
	b.seen[h.Name] = h
	h.Mtime = b.mtime
	var f *os.File
	if src != "" {
		var err error
		if f, err = os.Open(src); err != nil {
			return err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", src)
		}
		if st.Size() > 0xFFFFFFFF {
			return fmt.Errorf("%s is larger than 4 GiB", src)
		}
		h.Size = uint32(st.Size())
	}
	if err := b.w.WriteHeader(h); err != nil {
		return err
	}
	if f != nil {
		// Symlink targets are written by the caller after emit returns.
		n, err := io.Copy(b.w, f)
		if err != nil {
			return err
		}
		if n != int64(h.Size) {
			return fmt.Errorf("%s changed size while reading", src)
		}
	}
	return nil
}

func resolve(base, src string) string {
	if filepath.IsAbs(src) {
		return src
	}
	return filepath.Join(base, filepath.FromSlash(src))
}

func (b *builder) line(l SpecLine, base string) error {
	switch l.Kind {
	case "dir":
		return b.emit(Header{Name: l.Name, Mode: TypeDir | l.Mode, UID: l.UID, GID: l.GID}, "")
	case "file":
		return b.emit(Header{Name: l.Name, Mode: TypeRegular | l.Mode, UID: l.UID, GID: l.GID}, resolve(base, l.Src))
	case "slink":
		if err := b.emit(Header{Name: l.Name, Mode: TypeSymlink | l.Mode, UID: l.UID, GID: l.GID, Size: uint32(len(l.Src))}, ""); err != nil {
			return err
		}
		_, err := b.w.Write([]byte(l.Src))
		return err
	case "nod":
		t := uint32(TypeChar)
		if l.DevType == 'b' {
			t = TypeBlock
		}
		return b.emit(Header{Name: l.Name, Mode: t | l.Mode, UID: l.UID, GID: l.GID, RdevMajor: l.Major, RdevMinor: l.Minor}, "")
	case "tree":
		return b.tree(l, resolve(base, l.Src))
	}
	return fmt.Errorf("unknown directive %q", l.Kind)
}

func (b *builder) tree(l SpecLine, root string) error {
	if err := b.emit(Header{Name: l.Name, Mode: TypeDir | l.DirMode}, ""); err != nil {
		return err
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	for _, e := range ents {
		name := l.Name + "/" + e.Name()
		src := filepath.Join(root, e.Name())
		switch {
		case e.Type()&os.ModeSymlink != 0:
			return fmt.Errorf("%s: symlinks in tree sources are not supported", src)
		case e.IsDir():
			sub := l
			sub.Name = name
			if err := b.tree(sub, src); err != nil {
				return err
			}
		default:
			if err := b.emit(Header{Name: name, Mode: TypeRegular | l.Mode}, src); err != nil {
				return err
			}
		}
	}
	return nil
}
