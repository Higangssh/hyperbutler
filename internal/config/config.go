// Package config describes every user setting of hyperbutler (REQ-UX-006): its name,
// type, default, validator and when a change takes effect. `hyperbutler config` and,
// later, the GUI and the MCP server (ADR 0008) use it, so there is one implementation.
//
// The values live in settings.json (engine.Settings). Set and Unset change a copy of the
// settings and validate it; the caller writes it atomically under the ops lock and never
// writes when the current file is invalid.
package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Higangssh/hyperbutler/internal/dockerenv"
	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/netstack"
	"github.com/Higangssh/hyperbutler/internal/transport"
)

// ApplyTime says when a change takes effect.
type ApplyTime string

const (
	// ApplyLive: at once (the next command that uses it); nothing restarts.
	ApplyLive ApplyTime = "live"
	// ApplyDockerd: when dockerd restarts (`config daemon apply`); the VM stays up.
	ApplyDockerd ApplyTime = "restart-dockerd"
	// ApplyVM: when the VM restarts (`hyperbutler restart`).
	ApplyVM ApplyTime = "restart-vm"
	// ApplyFirstStart: when the data disk is created. It cannot change afterwards.
	ApplyFirstStart ApplyTime = "first-start"
)

// Source says where a value came from.
type Source string

const (
	SourceDefault Source = "default"
	SourceFile    Source = "file" // settings.json
	SourceEnv     Source = "env"  // an environment variable overrides the file
)

// Env is the host context a key needs.
type Env struct {
	// Home is the state directory (the default data location).
	Home string
	// HostCPUs is the host's logical CPU count (for the default CPU count).
	HostCPUs int
	// Getenv reads environment variables (nil: none are set).
	Getenv func(string) string
	// DiskExists reports whether a data disk exists in dir (nil: none).
	DiskExists func(dir string) bool
	// ShowSecrets makes Get show secret values (proxy credentials) as they are;
	// otherwise they are redacted (`config list/get --show-secrets`).
	ShowSecrets bool
}

func (e Env) getenv(k string) string {
	if e.Getenv == nil {
		return ""
	}
	return e.Getenv(k)
}

func (e Env) diskExists(dir string) bool { return e.DiskExists != nil && e.DiskExists(dir) }

// Key is one setting.
type Key struct {
	Name string
	// Type is the value's kind: "size", "int", "bool", "path", "pipe", "memlimit" or
	// "ports" (a port list).
	Type        string
	Apply       ApplyTime
	Description string
	// Engine marks an ApplyLive key the running engine uses: `config set` asks it to
	// read settings.json again (engine.RequestSettingsReload).
	Engine bool
	// Secret marks a value that can hold credentials (a proxy URL): Get shows it
	// redacted unless Env.ShowSecrets, and errors never quote it.
	Secret bool

	def   func(Env) string
	get   func(*engine.Settings, Env) (value string, inFile bool)
	set   func(*engine.Settings, string, Env) error
	unset func(*engine.Settings, Env) error
	env   string // an environment variable that overrides the file, if any
}

// ErrUnknownKey and ErrReserved are returned for names that are not settable keys.
var (
	ErrUnknownKey = errors.New("unknown setting")
	ErrReserved   = errors.New("reserved for a later release")
)

// Reserved are names kept for settings that are planned but not implemented
// (docs/research/settings-and-gui.md). Set and Get refuse them with ErrReserved.
var Reserved = []string{
	"proxy.mode",        // REQ-UX-009: the Windows system proxy (WinINET/WinHTTP), 0.2
	"updates.check",     // REQ-UX-005
	"autostart", "tray", // ADR 0008
	"ports.lan",              // REQ-NET
	"buildkit.gcKeepStorage", // REQ-DISK-006
}

