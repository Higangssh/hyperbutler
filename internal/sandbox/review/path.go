// Package review holds the host-side rules for a sandbox session's changes before they
// are applied to the user's project (ADR 0009, "Bringing changes out"): the path grammar
// every changed file must match, how guest-controlled text is shown, and which changes
// are listed first because they run code or point tools at a server later. Everything
// here reads guest-controlled input and is pure, so it is unit and fuzz tested without a
// session.
package review

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// Path limits. A segment is limited as NTFS limits a name (255 UTF-16 code units); the
// whole path is kept well under MAX_PATH-free limits so that the project directory in
// front of it still fits in the 32767 units of an NT path.
const (
	MaxSegment = 255
	MaxPath    = 1024
	MaxDepth   = 64
)

// ErrPath is wrapped by every path refusal.
var ErrPath = errors.New("path not allowed")

// CheckPath checks p against the grammar a changed file's path must match and returns its
// segments. The grammar allows rather than forbids: p is relative and '/'-separated, each
// segment is 1 to MaxSegment UTF-16 code units of allowed characters (see allowedRune),
// is not "." or "..", does not start with a space or end with a space or dot, and is not
// a Windows device name with or without an extension. The error is caller-safe: it
// shows p escaped.
//
// What CheckPath cannot decide alone is left to the apply step, which opens relative to
// the project's handle: whether a segment is an existing 8.3 short name or differs only
// in case from an existing name in a case-insensitive directory.
func CheckPath(p string) ([]string, error) {
	if p == "" {
		return nil, fmt.Errorf("%w: empty path", ErrPath)
	}
	if !utf8.ValidString(p) {
		return nil, fmt.Errorf("%w: %s is not valid UTF-8", ErrPath, Escape(p))
	}
	if n := utf16Len(p); n > MaxPath {
		return nil, fmt.Errorf("%w: %s is %d UTF-16 code units long (at most %d)", ErrPath, Shorten(p, 80), n, MaxPath)
	}
	segs := strings.Split(p, "/")
	if len(segs) > MaxDepth {
		return nil, fmt.Errorf("%w: %s is %d levels deep (at most %d)", ErrPath, Shorten(p, 80), len(segs), MaxDepth)
	}
	for _, s := range segs {
		if err := checkSegment(s); err != nil {
			return nil, fmt.Errorf("%w: %s: %s", ErrPath, Shorten(p, 120), err)
		}
	}
	return segs, nil
}

func checkSegment(s string) error {
	switch {
	case s == "":
		return errors.New("empty segment (absolute path, or doubled or trailing '/')")
	case s == "." || s == "..":
		return errors.New("'.' or '..' segment")
	case utf16Len(s) > MaxSegment:
		return fmt.Errorf("a segment longer than %d UTF-16 code units", MaxSegment)
	case s[0] == ' ':
		return errors.New("a segment starting with a space")
	case strings.HasSuffix(s, " ") || strings.HasSuffix(s, "."):
		return errors.New("a segment ending in a space or dot")
	}
	for _, r := range s {
		if !allowedRune(r) {
			return fmt.Errorf("character %s is not allowed in a path", Escape(string(r)))
		}
	}
	if isDeviceName(s) {
		return fmt.Errorf("%s is a Windows device name", Escape(s))
	}
	return nil
}

// allowedRune is the character set of a path segment: printable ASCII other than the
// characters Windows or NTFS give a meaning (" * : < > ? \ |), and outside ASCII letters
// and digits only. Refused outside ASCII:
//   - marks (category M), so that a name in decomposed form cannot sit beside the same
//     name precomposed: NTFS does not normalise, and the two look the same in review;
//   - conjoining Hangul jamo, which compose into syllables the same way;
//   - characters whose canonical decomposition is another single character (the Kelvin
//     and Ohm signs, CJK compatibility ideographs), for the same reason;
//   - everything else: spaces other than U+0020, format characters (bidirectional
//     controls, joiners), private use (where the file server maps reserved characters),
//     symbols and punctuation, which hold most look-alikes of '/', '.' and quotes.
//
// A name the set refuses is listed in the review as not applicable; the user can copy
// that file by hand.
func allowedRune(r rune) bool {
	if r < utf8.RuneSelf {
		return r >= 0x20 && r < 0x7f && !strings.ContainsRune(`"*:<>?\|`, r)
	}
	switch {
	case r >= 0x1100 && r <= 0x11ff, r >= 0xa960 && r <= 0xa97f, r >= 0xd7b0 && r <= 0xd7ff:
		return false // conjoining jamo
	case r >= 0xf900 && r <= 0xfaff, r >= 0x2f800 && r <= 0x2fa1f:
		return false // CJK compatibility ideographs
	case r == 0x2126 || r == 0x212a || r == 0x212b:
		return false // Ohm, Kelvin and Angstrom signs
	case r >= 0xff00 && r <= 0xffef:
		return false // fullwidth and halfwidth forms: look-alikes of ASCII
	}
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// isDeviceName reports whether s names a Windows device: CON, PRN, AUX, NUL, COM0-9,
// LPT0-9 (with the superscript digits Windows also accepts), CONIN$ and CONOUT$,
// compared without case and before the first dot, with spaces before the dot ignored.
func isDeviceName(s string) bool {
	base, _, _ := strings.Cut(s, ".")
	base = strings.ToUpper(strings.TrimRight(base, " "))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(base) >= 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		switch base[3:] {
		case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
			return true
		}
	}
	return false
}

// FoldKey is the key two paths collide on in a case-insensitive directory: each rune
// upper-cased as NTFS's upcase table does for the characters allowedRune lets through.
// The apply step still opens every path and checks the name it found, so FoldKey only
// has to catch collisions inside one change set.
func FoldKey(p string) string {
	return strings.Map(unicode.ToUpper, p)
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}
