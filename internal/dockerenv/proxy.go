// Package dockerenv is the environment hyperbutler gives the guest's dockerd
// (REQ-UX-009): the proxy settings (HTTP_PROXY, HTTPS_PROXY, NO_PROXY) and the CA
// certificates imported from the Windows certificate store, which dockerd trusts in
// addition to the guest's stock bundle.
//
// The host builds a Doc from settings.json and the certificate store; the guest
// (guest/cmd/hyperbutlerd) decodes it strictly, writes dockerd's environment and the
// combined CA bundle to /run, and restarts dockerd when they change. Both ends use this
// package, so the checks are the same on either side.
//
// Proxy URLs can carry credentials. Redact is the only form that may be printed or
// logged; error messages never quote a proxy URL.
package dockerenv

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// MaxProxyURL bounds a proxy URL; MaxNoProxy bounds the user's NO_PROXY list
// (proxy.noProxy). MaxDocNoProxy bounds the list dockerd gets, which is the user's with
// the entries the host adds (localhost, the guest network, daemon.json's registries): it
// leaves room for them, so the user's list always fits.
const (
	MaxProxyURL   = 2048
	MaxNoProxy    = 4096
	MaxDocNoProxy = MaxNoProxy + 1024
)

// ErrProxy is wrapped by every proxy validation error.
var ErrProxy = errors.New("invalid proxy")

// ParseProxyURL checks a proxy URL: http, https or socks5 (what Go's
// http.ProxyFromEnvironment, and so dockerd, supports), a host, an optional port, and
// nothing else (no path, query or fragment). Credentials are allowed. The error never
// contains the URL.
func ParseProxyURL(v string) (*url.URL, error) {
	if v == "" || len(v) > MaxProxyURL {
		return nil, fmt.Errorf("%w: want a URL such as http://proxy.example.com:3128, at most %d bytes", ErrProxy, MaxProxyURL)
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c <= ' ' || c >= 0x7f {
			return nil, fmt.Errorf("%w: the URL contains a space, a control character or a non-ASCII byte (percent-encode it)", ErrProxy)
		}
	}
	u, err := url.Parse(v)
	if err != nil {
		// url.Error quotes the input, which may hold a password: say only what is wrong.
		return nil, fmt.Errorf("%w: the URL does not parse", ErrProxy)
	}
	switch u.Scheme {
	case "http", "https", "socks5":
	default:
		return nil, fmt.Errorf("%w: the scheme must be http, https or socks5", ErrProxy)
	}
	if u.Opaque != "" || u.Host == "" {
		return nil, fmt.Errorf("%w: want scheme://host[:port]", ErrProxy)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || u.RawFragment != "" {
		return nil, fmt.Errorf("%w: a proxy URL has no path, query or fragment", ErrProxy)
	}
	host := u.Hostname()
	if host == "" || strings.ContainsAny(host, "%") {
		return nil, fmt.Errorf("%w: the URL has no usable host name", ErrProxy)
	}
	if !validHost(host) {
		return nil, fmt.Errorf("%w: the host is not a host name or an IP address", ErrProxy)
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 || p[0] == '0' {
			return nil, fmt.Errorf("%w: the port must be 1 to 65535", ErrProxy)
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return nil, fmt.Errorf("%w: empty port", ErrProxy)
	}
	if u.User != nil && u.User.Username() == "" {
		return nil, fmt.Errorf("%w: empty user name", ErrProxy)
	}
	return u, nil
}

// validHost accepts an IP address or a DNS name of letters, digits, '-', '_' and dots.
func validHost(h string) bool {
	if _, err := netip.ParseAddr(h); err == nil {
		return !strings.Contains(h, "%") // no zone
	}
	if len(h) > 253 {
		return false
	}
	for _, l := range strings.Split(h, ".") {
		if l == "" || len(l) > 63 {
			return false
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
				return false
			}
		}
	}
	return true
}

// ValidateProxyURL is ParseProxyURL without the result.
func ValidateProxyURL(v string) error {
	_, err := ParseProxyURL(v)
	return err
}

// Redact returns v with its credentials replaced: "http://user:***@host:3128" when it
// has a password, "http://***@host:3128" for a user name alone (often a token). A
// value that does not parse is replaced entirely. Empty stays empty.
func Redact(v string) string {
	if v == "" {
		return ""
	}
	u, err := ParseProxyURL(v)
	if err != nil {
		return "(invalid proxy URL, not shown)"
	}
	return redactURL(u)
}

func redactURL(u *url.URL) string {
	var b strings.Builder
	b.WriteString(u.Scheme)
	b.WriteString("://")
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			b.WriteString(url.User(u.User.Username()).String())
			b.WriteString(":***@")
		} else {
			b.WriteString("***@")
		}
	}
	b.WriteString(u.Host)
	b.WriteString(u.Path)
	if b.Len() > MaxProxyURL && u.User != nil {
		// Escaping can triple a user name past the limit; then the mask stands for
		// all of the user info, so the redacted form stays a valid proxy URL.
		u2 := *u
		u2.User = nil
		return u.Scheme + "://***@" + strings.TrimPrefix(redactURL(&u2), u.Scheme+"://")
	}
	return b.String()
}