// Keys lists the settings, in display order.
var Keys = []*Key{
	{
		Name: "memory", Type: "size", Apply: ApplyVM,
		Description: "guest memory, such as 4GiB or 4096 (MiB)",
		def:         func(Env) string { return engine.FormatSize(engine.DefaultMemoryMB << 20) },
		get: func(s *engine.Settings, _ Env) (string, bool) {
			return engine.FormatSize(uint64(s.Memory()) << 20), s.MemoryMB != 0
		},
		set: func(s *engine.Settings, v string, _ Env) error {
			mb, err := ParseMemoryMB(v)
			s.MemoryMB = mb
			return err
		},
		unset: func(s *engine.Settings, _ Env) error { s.MemoryMB = 0; return nil },
	},
	{
		Name: "cpus", Type: "int", Apply: ApplyVM,
		Description: fmt.Sprintf("virtual CPUs, 1 to %d", engine.MaxCPUs),
		def:         func(e Env) string { return strconv.Itoa(engine.DefaultCPUs(e.HostCPUs)) },
		get: func(s *engine.Settings, e Env) (string, bool) {
			return strconv.Itoa(s.CPUCount(e.HostCPUs)), s.CPUs != 0
		},
		set: func(s *engine.Settings, v string, e Env) error {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			limit := engine.MaxCPUs
			if e.HostCPUs > 0 {
				limit = min(limit, e.HostCPUs) // Hyper-V refuses more than the host has
			}
			if err != nil || n < 1 || n > limit {
				return fmt.Errorf("%q: want a whole number from 1 to %d (this host's logical CPUs)", v, limit)
			}
			s.CPUs = n
			return nil
		},
		unset: func(s *engine.Settings, _ Env) error { s.CPUs = 0; return nil },
	},
	{
		Name: "disk.size", Type: "size", Apply: ApplyFirstStart,
		Description: "data disk size, such as 64GiB (the disk is sparse)",
		def:         func(Env) string { return engine.FormatSize(engine.DefaultDiskSize) },
		get: func(s *engine.Settings, _ Env) (string, bool) {
			return engine.FormatSize(s.DiskSize), true // pinned in every settings.json
		},
		set: func(s *engine.Settings, v string, e Env) error {
			n, err := engine.ParseSize(v)
			if err != nil {
				return err
			}
			if n < engine.MinDiskSize || n > engine.MaxDiskSize || n%(1<<20) != 0 {
				return fmt.Errorf("disk.size %q: want a multiple of 1MiB from %s to %s", v, engine.FormatSize(engine.MinDiskSize), engine.FormatSize(engine.MaxDiskSize))
			}
			if n != s.DiskSize && e.diskExists(s.DataDir) {
				return fmt.Errorf("the data disk already exists with %s; its size is fixed", engine.FormatSize(s.DiskSize))
			}
			s.DiskSize = n
			return nil
		},
	},
	{
		Name: "data.dir", Type: "path", Apply: ApplyFirstStart,
		Description: "where the data disk lives (a local directory)",
		def:         func(e Env) string { return e.Home },
		get: func(s *engine.Settings, e Env) (string, bool) {
			return s.DataDir, true // pinned in every settings.json
		},
		set: func(s *engine.Settings, v string, e Env) error {
			if v == "" || len(v) > 1024 || !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
				return fmt.Errorf("data.dir %q: want a local directory", v)
			}
			abs, err := filepath.Abs(v)
			if err != nil {
				return err
			}
			if strings.HasPrefix(abs, `\\`) || runtime.GOOS == "windows" && !drivePath(abs) {
				return fmt.Errorf("data.dir %s: the data disk must be on a local volume (a path such as D:\\hyperbutler)", v)
			}
			// Windows drops trailing spaces and dots from path elements, so such a name
			// would not be the directory it says.
			for _, el := range strings.FieldsFunc(abs, func(r rune) bool { return r == '\\' || r == '/' }) {
				if strings.HasSuffix(el, " ") || strings.HasSuffix(el, ".") || strings.HasPrefix(el, " ") {
					return fmt.Errorf("data.dir %q: a path element starts or ends with a space or ends with a dot", v)
				}
			}
			if !engine.SamePath(abs, s.DataDir) && e.diskExists(s.DataDir) {
				return fmt.Errorf("the data disk already exists in %s; moving it (hyperbutler move) is not supported yet", s.DataDir)
			}
			s.DataDir = abs
			return nil
		},
	},
	{
		Name: "pipe", Type: "pipe", Apply: ApplyVM,
		Description: `the Docker API pipe, such as \\.\pipe\docker_engine_hyperbutler`,
		def:         func(Env) string { return transport.DefaultPipeName },
		get: func(s *engine.Settings, _ Env) (string, bool) {
			return s.PipeName(), s.Pipe != ""
		},
		set: func(s *engine.Settings, v string, _ Env) error {
			if err := transport.ValidatePipeName(v); err != nil {
				return err
			}
			s.Pipe = v
			if v == transport.DefaultPipeName {
				s.Pipe = ""
			}
			return nil
		},
		unset: func(s *engine.Settings, _ Env) error { s.Pipe = ""; return nil },
	},
	{
		Name: "context.manage", Type: "bool", Apply: ApplyLive,
		Description: `whether hyperbutler start creates and selects the "hyperbutler" docker context`,
		def:         func(Env) string { return "true" },
		get: func(s *engine.Settings, _ Env) (string, bool) {
			return strconv.FormatBool(s.ContextManaged()), s.ManageContext != nil
		},
		set: func(s *engine.Settings, v string, _ Env) error {
			b, err := ParseBool(v)
			if err != nil {
				return err
			}
			s.ManageContext = &b
			return nil
		},
		unset: func(s *engine.Settings, _ Env) error { s.ManageContext = nil; return nil },
	},
	{
		Name: "hostGateway.enabled", Type: "bool", Apply: ApplyLive, Engine: true,
		Description: "whether containers reach services on this host's 127.0.0.1 as host.docker.internal, as in Docker Desktop",
		def:         func(Env) string { return "true" },
		get: func(s *engine.Settings, _ Env) (string, bool) {
			return strconv.FormatBool(s.HostGateway().Enabled), s.HostGatewayEnabled != nil
		},
		set: func(s *engine.Settings, v string, _ Env) error {
			b, err := ParseBool(v)
			if err != nil {
				return err
			}
			s.HostGatewayEnabled = &b
			return nil
		},
		unset: func(s *engine.Settings, _ Env) error { s.HostGatewayEnabled = nil; return nil },
	},
	{
		Name: "hostGateway.ports", Type: "ports", Apply: ApplyLive, Engine: true,
		Description: `the host ports host.docker.internal forwards, such as "5432,8000-8100", or "all"`,
		def:         func(Env) string { return "all" },
		get: func(s *engine.Settings, _ Env) (string, bool) {
			return s.HostGateway().Ports.String(), s.HostGatewayPorts != ""
		},
		set: func(s *engine.Settings, v string, _ Env) error {
			l, err := netstack.ParsePortList(v)
			if err != nil {
				return err
			}
			// Stored in canonical form; every port is the default (no entry).
			s.HostGatewayPorts = ""
			if len(l) > 0 {
				s.HostGatewayPorts = l.String()
			}
			return nil
		},
		unset: func(s *engine.Settings, _ Env) error { s.HostGatewayPorts = ""; return nil },
	},
	{
		Name: "ca.import", Type: "bool", Apply: ApplyDockerd,
		Description: "whether dockerd also trusts the CA certificates of the Windows root stores (a corporate TLS-inspecting proxy's, for example)",
		def:         func(Env) string { return "true" },
		get: func(s *engine.Settings, _ Env) (string, bool) {
			return strconv.FormatBool(s.CAImported()), s.CAImport != nil
		},
		set: func(s *engine.Settings, v string, _ Env) error {
			b, err := ParseBool(v)
			if err != nil {
				return err
			}
			s.CAImport = &b
			return nil
		},
		unset: func(s *engine.Settings, _ Env) error { s.CAImport = nil; return nil },
	},
	{
		Name: "ca.extraFile", Type: "path", Apply: ApplyDockerd,
		Description: "a PEM file of more CA certificates for dockerd to trust, or empty",
		def:         func(Env) string { return "" },
		get: func(s *engine.Settings, _ Env) (string, bool) {
			return s.CAExtraFile, s.CAExtraFile != ""
		},
		set: func(s *engine.Settings, v string, _ Env) error {
			if v == "" {
				s.CAExtraFile = ""
				return nil
			}
			if len(v) > 1024 || !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
				return fmt.Errorf("ca.extraFile %q: want a PEM file", v)
			}
			abs, err := filepath.Abs(v)
			if err != nil {
				return err
			}
			// A network path would be read at every engine start and apply, and could
			// hang it or send credentials to another machine.
			if strings.HasPrefix(abs, `\\`) || runtime.GOOS == "windows" && !drivePath(abs) {
				return fmt.Errorf("ca.extraFile %s: want a file on a local drive", v)
			}
			if _, err := engine.ReadCAFile(abs); err != nil {
				return err
			}
			s.CAExtraFile = abs
			return nil
		},
		unset: func(s *engine.Settings, _ Env) error { s.CAExtraFile = ""; return nil },
	},
	{
		Name: "proxy.http", Type: "url", Apply: ApplyDockerd, Secret: true,
		Description: "dockerd's proxy for http:// (HTTP_PROXY), such as http://proxy.corp:3128; a proxy on this host's 127.0.0.1 (px, cntlm) works too",
		def:         func(Env) string { return "" },
		get:         func(s *engine.Settings, _ Env) (string, bool) { return s.HTTPProxy, s.HTTPProxy != "" },
		set: func(s *engine.Settings, v string, _ Env) error {
			if v != "" {
				if err := dockerenv.ValidateProxyURL(v); err != nil {
					return err
				}
			}
			s.HTTPProxy = v
			return nil
		},
		unset: func(s *engine.Settings, _ Env) error { s.HTTPProxy = ""; return nil },
	},
	{
		Name: "proxy.https", Type: "url", Apply: ApplyDockerd, Secret: true,
		Description: "dockerd's proxy for https:// (HTTPS_PROXY): registries and image pulls; usually the same URL as proxy.http",
		def:         func(Env) string { return "" },
		get:         func(s *engine.Settings, _ Env) (string, bool) { return s.HTTPSProxy, s.HTTPSProxy != "" },
		set: func(s *engine.Settings, v string, _ Env) error {
			if v != "" {
				if err := dockerenv.ValidateProxyURL(v); err != nil {
					return err
				}
			}
			s.HTTPSProxy = v
			return nil
		},
		unset: func(s *engine.Settings, _ Env) error { s.HTTPSProxy = ""; return nil },
	},
	{
		Name: "proxy.noProxy", Type: "hosts", Apply: ApplyDockerd,
		Description: "hosts dockerd reaches without the proxy (NO_PROXY), such as .corp.example,10.0.0.0/8; localhost and the guest network are always added",
		def:         func(Env) string { return "" },
		get:         func(s *engine.Settings, _ Env) (string, bool) { return s.NoProxy, s.NoProxy != "" },
		set: func(s *engine.Settings, v string, _ Env) error {
			if v == "" {
				s.NoProxy = ""
				return nil
			}
			if err := dockerenv.ValidateNoProxy(v); err != nil {
				return err
			}
			s.NoProxy = dockerenv.NormalizeNoProxy(v)
			return nil
		},
		unset: func(s *engine.Settings, _ Env) error { s.NoProxy = ""; return nil },
	},
	{
		Name: "engine.memoryLimit", Type: "memlimit", Apply: ApplyVM,
		Description: `Go soft memory limit of the engine process, such as 256MiB or "off" (GOMEMLIMIT wins)`,
		env:         "GOMEMLIMIT",
		def:         func(Env) string { return engine.DefaultEngineMemoryLimit },
		get: func(s *engine.Settings, _ Env) (string, bool) {
			return s.EngineMemoryLimit(), s.MemoryLimit != ""
		},
		set: func(s *engine.Settings, v string, _ Env) error {
			if err := engine.ValidateMemoryLimit(v); err != nil {
				return err
			}
			s.MemoryLimit = v
			if v == engine.DefaultEngineMemoryLimit {
				s.MemoryLimit = ""
			}
			return nil
		},
		unset: func(s *engine.Settings, _ Env) error { s.MemoryLimit = ""; return nil },
	},
}

