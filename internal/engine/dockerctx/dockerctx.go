// Package dockerctx registers and selects the docker CLI context that points at hyperbutler
// (REQ-UX-001), by writing the docker CLI's own files the way `docker context create`
// and `docker context use` do. It needs no docker.exe.
//
// The files, under the docker config directory (%DOCKER_CONFIG% or ~/.docker):
//
//   - contexts/meta/<sha256(name) hex>/meta.json: the context, with its endpoint.
//   - config.json: "currentContext" selects one. Every other key (registry
//     credentials among them) keeps its value; the file is re-indented and its keys
//     sorted, as the docker CLI itself does when it saves it.
//
// A context is hyperbutler's own when its metadata carries ManagedKey = true. A context of
// the same name without it was made by someone else and is never overwritten or
// removed.
package dockerctx

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// ManagedKey marks a context created by hyperbutler. The docker CLI keeps unknown metadata
// fields when it rewrites a context.
const ManagedKey = "HyperbutlerManaged"

// ErrNotOurs means a context of that name exists and hyperbutler did not create it.
var ErrNotOurs = errors.New("dockerctx: a context of that name exists and was not created by hyperbutler")

// maxFile bounds the files read here. config.json can hold credentials and
// plugin settings but is small; anything larger is not a docker CLI file.
const maxFile = 1 << 20

// Store is a docker config directory.
type Store struct{ Dir string }

// DefaultStore is %DOCKER_CONFIG%, or .docker in the user's home directory.
func DefaultStore() (Store, error) {
	if d := os.Getenv("DOCKER_CONFIG"); d != "" {
		return Store{Dir: d}, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return Store{}, err
	}
	return Store{Dir: filepath.Join(h, ".docker")}, nil
}

// Context is the subset of a context's meta.json that hyperbutler reads.
type Context struct {
	Name        string
	Host        string
	Description string
	Managed     bool
}

// metaDir is where the docker CLI keeps a context's metadata.
func (s Store) metaDir(name string) string {
	sum := sha256.Sum256([]byte(name))
	return filepath.Join(s.Dir, "contexts", "meta", hex.EncodeToString(sum[:]))
}

func (s Store) metaPath(name string) string { return filepath.Join(s.metaDir(name), "meta.json") }

// Get reads context name. It returns an error wrapping os.ErrNotExist if there is none.
func (s Store) Get(name string) (*Context, error) {
	b, err := readBounded(s.metaPath(name))
	if err != nil {
		return nil, err
	}
	c, err := ParseMeta(b)
	if err != nil {
		return nil, err
	}
	if c.Name != name {
		return nil, fmt.Errorf("dockerctx: %s holds context %q, not %q", s.metaPath(name), c.Name, name)
	}
	return c, nil
}

type metaFile struct {
	Name      string                     `json:"Name"`
	Metadata  map[string]json.RawMessage `json:"Metadata"`
	Endpoints map[string]json.RawMessage `json:"Endpoints"`
}

type dockerEndpoint struct {
	Host          string `json:"Host"`
	SkipTLSVerify bool   `json:"SkipTLSVerify"`
}

// ParseMeta decodes a context's meta.json.
func ParseMeta(b []byte) (*Context, error) {
	if len(b) > maxFile || !utf8.Valid(b) {
		return nil, errors.New("dockerctx: meta.json is too large or not UTF-8")
	}
	var m metaFile
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("dockerctx: meta.json: %w", err)
	}
	if m.Name == "" {
		return nil, errors.New("dockerctx: meta.json has no name")
	}
	c := &Context{Name: m.Name}
	if raw, ok := m.Endpoints["docker"]; ok {
		var ep dockerEndpoint
		if err := json.Unmarshal(raw, &ep); err != nil {
			return nil, fmt.Errorf("dockerctx: docker endpoint: %w", err)
		}
		c.Host = ep.Host
	}
	if raw, ok := m.Metadata["Description"]; ok {
		_ = json.Unmarshal(raw, &c.Description) // not a string: shown as none
	}
	if raw, ok := m.Metadata[ManagedKey]; ok {
		var v bool
		c.Managed = json.Unmarshal(raw, &v) == nil && v
	}
	return c, nil
}

