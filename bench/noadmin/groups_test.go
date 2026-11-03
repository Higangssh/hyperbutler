package main

import (
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

// A standard user's `whoami /groups /fo csv /nh`, with names in another language and
// code page as a localized Windows prints them (one record in CP949 bytes).
const standardUser = `"Everyone","Well-known group","S-1-1-0","Mandatory group, Enabled by default, Enabled group"
"BUILTIN\Users","Alias","S-1-5-32-545","Mandatory group, Enabled by default, Enabled group"
"NT AUTHORITY\INTERACTIVE","Well-known group","S-1-5-4","Mandatory group, Enabled by default, Enabled group"
` + "\"\xc7\xd1\xb1\xb9\xbe\xee\",\"\xb1\xd7\xb7\xec\",\"S-1-2-0\",\"\xc7\xca\xbc\xf6\"\r\n" +
	`"Mandatory Label\Medium Mandatory Level","Label","S-1-16-8192",""
`

// An administrator under UAC: Administrators is there, as deny-only.
const filteredAdmin = `"Everyone","Well-known group","S-1-1-0","Mandatory group, Enabled by default, Enabled group"
"BUILTIN\Administrators","Alias","S-1-5-32-544","Group used for deny only"
"BUILTIN\Hyper-V Administrators","Alias","S-1-5-32-578","Mandatory group, Enabled by default, Enabled group"
"Mandatory Label\Medium Mandatory Level","Label","S-1-16-8192",""
`

const elevated = `"BUILTIN\Administrators","Alias","S-1-5-32-544","Mandatory group, Enabled by default, Enabled group, Group owner"
"Mandatory Label\High Mandatory Level","Label","S-1-16-12288",""
`

func TestParseWhoamiGroups(t *testing.T) {
	sids, err := parseWhoamiGroups(strings.NewReader(standardUser))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"S-1-1-0", "S-1-5-32-545", "S-1-5-4", "S-1-2-0", "S-1-16-8192"}
	if strings.Join(sids, " ") != strings.Join(want, " ") {
		t.Fatalf("sids = %v, want %v", sids, want)
	}
	if _, err := parseWhoamiGroups(strings.NewReader("")); err == nil {
		t.Fatal("empty output parsed")
	}
	if _, err := parseWhoamiGroups(strings.NewReader("INFO: no groups\r\n")); err == nil {
		t.Fatal("output without SIDs parsed")
	}
}

func TestJudgeGroups(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		ok   []bool // Administrators, Hyper-V Administrators, medium
	}{
		{"standard", standardUser, []bool{true, true, true}},
		{"filtered admin", filteredAdmin, []bool{false, false, true}},
		{"elevated", elevated, []bool{false, true, false}},
	} {
		sids, err := parseWhoamiGroups(strings.NewReader(tc.in))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got := judgeGroups(sids)
		if len(got) != len(tc.ok) {
			t.Fatalf("%s: %d checks", tc.name, len(got))
		}
		for i, c := range got {
			if c.OK != tc.ok[i] {
				t.Errorf("%s: %s ok=%v, want %v (%s)", tc.name, c.Name, c.OK, tc.ok[i], c.Detail)
			}
		}
	}
}
