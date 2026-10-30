package engine

import (
	"errors"
	"fmt"
	"math/bits"
	"strconv"
	"strings"
)

// sizeUnits are binary: "64G" and "64GiB" are both 64 x 2^30, as in docker's own
// memory flags. Fractions are not accepted.
var sizeUnits = []struct {
	suffix string
	shift  uint
}{
	{"kib", 10}, {"mib", 20}, {"gib", 30}, {"tib", 40},
	{"kb", 10}, {"mb", 20}, {"gb", 30}, {"tb", 40},
	{"k", 10}, {"m", 20}, {"g", 30}, {"t", 40},
	{"b", 0},
}

// maxSizeLen bounds the text ParseSize looks at.
const maxSizeLen = 32

// ParseSize parses a byte count such as "64GiB", "64g", "2048MB" or "1073741824".
func ParseSize(s string) (uint64, error) {
	if len(s) == 0 || len(s) > maxSizeLen {
		return 0, fmt.Errorf("size %q: want a number with an optional unit, such as 64GiB", s)
	}
	num := strings.ToLower(strings.TrimSpace(s))
	var shift uint
	for _, u := range sizeUnits {
		if strings.HasSuffix(num, u.suffix) {
			num, shift = strings.TrimSpace(num[:len(num)-len(u.suffix)]), u.shift
			break
		}
	}
	if num == "" || num[0] < '0' || num[0] > '9' {
		return 0, fmt.Errorf("size %q: want a number with an optional unit, such as 64GiB", s)
	}
	n, err := strconv.ParseUint(num, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, errors.Unwrap(err))
	}
	if hi, _ := bits.Mul64(n, 1<<shift); hi != 0 {
		return 0, fmt.Errorf("size %q is too large", s)
	}
	return n << shift, nil
}

// FormatSize renders n with the largest binary unit that divides it exactly, so
// ParseSize(FormatSize(n)) == n.
func FormatSize(n uint64) string {
	for _, u := range []struct {
		name  string
		shift uint
	}{{"TiB", 40}, {"GiB", 30}, {"MiB", 20}, {"KiB", 10}} {
		if n != 0 && n%(1<<u.shift) == 0 {
			return strconv.FormatUint(n>>u.shift, 10) + u.name
		}
	}
	return strconv.FormatUint(n, 10) + "B"
}

// HumanSize renders n approximately, for display: "1.2 GiB".
func HumanSize(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
