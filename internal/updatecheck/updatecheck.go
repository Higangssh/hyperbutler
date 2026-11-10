// Package updatecheck is hyperbutler's notify-only update check (REQ-UX-022). At most
// once a day, `hyperbutler start` and `hyperbutler status` ask GitHub's release API
// for the latest release and, when it is newer than the running build, print one line
// with its URL. Nothing is ever downloaded or installed.
//
// The request is one HTTPS GET of ReleaseURL with a fixed User-Agent
// ("hyperbutler/<version>") and no other header, cookie or identifier; it honors the
// proxy settings and trusts the Windows certificate store. A failure of any kind is
// silent (the caller logs it at debug level) and waits for the next day, like a
// success. `hyperbutler config set updateCheck false` turns it off; bench/no-telemetry
// runs with it off, since it is the one request hyperbutler makes of its own
// (REQ-UX-005).
package updatecheck

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ReleaseURL is the GitHub API document of the latest (non-draft, non-prerelease)
// release.
const ReleaseURL = "https://api.github.com/repos/Higangssh/hyperbutler/releases/latest"

// releasePagePrefix is where a release's own page must be: an html_url elsewhere is not
// printed, the tag's page is.
const releasePagePrefix = "https://github.com/Higangssh/hyperbutler/releases/tag/"

const (
	// Interval is the shortest time between two checks.
	Interval = 24 * time.Hour
	// Timeout bounds one check, connection and proxy included.
	Timeout = 3 * time.Second
	// maxBody bounds the release document read (GitHub's are a few KiB).
	maxBody = 1 << 20
	// maxRecord bounds the record file.
	maxRecord = 4 << 10
)

// Record is what the last check found; the caller stores it (a small file in the state
// directory) and `status --json` shows it.
type Record struct {
	// Checked is when the last check was made, whether it worked or not.
	Checked time.Time `json:"checked"`
	// Latest and URL are the newest release and its page, from the last check that
	// got an answer; empty when none did or there is no release.
	Latest string `json:"latest,omitempty"`
	URL    string `json:"url,omitempty"`
}

// ParseRecord reads a stored Record. A record that is not valid is taken as none.
func ParseRecord(b []byte) Record {
	var r Record
	if len(b) > maxRecord || json.Unmarshal(b, &r) != nil {
		return Record{}
	}
	if _, ok := parseSemver(r.Latest); !ok || !validPage(r.URL) {
		r.Latest, r.URL = "", ""
	}
	return r
}

// Encode is r as stored.
func (r Record) Encode() []byte {
	b, _ := json.MarshalIndent(r, "", "  ")
	return append(b, '\n')
}

// Due reports whether a check is due at now: never checked, a day passed, or the
// recorded time is in the future (the clock was set back).
func (r Record) Due(now time.Time) bool {
	return r.Checked.IsZero() || now.Sub(r.Checked) >= Interval || r.Checked.After(now.Add(time.Hour))
}

// Checker makes the check. The zero value is not usable; see New.
type Checker struct {
	// URL is the release document (ReleaseURL; a test server in the tests).
	URL string
	// Version is the running build's version (the module version, "(devel)" for a
	// source build), sent in the User-Agent and compared with the release.
	Version string
	// Client makes the request.
	Client *http.Client
}

// New is a Checker for ReleaseURL whose requests go through proxy (nil: none) and
// trust roots (nil: the system's, which on Windows is the certificate store).
func New(version string, proxy func(*http.Request) (*url.URL, error), roots *x509.CertPool) *Checker {
	return &Checker{URL: ReleaseURL, Version: version, Client: NewClient(proxy, roots)}
}

