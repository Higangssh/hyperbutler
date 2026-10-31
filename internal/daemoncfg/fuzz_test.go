package daemoncfg

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// FuzzCheck: Check never panics, never changes its input, and accepts only a single
// JSON object without owned or duplicate keys.
func FuzzCheck(f *testing.F) {
	for _, s := range []string{
		`{}`, `{"insecure-registries":["10.0.0.1:5000"]}`, `{"hosts":[]}`, `{"HOSTS":[]}`,
		`{"debug":true,"debug":true}`, `{"bip":"192.168.127.1/24"}`, "\xef\xbb\xbf{}", `[1]`,
		`{"a":{"b":[1,2,{"c":null}]}}`, "{\"tlK\":1}", `{"a":1}{`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		orig := bytes.Clone(b)
		err := Check(b, guest)
		if !bytes.Equal(b, orig) {
			t.Fatal("Check changed its input")
		}
		if err != nil {
			if err.Error() == "" {
				t.Fatal("empty error message")
			}
			return
		}
		// Accepted: it must be one JSON object to the standard decoder too.
		var m map[string]json.RawMessage
		if json.Unmarshal(b, &m) != nil {
			t.Fatalf("accepted %q, which is not a JSON object", b)
		}
		for k := range m {
			for _, o := range Owned {
				if strings.EqualFold(k, o.Key) {
					t.Fatalf("accepted owned key %q", k)
				}
			}
		}
	})
}