func init() {
	// The first-start keys reset to their defaults, which the file then records.
	for _, k := range Keys {
		if k.unset == nil {
			k := k
			k.unset = func(s *engine.Settings, e Env) error { return k.set(s, k.def(e), e) }
		}
	}
}

// Lookup finds a key by name (case-insensitively).
func Lookup(name string) (*Key, error) {
	for _, k := range Keys {
		if strings.EqualFold(k.Name, name) {
			return k, nil
		}
	}
	for _, r := range Reserved {
		if strings.EqualFold(r, name) {
			return nil, fmt.Errorf("%q: %w", r, ErrReserved)
		}
	}
	return nil, fmt.Errorf("%w %q; `hyperbutler config list` shows them", ErrUnknownKey, name)
}

// Item is a key with its current value, for display and --json.
type Item struct {
	Key         string    `json:"key"`
	Value       string    `json:"value"`
	Default     string    `json:"default"`
	Source      Source    `json:"source"`
	Type        string    `json:"type"`
	Apply       ApplyTime `json:"apply"`
	Description string    `json:"description"`
}

// NewSettings returns the settings of an installation without settings.json: the
// defaults, with the data disk's location and size pinned.
func NewSettings(e Env) *engine.Settings {
	return &engine.Settings{Version: engine.SettingsVersion, DataDir: e.Home, DiskSize: engine.DefaultDiskSize}
}

