// Package redact removes secrets from text and JSON before it leaves hyperbutler in a
// `hyperbutler diagnose` bundle (REQ-UX-008, ADR 0008: the MCP server is to redact the
// same way).
//
// What counts as a secret:
//
//   - the value of a key that names one (IsSecretKey: password, token, secret, auth,
//     Authorization, ...), in JSON (also JSON escaped inside a string, and a member split
//     across lines), as key=value, key = 'value' or "key: value" (also with a space
//     before the colon or none after it), and as a "--password VALUE" flag;
//   - docker login's -p and -u values, on lines with the word login only (elsewhere -p
//     is a port);
//   - the user information of a URL (https://user:pass@host, and user:pass@host without a
//     scheme);
//   - HTTP Authorization, Proxy-Authorization, Cookie and X-Registry-Auth header values;
//   - Bearer tokens, JWTs, PEM private keys, and tokens with a well-known prefix (GitHub,
//     GitLab, Docker Hub, AWS access key ids, Slack).
//
// Paths and user names in paths are not secrets and stay. Redaction errs on the side of
// removing too much: a word after "token:" in an error message is removed too.
package redact

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// Mark replaces every redacted value.
const Mark = "[REDACTED]"

// secretWords are substrings of a normalized key (IsSecretKey) that make its value a
// secret.
var secretWords = []string{
	"password", "passwd", "passphrase", "secret", "token", "apikey", "credential",
	"privatekey", "accesskey", "sessionkey", "signature", "cookie",
}

// secretKeys are normalized keys whose value is a secret although they contain none of
// secretWords.
var secretKeys = map[string]bool{
	"auth": true, "authorization": true, "pass": true, "pwd": true, "pw": true,
	"xregistryauth": true, "xregistryconfig": true, "registryauth": true,
}

