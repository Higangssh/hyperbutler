package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// linkFS is what resolveLocal needs of the file system.
type linkFS interface {
	Lstat(path string) (fs.FileInfo, error)
	Readlink(path string) (string, error)
	// Remote reports whether root (such as `Z:\`) is a network drive.
	Remote(root string) bool
}

type osLinkFS struct{}

func (osLinkFS) Lstat(p string) (fs.FileInfo, error) { return os.Lstat(p) }
func (osLinkFS) Readlink(p string) (string, error)   { return os.Readlink(p) }
func (osLinkFS) Remote(root string) bool             { return remoteDrive(root) }

func (e Env) linkFS() linkFS {
	if e.links == nil {
		return osLinkFS{}
	}
	return e.links
}

// maxLinkHops bounds the links resolveLocal follows (Windows itself stops at 63).
const maxLinkHops = 32

var errNotLocal = errors.New("want a file on a local drive, not a network path or a link to one")

// resolveLocal follows every symbolic link and junction in p, an absolute X:\ path,
// without opening what they point to, and returns the path they lead to. It refuses a
// path that is, or leads to, a UNC or device path or a network drive, so the file is
// read from a local disk only.
func resolveLocal(p string, fsys linkFS) (string, error) {
	p = filepath.Clean(p)
	for hops := 0; ; hops++ {
		if hops > maxLinkHops {
			return "", errors.New("too many links")
		}
		if !drivePath(p) {
			return "", errNotLocal
		}
		vol := p[:3]
		if fsys.Remote(vol) {
			return "", fmt.Errorf("%s is a network drive: %w", vol[:2], errNotLocal)
		}
		elems := strings.Split(p[3:], `\`)
		cur, next := vol, ""
		for i, el := range elems {
			if el == "" {
				continue
			}
			cur = filepath.Join(cur, el)
			fi, err := fsys.Lstat(cur)
			if err != nil {
				return "", err
			}
			if fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) == 0 {
				continue
			}
			t, err := fsys.Readlink(cur)
			if err != nil {
				if fi.Mode()&fs.ModeSymlink == 0 {
					continue // another kind of reparse point (a cloud file, for one): not a link
				}
				return "", fmt.Errorf("%s: %w", cur, err)
			}
			if t, err = linkTarget(t, vol, filepath.Dir(cur)); err != nil {
				return "", fmt.Errorf("%s: %w", cur, err)
			}
			next = filepath.Join(append([]string{t}, elems[i+1:]...)...)
			break
		}
		if next == "" {
			return p, nil
		}
		p = next
	}
}

// linkTarget turns what a link at dir points to into a path to check again: an NT
// path (\??\C:\x, \\?\C:\x) into C:\x, \??\UNC\server\share into \\server\share (which
// is then refused), a path from the root of the drive or a relative one into an
// absolute one. Other NT and device paths are refused.
func linkTarget(t, vol, dir string) (string, error) {
	for _, pre := range []string{`\??\`, `\\?\`, `\\.\`} {
		if !strings.HasPrefix(t, pre) {
			continue
		}
		rest := t[len(pre):]
		if len(rest) >= 4 && strings.EqualFold(rest[:4], `UNC\`) {
			return `\\` + rest[4:], nil
		}
		if !drivePath(rest) {
			return "", errNotLocal
		}
		return rest, nil
	}
	switch {
	case strings.HasPrefix(t, `\\`) || drivePath(t):
		return t, nil
	case strings.HasPrefix(t, `\`) || strings.HasPrefix(t, `/`):
		return filepath.Join(vol, t), nil
	case len(t) >= 2 && t[1] == ':':
		return "", errNotLocal // C:x, relative to a drive's current directory
	}
	return filepath.Join(dir, t), nil
}
