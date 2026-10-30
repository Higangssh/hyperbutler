//go:build windows

package share

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Registry registers bind sources as shares on demand, for RewriteCreate.
type Registry struct {
	// Server serves the shares; MountPoint is where the guest relay mounted it.
	Server     *Server
	MountPoint string

	mu    sync.Mutex
	names map[string]string // share name -> guest path, for reuse
}

// shareName is stable for a host path, so the same source maps to the same share.
func shareName(key string) string {
	h := sha256.Sum256([]byte(strings.ToLower(key)))
	return "b-" + hex.EncodeToString(h[:8])
}

// Resolve returns the guest path for a Windows bind source, sharing it first if needed.
// A directory becomes a share of its own; a file shares its directory with only that
// file visible. A missing source is created as a directory, as dockerd does for -v.
func (r *Registry) Resolve(winPath string) (string, error) {
	if !WindowsPath(winPath) {
		return "", fmt.Errorf("share: %q is not an absolute Windows path", winPath)
	}
	p := filepath.Clean(strings.TrimPrefix(winPath, `\\?\`))
	fi, err := os.Stat(p)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return "", err
		}
		fi, err = os.Stat(p)
	}
	if err != nil {
		return "", err
	}
	dir, only := p, ""
	if !fi.IsDir() {
		if !fi.Mode().IsRegular() {
			return "", fmt.Errorf("share: %s is neither a directory nor a regular file", p)
		}
		dir, only = filepath.Dir(p), fromNT(filepath.Base(p))
		if checkName(only) != nil {
			return "", fmt.Errorf("share: bad file name in %s", p)
		}
	}
	fs, err := OpenNTFS(dir)
	if err != nil {
		return "", err
	}
	// The root's identity is part of the key: a share holds its root open, so after the
	// directory is deleted and made again the old share still serves the deleted one.
	// The same path then gets a new share (an old container keeps the old one).
	key := fs.RootPath() + "\x00" + fs.RootID()
	if only != "" {
		key += "\x00" + only
	}
	name := shareName(key)
	guest := r.MountPoint + "/" + name
	if only != "" {
		guest += "/" + only
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.names == nil {
		r.names = map[string]string{}
	}
	if g, ok := r.names[name]; ok {
		fs.Close()
		return g, nil
	}
	sh := &Share{Name: name, FS: fs}
	if only != "" {
		sh.Only = []string{only}
	}
	if err := r.Server.AddShare(sh); err != nil {
		fs.Close()
		return "", err
	}
	r.names[name] = guest
	return guest, nil
}
