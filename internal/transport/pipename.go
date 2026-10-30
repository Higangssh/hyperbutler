package transport

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// DefaultPipeName is the named pipe winbox serves the Docker API on. It is distinct from
// Docker Desktop's \\.\pipe\docker_engine so both can be installed side by side.
const DefaultPipeName = `\\.\pipe\docker_engine_winbox`

const (
	localPipePrefix = `\\.\pipe\`
	// maxPipeNameLen is the documented limit for the whole pipe name, in UTF-16 code
	// units ("The entire pipe name string can be up to 256 characters long").
	maxPipeNameLen = 256
)

// ValidatePipeName reports whether name is a local named pipe path the listener accepts:
// `\\.\pipe\` (any case) followed by a non-empty name with no backslash, slash, NUL, or
// other control character, valid UTF-8, and at most 256 UTF-16 code units in total.
//
// Remote (\\server\pipe\x) and NT-namespace (\\?\..., \Device\...) paths are rejected.
func ValidatePipeName(name string) error {
	if len(name) < len(localPipePrefix) || !strings.EqualFold(name[:len(localPipePrefix)], localPipePrefix) {
		return errors.New(`transport: pipe name must start with \\.\pipe\`)
	}
	rest := name[len(localPipePrefix):]
	if rest == "" {
		return errors.New("transport: pipe name is empty")
	}
	if !utf8.ValidString(name) {
		return errors.New("transport: pipe name is not valid UTF-8")
	}
	units := 0
	for _, r := range name {
		if r >= 0x10000 {
			units += 2
		} else {
			units++
		}
	}
	if units > maxPipeNameLen {
		return errors.New("transport: pipe name is longer than 256 characters")
	}
	for _, r := range rest {
		switch {
		case r == '\\' || r == '/':
			return errors.New("transport: pipe name must not contain a path separator")
		case r < 0x20 || r == 0x7f:
			return errors.New("transport: pipe name must not contain control characters")
		}
	}
	return nil
}
