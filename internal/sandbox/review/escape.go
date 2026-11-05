package review

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Escape returns s as it is shown in a review: printable text unchanged, and in Go
// escape form every byte or character that could make the text read as something it is
// not (REQ-SBX-009): invalid UTF-8, control characters, format characters (bidirectional
// overrides and isolates, zero-width joiners), separators and spaces other than U+0020,
// private-use and unassigned characters, and the backslash itself so that an escape in
// the output always came from Escape. Newlines in a diff are the caller's to split on.
func Escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n <= 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r == '\\':
			b.WriteString(`\\`)
		case shownAsIs(r):
			b.WriteRune(r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
		i += n
	}
	return b.String()
}

func shownAsIs(r rune) bool {
	if r < utf8.RuneSelf {
		return r >= 0x20 && r < 0x7f
	}
	if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs, unicode.Zl, unicode.Zp, unicode.Zs, unicode.Other_Default_Ignorable_Code_Point) {
		return false
	}
	return unicode.IsGraphic(r)
}

// Shorten is Escape cut to at most max runes, with the original length in bytes shown
// when it was cut, so that a long name cannot push what follows it out of view.
func Shorten(s string, max int) string {
	e := Escape(s)
	if utf8.RuneCountInString(e) <= max {
		return e
	}
	cut := 0
	for i := range e {
		if cut == max {
			e = e[:i]
			break
		}
		cut++
	}
	return fmt.Sprintf("%s… (%d bytes)", e, len(s))
}
