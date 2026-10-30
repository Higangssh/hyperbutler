package hcn

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzParseErrorRecord: never panics, and the message is always safe to log (valid
// UTF-8, no control characters, bounded length).
func FuzzParseErrorRecord(f *testing.F) {
	f.Add(`{"Success":false,"Error":"Access is denied.\r\n","ErrorCode":2147942405}`)
	f.Add(`{"Error":"\u001b[31mred"}`)
	f.Add(`{"ErrorCode":-1}`)
	f.Add(`null`)
	f.Fuzz(func(t *testing.T, doc string) {
		msg, _, err := ParseErrorRecord(doc)
		if err != nil {
			return
		}
		if !utf8.ValidString(msg) {
			t.Fatalf("invalid UTF-8 message %q", msg)
		}
		if len(msg) > 512+len("...")+utf8.UTFMax {
			t.Fatalf("message of %d bytes", len(msg))
		}
		for _, r := range msg {
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
				t.Fatalf("control character %U in %q", r, msg)
			}
		}
	})
}

// FuzzParseIDList: never panics; every accepted ID is a canonical upper-case GUID that
// NormalizeGUID accepts unchanged.
func FuzzParseIDList(f *testing.F) {
	f.Add(`["3afee72c-1e7c-4cb2-9fbc-f9663a27c59f"]`)
	f.Add(`["{7E6081D5-03C0-46AC-A30A-72A47B0C7DEA}",""]`)
	f.Add(`[]`)
	f.Add(`[null]`)
	f.Fuzz(func(t *testing.T, doc string) {
		ids, err := ParseIDList(doc)
		if err != nil {
			return
		}
		for _, id := range ids {
			if len(id) != 36 || strings.ToUpper(id) != id {
				t.Fatalf("non-canonical id %q", id)
			}
			again, err := NormalizeGUID(id)
			if err != nil || again != id {
				t.Fatalf("NormalizeGUID(%q) = %q, %v", id, again, err)
			}
		}
	})
}

// FuzzParseNetwork: never panics, and the delete guard cannot be talked past. If the
// parsed Name passes ValidateName, that exact string must appear as the value of a
// top-level "Name" key in the raw document, matched case-insensitively and counting
// duplicates. So a document whose every Name-like key has a non-hyperbutler value (for
// example "WSL") can never parse to a deletable name through key-case folding,
// duplicate keys, nesting or trailing data.
func FuzzParseNetwork(f *testing.F) {
	f.Add(`{"ID":"3afee72c-1e7c-4cb2-9fbc-f9663a27c59f","Name":"hyperbutler-spike-nat","Type":"NAT","Flags":8,"Ipams":[{"Subnets":[{"IpAddressPrefix":"10.239.77.0/24"}]}]}`)
	f.Add(`{"Name":"WSL","Flags":9}`)
	f.Add(`{"Name":"hyperbutler-a","Name":"WSL"}`)
	f.Add(`{"name":"WSL","NAME":"hyperbutler-a"}`)
	f.Add(`{"Policies":[{"Name":"hyperbutler-a"}],"Name":"WSL"}`)
	f.Add(`{"Name":"hyperbutler-\u0000"}`)
	f.Fuzz(func(t *testing.T, doc string) {
		n, err := ParseNetwork(doc)
		if err != nil || ValidateName(n.Name) != nil {
			_, _ = ParseEndpoint(doc)
			return
		}
		values, ok := topLevelNames(doc)
		if !ok {
			t.Fatalf("ParseNetwork accepted %q but it is not a JSON object", doc)
		}
		for _, v := range values {
			if v == n.Name {
				return
			}
		}
		t.Fatalf("parsed deletable name %q not among top-level Name values %q", n.Name, values)
	})
}

// topLevelNames returns the string values of every top-level key equal to "Name"
// ignoring case, in document order, including duplicates.
func topLevelNames(doc string) ([]string, bool) {
	d := json.NewDecoder(strings.NewReader(doc))
	if tok, err := d.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	var out []string
	for d.More() {
		tok, err := d.Token()
		if err != nil {
			return nil, false
		}
		key, _ := tok.(string)
		var v json.RawMessage
		if err := d.Decode(&v); err != nil {
			return nil, false
		}
		if strings.EqualFold(key, "Name") {
			var s string
			if json.Unmarshal(v, &s) == nil {
				out = append(out, s)
			}
		}
	}
	return out, true
}
