// Package daemoncfg checks a user's daemon.json before hyperbutler hands it to the
// guest's dockerd (REQ-UX-007).
//
// The file is passed through byte for byte: hyperbutler never rewrites it. Check refuses
// what cannot be delivered safely and the keys hyperbutler owns, because the guest starts
// dockerd with them as flags or relies on their defaults (see Owned). dockerd checks the
// rest itself (`dockerd --validate`) in the guest before the file is used.
package daemoncfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxSize bounds a daemon.json. Real ones are a few hundred bytes.
const MaxSize = 64 << 10

// OwnedKey is a top-level daemon.json key hyperbutler sets or depends on.
type OwnedKey struct {
	Key string
	Why string
}

// Owned lists the keys a user daemon.json may not set, with the reason shown to the user.
// The guest init (guest/prototype/init) starts dockerd as
//
//	dockerd --host=unix:///run/docker.sock --iptables=false --ip6tables=false
//	        --shutdown-timeout=10 --log-level=LEVEL [--config-file=...]
//
// and dockerd refuses to start when a flag is also set in the file.
var Owned = []OwnedKey{
	{"hosts", "dockerd listens on /run/docker.sock, which the guest agent relays to the Windows pipe; change the pipe with `hyperbutler config set pipe`"},
	{"iptables", "the guest network is routed through hyperbutler's user-mode stack (ADR 0006); dockerd runs with --iptables=false"},
	{"ip6tables", "the guest network is routed through hyperbutler's user-mode stack (ADR 0006); dockerd runs with --ip6tables=false"},
	{"userland-proxy", "with iptables off, published ports (-p) work only through docker-proxy"},
	{"userland-proxy-path", "with iptables off, published ports (-p) work only through the guest's docker-proxy"},
	{"data-root", "/var/lib/docker is the data disk (hyperbutler config set data.dir moves it on the host)"},
	{"exec-root", "the guest's runtime state lives on its /run tmpfs; the system disk is read-only"},
	{"pidfile", "the guest's runtime state lives on its /run tmpfs; the system disk is read-only"},
	{"containerd", "dockerd starts and manages its own containerd in the guest"},
	{"tls", "the API is reached through the Windows pipe, whose access is limited to you; TLS on the guest socket would break the relay"},
	{"tlsverify", "the API is reached through the Windows pipe, whose access is limited to you; TLS on the guest socket would break the relay"},
	{"tlscacert", "the API is reached through the Windows pipe, whose access is limited to you; TLS on the guest socket would break the relay"},
	{"tlscert", "the API is reached through the Windows pipe, whose access is limited to you; TLS on the guest socket would break the relay"},
	{"tlskey", "the API is reached through the Windows pipe, whose access is limited to you; TLS on the guest socket would break the relay"},
	{"shutdown-timeout", "hyperbutler bounds the guest's orderly shutdown and passes --shutdown-timeout=10"},
	{"log-level", `hyperbutler passes --log-level; use "debug": true for debug logs`},
	{"live-restore", "the guest's orderly shutdown stops dockerd and then unmounts the data disk; containers left running would keep it busy and power the VM off with a dirty file system"},
}

// Error is a daemon.json that hyperbutler will not deliver.
type Error struct {
	// Key is the offending top-level key, if any.
	Key string
	Msg string
}

func (e *Error) Error() string { return e.Msg }

// ErrEmpty is a file with nothing but white space in it.
var ErrEmpty = errors.New("daemon.json is empty; write {} or delete the file")

// Options are the host facts Check needs.
type Options struct {
	// GuestSubnet is the guest's own network (netstack.DefaultSubnet by default). A
	// container network that overlaps it would cut the guest off.
	GuestSubnet netip.Prefix
}