// encodeMeta renders the meta.json of a hyperbutler-managed context.
func encodeMeta(name, host, description string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"Name":      name,
		"Metadata":  map[string]any{"Description": description, ManagedKey: true},
		"Endpoints": map[string]any{"docker": dockerEndpoint{Host: host}},
	})
}

// Ensure creates context name pointing at host with description, or updates it if it is
// hyperbutler's own.
// It returns ErrNotOurs, and changes nothing, if a context of that name exists that
// hyperbutler did not create, or that cannot be read.
func (s Store) Ensure(name, host, description string) error {
	if name == "" || !utf8.ValidString(name) || !utf8.ValidString(host) || !utf8.ValidString(description) {
		return errors.New("dockerctx: context name, host and description must be non-empty UTF-8")
	}
	c, err := s.Get(name)
	switch {
	case err == nil && !c.Managed:
		return ErrNotOurs
	case err == nil && c.Host == host && c.Description == description:
		return nil
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("%w (%w)", ErrNotOurs, err)
	}
	b, err := encodeMeta(name, host, description)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.metaDir(name), 0o700); err != nil {
		return err
	}
	return writeAtomic(s.metaPath(name), b)
}

// Remove deletes context name if it is hyperbutler's own; a missing one is not an error.
func (s Store) Remove(name string) error {
	c, err := s.Get(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !c.Managed {
		return ErrNotOurs
	}
	return os.RemoveAll(s.metaDir(name))
}

func (s Store) configPath() string { return filepath.Join(s.Dir, "config.json") }

// Current returns the selected context ("" means docker's "default").
func (s Store) Current() (string, error) {
	b, err := readBounded(s.configPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return CurrentContext(b)
}

// Use selects context name ("" selects docker's default). Every other key in
// config.json keeps its value.
func (s Store) Use(name string) error {
	b, err := readBounded(s.configPath())
	if errors.Is(err, os.ErrNotExist) {
		b, err = nil, nil
	}
	if err != nil {
		return err
	}
	out, err := SetCurrentContext(b, name)
	if err != nil {
		return err
	}
	if bytes.Equal(out, b) {
		return nil
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	return writeAtomic(s.configPath(), out)
}

// CurrentContext reads "currentContext" from a config.json.
func CurrentContext(config []byte) (string, error) {
	m, err := parseConfig(config)
	if err != nil {
		return "", err
	}
	raw, ok := m["currentContext"]
	if !ok {
		return "", nil
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil {
		return "", fmt.Errorf("dockerctx: config.json currentContext: %w", err)
	}
	return name, nil
}

// SetCurrentContext returns config with "currentContext" set to name (removed if name
// is empty). Other keys keep their raw values. An empty config starts a new one.
func SetCurrentContext(config []byte, name string) ([]byte, error) {
	if !utf8.ValidString(name) {
		// JSON cannot carry it: it would be stored as U+FFFD, another name.
		return nil, fmt.Errorf("dockerctx: context name %q is not UTF-8", name)
	}
	m, err := parseConfig(config)
	if err != nil {
		return nil, err
	}
	if name == "" {
		if _, ok := m["currentContext"]; !ok {
			return config, nil
		}
		delete(m, "currentContext")
	} else {
		v, err := json.Marshal(name)
		if err != nil {
			return nil, err
		}
		if old, ok := m["currentContext"]; ok && bytes.Equal(bytes.TrimSpace(old), v) {
			return config, nil
		}
		m["currentContext"] = v
	}
	return json.MarshalIndent(m, "", "\t")
}

func parseConfig(config []byte) (map[string]json.RawMessage, error) {
	if len(config) > maxFile || !utf8.Valid(config) {
		return nil, errors.New("dockerctx: config.json is too large or not UTF-8")
	}
	m := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(config)) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(config, &m); err != nil {
		return nil, fmt.Errorf("dockerctx: config.json: %w", err)
	}
	if m == nil { // the document was "null"
		m = map[string]json.RawMessage{}
	}
	return m, nil
}

func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: reading the named Docker CLI file is this function's job; size is bounded below
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFile {
		return nil, fmt.Errorf("dockerctx: %s is larger than %d bytes", path, maxFile)
	}
	return b, nil
}

// writeAtomic replaces path through a temporary file and a rename, so the docker CLI
// never reads half a file.
func writeAtomic(path string, data []byte) error {
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	tmp := path + ".hyperbutler-" + hex.EncodeToString(rnd[:])
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
