package dockerctx

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// CredHelper is a credential helper config.json names: "credsStore" for every registry
// (Registry is empty), or a "credHelpers" entry for one registry. The docker CLI runs
// docker-credential-<Name> for it. hyperbutler only reads these keys (REQ-UX-019).
type CredHelper struct {
	Registry string
	Name     string
}

// Program is the helper's program name, as the docker CLI looks it up on PATH.
func (h CredHelper) Program() string { return "docker-credential-" + h.Name }

// maxCredHelpers bounds the credHelpers entries reported.
const maxCredHelpers = 64

// ValidHelperName reports whether name can be a helper's name: something the docker CLI
// would append to "docker-credential-" and look up on PATH, never a path.
func ValidHelperName(name string) bool {
	return name != "" && len(name) <= 64 && strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") == "" &&
		!strings.HasPrefix(name, ".")
}

// CredHelpers reads the credential helpers a config.json names: credsStore first, then
// the credHelpers entries sorted by registry. Empty values, which the docker CLI
// ignores, are left out; a name that could not be a helper's is an error.
func CredHelpers(config []byte) ([]CredHelper, error) {
	m, err := parseConfig(config)
	if err != nil {
		return nil, err
	}
	var out []CredHelper
	if raw, ok := m["credsStore"]; ok {
		var name string
		if err := json.Unmarshal(raw, &name); err != nil {
			return nil, fmt.Errorf("dockerctx: config.json credsStore: %w", err)
		}
		if name != "" {
			if !ValidHelperName(name) {
				return nil, fmt.Errorf("dockerctx: config.json credsStore %q is not a helper name", name)
			}
			out = append(out, CredHelper{Name: name})
		}
	}
	if raw, ok := m["credHelpers"]; ok {
		var helpers map[string]string
		if err := json.Unmarshal(raw, &helpers); err != nil {
			return nil, fmt.Errorf("dockerctx: config.json credHelpers: %w", err)
		}
		if len(helpers) > maxCredHelpers {
			return nil, fmt.Errorf("dockerctx: config.json has %d credHelpers, more than %d", len(helpers), maxCredHelpers)
		}
		regs := make([]string, 0, len(helpers))
		for r := range helpers {
			regs = append(regs, r)
		}
		sort.Strings(regs)
		for _, r := range regs {
			name := helpers[r]
			if name == "" {
				continue
			}
			if !ValidHelperName(name) {
				return nil, fmt.Errorf("dockerctx: config.json credHelpers[%q] %q is not a helper name", r, name)
			}
			out = append(out, CredHelper{Registry: r, Name: name})
		}
	}
	return out, nil
}

// CredHelpers reads the credential helpers of the store's config.json (none when it
// does not exist).
func (s Store) CredHelpers() ([]CredHelper, error) {
	b, err := readBounded(s.configPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return CredHelpers(b)
}

// ConfigPath is the store's config.json.
func (s Store) ConfigPath() string { return s.configPath() }
