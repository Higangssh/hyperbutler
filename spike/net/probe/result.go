package probe

import (
	"errors"
	"fmt"
	"strings"
)

// maxLineLen bounds a WBNET line read from the guest's serial console.
const maxLineLen = 1024

// maxFields bounds the number of key=value pairs on one line.
const maxFields = 16

// Result is one parsed "WBNET key=value ..." line.
type Result struct {
	Step   string
	OK     bool
	Fields map[string]string
}

// ParseResult parses a line the guest printed. ok is false for lines that are not WBNET
// lines at all (ordinary console output). Serial console output is untrusted: lines may
// be truncated, interleaved with kernel messages, or contain arbitrary bytes.
func ParseResult(line string) (r Result, ok bool, err error) {
	line = strings.TrimRight(line, "\r\n")
	i := strings.Index(line, "WBNET ")
	if i < 0 {
		return Result{}, false, nil
	}
	line = line[i+len("WBNET "):]
	if len(line) > maxLineLen {
		return Result{}, true, fmt.Errorf("probe: WBNET line of %d bytes exceeds %d", len(line), maxLineLen)
	}
	r.Fields = make(map[string]string)
	// Split on ASCII space and tab only, the separators quote removes. strings.Fields
	// would also split on Unicode spaces such as U+00A0 that quote leaves in values.
	for _, tok := range strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '\t' }) {
		k, v, found := strings.Cut(tok, "=")
		if !found || k == "" {
			return Result{}, true, fmt.Errorf("probe: malformed field %q", truncate(tok))
		}
		if !validKey(k) {
			return Result{}, true, fmt.Errorf("probe: bad key %q", truncate(k))
		}
		if _, dup := r.Fields[k]; dup {
			return Result{}, true, fmt.Errorf("probe: duplicate key %q", k)
		}
		if len(r.Fields) == maxFields {
			return Result{}, true, errors.New("probe: too many fields")
		}
		r.Fields[k] = v
	}
	r.Step = r.Fields["step"]
	if r.Step == "" {
		return Result{}, true, errors.New("probe: WBNET line without step")
	}
	switch r.Fields["ok"] {
	case "true":
		r.OK = true
	case "false", "":
	default:
		return Result{}, true, fmt.Errorf("probe: ok=%q is not a bool", truncate(r.Fields["ok"]))
	}
	return r, true, nil
}

func validKey(k string) bool {
	if len(k) > 32 {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}
