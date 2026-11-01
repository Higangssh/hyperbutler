// Package i18n is the GUI's message catalog (REQ-UX-013, ADR 0008 "Languages"): one
// JSON file per language in messages/, keyed, with whole sentences and {name}
// placeholders. English is the source; a language's file may leave a key out, and the
// English text is used for it. Plural forms are keys ending in "#one", "#other" (the
// CLDR categories the page's Intl.PluralRules returns for the language); a language
// gives every category it uses.
//
// Logs, the CLI, diagnose and MCP stay English; only the GUI is translated.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

//go:embed messages/*.json
var files embed.FS

// Source is the language every other is translated from.
const Source = "en"

// Pseudo is the pseudo-locale for layout checks: English made about 30% longer, with
// accented letters, so text that is cut, overflows or is not from the catalog shows.
const Pseudo = "pseudo"

// PluralForms are the plural categories each language uses (CLDR).
var PluralForms = map[string][]string{
	"en": {"one", "other"},
	"ko": {"other"},
}

var catalogs = map[string]map[string]string{}

func init() {
	ents, err := files.ReadDir("messages")
	if err != nil {
		panic("i18n: " + err.Error()) // embedded at build time
	}
	for _, e := range ents {
		lang := strings.TrimSuffix(e.Name(), ".json")
		b, err := files.ReadFile("messages/" + e.Name())
		if err != nil {
			panic("i18n: " + err.Error())
		}
		m := map[string]string{}
		if err := json.Unmarshal(b, &m); err != nil {
			panic(fmt.Sprintf("i18n: messages/%s: %v", e.Name(), err)) // TestCatalogs catches it first
		}
		catalogs[lang] = m
	}
}

// Languages lists the shipped languages, the source first.
func Languages() []string {
	out := []string{Source}
	for l := range catalogs {
		if l != Source {
			out = append(out, l)
		}
	}
	sort.Strings(out[1:])
	return out
}

// Raw returns a language's own catalog (nil if it has none), for tests.
func Raw(lang string) map[string]string { return catalogs[lang] }

// Messages returns the catalog for lang merged over English, or the pseudo-locale.
func Messages(lang string) map[string]string {
	out := maps.Clone(catalogs[Source])
	if lang == Pseudo {
		return pseudo(out)
	}
	maps.Copy(out, catalogs[lang])
	return out
}

// Resolve picks the GUI's language: the setting (gui.language) when it names a shipped
// language, else the first of the Windows display languages (BCP 47 tags such as
// "ko-KR", in preference order) that is shipped, else English.
func Resolve(setting string, windows []string) string {
	if setting == Pseudo {
		return Pseudo
	}
	if s := base(setting); setting != "" && setting != "auto" && catalogs[s] != nil {
		return s
	}
	for _, tag := range windows {
		if b := base(tag); catalogs[b] != nil {
			return b
		}
	}
	return Source
}

func base(tag string) string {
	tag = strings.ToLower(strings.TrimSpace(tag))
	if i := strings.IndexAny(tag, "-_"); i >= 0 {
		tag = tag[:i]
	}
	return tag
}

var rePlaceholder = regexp.MustCompile(`\{(\w+)\}`)

// Placeholders returns the placeholder names in s, sorted, without duplicates.
func Placeholders(s string) []string {
	var out []string
	for _, m := range rePlaceholder.FindAllStringSubmatch(s, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

// pseudoMap replaces ASCII letters with accented look-alikes.
var pseudoMap = map[rune]rune{
	'a': 'á', 'b': 'ƀ', 'c': 'ç', 'd': 'ð', 'e': 'é', 'f': 'ƒ', 'g': 'ĝ', 'h': 'ĥ', 'i': 'í', 'j': 'ĵ',
	'k': 'ķ', 'l': 'ļ', 'm': 'ɱ', 'n': 'ñ', 'o': 'ö', 'p': 'þ', 'q': 'ʠ', 'r': 'ŕ', 's': 'š', 't': 'ţ',
	'u': 'ú', 'v': 'ṽ', 'w': 'ŵ', 'x': 'ẋ', 'y': 'ý', 'z': 'ž',
	'A': 'Á', 'B': 'Ɓ', 'C': 'Ç', 'D': 'Ð', 'E': 'É', 'F': 'Ƒ', 'G': 'Ĝ', 'H': 'Ĥ', 'I': 'Í', 'J': 'Ĵ',
	'K': 'Ķ', 'L': 'Ļ', 'M': 'Ṁ', 'N': 'Ñ', 'O': 'Ö', 'P': 'Þ', 'Q': 'Ǫ', 'R': 'Ŕ', 'S': 'Š', 'T': 'Ţ',
	'U': 'Ú', 'V': 'Ṽ', 'W': 'Ŵ', 'X': 'Ẋ', 'Y': 'Ý', 'Z': 'Ž',
}

// PseudoText is s in the pseudo-locale: letters accented, placeholders kept, and
// padded to at least 30% more characters between brackets.
func PseudoText(s string) string {
	var b strings.Builder
	n := 0
	for i := 0; i < len(s); {
		if loc := rePlaceholder.FindStringIndex(s[i:]); loc != nil && loc[0] == 0 {
			b.WriteString(s[i : i+loc[1]])
			n += utf8.RuneCountInString(s[i : i+loc[1]])
			i += loc[1]
			continue
		}
		r := rune(s[i])
		size := 1
		if r >= 0x80 {
			r, size = decodeRune(s[i:])
		}
		if p, ok := pseudoMap[r]; ok {
			r = p
		}
		b.WriteRune(r)
		n++
		i += size
	}
	pad := (n*3 + 9) / 10
	return "[" + b.String() + " " + strings.Repeat("~", max(pad, 1)) + "]"
}

func decodeRune(s string) (rune, int) {
	for _, r := range s {
		return r, len(string(r))
	}
	return 0, 1
}

func pseudo(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = PseudoText(v)
	}
	return out
}
