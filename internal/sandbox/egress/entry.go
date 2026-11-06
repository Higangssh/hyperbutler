package egress

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	maxNameLen  = 253
	maxLabelLen = 63
	// maxEntryLen is the longest spec ParseEntry reads: lan: or *., a name and :65535.
	maxEntryLen = 4 + maxNameLen + 1 + 6
)

// Entry is one thing a session may reach. Only ParseEntry's forms are valid: a port, a
// wildcard and lan: do not combine.
type Entry struct {
	// Name is lower-case ASCII without a trailing dot. For a wildcard, it is the zone
	// after "*.".
	Name string
	// Port is 0 for the web ports (443, and 80 in dev). host:port allows that port only,
	// which is why the preset lists github.com and github.com:22.
	Port uint16
	// Wildcard covers every name below Name, at any depth, but not Name itself.
	Wildcard bool
	// LAN keeps private answers for this name.
	LAN bool
}

// String is the form ParseEntry reads back.
func (e Entry) String() string {
	switch {
	case e.Wildcard:
		return "*." + e.Name
	case e.LAN:
		return "lan:" + e.Name
	case e.Port != 0:
		return e.Name + ":" + strconv.Itoa(int(e.Port))
	}
	return e.Name
}

func (e Entry) validate() error {
	if canon, ok := canonName(e.Name); !ok || canon != e.Name {
		return fmt.Errorf("egress: entry name %s is not a lower-case DNS name without a trailing dot", quoted(e.Name))
	}
	if (e.Wildcard && e.LAN) || (e.Wildcard && e.Port != 0) || (e.LAN && e.Port != 0) {
		return fmt.Errorf("egress: entry %s: a port, a wildcard and lan: do not combine", quoted(e.Name))
	}
	if e.Name == "lan" && e.Port != 0 {
		return errors.New("egress: a host called lan with a port reads as a lan: entry")
	}
	if e.Wildcard && !strings.Contains(e.Name, ".") {
		return fmt.Errorf("egress: wildcard *.%s would cover a whole top-level domain", e.Name)
	}
	return nil
}

// ParseEntry parses host, host:port, *.host or lan:host. The input comes from the user or
// a config file and is treated as untrusted.
func ParseEntry(s string) (Entry, error) {
	if len(s) > maxEntryLen {
		return Entry{}, errors.New("egress: entry is too long")
	}
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return Entry{}, fmt.Errorf("egress: entry %s is not ASCII: use the name's punycode (xn--) form", quoted(s))
		}
	}
	rest := lowerASCII(s)
	var e Entry
	if after, ok := strings.CutPrefix(rest, "lan:"); ok {
		if allDigits(after) {
			return Entry{}, fmt.Errorf("egress: entry %s: a name cannot be a number, and a host called lan is not told apart from lan:", quoted(s))
		}
		e.LAN, rest = true, after
	}
	if after, ok := strings.CutPrefix(rest, "*."); ok {
		e.Wildcard, rest = true, after
	}
	if i := strings.LastIndexByte(rest, ':'); i >= 0 {
		port, err := parsePort(rest[i+1:])
		if err != nil {
			return Entry{}, fmt.Errorf("egress: entry %s: %w", quoted(s), err)
		}
		e.Port, rest = port, rest[:i]
	}
	name, ok := canonName(rest)
	if !ok {
		return Entry{}, fmt.Errorf("egress: entry %s: not a DNS name (letters, digits and hyphens, not an IP address)", quoted(s))
	}
	e.Name = name
	if err := e.validate(); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// parsePort reads a decimal port of 1..65535 in its one spelling.
func parsePort(s string) (uint16, error) {
	if !allDigits(s) || len(s) > 5 || s[0] == '0' {
		return 0, errors.New("bad port")
	}
	n, err := strconv.Atoi(s)
	if err != nil || n > 65535 {
		return 0, errors.New("bad port")
	}
	return uint16(n), nil //nolint:gosec // G115: 1..65535, checked above
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// lowerASCII lower-cases A-Z only. strings.ToLower would turn the Kelvin sign into a k.
func lowerASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			b := []byte(s)
			for ; i < len(b); i++ {
				if b[i] >= 'A' && b[i] <= 'Z' {
					b[i] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// canonName returns a name in the one form all comparisons use: lower-case ASCII without
// the trailing dot. It refuses anything but letters, digits and hyphens in valid labels,
// and a name whose last label is a number, which resolvers take for an IP address
// ("127.1", "0x7f.1") without asking any server.
func canonName(s string) (string, bool) {
	s = strings.TrimSuffix(s, ".")
	if s == "" || len(s) > maxNameLen {
		return "", false
	}
	start := 0
	for i := 0; i <= len(s); i++ {
		if i < len(s) && s[i] != '.' {
			c := s[i]
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
				return "", false
			}
			continue
		}
		label := s[start:i]
		if label == "" || len(label) > maxLabelLen || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		start = i + 1
	}
	last := lowerASCII(s[strings.LastIndexByte(s, '.')+1:])
	if allDigits(last) || (strings.HasPrefix(last, "0x") && isHex(last[2:])) {
		return "", false
	}
	return lowerASCII(s), true
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// DevPreset is the dev mode's list: exact names only, because a wildcard under a zone
// where anyone can get a name would allow that zone's every tenant (ADR 0009). New does
// not add it by itself.
func DevPreset() []Entry {
	names := []string{
		// npm
		"registry.npmjs.org",
		// PyPI
		"pypi.org", "files.pythonhosted.org",
		// Debian
		"deb.debian.org", "security.debian.org",
		// crates
		"crates.io", "static.crates.io", "index.crates.io",
		// Go
		"proxy.golang.org", "sum.golang.org",
		// Node
		"nodejs.org",
		// GitHub
		"github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com",
		"raw.githubusercontent.com", "ghcr.io", "pkg-containers.githubusercontent.com",
		// GitLab
		"gitlab.com",
		// Docker Hub
		"registry-1.docker.io", "auth.docker.io", "production.cloudflare.docker.com",
		// Model providers' API hosts
		"api.anthropic.com", "api.openai.com", "generativelanguage.googleapis.com",
	}
	out := make([]Entry, 0, len(names)+1)
	for _, n := range names {
		out = append(out, Entry{Name: n})
	}
	return append(out, Entry{Name: "github.com", Port: 22})
}
