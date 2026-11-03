package vm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/guid"
)

// FuzzUserOnlySDDL: an accepted SID can never add an SDDL clause or widen the DACL; the
// result is always exactly one allow ACE for that SID.
func FuzzUserOnlySDDL(f *testing.F) {
	f.Add(testSID)
	f.Add("S-1-5-18)(A;;FA;;;WD")
	f.Add("S-1-5-18-")
	f.Fuzz(func(t *testing.T, sid string) {
		sddl, err := UserOnlySDDL(sid)
		if err != nil {
			return
		}
		if strings.ContainsAny(sid, "();:") || strings.Count(sddl, "(") != 1 {
			t.Fatalf("SID %q produced %q", sid, sddl)
		}
		if sddl != "D:P(A;;FA;;;"+sid+")" {
			t.Fatalf("unexpected SDDL %q", sddl)
		}
	})
}

// FuzzParseRuntimeID: never panics, and an accepted ID is never the nil GUID and matches
// the RuntimeId field of the document.
func FuzzParseRuntimeID(f *testing.F) {
	f.Add(`{"RuntimeId":"7d757fea-5532-5530-aa44-69bf7ad959da"}`)
	f.Add(`{"RuntimeId":"{7d757fea-5532-5530-aa44-69bf7ad959da}"}`)
	f.Add(`{"RuntimeId":null}`)
	f.Fuzz(func(t *testing.T, doc string) {
		id, err := parseRuntimeID(doc)
		if err != nil {
			return
		}
		if id == (guid.GUID{}) {
			t.Fatal("accepted the nil GUID")
		}
		var p struct{ RuntimeId string }
		if err := json.Unmarshal([]byte(doc), &p); err != nil {
			t.Fatalf("accepted invalid JSON: %v", err)
		}
		again, err := guid.FromString(p.RuntimeId)
		if err != nil || again != id {
			t.Fatalf("RuntimeId %q parsed as %s", p.RuntimeId, id)
		}
	})
}
