package engine

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Higangssh/hyperbutler/internal/diag"
	"github.com/Higangssh/hyperbutler/internal/netstack"
	"github.com/Higangssh/hyperbutler/internal/transport"
)

// The state directory's JSON files. Each parser is strict: one JSON object of the
// current version, no unknown fields, no trailing data, every field in range. A file
// that fails to parse is reported, never guessed at. They are fuzzed in files_test.go.

// maxFileSize bounds every state file read.
const maxFileSize = 64 << 10

// Phase is where the engine is in its life.
type Phase string

const (
	PhaseStarting Phase = "starting" // booting; the Docker API is not answering yet
	PhaseRunning  Phase = "running"  // dockerd answers on the pipe
	PhaseStopping Phase = "stopping" // shutting the guest down
	PhaseStopped  Phase = "stopped"  // exited cleanly
	PhaseFailed   Phase = "failed"   // exited with Error
)

func (p Phase) valid() bool {
	switch p {
	case PhaseStarting, PhaseRunning, PhaseStopping, PhaseStopped, PhaseFailed:
		return true
	}
	return false
}

// Live reports whether an engine in this phase still has (or is about to have) a VM.
func (p Phase) Live() bool { return p == PhaseStarting || p == PhaseRunning || p == PhaseStopping }

// Error codes in State.ErrorCode, for the CLI to explain.
const (
	CodeHCSAccessDenied = "hcs-access-denied" // not in Hyper-V Administrators (ADR 0004)
	CodePipeInUse       = "pipe-in-use"       // another program serves the Docker pipe
	CodeAlreadyRunning  = "already-running"
	CodeVMExited        = "vm-exited" // the guest powered off or crashed on its own
	CodeDataDisk        = "data-disk"
)

// State is what the engine publishes about itself in state.json.
type State struct {
	Version int   `json:"version"`
	Phase   Phase `json:"phase"`
	// PID and PIDCreated identify the engine process. PIDCreated is its creation time
	// (FILETIME, 100 ns units), so a reused PID is never taken for the engine.
	PID        int       `json:"pid"`
	PIDCreated int64     `json:"pidCreated"`
	VMID       string    `json:"vmId,omitempty"`
	RuntimeID  string    `json:"runtimeId,omitempty"`
	Pipe       string    `json:"pipe"`
	DataDir    string    `json:"dataDir"`
	Started    time.Time `json:"started"`
	// ReadyMillis is the time from Started until dockerd answered, 0 before that.
	ReadyMillis int64     `json:"readyMillis,omitempty"`
	Updated     time.Time `json:"updated"`
	Error       string    `json:"error,omitempty"`
	ErrorCode   string    `json:"errorCode,omitempty"`
	// Daemon is the guest's daemon.json delivery (REQ-UX-007), once there was one.
	Daemon *DaemonState `json:"daemonConfig,omitempty"`
	// ImageDir is the directory of the guest image the engine runs, so `hyperbutler
	// restart` can start the same one.
	ImageDir string `json:"imageDir,omitempty"`
	// HostGateway is the host gateway setting in effect (REQ-NET-004), once the guest
	// network is up.
	HostGateway *HostGatewayState `json:"hostGateway,omitempty"`
}

// HostGatewayState is the host gateway setting the running engine applies.
type HostGatewayState struct {
	// Address is the guest-side address host.docker.internal resolves to.
	Address string `json:"address"`
	Enabled bool   `json:"enabled"`
	// Ports is the forwarded port list in canonical form ("all", "80,8000-8100").
	Ports string `json:"ports"`
	// Reloads counts the settings reloads the engine applied (`hyperbutler config set
	// hostGateway.*` waits for it to grow); Error is the last reload's failure.
	Reloads uint64 `json:"reloads"`
	Error   string `json:"error,omitempty"`
}