// Get describes k under s (nil: no settings.json yet).
func (k *Key) Get(s *engine.Settings, e Env) Item {
	haveFile := s != nil
	if s == nil {
		s = NewSettings(e)
	}
	it := Item{Key: k.Name, Default: k.def(e), Source: SourceDefault, Type: k.Type, Apply: k.Apply, Description: k.Description}
	v, inFile := k.get(s, e)
	it.Value = v
	if k.Secret && !e.ShowSecrets {
		it.Value = dockerenv.Redact(v)
	}
	if inFile && haveFile {
		it.Source = SourceFile
	}
	if k.env != "" {
		if ev := e.getenv(k.env); ev != "" {
			it.Value, it.Source = ev, SourceEnv
		}
	}
	return it
}

// List describes every key.
func List(s *engine.Settings, e Env) []Item {
	out := make([]Item, 0, len(Keys))
	for _, k := range Keys {
		out = append(out, k.Get(s, e))
	}
	return out
}

// Set returns a copy of s (nil: no settings.json yet) with k set to v, validated.
func (k *Key) Set(s *engine.Settings, v string, e Env) (*engine.Settings, error) {
	cp := copySettings(s, e)
	if err := k.set(cp, v, e); err != nil {
		return nil, fmt.Errorf("%s: %w", k.Name, err)
	}
	if err := k.check(cp); err != nil {
		return nil, fmt.Errorf("%s: %w", k.Name, err)
	}
	return cp, nil
}