// NewClient is the HTTP client of the check: no cookie jar, no compression (so no
// Accept-Encoding header), TLS 1.2 or later, redirects only to another https URL on the
// same host, and Timeout for the whole request.
func NewClient(proxy func(*http.Request) (*url.URL, error), roots *x509.CertPool) *http.Client {
	if roots == nil {
		if p, err := x509.SystemCertPool(); err == nil {
			roots = p
		}
	}
	tr := &http.Transport{
		Proxy:                 proxy,
		TLSClientConfig:       &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		DisableCompression:    true,
		TLSHandshakeTimeout:   Timeout,
		ResponseHeaderTimeout: Timeout,
		MaxIdleConns:          1,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || req.URL.Scheme != "https" || req.URL.Host != via[0].URL.Host {
				return errors.New("updatecheck: redirect refused")
			}
			return nil
		},
	}
}

// UserAgent is the request's only header: "hyperbutler/" and the version, with
// anything but letters, digits, '.', '-' and '+' left out ("(devel)" becomes "devel").
func UserAgent(version string) string {
	v := strings.Map(func(r rune) rune {
		if r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '+') {
			return r
		}
		return -1
	}, version)
	if v == "" {
		v = "unknown"
	}
	return "hyperbutler/" + v[:min(len(v), 64)]
}

// ErrNoRelease is a check that found no release (GitHub answers 404 for a repository
// with none, or one it does not show).
var ErrNoRelease = errors.New("updatecheck: no release")

// Check asks for the latest release. A repository without releases is ErrNoRelease.
func (c *Checker) Check(ctx context.Context) (latest, page string, err error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return "", "", err
	}
	// Only this header: Go adds Host (and nothing else, with compression off).
	req.Header = http.Header{"User-Agent": {UserAgent(c.Version)}}
	resp, err := c.Client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if t, ok := c.Client.Transport.(*http.Transport); ok {
		defer t.CloseIdleConnections()
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", "", ErrNoRelease
	default:
		return "", "", fmt.Errorf("updatecheck: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return "", "", err
	}
	return ParseRelease(body)
}