// Validate reports the first field out of range.
func (h *HostGatewayState) Validate() error {
	if a, err := netip.ParseAddr(h.Address); err != nil || !a.Is4() {
		return fmt.Errorf("host gateway: bad address %q", h.Address)
	}
	if _, err := netstack.ParsePortList(h.Ports); err != nil {
		return fmt.Errorf("host gateway: %w", err)
	}
	if len(h.Error) > maxErrorLen || !utf8.ValidString(h.Error) {
		return errors.New("host gateway: error text too long or not UTF-8")
	}
	return nil
}

const stateVersion = 1

const (
	maxErrorLen = 4096
	maxVMIDLen  = 64
	maxPID      = 1 << 31
)

// Validate reports the first field out of range.
func (s *State) Validate() error {
	switch {
	case s.Version != stateVersion:
		return fmt.Errorf("state: version %d, want %d", s.Version, stateVersion)
	case !s.Phase.valid():
		return fmt.Errorf("state: unknown phase %q", s.Phase)
	case s.PID < 0 || s.PID >= maxPID:
		return fmt.Errorf("state: pid %d out of range", s.PID)
	case s.PIDCreated < 0:
		return errors.New("state: negative process creation time")
	case s.ReadyMillis < 0:
		return errors.New("state: negative ready time")
	case len(s.Error) > maxErrorLen || !utf8.ValidString(s.Error):
		return errors.New("state: error text too long or not UTF-8")
	case !validToken(s.ErrorCode, 64, false):
		return fmt.Errorf("state: bad error code %q", s.ErrorCode)
	case s.VMID != "" && (!validToken(s.VMID, maxVMIDLen, true) || !strings.HasPrefix(s.VMID, "hyperbutler-")):
		return fmt.Errorf("state: bad VM id %q", s.VMID)
	case s.RuntimeID != "" && !validToken(s.RuntimeID, 40, true):
		return fmt.Errorf("state: bad runtime id %q", s.RuntimeID)
	case s.DataDir != "" && !filepath.IsAbs(s.DataDir):
		return fmt.Errorf("state: data dir %q is not absolute", s.DataDir)
	case s.ImageDir != "" && (!filepath.IsAbs(s.ImageDir) || len(s.ImageDir) > 1024 || !utf8.ValidString(s.ImageDir) || strings.ContainsRune(s.ImageDir, 0)):
		return fmt.Errorf("state: bad image dir %q", s.ImageDir)
	}
	if s.Pipe != "" {
		if err := transport.ValidatePipeName(s.Pipe); err != nil {
			return fmt.Errorf("state: %w", err)
		}
	}
	if s.Phase.Live() && s.PID == 0 {
		return errors.New("state: a live phase without a pid")
	}
	if s.Daemon != nil {
		if err := s.Daemon.Validate(); err != nil {
			return fmt.Errorf("state: %w", err)
		}
	}
	if s.HostGateway != nil {
		if err := s.HostGateway.Validate(); err != nil {
			return fmt.Errorf("state: %w", err)
		}
	}
	return nil
}

