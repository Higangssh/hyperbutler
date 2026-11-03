package guid

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestKnownValues(t *testing.T) {
	// The Hyper-V socket service ID template (port 0x2375 in Data1), and HV_GUID_WILDCARD.
	const s = "00002375-facb-11e6-bd58-64006a7986d3"
	g, err := FromString(s)
	if err != nil {
		t.Fatal(err)
	}
	if g.Data1 != 0x2375 || g.Data2 != 0xfacb || g.Data3 != 0x11e6 || g.Data4 != [8]byte{0xbd, 0x58, 0x64, 0x00, 0x6a, 0x79, 0x86, 0xd3} {
		t.Fatalf("fields %+v", g)
	}
	if g.String() != s {
		t.Fatalf("String %s", g)
	}
	if up, err := FromString(strings.ToUpper(s)); err != nil || up != g {
		t.Fatalf("upper case: %v %v", up, err)
	}
	if b := g.ToArray(); b[0] != 0 || b[2] != 0x23 || b[3] != 0x75 {
		t.Fatalf("big-endian bytes %x", b)
	}
	if b := g.ToWindowsArray(); b[0] != 0x75 || b[1] != 0x23 {
		t.Fatalf("Windows bytes %x", b)
	}
	if FromArray(g.ToArray()) != g || FromWindowsArray(g.ToWindowsArray()) != g {
		t.Fatal("array round trips")
	}
	if (GUID{}).String() != "00000000-0000-0000-0000-000000000000" {
		t.Fatal("zero")
	}
}

func TestFromStringRefuses(t *testing.T) {
	for _, s := range []string{
		"", "{00002375-facb-11e6-bd58-64006a7986d3}", "00002375facb11e6bd5864006a7986d3",
		"00002375-facb-11e6-bd58-64006a7986dz", "00002375-facb-11e6+bd58-64006a7986d3",
		"0000237-5facb-11e6-bd58-64006a7986d3", " 0002375-facb-11e6-bd58-64006a7986d3",
	} {
		if _, err := FromString(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestNewV4V5(t *testing.T) {
	a, err := NewV4()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewV4()
	if a == b || a.Data3>>12 != 4 || a.Data4[0]>>6 != 2 {
		t.Fatalf("v4: %s %s", a, b)
	}
	// RFC 4122 appendix-style check: the DNS namespace and "python.org" give this
	// version 5 GUID (as Python's uuid.uuid5 does).
	dns, _ := FromString("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	v5, _ := NewV5(dns, []byte("python.org"))
	if v5.String() != "886313e1-3b8a-5372-9b90-0c9aee199e5d" {
		t.Fatalf("v5 %s", v5)
	}
}

func TestText(t *testing.T) {
	g, _ := NewV4()
	b, err := json.Marshal(map[string]GUID{"id": g})
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]GUID
	if err := json.Unmarshal(b, &back); err != nil || back["id"] != g {
		t.Fatalf("%s: %v %v", b, back, err)
	}
	var bad GUID
	if err := bad.UnmarshalText([]byte("nope")); err == nil {
		t.Fatal("bad text accepted")
	}
}

// FuzzFromString: whatever parses prints back as the same GUID, lower case.
func FuzzFromString(f *testing.F) {
	f.Add("00002375-facb-11e6-bd58-64006a7986d3")
	f.Add("FFFFFFFF-FFFF-FFFF-FFFF-FFFFFFFFFFFF")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		g, err := FromString(s)
		if err != nil {
			return
		}
		if g.String() != strings.ToLower(s) {
			t.Fatalf("%q printed as %q", s, g)
		}
		if back, err := FromString(g.String()); err != nil || back != g {
			t.Fatalf("round trip of %q: %v %v", s, back, err)
		}
	})
}