// ParseRelease reads the tag and page of a GitHub release document. The page is the
// document's html_url when it is this repository's page for that tag, else the tag's
// page.
func ParseRelease(body []byte) (latest, page string, err error) {
	if len(body) > maxBody {
		return "", "", errors.New("updatecheck: release document too large")
	}
	var doc struct {
		Tag        string `json:"tag_name"`
		Page       string `json:"html_url"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	d := json.NewDecoder(bytes.NewReader(body))
	if err := d.Decode(&doc); err != nil {
		return "", "", fmt.Errorf("updatecheck: release document: %w", err)
	}
	if doc.Draft || doc.Prerelease {
		return "", "", ErrNoRelease
	}
	if _, ok := parseSemver(doc.Tag); !ok {
		return "", "", fmt.Errorf("updatecheck: release tag %q is not a version", printable(doc.Tag))
	}
	page = releasePagePrefix + doc.Tag
	if validPage(doc.Page) && doc.Page == page {
		page = doc.Page
	}
	return doc.Tag, page, nil
}

// validPage reports whether u is empty or a release page of this repository.
func validPage(u string) bool {
	if u == "" {
		return true
	}
	tag, ok := strings.CutPrefix(u, releasePagePrefix)
	if !ok {
		return false
	}
	_, ok = parseSemver(tag)
	return ok
}

// Run makes the check if r says one is due at now and returns the record to store
// (changed is false when no check was made). A failed check records the time and keeps
// the last answer, so it is not retried before the next day either; its error is
// returned for the caller's debug log.
func (c *Checker) Run(ctx context.Context, r Record, now time.Time) (next Record, changed bool, err error) {
	if !r.Due(now) {
		return r, false, nil
	}
	next = Record{Checked: now.UTC().Truncate(time.Second), Latest: r.Latest, URL: r.URL}
	latest, page, err := c.Check(ctx)
	switch {
	case errors.Is(err, ErrNoRelease):
		next.Latest, next.URL = "", ""
	case err == nil:
		next.Latest, next.URL = latest, page
	}
	return next, true, err
}

// Notice is the line to print for r when the running version is version: empty for a
// development build, when nothing is known, or when the running build is as new.
func Notice(r Record, version string) string {
	if IsDevBuild(version) || r.Latest == "" || r.URL == "" || !Newer(r.Latest, version) {
		return ""
	}
	return fmt.Sprintf("hyperbutler %s is available (this is %s): %s", r.Latest, version, r.URL)
}

// pseudoVersion matches the timestamp and revision of a Go pseudo-version
// (v0.0.0-20261101120000-0123456789ab), which `go build` stamps on an untagged commit.
var pseudoVersion = regexp.MustCompile(`\d{14}-[0-9a-f]{12}`)

// IsDevBuild reports whether version is a development build, which never nags: a
// source build without a module version ("(devel)", ""), a pseudo-version, or a
// build with local changes ("+dirty").
func IsDevBuild(version string) bool {
	if version == "" || version == "(devel)" || strings.Contains(version, "+dirty") || pseudoVersion.MatchString(version) {
		return true
	}
	_, ok := parseSemver(version)
	return !ok
}

// Newer reports whether release a is newer than version b (semantic versioning; a
// "v" prefix is optional, build metadata is ignored). Anything unparsable is not newer.
func Newer(a, b string) bool {
	va, ok1 := parseSemver(a)
	vb, ok2 := parseSemver(b)
	return ok1 && ok2 && compare(va, vb) > 0
}

type semver struct {
	num [3]uint64
	pre []string
}

// parseSemver parses "v1.2.3", "1.2.3-rc.1" and "v1.2.3+meta" (at most 64 bytes).
func parseSemver(s string) (semver, bool) {
	var v semver
	if len(s) > 64 || !utf8.ValidString(s) {
		return v, false
	}
	s = strings.TrimPrefix(s, "v")
	s, meta, hasMeta := strings.Cut(s, "+")
	if hasMeta {
		// Build metadata does not order versions, but it ends up in the release page
		// URL, so it is held to the SemVer grammar: dot-separated [0-9A-Za-z-]+.
		for _, id := range strings.Split(meta, ".") {
			if id == "" || strings.Trim(id, semverIDChars) != "" {
				return v, false
			}
		}
	}
	core, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		if !numeric(p) || len(p) > 1 && p[0] == '0' {
			return v, false
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return v, false
		}
		v.num[i] = n
	}
	if hasPre {
		for _, id := range strings.Split(pre, ".") {
			if id == "" || strings.Trim(id, semverIDChars) != "" ||
				numeric(id) && (len(id) > 1 && id[0] == '0' || len(id) > 19) {
				return v, false
			}
			v.pre = append(v.pre, id)
		}
	}
	return v, true
}

// semverIDChars are the characters of a SemVer pre-release or build identifier.
const semverIDChars = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-"

func numeric(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }

// compare orders a and b by semantic versioning precedence.
func compare(a, b semver) int {
	for i := range a.num {
		if a.num[i] != b.num[i] {
			if a.num[i] < b.num[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1 // a release is newer than its prereleases
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		x, y := a.pre[i], b.pre[i]
		if x == y {
			continue
		}
		nx, ny := numeric(x), numeric(y)
		switch {
		case nx && ny:
			ix, _ := strconv.ParseUint(x, 10, 64)
			iy, _ := strconv.ParseUint(y, 10, 64)
			if ix < iy {
				return -1
			}
			return 1
		case nx:
			return -1
		case ny:
			return 1
		case x < y:
			return -1
		default:
			return 1
		}
	}
	switch {
	case len(a.pre) < len(b.pre):
		return -1
	case len(a.pre) > len(b.pre):
		return 1
	}
	return 0
}

// printable is s with anything but printable ASCII replaced, for an error message.
func printable(s string) string {
	s = s[:min(len(s), 64)]
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '?'
		}
		return r
	}, s)
}