// validToken accepts lowercase letters, digits and '-', and with digitsFirst also a
// leading digit; empty is valid.
func validToken(s string, maxLen int, digitsFirst bool) bool {
	if len(s) > maxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c == '-':
		case c >= '0' && c <= '9':
			if i == 0 && !digitsFirst {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// ParseState decodes and validates state.json.
func ParseState(b []byte) (*State, error) {
	var s State
	if err := decodeStrict(b, &s); err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// Settings are the user's choices, kept in settings.json. `hyperbutler config` reads and
// changes them (REQ-UX-006); internal/config describes each key.
type Settings struct {
	Version int `json:"version"`
	// DataDir holds the data disk (REQ-DISK-007). DataDir and DiskSize are recorded when
	// settings.json is created and pin the data disk: they change only while there is
	// no data disk yet.
	DataDir string `json:"dataDir"`
	// DiskSize is the data disk's virtual size in bytes, used when it is created.
	DiskSize uint64 `json:"diskSizeBytes"`
	// MemoryMB and CPUs size the VM; 0 means the default (Memory, CPUCount).
	MemoryMB int `json:"memoryMB,omitempty"`
	CPUs     int `json:"cpus,omitempty"`
	// Pipe is the Docker API pipe; empty means transport.DefaultPipeName. Another name
	// lets two installations (or a development harness) run side by side.
	Pipe string `json:"pipe,omitempty"`
	// MemoryLimit is the engine process's Go soft memory limit in GOMEMLIMIT syntax
	// ("256MiB", "off"). Empty means DefaultEngineMemoryLimit. The GOMEMLIMIT
	// environment variable overrides it (ADR 0003, guardrail 4).
	MemoryLimit string `json:"memoryLimit,omitempty"`
	// ManageContext says whether `hyperbutler start` creates and selects the
	// "hyperbutler" docker context; nil means yes.
	ManageContext *bool `json:"manageContext,omitempty"`
	// HostGatewayEnabled says whether containers reach this host's loopback as
	// host.docker.internal (REQ-NET-004); nil means yes, as in Docker Desktop.
	HostGatewayEnabled *bool `json:"hostGatewayEnabled,omitempty"`
	// HostGatewayPorts limits the host gateway to these ports (netstack.ParsePortList
	// syntax, "5432,8000-8100"); empty means every port.
	HostGatewayPorts string `json:"hostGatewayPorts,omitempty"`
	// PreviousContext is the docker context that was selected when `hyperbutler start`
	// first selected its own ("default" for docker's default), so `hyperbutler
	// uninstall` can select it again (REQ-UX-010). Not a user setting.
	PreviousContext string `json:"previousDockerContext,omitempty"`
}

// HostGateway is the host gateway setting of s. The ports were checked by Validate.
func (s *Settings) HostGateway() netstack.HostGateway {
	ports, _ := netstack.ParsePortList(s.HostGatewayPorts)
	return netstack.HostGateway{Enabled: s.HostGatewayEnabled == nil || *s.HostGatewayEnabled, Ports: ports}
}

// Defaults of Settings values.
const (
	DefaultDiskSize = 64 << 30
	DefaultMemoryMB = 4096
)

// DefaultCPUs is the CPU count of a VM whose settings name none: half the host's
// logical CPUs, from 2 to 8, and never more than the host has.
func DefaultCPUs(hostCPUs int) int {
	n := min(max(hostCPUs/2, 2), 8)
	if hostCPUs > 0 {
		n = min(n, hostCPUs)
	}
	return n
}

// Memory is the guest memory in MiB.
func (s *Settings) Memory() int {
	if s.MemoryMB == 0 {
		return DefaultMemoryMB
	}
	return s.MemoryMB
}

// CPUCount is the VM's CPU count on a host with hostCPUs logical CPUs.
func (s *Settings) CPUCount(hostCPUs int) int {
	if s.CPUs == 0 {
		return DefaultCPUs(hostCPUs)
	}
	return s.CPUs
}

// ContextManaged reports whether start manages the docker context.
func (s *Settings) ContextManaged() bool { return s.ManageContext == nil || *s.ManageContext }

// DefaultEngineMemoryLimit is the engine's Go soft memory limit when settings.json sets
// none. The engine's live heap is a few MiB (ADR 0003), so the limit acts only on
// runaway growth: the GC works harder long before the host would page.
const DefaultEngineMemoryLimit = "256MiB"

// EngineMemoryLimit is the memory limit the engine process applies.
func (s *Settings) EngineMemoryLimit() string {
	if s.MemoryLimit == "" {
		return DefaultEngineMemoryLimit
	}
	return s.MemoryLimit
}

// ValidateMemoryLimit checks an engine memory limit in GOMEMLIMIT syntax: "off", or a
// byte count of at least diag.MinMemoryLimit.
func ValidateMemoryLimit(v string) error {
	n, err := diag.ParseMemoryLimit(v)
	if err != nil {
		return err
	}
	if n < diag.MinMemoryLimit {
		return fmt.Errorf("memory limit %q is below %d MiB", v, diag.MinMemoryLimit>>20)
	}
	return nil
}

// SettingsVersion is the current settings.json version.
const SettingsVersion = 1

// Bounds of Settings values.
const (
	MinDiskSize = 4 << 30
	MaxDiskSize = 16 << 40 // well below the VHDX limit of 64 TiB
	MinMemoryMB = 1024     // dockerd does not start in 512 MB
	MaxMemoryMB = 1 << 20
	MaxCPUs     = 256
)

// Validate reports the first field out of range.
func (s *Settings) Validate() error {
	switch {
	case s.Version != SettingsVersion:
		return fmt.Errorf("settings: version %d, want %d", s.Version, SettingsVersion)
	case !filepath.IsAbs(s.DataDir) || len(s.DataDir) > 1024 || !utf8.ValidString(s.DataDir) || strings.ContainsRune(s.DataDir, 0):
		return fmt.Errorf("settings: data dir %q must be an absolute path", s.DataDir)
	case s.DiskSize < MinDiskSize || s.DiskSize > MaxDiskSize || s.DiskSize%(1<<20) != 0:
		return fmt.Errorf("settings: disk size %d must be a multiple of 1 MiB from %s to %s", s.DiskSize, FormatSize(MinDiskSize), FormatSize(MaxDiskSize))
	case s.MemoryMB != 0 && (s.MemoryMB < MinMemoryMB || s.MemoryMB > MaxMemoryMB):
		return fmt.Errorf("settings: memory %d MiB is outside %d..%d", s.MemoryMB, MinMemoryMB, MaxMemoryMB)
	case s.CPUs < 0 || s.CPUs > MaxCPUs:
		return fmt.Errorf("settings: %d CPUs is outside 1..%d", s.CPUs, MaxCPUs)
	}
	if s.Pipe != "" {
		if err := transport.ValidatePipeName(s.Pipe); err != nil {
			return fmt.Errorf("settings: %w", err)
		}
	}
	if s.MemoryLimit != "" {
		if err := ValidateMemoryLimit(s.MemoryLimit); err != nil {
			return fmt.Errorf("settings: %w", err)
		}
	}
	if _, err := netstack.ParsePortList(s.HostGatewayPorts); err != nil {
		return fmt.Errorf("settings: host gateway ports: %w", err)
	}
	if len(s.PreviousContext) > 256 || !utf8.ValidString(s.PreviousContext) || strings.ContainsRune(s.PreviousContext, 0) {
		return errors.New("settings: previous docker context name too long or not UTF-8")
	}
	return nil
}

// PipeName is the Docker API pipe of these settings.
func (s *Settings) PipeName() string {
	if s.Pipe == "" {
		return transport.DefaultPipeName
	}
	return s.Pipe
}

// ParseSettings decodes and validates settings.json.
func ParseSettings(b []byte) (*Settings, error) {
	var s Settings
	if err := decodeStrict(b, &s); err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// DiskMeta describes the data disk, in disk.json next to it.
type DiskMeta struct {
	Version   int    `json:"version"`
	SizeBytes uint64 `json:"sizeBytes"`
	BlockSize uint32 `json:"blockSize"`
	// MetadataBytes is the VHDX's host allocation right after it was created, before the
	// guest wrote anything: the free-space guard's stand-in for the VHDX metadata size
	// (overstating it is safe, see internal/spaceguard).
	MetadataBytes uint64    `json:"metadataBytes"`
	Created       time.Time `json:"created"`
}

const diskMetaVersion = 1

// Validate reports the first field out of range.
func (d *DiskMeta) Validate() error {
	switch {
	case d.Version != diskMetaVersion:
		return fmt.Errorf("disk: version %d, want %d", d.Version, diskMetaVersion)
	case d.SizeBytes < MinDiskSize || d.SizeBytes > MaxDiskSize || d.SizeBytes%(1<<20) != 0:
		return fmt.Errorf("disk: size %d out of range", d.SizeBytes)
	case d.BlockSize != 1<<20:
		return fmt.Errorf("disk: block size %d, want 1 MiB", d.BlockSize)
	case d.MetadataBytes > 1<<30:
		return fmt.Errorf("disk: metadata size %d out of range", d.MetadataBytes)
	}
	return nil
}

// ParseDiskMeta decodes and validates disk.json.
func ParseDiskMeta(b []byte) (*DiskMeta, error) {
	var d DiskMeta
	if err := decodeStrict(b, &d); err != nil {
		return nil, fmt.Errorf("disk: %w", err)
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return &d, nil
}

// decodeStrict decodes exactly one JSON object with known fields only.
func decodeStrict(b []byte, v any) error {
	if len(b) > maxFileSize {
		return errors.New("file too large")
	}
	if !utf8.Valid(b) {
		return errors.New("not UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON object")
	}
	return nil
}

// encode renders v as indented JSON with a trailing newline.
func encode(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// WriteFileAtomic replaces path with data: it writes a temporary file in the same
// directory, flushes it, and renames it over path, so a reader or a crash sees either
// the old or the new content. A rename that collides with a reader is retried.
func WriteFileAtomic(path string, data []byte) error {
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	tmp := path + ".tmp-" + hex.EncodeToString(rnd[:])
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: a new file next to the caller's path, created exclusively
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = retrySharing(func() error { return os.Rename(tmp, path) })
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// ReadFileBounded reads at most maxFileSize bytes of path, retrying sharing
// violations (an atomic rename in progress).
func ReadFileBounded(path string) ([]byte, error) {
	var b []byte
	err := retrySharing(func() error {
		f, err := os.Open(path) //nolint:gosec // G304: reading the caller's file is this function's job; size is bounded below
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }() // read-only
		b, err = io.ReadAll(io.LimitReader(f, maxFileSize+1))
		if err == nil && len(b) > maxFileSize {
			err = fmt.Errorf("%s is larger than %d bytes", path, maxFileSize)
		}
		return err
	})
	return b, err
}

func retrySharing(fn func() error) error {
	var err error
	for i := 0; i < 50; i++ {
		if err = fn(); err == nil || !isSharingViolation(err) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return err
}

// LoadState reads state.json. It returns os.ErrNotExist (wrapped) if there is none.
func LoadState(l Layout) (*State, error) {
	b, err := ReadFileBounded(l.StatePath())
	if err != nil {
		return nil, err
	}
	return ParseState(b)
}

// SaveState validates s, stamps Updated and writes state.json.
func SaveState(l Layout, s *State) error {
	s.Version = stateVersion
	s.Updated = time.Now().UTC().Truncate(time.Millisecond)
	if len(s.Error) > maxErrorLen {
		s.Error = s.Error[:maxErrorLen]
	}
	s.Error = strings.ToValidUTF8(s.Error, "?")
	if err := s.Validate(); err != nil {
		return err
	}
	b, err := encode(s)
	if err != nil {
		return err
	}
	return WriteFileAtomic(l.StatePath(), b)
}

// LoadSettings reads settings.json; os.ErrNotExist (wrapped) before the first start.
func LoadSettings(l Layout) (*Settings, error) {
	b, err := ReadFileBounded(l.SettingsPath())
	if err != nil {
		return nil, err
	}
	return ParseSettings(b)
}

// SaveSettings validates s and writes settings.json.
func SaveSettings(l Layout, s *Settings) error {
	s.Version = SettingsVersion
	if err := s.Validate(); err != nil {
		return err
	}
	b, err := encode(s)
	if err != nil {
		return err
	}
	return WriteFileAtomic(l.SettingsPath(), b)
}

// LoadDiskMeta reads disk.json; os.ErrNotExist (wrapped) if there is none.
func LoadDiskMeta(l Layout) (*DiskMeta, error) {
	b, err := ReadFileBounded(l.DiskMetaPath())
	if err != nil {
		return nil, err
	}
	return ParseDiskMeta(b)
}

func saveDiskMeta(l Layout, d *DiskMeta) error {
	d.Version = diskMetaVersion
	if err := d.Validate(); err != nil {
		return err
	}
	b, err := encode(d)
	if err != nil {
		return err
	}
	return WriteFileAtomic(l.DiskMetaPath(), b)
}
