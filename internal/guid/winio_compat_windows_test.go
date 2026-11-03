package guid

import (
	"testing"

	wguid "github.com/Microsoft/go-winio/pkg/guid"
)

// TestSameAsWinio: while parts of hyperbutler still use go-winio's GUID, this one must
// convert to it with a plain conversion and agree on every form.
func TestSameAsWinio(t *testing.T) {
	for i := 0; i < 1000; i++ {
		g, err := NewV4()
		if err != nil {
			t.Fatal(err)
		}
		w := wguid.GUID(g)
		if w.String() != g.String() || w.ToArray() != g.ToArray() || w.ToWindowsArray() != g.ToWindowsArray() {
			t.Fatalf("%s: winio says %s", g, w)
		}
		if p, err := wguid.FromString(g.String()); err != nil || GUID(p) != g {
			t.Fatalf("winio parsed %s as %s", g, p)
		}
	}
	ns, _ := FromString("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	a, _ := NewV5(ns, []byte("hyperbutler"))
	b, _ := wguid.NewV5(wguid.GUID(ns), []byte("hyperbutler"))
	if wguid.GUID(a) != b {
		t.Fatalf("v5: %s, winio %s", a, b)
	}
}
