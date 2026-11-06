package egress

import (
	"bytes"
	"strings"

	"github.com/Higangssh/hyperbutler/internal/netstack"
)

// maxHeadBytes bounds a request head; one that has not ended by then is denied.
const maxHeadBytes = netstack.MaxInspectBytes

func headBad() parsed { return parsed{state: parseBad, kind: KindHTTPHost} }

// parseHTTPHead reads an HTTP/1.x request head from the start of a stream and returns
// its one Host, without the port. It refuses what a server might read differently:
// anything but CRLF line ends, folded lines, a space before a header's colon, blank lines
// first, control characters, and the request forms that make a server ignore Host
// (absolute-form targets) or that ask it to tunnel (CONNECT).
//
// It sees the first request only. Later requests on the connection go by unread.
func parseHTTPHead(b []byte) parsed {
	full := len(b) >= maxHeadBytes
	if full {
		b = b[:maxHeadBytes]
	}
	host := ""
	hosts := 0
	first := true
	for {
		end := bytes.IndexByte(b, '\n')
		if end < 0 {
			if !plausibleTail(b, first) || full {
				return headBad()
			}
			return parsed{state: parseMore}
		}
		if end == 0 || b[end-1] != '\r' {
			return headBad()
		}
		line := b[:end-1]
		b = b[end+1:]
		if !cleanLine(line, first) {
			return headBad()
		}
		switch {
		case first:
			if !requestLineOK(string(line)) {
				return headBad()
			}
			first = false
		case len(line) == 0:
			if hosts != 1 {
				return headBad()
			}
			return parsed{state: parseDone, name: host}
		default:
			name, value, ok := splitHeader(line)
			if !ok {
				return headBad()
			}
			if strings.EqualFold(name, "host") {
				if hosts++; hosts > 1 {
					return headBad()
				}
				if host, ok = hostWithoutPort(value); !ok {
					return headBad()
				}
			}
		}
	}
}

// plausibleTail looks at a line that has not ended yet, so that something other than HTTP
// is denied at once and not after the stack's deadline. It refuses only what cleanLine or
// the method check would refuse when the line ends.
func plausibleTail(b []byte, first bool) bool {
	if n := len(b); n > 0 && b[n-1] == '\r' {
		b = b[:n-1]
	}
	if !cleanLine(b, first) {
		return false
	}
	if first {
		if sp := bytes.IndexByte(b, ' '); sp >= 0 {
			b = b[:sp]
		}
		return isToken(string(b)) || len(b) == 0
	}
	return true
}

// cleanLine refuses control characters: a request line has none, a header line only tabs.
func cleanLine(line []byte, requestLine bool) bool {
	for _, c := range line {
		if (c < 0x20 || c == 0x7f) && (c != '\t' || requestLine) {
			return false
		}
	}
	return true
}

// requestLineOK takes METHOD SP target SP HTTP/1.0 or HTTP/1.1, with target in origin
// form, or * for OPTIONS. Under an absolute-form target a server ignores Host.
func requestLineOK(line string) bool {
	parts := strings.Split(line, " ")
	if len(parts) != 3 {
		return false
	}
	method, target, version := parts[0], parts[1], parts[2]
	if !isToken(method) || strings.EqualFold(method, "CONNECT") {
		return false
	}
	if version != "HTTP/1.1" && version != "HTTP/1.0" {
		return false
	}
	if target == "*" {
		return method == "OPTIONS"
	}
	return strings.HasPrefix(target, "/")
}

// splitHeader splits name: value. A line that starts with a space or a tab (obsolete line
// folding) has no valid name, and neither has one with a space before the colon.
func splitHeader(line []byte) (name, value string, ok bool) {
	colon := bytes.IndexByte(line, ':')
	if colon < 0 {
		return "", "", false
	}
	name = string(line[:colon])
	if !isToken(name) {
		return "", "", false
	}
	return name, strings.Trim(string(line[colon+1:]), " \t"), true
}

// hostWithoutPort strips an optional :port. A second colon means an IPv6 address, which
// no name can be pinned as, and no host has white space in it, before the port or anywhere
// else. The rest is left for canonName, which refuses user information and lists.
func hostWithoutPort(v string) (string, bool) {
	if i := strings.LastIndexByte(v, ':'); i >= 0 {
		if _, err := parsePort(v[i+1:]); err != nil {
			return "", false
		}
		v = v[:i]
	}
	return v, v != "" && !strings.ContainsAny(v, ": \t")
}

// isToken is RFC 9110's token.
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}
