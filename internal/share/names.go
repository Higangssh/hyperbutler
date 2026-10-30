package share

import (
	"unicode/utf8"
)

// Linux names can hold characters NTFS refuses. They are mapped to the Unicode private
// use range U+F000+c, as WSL's drvfs does, so files created by either one look the same
// to the other:
//   - the control characters U+0001..U+001F;
//   - " * : < > ? \ |
//   - a trailing space or dot (Windows programs strip them, so a name ending in one would
//     be unreachable from Windows).
//
// The mapping is undone when the host lists a directory. A host name that contains the
// mapped code points shows up in the guest with the original characters, so the Linux
// names "a:" and "a" are the same file. That aliasing is inherited from drvfs.

const puaBase = 0xF000

func ntReserved(r rune) bool {
	switch r {
	case '"', '*', ':', '<', '>', '?', '\\', '|':
		return true
	}
	return r > 0 && r < 0x20
}

// toNT maps one checked Linux name component to its NTFS name. It refuses names that
// are not valid UTF-8, because NTFS names are UTF-16 and a lossy conversion would make
// two Linux names one host file.
func toNT(name string) (string, error) {
	if err := checkName(name); err != nil {
		return "", err
	}
	if !utf8.ValidString(name) {
		return "", EINVAL
	}
	plain := true
	for i := 0; i < len(name); i++ {
		if c := name[i]; c < 0x20 || ntReserved(rune(c)) {
			plain = false
			break
		}
	}
	last := name[len(name)-1]
	if plain && last != ' ' && last != '.' {
		return name, nil
	}
	out := make([]rune, 0, len(name))
	for _, r := range name {
		if ntReserved(r) {
			r += puaBase
		}
		out = append(out, r)
	}
	if l := out[len(out)-1]; l == ' ' || l == '.' {
		out[len(out)-1] = l + puaBase
	}
	s := string(out)
	if utf16Len(s) > maxNameLen {
		return "", ENAMETOOLONG
	}
	return s, nil
}

// fromNT maps an NTFS name back to the Linux name the guest sees. It is the inverse of
// toNT: toNT(fromNT(n)) == n for every n whose result is a valid Linux name. A host name
// that would map to something the guest cannot use ("." or "..") is shown unmapped.
func fromNT(name string) string {
	mapped := false
	for _, r := range name {
		if r >= puaBase && r < puaBase+0x80 {
			mapped = true
			break
		}
	}
	if !mapped {
		return name
	}
	rs := []rune(name)
	for i, r := range rs {
		if r < puaBase || r >= puaBase+0x80 {
			continue
		}
		o := r - puaBase
		if ntReserved(o) || ((o == ' ' || o == '.') && i == len(rs)-1) {
			rs[i] = o
		}
	}
	out := string(rs)
	if checkName(out) != nil {
		return name
	}
	if back, err := toNT(out); err != nil || back != name {
		return name
	}
	return out
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}
