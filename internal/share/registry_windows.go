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
	"time"
)

// Registry registers bind sources as shares on demand, for RewriteCreate.
type Registry struct {
	// Server serves the shares; MountPoint is where the guest relay mounted it.
	Server     *Server
	MountPoint string
	// MaxShares bounds the live shares (each holds a root handle and a watcher)
	// [DefaultMaxShares]. Past it a new bind source is refused until Prune removes
	// unused ones.
	MaxShares int

	mu       sync.Mutex
	names    map[string]string    // share name -> guest path, for reuse
	resolved map[string]time.Time // share name -> last Resolve that returned it
}

// DefaultMaxShares is Registry.MaxShares when it is zero.
const DefaultMaxShares = 1024

// Prune removes every share that used does not name and that no Resolve returned since
// before. The caller lists the containers first and passes the time it started minus
// a grace period, so a share that a create resolved but whose container dockerd has not
// recorded yet is kept. It returns the names removed.
func (r *Registry) Prune(used map[string]bool, before time.Time) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var removed []string
	for name := range r.names {
		if used[name] || !r.resolved[name].Before(before) {
			continue
		}
		// Unregistered first, so a failing Close cannot leave a name that maps to no
		// share.
		delete(r.names, name)
		delete(r.resolved, name)
		_ = r.Server.RemoveShare(name)
		removed = append(removed, name)
	}
	return removed
}

// Len returns the number of live shares.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.names)
}

// shareName is stable for a host path, so the same source maps to the same share.
func shareName(key string) string {
	h := sha256.Sum256([]byte(strings.ToLower(key)))
	return "b-" + hex.EncodeToString(h[:8])
}

// Resolve returns the guest path for a Windows bind source, sharing it first if needed.
// A directory becomes a share of its own; a file shares its directory with only that
// file visible. A missing source is created as a directory, as dockerd does for -v.
func (r *Registry) Resolve(winPath string) (string, error) { return r.resolve(winPath, true) }

// ResolveExisting is Resolve for --mount sources: a missing source is an error, as
// dockerd reports it ("bind source path does not exist").
func (r *Registry) ResolveExisting(winPath string) (string, error) {
	return r.resolve(winPath, false)
}

func (r *Registry) resolve(winPath string, create bool) (string, error) {
	if !WindowsPath(winPath) {
		return "", fmt.Errorf("share: %q is not an absolute Windows path", winPath)
	}
	p := filepath.Clean(strings.TrimPrefix(winPath, `\\?\`))
	fi, err := os.Stat(p)
	if os.IsNotExist(err) && !create {
		return "", fmt.Errorf("bind source path does not exist: %s", p)
	}
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
		r.resolved = map[string]time.Time{}
	}
	if g, ok := r.names[name]; ok {
		fs.Close()
		r.resolved[name] = time.Now()
		return g, nil
	}
	max := r.MaxShares
	if max <= 0 {
		max = DefaultMaxShares
	}
	if len(r.names) >= max {
		fs.Close()
		return "", fmt.Errorf("share: %d bind-mount sources are in use, the limit; remove containers that no longer need theirs", max)
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
	r.resolved[name] = time.Now()
	return guest, nil
}
