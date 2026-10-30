package probe

import (
	"strings"
	"testing"
)

// FuzzParseResult: never panics; an accepted line has a step, at most maxFields fields,
// and only valid keys; and every value quote produces parses back as one token.
func FuzzParseResult(f *testing.F) {
	f.Add("WBNET step=blob ok=true bytes=29767612 ms=808.9")
	f.Add("garbageWBNET step=x ok=false err=a_b")
	f.Add("WBNET step=x ok=true ok=false")
	f.Add("WBNET  step=x\tok=true")
	f.Add("WBNET step=\xff ok=true")
	f.Fuzz(func(t *testing.T, line string) {
		r, ok, err := ParseResult(line)
		if !ok && err != nil {
			t.Fatalf("error %v for a non-result line", err)
		}
		if err == nil && ok {
			if r.Step == "" || len(r.Fields) > maxFields {
				t.Fatalf("accepted %+v", r)
			}
			for k := range r.Fields {
				if !validKey(k) {
					t.Fatalf("accepted key %q", k)
				}
			}
		}
		q := quote(line)
		got, ok, err := ParseResult("WBNET step=x v=" + q)
		if q != "" && !strings.Contains(q, "WBNET ") && (!ok || err != nil || got.Fields["v"] != q) {
			t.Fatalf("quote(%q) = %q does not round-trip: %+v %v", line, q, got, err)
		}
	})
}