// IsSecretKey reports whether a key (of JSON, a header, key=value or a flag) names a
// secret. Case, '-', '_', '.' and spaces are ignored: "Proxy-Authorization",
// "identitytoken" and "HTTP_PROXY_PASSWORD" are secret keys; "auths" (the docker
// config's map of registries) and "credsStore" are not.
func IsSecretKey(key string) bool {
	n := normalize(key)
	if secretKeys[n] || strings.HasSuffix(n, "auth") || strings.HasSuffix(n, "authorization") {
		return true
	}
	for _, w := range secretWords {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

func normalize(key string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(key) {
		switch r {
		case '-', '_', '.', ' ', '\\', '"', '\'':
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// A rule finds byte ranges to redact in s. Every rule runs on the original text and the
// ranges are merged afterwards, so one rule cannot hide a secret from another by
// rewriting its context.
type rule func(s string, add func(lo, hi int))

var (
	rePEM = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----(?:.*?-----END [A-Z0-9 ]*PRIVATE KEY-----|.*$)`)
	// scheme://userinfo@: the user information is everything up to the last '@' before
	// the path, a space or a quote (a password may hold an unescaped '@').
	reURLUser = regexp.MustCompile(`(?i)[a-z][a-z0-9+.\-]*://([^\s/?#"'<>\\]*)@`)
	// user:pass@host without a scheme (a proxy setting), after a separator.
	reBareUser = regexp.MustCompile(`(?:^|[\s"'=,;(\[{<])([^\s"'=,;:/@<>\\(\[{]+:[^\s"'<>\\/]*)@`)
	// Header: value, to the end of the line or a quote.
	reHeader = regexp.MustCompile(`(?i)(?:proxy-)?authorization|x-registry-auth|x-registry-config|set-cookie|cookie|x-api-key`)
	reHdrVal = regexp.MustCompile(`^(?:["']?[ \t]*[:=][ \t]*["']?)([^"'\r\n]*)`)
	// '=' anywhere: a token is not always base64 with padding at the end only.
	reBearer = regexp.MustCompile(`(?i)\bbearer[ \t]+([a-z0-9._~+/=\-]+)`)
	// A JSON member: "key": "value" or "key": scalar.
	reJSONKV = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"\s*:\s*("(?:[^"\\]|\\.)*"|[^\s,}\]"]+)`)
	// The same, JSON-escaped inside a string (a log line that quotes a JSON document).
	reEscKV = regexp.MustCompile(`\\+"([^"\\]{1,64})\\+"\s*:\s*(\\+"[^"]*?\\+"|[^\s,}\]"\\]+)`)
	// key=value, key = value, key="value" and key='value' (slog, URL queries, the
	// environment, shell and command lines: export DOCKER_PASSWORD='x').
	reEqKV = regexp.MustCompile(`([A-Za-z0-9_.\-]{1,64})[ \t]*=[ \t]*("(?:[^"\\]|\\.)*"|'[^']*'|[^\s&"',;]+)`)
	// key: value, key:value and key : value, as in YAML or an error message.
	reColonKV = regexp.MustCompile(`([A-Za-z0-9_.\-]{1,64})[ \t]*:[ \t]*("(?:[^"\\]|\\.)*"|'[^']*'|[^\s"',;]+)`)
	// --password VALUE. The value may start with '-' ("--password -x"): only a secret
	// flag's value is redacted, so taking the next flag for one costs nothing.
	reFlag = regexp.MustCompile(`(?i)(--?[a-z0-9\-]{1,64})[ \t]+("(?:[^"\\]|\\.)*"|'[^']*'|[^\s"']+)`)
	// docker login's -p and -u (and -p=x, -pVALUE). Short flags mean different things to
	// different commands (-p 8080:80 publishes a port), so this rule runs only on lines
	// with the word login.
	reLogin     = regexp.MustCompile(`(?i)\blogin\b`)
	reLoginFlag = regexp.MustCompile(`(?:^|[\s"'])-[pu](?:[ \t]*=?[ \t]*)("(?:[^"\\]|\\.)*"|'[^']*'|[^\s"']+)`)
	// A JSON member split across lines (text that is not one JSON document: a log, a
	// truncated file): a secret key at the end of a line...
	reTrailKey = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"[ \t]*(:?)[ \t]*\r?\n?$`)
	// ...and its value at the start of a following one.
	reLeadVal = regexp.MustCompile(`^(?:"(?:[^"\\]|\\.)*"|[^\s,}\]"]+)`)
	reJWT     = regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]*`)
	// Tokens with a published prefix.
	reToken = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_\-]{20,}|dckr_pat_[A-Za-z0-9_\-]{20,}|AKIA[0-9A-Z]{16}|xox[abposr]-[A-Za-z0-9\-]{10,})`)
)

// keyNeedles are in the normalized form (normalize) of every line that holds a secret
// key or header name: every IsSecretKey key contains one.
var keyNeedles = append([]string{"auth", "pass", "pw", "registryconfig"}, secretWords...)

// lineRules run on each line. A rule runs only on lines its trigger accepts (raw is
// the line, norm its normalized form), so most log lines cost a few substring searches.
var lineRules = []struct {
	trigger func(raw, norm string) bool
	run     rule
}{
	{func(raw, _ string) bool { return strings.Contains(raw, "eyJ") }, whole(reJWT)},
	{func(raw, _ string) bool {
		return strings.Contains(raw, "_") || strings.Contains(raw, "-") || strings.Contains(raw, "AKIA")
	}, whole(reToken)},
	{func(raw, _ string) bool { return strings.Contains(raw, "@") }, group(reURLUser, 1)},
	{func(raw, _ string) bool { return strings.Contains(raw, "@") }, group(reBareUser, 1)},
	{func(_, norm string) bool { return strings.Contains(norm, "bearer") }, group(reBearer, 1)},
	{hasKeyNeedle, func(s string, add func(lo, hi int)) {
		for _, m := range reHeader.FindAllStringIndex(s, -1) {
			if v := reHdrVal.FindStringSubmatchIndex(s[m[1]:]); v != nil && v[3] > v[2] {
				add(m[1]+v[2], m[1]+v[3])
			}
		}
	}},
	{hasKeyNeedle, keyed(reJSONKV)},
	{hasKeyNeedle, keyed(reEscKV)},
	{hasKeyNeedle, keyed(reEqKV)},
	{hasKeyNeedle, keyed(reColonKV)},
	{hasKeyNeedle, keyed(reFlag)},
	{func(raw, norm string) bool { return strings.Contains(norm, "login") && reLogin.MatchString(raw) }, quoted(reLoginFlag, 1)},
}

func hasKeyNeedle(_, norm string) bool {
	for _, n := range keyNeedles {
		if strings.Contains(norm, n) {
			return true
		}
	}
	return false
}

// whole redacts every match of re.
func whole(re *regexp.Regexp) rule {
	return func(s string, add func(lo, hi int)) {
		for _, m := range re.FindAllStringIndex(s, -1) {
			add(m[0], m[1])
		}
	}
}

// overlapping calls fn with the submatch indexes of every match of re in s, including
// matches that start inside an earlier match's submatch g: in `"": "auth":"x"` the
// member `"": "auth"` must not hide `"auth":"x"`.
func overlapping(re *regexp.Regexp, s string, g int, fn func(m []int)) {
	for pos := 0; pos < len(s); {
		m := re.FindStringSubmatchIndex(s[pos:])
		if m == nil {
			return
		}
		for i := range m {
			if m[i] >= 0 {
				m[i] += pos
			}
		}
		fn(m)
		next := m[1]
		if m[2*g] >= 0 {
			next = m[2*g]
		}
		pos = max(next, m[0]+1)
	}
}

// group redacts submatch g of every match of re.
func group(re *regexp.Regexp, g int) rule {
	return func(s string, add func(lo, hi int)) {
		overlapping(re, s, g, func(m []int) {
			if m[2*g] >= 0 && m[2*g+1] > m[2*g] {
				add(m[2*g], m[2*g+1])
			}
		})
	}
}

// keyed redacts submatch 2 (the value) of every match of re whose submatch 1 is a
// secret key. A quoted value keeps its quotes.
func keyed(re *regexp.Regexp) rule {
	return func(s string, add func(lo, hi int)) {
		overlapping(re, s, 2, func(m []int) {
			if IsSecretKey(s[m[2]:m[3]]) {
				addValue(s, m[4], m[5], add)
			}
		})
	}
}

// quoted redacts submatch g of every match of re, keeping its quotes.
func quoted(re *regexp.Regexp, g int) rule {
	return func(s string, add func(lo, hi int)) {
		overlapping(re, s, g, func(m []int) {
			if m[2*g] >= 0 {
				addValue(s, m[2*g], m[2*g+1], add)
			}
		})
	}
}

// addValue redacts s[lo:hi] without its quotes (and the backslashes of escaped ones).
func addValue(s string, lo, hi int, add func(lo, hi int)) {
	for lo < hi && (s[lo] == '"' || s[lo] == '\'' || s[lo] == '\\') {
		lo++
	}
	for hi > lo && (s[hi-1] == '"' || s[hi-1] == '\'' || s[hi-1] == '\\') {
		hi--
	}
	if hi > lo && s[lo:hi] != Mark {
		add(lo, hi)
	}
}

// String returns s with every secret replaced by Mark.
func String(s string) string {
	type span struct{ lo, hi int }
	var spans []span
	off := 0
	add := func(lo, hi int) { spans = append(spans, span{off + lo, off + hi}) }
	whole(rePEM)(s, add)
	// Line by line: every other rule stays within a line, and short inputs keep the
	// regexp engine on its fast path. split carries a JSON member split across lines: a
	// secret key ended a line (needColon: without its ':'), and its value starts a later
	// non-blank one.
	split, needColon := false, false
	for rest := s; rest != ""; {
		line := rest
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			line = rest[:i+1]
		}
		norm := normalize(line)
		if split && strings.TrimSpace(line) != "" {
			split = false
			v := strings.TrimLeft(line, " \t\r")
			ok := !needColon
			if needColon && v[0] == ':' {
				ok, v = true, strings.TrimLeft(v[1:], " \t\r")
				if strings.TrimSpace(v) == "" {
					split, needColon = true, false // "key"\n:\n"value"
				}
			}
			if ok && !split {
				if m := reLeadVal.FindStringIndex(v); m != nil {
					i := len(line) - len(v)
					addValue(line, i+m[0], i+m[1], add)
				}
			}
		}
		for _, r := range lineRules {
			if r.trigger(line, norm) {
				r.run(line, add)
			}
		}
		if t := strings.TrimRight(line, " \t\r\n"); t != "" && (t[len(t)-1] == '"' || t[len(t)-1] == ':') && hasKeyNeedle(line, norm) {
			if m := reTrailKey.FindStringSubmatchIndex(line); m != nil && IsSecretKey(line[m[2]:m[3]]) {
				split, needColon = true, m[5] == m[4]
			}
		}
		off += len(line)
		rest = rest[len(line):]
	}
	if len(spans) == 0 {
		return s
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].lo < spans[j].lo })
	var b strings.Builder
	b.Grow(len(s))
	pos := 0
	for i := 0; i < len(spans); {
		lo, hi := spans[i].lo, spans[i].hi
		for i++; i < len(spans) && spans[i].lo <= hi; i++ {
			hi = max(hi, spans[i].hi)
		}
		if lo < pos {
			lo = pos
		}
		b.WriteString(s[pos:lo])
		b.WriteString(Mark)
		pos = hi
	}
	b.WriteString(s[pos:])
	return b.String()
}

// Text is String for bytes.
func Text(b []byte) []byte { return []byte(String(string(b))) }

// JSON redacts a JSON document structurally: the value of every secret key (IsSecretKey)
// becomes Mark, whatever its type (except null and booleans, which carry no secret), and
// every other string and object key goes through String. The result is indented, with
// object keys sorted. Input that is not one JSON value is redacted as text.
func JSON(b []byte) []byte {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil || d.More() {
		return Text(b)
	}
	if _, err := d.Token(); err == nil {
		return Text(b) // trailing data
	}
	var out bytes.Buffer
	e := json.NewEncoder(&out)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	if err := e.Encode(walk(v)); err != nil {
		return Text(b)
	}
	return out.Bytes()
}

func walk(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			nk := String(k)
			switch {
			case IsSecretKey(k) && e != nil:
				if _, isBool := e.(bool); isBool {
					m[nk] = e
				} else {
					m[nk] = Mark
				}
			default:
				m[nk] = walk(e)
			}
		}
		return m
	case []any:
		for i, e := range x {
			x[i] = walk(e)
		}
		return x
	case string:
		return String(x)
	}
	return v
}