// HasCredentials reports whether v carries a user name or password.
func HasCredentials(v string) bool {
	u, err := ParseProxyURL(v)
	return err == nil && u.User != nil
}

// ValidateNoProxy checks a NO_PROXY list: comma-separated host names, domains
// (".example.com" or "example.com"), "*", IP addresses or CIDR prefixes, each with an
// optional port. Spaces around entries are allowed.
func ValidateNoProxy(v string) error { return validateNoProxy(v, MaxNoProxy) }

// validateNoProxy is ValidateNoProxy with another length limit.
func validateNoProxy(v string, limit int) error {
	if len(v) > limit {
		return fmt.Errorf("%w: the no-proxy list is longer than %d bytes", ErrProxy, limit)
	}
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("%w: empty no-proxy list (unset the key instead)", ErrProxy)
	}
	for _, e := range strings.Split(v, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			return fmt.Errorf("%w: empty entry in the no-proxy list", ErrProxy)
		}
		if !validNoProxyEntry(e) {
			return fmt.Errorf("%w: no-proxy entry %q is not a host, domain, IP address or CIDR prefix", ErrProxy, e)
		}
	}
	return nil
}

func validNoProxyEntry(e string) bool {
	if e == "*" {
		return true
	}
	if _, err := netip.ParsePrefix(e); err == nil {
		return true
	}
	host := e
	if h, p, err := net.SplitHostPort(e); err == nil {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
		host = h
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return a.Zone() == ""
	}
	host = strings.TrimPrefix(strings.TrimPrefix(host, "*"), ".")
	return host != "" && validHost(host)
}

// NormalizeNoProxy is the list as stored: entries trimmed, joined with ",".
func NormalizeNoProxy(v string) string {
	parts := strings.Split(v, ",")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return strings.Join(parts, ",")
}

// LoopbackHost reports whether h names this host's loopback, and whether the host
// gateway reaches it there. The gateway connects to 127.0.0.1, and to [::1] only when
// 127.0.0.1 refuses, so it reaches localhost and 127.0.0.1; a proxy given as [::1] or
// another 127.x address would get whatever listens on 127.0.0.1 with that port, and the
// proxy's credentials with it.
func LoopbackHost(h string) (loopback, viaGateway bool) {
	if strings.EqualFold(h, "localhost") {
		return true, true
	}
	a, err := netip.ParseAddr(h)
	if err != nil || !a.Unmap().IsLoopback() {
		return false, false
	}
	return true, a.Unmap() == netip.AddrFrom4([4]byte{127, 0, 0, 1})
}

// RewriteLoopback replaces localhost or 127.0.0.1 in a proxy URL with gw, the guest's
// address for this host's loopback (the host gateway): a proxy that runs on Windows,
// such as a local NTLM relay, listens on 127.0.0.1, which in the guest is the guest
// itself. Other URLs, including other loopback addresses (LoopbackHost), are returned
// unchanged. ok reports a rewrite.
func RewriteLoopback(v string, gw netip.Addr) (out string, ok bool) {
	u, err := ParseProxyURL(v)
	if err != nil || !gw.IsValid() {
		return v, false
	}
	if _, via := LoopbackHost(u.Hostname()); !via {
		return v, false
	}
	if p := u.Port(); p != "" {
		u.Host = net.JoinHostPort(gw.String(), p)
	} else {
		u.Host = gw.String()
	}
	return u.String(), true
}
