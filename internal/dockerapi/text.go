package dockerapi

import (
	"bytes"
	"encoding/binary"
	"strings"
	"unicode/utf8"

	"github.com/Higangssh/hyperbutler/internal/redact"
)

// Clip cuts s to at most n bytes on a UTF-8 boundary.
func Clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// CleanText makes text that a container, dockerd or the guest wrote safe to show:
// invalid UTF-8 becomes U+FFFD, and so do control characters (escape sequences
// included), the C1 controls and the bidirectional overrides that make text read
// differently than it is. Newlines and tabs stay when multiline.
func CleanText(s string, multiline bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToValidUTF8(s, string(utf8.RuneError)) {
		switch {
		case multiline && (r == '\n' || r == '\t'):
		case r == '\r' && multiline:
			continue // CRLF and progress-bar carriage returns
		case r < 0x20, r == 0x7f, r >= 0x80 && r < 0xa0,
			r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == 0x200e, r == 0x200f, r == 0x061c:
			r = utf8.RuneError
		}
		b.WriteRune(r)
	}
	return b.String()
}

// LogText is untrusted log text as a front end shows it: redacted first (so a cut can
// never leave part of a secret behind), cleaned, then the last lines lines and at most
// maxBytes of them. cut says whether anything was left out.
func LogText(raw []byte, lines, maxBytes int) (text string, n int, cut bool) {
	s := CleanText(redact.String(string(raw)), true)
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return "", 0, false
	}
	all := strings.Split(s, "\n")
	if len(all) > lines {
		all, cut = all[len(all)-lines:], true
	}
	size := 0
	keep := len(all)
	for i := len(all) - 1; i >= 0; i-- {
		size += len(all[i]) + 1
		if size > maxBytes {
			break
		}
		keep = i
	}
	if keep == len(all) {
		// One line longer than maxBytes: its end.
		last := all[len(all)-1]
		start := len(last) - maxBytes
		for start < len(last) && !utf8.RuneStart(last[start]) {
			start++
		}
		return last[start:], 1, true
	}
	if keep > 0 {
		all, cut = all[keep:], true
	}
	return strings.Join(all, "\n"), len(all), cut
}

// Demux turns Docker's multiplexed log stream (a container without a TTY) into its text:
// frames of an 8-byte header (stream 0, 1 or 2, three zero bytes, the payload size
// big-endian) and the payload, stdout and stderr in the order written. A last frame cut
// short keeps what arrived. ok is false when the stream is not one: then out holds the
// frames before the first bad header.
func Demux(b []byte) (out []byte, ok bool) {
	var buf bytes.Buffer
	for len(b) > 0 {
		if len(b) < 8 {
			return buf.Bytes(), false
		}
		if b[0] > 2 || b[1] != 0 || b[2] != 0 || b[3] != 0 {
			return buf.Bytes(), false
		}
		n := binary.BigEndian.Uint32(b[4:8])
		b = b[8:]
		if uint64(n) > uint64(len(b)) {
			buf.Write(b)
			return buf.Bytes(), true
		}
		buf.Write(b[:n])
		b = b[n:]
	}
	return buf.Bytes(), true
}

// Resync returns b from the first offset where a well-formed run of multiplexed frames
// starts (each header followed by its whole payload, up to the end of b), for a log
// read from the middle of the stream. It returns nil when there is none.
func Resync(b []byte) []byte {
	for i := 0; i+8 <= len(b); i++ {
		if b[i] < 1 || b[i] > 2 || b[i+1] != 0 || b[i+2] != 0 || b[i+3] != 0 {
			continue
		}
		if frames(b[i:]) {
			return b[i:]
		}
	}
	return nil
}

// frames reports whether b starts with a run of frames: 16 well-formed ones, or fewer up
// to the end of b with the last one possibly cut short. The bound keeps Resync linear.
func frames(b []byte) bool {
	for k := 0; len(b) > 0 && k < 16; k++ {
		if len(b) < 8 || b[0] > 2 || b[1] != 0 || b[2] != 0 || b[3] != 0 {
			return false
		}
		n := binary.BigEndian.Uint32(b[4:8])
		b = b[8:]
		if uint64(n) >= uint64(len(b)) {
			return true
		}
		b = b[n:]
	}
	return true
}
