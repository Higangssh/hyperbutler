package copyin

import (
	"archive/tar"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Copied is what the host sent for one file: the review's conflict check compares the
// project file with Sum before apply replaces it.
type Copied struct {
	Sum  [sha256.Size]byte
	Link string
}

// WriteTar writes the plan's files from dir to w as a tar stream (regular files and
// symbolic links, no owners or times) and returns the hash of each file as sent. A
// file whose size changed since Select is sent as it is now, and its hash is of what
// was sent.
func WriteTar(w io.Writer, dir string, p Plan) (map[string]Copied, error) {
	tw := tar.NewWriter(w)
	sent := make(map[string]Copied, len(p.Files))
	for _, e := range p.Files {
		h := &tar.Header{Name: e.Path, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		if e.Link != "" {
			h.Typeflag, h.Linkname = tar.TypeSymlink, e.Link
			if err := tw.WriteHeader(h); err != nil {
				return nil, err
			}
			sent[e.Path] = Copied{Link: e.Link}
			continue
		}
		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(e.Path)))
		if err != nil {
			return nil, err
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		h.Typeflag, h.Size, h.Mode = tar.TypeReg, fi.Size(), 0o644
		if isExecutable(e.Path) {
			h.Mode = 0o755
		}
		if err := tw.WriteHeader(h); err != nil {
			f.Close()
			return nil, err
		}
		d := sha256.New()
		n, err := io.Copy(io.MultiWriter(tw, d), io.LimitReader(f, h.Size))
		f.Close()
		if err != nil {
			return nil, err
		}
		if n != h.Size {
			return nil, fmt.Errorf("%s shrank while it was copied", e.Path)
		}
		var c Copied
		copy(c.Sum[:], d.Sum(nil))
		sent[e.Path] = c
	}
	return sent, tw.Close()
}

// isExecutable guesses the execute bit Windows does not keep: scripts with a shebang
// would need the content, so only the common script extensions are marked.
func isExecutable(p string) bool {
	switch filepath.Ext(p) {
	case ".sh", ".bash", ".py", ".pl", ".rb":
		return true
	}
	return false
}