// Unset returns a copy of s with k back at its default, validated.
func (k *Key) Unset(s *engine.Settings, e Env) (*engine.Settings, error) {
	cp := copySettings(s, e)
	if err := k.unset(cp, e); err != nil {
		return nil, fmt.Errorf("%s: %w", k.Name, err)
	}
	if err := k.check(cp); err != nil {
		return nil, fmt.Errorf("%s: %w", k.Name, err)
	}
	return cp, nil
}

// check validates settings that k changed: as a whole, and a loopback proxy against the
// host gateway it needs (only for the keys that affect that, so an unrelated change is
// never refused for it).
func (k *Key) check(s *engine.Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if strings.HasPrefix(k.Name, "proxy.") || strings.HasPrefix(k.Name, "hostGateway.") {
		return s.CheckProxyReach()
	}
	return nil
}

func copySettings(s *engine.Settings, e Env) *engine.Settings {
	if s == nil {
		return NewSettings(e)
	}
	cp := *s
	cp.ManageContext = copyBool(s.ManageContext)
	cp.HostGatewayEnabled = copyBool(s.HostGatewayEnabled)
	cp.CAImport = copyBool(s.CAImport)
	cp.Version = engine.SettingsVersion
	return &cp
}

func copyBool(p *bool) *bool {
	if p == nil {
		return nil
	}
	b := *p
	return &b
}

func sameBool(a, b *bool) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// Equal reports whether a and b hold the same values.
func Equal(a, b *engine.Settings) bool {
	if a == nil || b == nil {
		return a == b
	}
	ac, bc := *a, *b
	ac.ManageContext, bc.ManageContext = nil, nil
	ac.HostGatewayEnabled, bc.HostGatewayEnabled = nil, nil
	ac.CAImport, bc.CAImport = nil, nil
	return ac == bc && sameBool(a.ManageContext, b.ManageContext) && sameBool(a.HostGatewayEnabled, b.HostGatewayEnabled) &&
		sameBool(a.CAImport, b.CAImport)
}

// Names returns the key names, sorted, for usage text.
func Names() []string {
	var n []string
	for _, k := range Keys {
		n = append(n, k.Name)
	}
	sort.Strings(n)
	return n
}

// ParseMemoryMB turns a memory value into whole MiB. A bare number means MiB, as in
// most VM tools; a unit is honored ("4GiB", "4096MiB").
func ParseMemoryMB(v string) (int, error) {
	t := strings.TrimSpace(v)
	if t != "" && strings.Trim(t, "0123456789") == "" {
		t += "MiB"
	}
	n, err := engine.ParseSize(t)
	if err != nil {
		return 0, err
	}
	if n%(1<<20) != 0 {
		return 0, fmt.Errorf("memory %q is not a whole number of MiB", v)
	}
	mb := n >> 20
	if mb < engine.MinMemoryMB || mb > engine.MaxMemoryMB {
		return 0, fmt.Errorf("memory %q is outside %d MiB..%d MiB", v, engine.MinMemoryMB, engine.MaxMemoryMB)
	}
	return int(mb), nil
}

// drivePath reports whether p is X:\... with no other colon (no alternate data stream).
func drivePath(p string) bool {
	if len(p) < 3 || p[1] != ':' || (p[2] != '\\' && p[2] != '/') || strings.Count(p, ":") != 1 {
		return false
	}
	c := p[0] | 0x20
	return c >= 'a' && c <= 'z'
}

// ParseBool accepts true/false, yes/no, on/off and 1/0, in any case.
func ParseBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "on", "1":
		return true, nil
	case "false", "no", "off", "0":
		return false, nil
	}
	return false, fmt.Errorf("%q: want true or false", v)
}
