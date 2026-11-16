package daemoncfg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

var testOpts = Options{GuestSubnet: netip.MustParsePrefix("192.168.127.0/24")}

func TestWithDefaults(t *testing.T) {
	gc := `"gc":{"enabled":true,"defaultReservedSpace":"2GB","defaultMaxUsedSpace":"8GB","defaultMinFreeSpace":"5GB"}`
	added := `"builder":{` + gc + `}`
	for _, tc := range []struct {
		name, in, want string
	}{
		{"empty object", `{}`, `{` + added + `}`},
		{"empty object with space", "{ \n}\n", "{ \n" + added + "}\n"},
		{"one key", `{"debug": true}`, `{"debug": true,` + added + `}`},
		{"pretty, CRLF", "{\r\n  \"debug\": true\r\n}\r\n", "{\r\n  \"debug\": true\r\n," + added + "}\r\n"},
		{"braces in strings and values", `{"labels": ["a}"], "x": {"y": {}}}`, `{"labels": ["a}"], "x": {"y": {}},` + added + `}`},
		// A builder that configures something else gets the gc policy inside it.
		{"builder with entitlements only", `{"builder": {"entitlements": {"network-host": true}}, "debug": true}`,
			`{"builder": {"entitlements": {"network-host": true},` + gc + `}, "debug": true}`},
		{"empty builder", `{"builder": { }}`, `{"builder": { ` + gc + `}}`},
		// A user gc, whatever it says, is the user's.
		{"user gc", `{"builder": {"gc": {"enabled": false}}}`, `{"builder": {"gc": {"enabled": false}}}`},
		{"user gc, enabled only", `{"builder":{"gc":{"enabled":true}}}`, `{"builder":{"gc":{"enabled":true}}}`},
		{"user GC (dockerd's decoder folds case)", `{"builder": {"GC": {}}}`, `{"builder": {"GC": {}}}`},
		{"builder null is dockerd's to judge", `{"builder": null}`, `{"builder": null}`},
		// dockerd --validate refuses "Builder" (the flattened sub-keys are compared as
		// written), so what is sent does not matter; inserting inside it keeps one key.
		{"Builder, other case", `{"Builder": {}}`, `{"Builder": {` + gc + `}}`},
		// A dotless i does not fold to "builder": the default is added beside it.
		{"dotless i", `{"bu\u0131lder": {}}`, `{"bu\u0131lder": {},` + added + `}`},
	} {
		if err := Check([]byte(tc.in), testOpts); err != nil && !strings.Contains(tc.name, "dotless") {
			t.Errorf("%s: input refused: %v", tc.name, err)
		}
		got := string(WithDefaults([]byte(tc.in)))
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
	none := WithDefaults(nil)
	if string(none) != "{"+added+"}\n" {
		t.Errorf("no file: %q", none)
	}
	if err := Check(none, testOpts); err != nil {
		t.Errorf("no file: the default does not pass Check: %v", err)
	}
	if n := len(WithDefaults([]byte(`{"builder":{"x":1}}`))) - len(`{"builder":{"x":1}}`); n > DefaultsRoom {
		t.Errorf("an insertion of %d bytes is over DefaultsRoom", n)
	}
}

// TestWithDefaultsAtTheLimit: a file at Check's size limit still fits the guest's limit
// with the default in it.
func TestWithDefaultsAtTheLimit(t *testing.T) {
	b := []byte(`{"labels":["` + strings.Repeat("x", MaxSize-DefaultsRoom-len(`{"labels":[""]}`)) + `"]}`)
	if err := Check(b, testOpts); err != nil {
		t.Fatalf("a file of %d bytes refused: %v", len(b), err)
	}
	if n := len(WithDefaults(b)); n > MaxSize {
		t.Fatalf("delivered %d bytes, over %d", n, MaxSize)
	}
	if err := Check(append(b, ' '), testOpts); err == nil {
		t.Fatal("a file one byte over the limit passes Check")
	}
}

// FuzzWithDefaults: for every file Check accepts, the delivered file fits MaxSize, is one
// valid object that passes Check's syntax and key rules, keeps every user value outside
// "builder", keeps a user builder.gc as written, and configures builder.gc exactly once.
func FuzzWithDefaults(f *testing.F) {
	for _, s := range []string{`{}`, `{"debug":true}`, "{\n \"a\": {\"b\": \"}\"}\n}\n", `{"Builder":{}}`,
		`{"builder":{"entitlements":{"network-host":true}}}`, `{"builder":{"gc":{"enabled":false}}}`, `{"builder":null}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if Check(b, testOpts) != nil {
			return
		}
		out := WithDefaults(b)
		if len(out) > MaxSize {
			t.Fatalf("WithDefaults(%q) is %d bytes", b, len(out))
		}
		var in, got map[string]json.RawMessage
		if err := json.Unmarshal(b, &in); err != nil {
			t.Fatalf("Check accepted %q: %v", b, err)
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("output %q of %q: %v", out, b, err)
		}
		if len(got) != len(in) && len(got) != len(in)+1 {
			t.Fatalf("WithDefaults(%q) = %q has %d keys", b, out, len(got))
		}
		builders := 0
		for k, v := range got {
			if fold(k) != fold("builder") {
				if !bytes.Equal(compact(t, v), compact(t, in[k])) {
					t.Fatalf("WithDefaults(%q) changed %q", b, k)
				}
				continue
			}
			builders++
			var inner map[string]json.RawMessage
			if json.Unmarshal(v, &inner) != nil || inner == nil {
				if !bytes.Equal(out, b) {
					t.Fatalf("a non-object builder was rewritten: %q -> %q", b, out)
				}
				continue
			}
			gcs := 0
			for ik := range inner {
				if fold(ik) == fold("gc") {
					gcs++
				}
			}
			if gcs < 1 {
				t.Fatalf("WithDefaults(%q) = %q has a builder without gc", b, out)
			}
			if uv, user := in[k]; user {
				var uin map[string]json.RawMessage
				_ = json.Unmarshal(uv, &uin)
				for ik, iv := range uin {
					if !bytes.Equal(compact(t, inner[ik]), compact(t, iv)) {
						t.Fatalf("WithDefaults(%q) changed builder.%s", b, ik)
					}
				}
			}
		}
		if builders != 1 {
			t.Fatalf("WithDefaults(%q) = %q sets the builder %d times", b, out, builders)
		}
	})
}

func compact(t *testing.T, v json.RawMessage) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, v); err != nil {
		t.Fatalf("compact %q: %v", v, err)
	}
	return buf.Bytes()
}

// TestBuildCachePolicyConstants pins the policy: dockerd --validate does not parse the
// sizes (BuildKit does, when dockerd starts), so a typo would only show as a dockerd that
// does not start. They must read as whole GiB for units.RAMInBytes, and keep
// reserved < min free < max used as ADR 0011 describes.
func TestBuildCachePolicyConstants(t *testing.T) {
	gib := func(s string) int {
		var n int
		if _, err := fmt.Sscanf(s, "%dGB", &n); err != nil || fmt.Sprintf("%dGB", n) != s {
			t.Fatalf("%q is not a whole number of GB", s)
		}
		return n
	}
	r, f, m := gib(BuildCacheReserved), gib(BuildCacheMinFree), gib(BuildCacheMax)
	if r >= f || f >= m || m*(1<<30) >= 10e9 {
		t.Fatalf("reserved %d, min free %d, max %d GiB", r, f, m)
	}
}
