package share

// Docker API path translation (ADR 0007). The docker CLI on Windows sends bind sources
// as Windows paths ("C:\src:/src", or --mount source=C:\src). The guest's dockerd
// cannot use them, so the API proxy rewrites POST /containers/create: each Windows
// source is registered as a share and replaced by its guest path under the FUSE mount.
// Everything here parses input from the docker client (a local, same-user caller, but
// still parsed strictly and fuzzed).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// maxCreateBody bounds the container create body the proxy will rewrite.
const maxCreateBody = 4 << 20

// WindowsPath reports whether p is an absolute Windows path with a drive letter
// ("C:\x", "C:/x", "C:\") or the same behind "\\?\". UNC and device paths are not.
func WindowsPath(p string) bool {
	return driveAbs(strings.TrimPrefix(p, `\\?\`))
}

// driveAbs reports whether p starts with a drive letter, a colon and a separator.
func driveAbs(p string) bool {
	if len(p) < 3 {
		return false
	}
	c := p[0] | 0x20
	return c >= 'a' && c <= 'z' && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

// splitBind splits a docker -v spec whose source is a Windows path into source, target
// and options. ok is false for any spec whose source is not a Windows path (a named
// volume or a guest path), which is left alone.
func splitBind(spec string) (src, target, opts string, ok bool, err error) {
	prefix := ""
	s := spec
	if strings.HasPrefix(s, `\\?\`) {
		prefix, s = `\\?\`, s[4:]
	}
	if !driveAbs(s) {
		// Only one "\\?\" prefix (FuzzSplitBind found "\\?\\\?\A:/").
		return "", "", "", false, nil
	}
	i := strings.IndexByte(s[2:], ':')
	if i < 0 {
		return "", "", "", true, fmt.Errorf("share: bind %q has no target", spec)
	}
	src = prefix + s[:2+i]
	rest := s[2+i+1:]
	target, opts, _ = strings.Cut(rest, ":")
	if target == "" || target[0] != '/' {
		return "", "", "", true, fmt.Errorf("share: bind %q: target must be an absolute guest path", spec)
	}
	if strings.ContainsAny(opts, ":") {
		return "", "", "", true, fmt.Errorf("share: bind %q: bad options", spec)
	}
	return src, target, opts, true, nil
}

// Resolver turns a Windows bind source into the guest path that serves it.
type Resolver func(windowsPath string) (guestPath string, err error)

// RewriteCreate rewrites a POST /containers/create body. It returns the body unchanged
// (same slice) when nothing needed translation.
func RewriteCreate(body []byte, resolve Resolver) ([]byte, error) {
	if len(body) > maxCreateBody {
		return nil, errors.New("share: container create body too large")
	}
	if !bytes.Contains(body, []byte(":")) {
		return body, nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("share: container create body: %w", err)
	}
	hc, _ := doc["HostConfig"].(map[string]any)
	if hc == nil {
		return body, nil
	}
	changed := false
	if binds, ok := hc["Binds"].([]any); ok {
		for i, b := range binds {
			spec, ok := b.(string)
			if !ok {
				continue
			}
			src, target, opts, win, err := splitBind(spec)
			if err != nil {
				return nil, err
			}
			if !win {
				continue
			}
			g, err := resolve(src)
			if err != nil {
				return nil, err
			}
			ns := g + ":" + target
			if opts != "" {
				ns += ":" + opts
			}
			binds[i] = ns
			changed = true
		}
	}
	if mounts, ok := hc["Mounts"].([]any); ok {
		for _, m := range mounts {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := mm["Type"].(string); t != "bind" {
				continue
			}
			src, _ := mm["Source"].(string)
			if !WindowsPath(src) {
				continue
			}
			g, err := resolve(src)
			if err != nil {
				return nil, err
			}
			mm["Source"] = g
			changed = true
		}
	}
	if !changed {
		return body, nil
	}
	return json.Marshal(doc)
}
