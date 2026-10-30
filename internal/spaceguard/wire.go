package spaceguard

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The spike carries the budget from host to guest as one text line per update on a
// serial port:
//
//	budget <bytes> <backed>\n     both unsigned decimal, no leading zeros
//	budget unlimited\n
//
// The real transport (hvsock, guest agent protocol) replaces this later. The line is
// parsed strictly because it crosses the VM boundary.

// MaxLineLen bounds a budget line, including the newline.
const MaxLineLen = 64

// FormatBudget encodes a budget as one line, ending in "\n".
func FormatBudget(b Budget) string {
	return "budget " + b.String() + "\n"
}

// ParseBudget decodes one line written by FormatBudget. A trailing "\n" or "\r\n" is
// accepted; nothing else around the values is.
func ParseBudget(line string) (Budget, error) {
	if len(line) > MaxLineLen {
		return Budget{}, errors.New("spaceguard: budget line too long")
	}
	// Only strip "\r" as part of "\r\n"; a bare trailing "\r" is not a line ending.
	if l, ok := strings.CutSuffix(line, "\n"); ok {
		line = strings.TrimSuffix(l, "\r")
	}
	v, ok := strings.CutPrefix(line, "budget ")
	if !ok {
		return Budget{}, fmt.Errorf("spaceguard: not a budget line: %q", line)
	}
	if v == "unlimited" {
		return Budget{Unlimited: true}, nil
	}
	bytesStr, backedStr, ok := strings.Cut(v, " ")
	if !ok {
		return Budget{}, fmt.Errorf("spaceguard: budget line %q needs two values", line)
	}
	n, err := parseDecimal(bytesStr)
	if err != nil {
		return Budget{}, err
	}
	backed, err := parseDecimal(backedStr)
	if err != nil {
		return Budget{}, err
	}
	return Budget{Bytes: n, Backed: backed}, nil
}

// parseDecimal accepts 1-20 ASCII digits without a leading zero (except "0") that fit
// in a uint64.
func parseDecimal(v string) (uint64, error) {
	if v == "" || len(v) > 20 {
		return 0, fmt.Errorf("spaceguard: bad budget value %q", v)
	}
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return 0, fmt.Errorf("spaceguard: bad budget value %q", v)
		}
	}
	if len(v) > 1 && v[0] == '0' {
		return 0, fmt.Errorf("spaceguard: budget value %q has a leading zero", v)
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("spaceguard: bad budget value %q: %w", v, err)
	}
	return n, nil
}
