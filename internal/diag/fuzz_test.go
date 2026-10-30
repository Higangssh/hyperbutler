package diag

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

func FuzzParseMemoryLimit(f *testing.F) {
	for _, s := range []string{"0", "512MiB", "off", "1TiB", "8388608TiB", "9223372036854775808", "1.5GiB", "-1", "B", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n, err := ParseMemoryLimit(s)
		if err != nil {
			return
		}
		if n < 0 {
			t.Fatalf("ParseMemoryLimit(%q) = %d < 0", s, n)
		}
		if s == "off" {
			if n != math.MaxInt64 {
				t.Fatalf("off = %d", n)
			}
			return
		}
		// Every accepted input is digits plus one known suffix, and the value is the
		// exact product (no wrap-around).
		num := strings.TrimRight(s, "BKMGTi")
		suffix := s[len(num):]
		mult := map[string]int64{"": 1, "B": 1, "KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40}[suffix]
		if mult == 0 {
			t.Fatalf("accepted %q with suffix %q", s, suffix)
		}
		v, perr := strconv.ParseUint(num, 10, 64)
		if perr != nil || num == "" || strings.ContainsAny(num, "+-") {
			t.Fatalf("accepted %q with number %q", s, num)
		}
		if v > math.MaxInt64/uint64(mult) || int64(v)*mult != n {
			t.Fatalf("ParseMemoryLimit(%q) = %d, want %d*%d", s, n, v, mult)
		}
	})
}

func FuzzParseSeconds(f *testing.F) {
	for _, s := range []string{"0", "60", "60.0001", "NaN", "-0", "1e1", "0x1p5", "Inf"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, err := ParseSeconds(s)
		if err != nil {
			return
		}
		if math.IsNaN(v) || v < 0 || v > MaxProfileSeconds {
			t.Fatalf("ParseSeconds(%q) = %v", s, v)
		}
	})
}