// Check reports why b cannot be used as the guest's daemon.json, or nil. It never
// changes b.
func Check(b []byte, opts Options) error {
	if len(b) > MaxSize {
		return &Error{Msg: fmt.Sprintf("daemon.json is %d bytes; the limit is %d", len(b), MaxSize)}
	}
	if bytes.HasPrefix(b, []byte("\xef\xbb\xbf")) {
		return &Error{Msg: "daemon.json starts with a UTF-8 byte order mark, which dockerd does not accept; save it as UTF-8 without BOM"}
	}
	if !utf8.Valid(b) {
		return &Error{Msg: "daemon.json is not valid UTF-8"}
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return ErrEmpty
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	tok, err := d.Token()
	if err != nil {
		return syntaxErr(b, d, err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return &Error{Msg: "daemon.json must be one JSON object ({ ... })"}
	}
	seen := map[string]string{} // folded -> as written
	values := map[string]json.RawMessage{}
	for d.More() {
		tok, err := d.Token()
		if err != nil {
			return syntaxErr(b, d, err)
		}
		key, ok := tok.(string)
		if !ok {
			return &Error{Msg: "daemon.json: object keys must be strings"}
		}
		var v json.RawMessage
		if err := d.Decode(&v); err != nil {
			return syntaxErr(b, d, err)
		}
		// dockerd decodes into a Go struct, which matches keys without regard to case
		// (and folds K/k and 탓/s): "Hosts" and "HOSTS" set "hosts". Compare the same way.
		f := fold(key)
		if prev, dup := seen[f]; dup {
			if prev == key {
				return &Error{Key: key, Msg: fmt.Sprintf("daemon.json sets %q twice", key)}
			}
			return &Error{Key: key, Msg: fmt.Sprintf("daemon.json sets %q and %q, which dockerd reads as the same key", prev, key)}
		}
		seen[f] = key
		for _, o := range Owned {
			if strings.EqualFold(key, o.Key) {
				return &Error{Key: key, Msg: fmt.Sprintf("daemon.json may not set %q: hyperbutler owns it (%s)", key, o.Why)}
			}
		}
		values[f] = v
	}
	if _, err := d.Token(); err != nil { // the closing brace
		return syntaxErr(b, d, err)
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return &Error{Msg: "daemon.json has data after its JSON object"}
	}
	return checkSubnets(values, opts)
}

// fold maps a key to the form under which dockerd's decoder would match it.
func fold(s string) string {
	var sb strings.Builder
	for _, r := range s {
		sb.WriteRune(foldRune(r))
	}
	return sb.String()
}

// foldRune is the smallest rune of r's simple case-folding orbit, so two runes fold
// alike exactly when strings.EqualFold says they are equal.
func foldRune(r rune) rune {
	least := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if f < least {
			least = f
		}
	}
	return least
}

func syntaxErr(b []byte, d *json.Decoder, err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return &Error{Msg: "daemon.json ends in the middle of its JSON object"}
	}
	off := d.InputOffset()
	var se *json.SyntaxError
	if errors.As(err, &se) {
		off = se.Offset
	}
	line, col := position(b, off)
	return &Error{Msg: fmt.Sprintf("daemon.json is not valid JSON at line %d, column %d: %v", line, col, err)}
}

func position(b []byte, off int64) (line, col int) {
	if off > int64(len(b)) {
		off = int64(len(b))
	}
	line, col = 1, 1
	for _, c := range b[:off] {
		if c == '\n' {
			line, col = line+1, 1
		} else {
			col++
		}
	}
	return line, col
}

// checkSubnets refuses container networks that overlap the guest's own subnet. Values
// of the wrong type are left to dockerd's own check.
func checkSubnets(values map[string]json.RawMessage, opts Options) error {
	if !opts.GuestSubnet.IsValid() {
		return nil
	}
	type cand struct{ key, val string }
	var cands []cand
	for _, k := range []string{"bip", "fixed-cidr"} {
		var s string
		if v, ok := values[fold(k)]; ok && json.Unmarshal(v, &s) == nil {
			cands = append(cands, cand{k, s})
		}
	}
	if v, ok := values[fold("default-address-pools")]; ok {
		var pools []map[string]json.RawMessage
		if json.Unmarshal(v, &pools) == nil {
			for _, p := range pools {
				for k, raw := range p {
					var s string
					if strings.EqualFold(k, "base") && json.Unmarshal(raw, &s) == nil {
						cands = append(cands, cand{"default-address-pools", s})
					}
				}
			}
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].key < cands[j].key })
	for _, c := range cands {
		p, err := netip.ParsePrefix(c.val)
		if err != nil {
			continue // dockerd reports it
		}
		if p.Overlaps(opts.GuestSubnet) {
			return &Error{Key: c.key, Msg: fmt.Sprintf("daemon.json %q %s overlaps hyperbutler's guest network %s; pick another range", c.key, c.val, opts.GuestSubnet)}
		}
	}
	return nil
}
