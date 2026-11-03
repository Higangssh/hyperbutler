package hvaddr

import (
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

// FuzzParseHvsockTarget: never panics, accepted input round-trips through String, and
// String's output is always accepted.
func FuzzParseHvsockTarget(f *testing.F) {
	f.Add(testVM + ":vsock:2375")
	f.Add("{" + testVM + "}:0000094f-facb-11e6-bd58-64006a7986d3")
	f.Add(testVM + ":vsock:4294967296")
	f.Add(":")
	f.Fuzz(func(t *testing.T, s string) {
		got, err := ParseHvsockTarget(s)
		if err != nil {
			return
		}
		if len(s) > maxHvsockTargetLen {
			t.Fatalf("accepted %d-byte input", len(s))
		}
		again, err := ParseHvsockTarget(got.String())
		if err != nil {
			t.Fatalf("String() output %q rejected: %v", got.String(), err)
		}
		if again != got {
			t.Fatalf("round trip %q: %v != %v", s, again, got)
		}
	})
}

// FuzzValidatePipeName: never panics, and accepted names satisfy every invariant the
// listener relies on.
func FuzzValidatePipeName(f *testing.F) {
	f.Add(DefaultPipeName)
	f.Add(`\\.\pipe\a\b`)
	f.Add(`\\server\pipe\x`)
	f.Add(`\\.\PiPe\` + "\U0001F600")
	f.Fuzz(func(t *testing.T, s string) {
		if ValidatePipeName(s) != nil {
			return
		}
		if !strings.EqualFold(s[:len(localPipePrefix)], localPipePrefix) {
			t.Fatalf("accepted %q without the local prefix", s)
		}
		rest := s[len(localPipePrefix):]
		if rest == "" || strings.ContainsAny(rest, "\\/\x00") {
			t.Fatalf("accepted bad name part %q", rest)
		}
		if !utf8.ValidString(s) {
			t.Fatalf("accepted invalid UTF-8 %q", s)
		}
		if n := len(utf16.Encode([]rune(s))); n > maxPipeNameLen {
			t.Fatalf("accepted %d UTF-16 units", n)
		}
	})
}
