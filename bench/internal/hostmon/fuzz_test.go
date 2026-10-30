package hostmon

import (
	"fmt"
	"strings"
	"testing"
)

func FuzzParseCIM(f *testing.F) {
	for _, s := range []string{
		"1234,7340032,215,16,51150848\r\n",
		"1,2,3,4,5\n7,8,9,10,11\n",
		"1,2,3,4,5\n1,2,3,4,5\n",
		"0,1,1,1,1",
		"Get-CimInstance : Invalid query",
		"\r\n\r\n",
		"1,2,3,4,99999999999999999999",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		m, err := ParseCIM(s)
		if err != nil {
			if m != nil {
				t.Fatal("result with error")
			}
			return
		}
		// Every accepted line is digits and commas only, one sample per non-blank line,
		// and the re-encoded samples parse back to the same values.
		lines := 0
		for _, l := range strings.Split(s, "\n") {
			l = strings.TrimSpace(l)
			if l == "" {
				continue
			}
			lines++
			if strings.Trim(l, "0123456789,") != "" || strings.Count(l, ",") != cimFields-1 {
				t.Fatalf("accepted line %q", l)
			}
		}
		if lines != len(m) {
			t.Fatalf("%d lines but %d samples", lines, len(m))
		}
		var b strings.Builder
		for pid, p := range m {
			if pid == 0 || p.PID != pid {
				t.Fatalf("bad PID key %d for %+v", pid, p)
			}
			fmt.Fprintf(&b, "%d,%d,%d,%d,%d\n", p.PID, p.WorkingSetPriv, p.HandleCount, p.ThreadCount, p.PrivateBytes)
		}
		m2, err := ParseCIM(b.String())
		if err != nil || len(m2) != len(m) {
			t.Fatalf("re-parse: %v, %d vs %d", err, len(m2), len(m))
		}
		for k, v := range m {
			if m2[k] != v {
				t.Fatalf("re-parse changed %d: %+v vs %+v", k, v, m2[k])
			}
		}
	})
}

func FuzzParseStats(f *testing.F) {
	for _, s := range []string{
		`{"goroutines":12,"handles":150,"memory_limit":1,"memstats":{"heap_inuse":4194304}}`,
		`{"goroutines":0}`,
		`{"goroutines":1}{"goroutines":1}`,
		`{"goroutines":1,"x":1}`,
		`{"goroutines":1e3}`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		st, err := ParseStats(strings.NewReader(s))
		if err != nil {
			return
		}
		if st.Goroutines < 1 || st.Handles < -1 {
			t.Fatalf("accepted implausible %+v", st)
		}
	})
}
