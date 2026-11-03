// Package vm creates and runs the hyperbutler utility VM through the Host Compute Service
// (HCS, computecore.dll), the primary backend chosen in ADR 0002.
//
// Config describes the VM and renders the HCS schema v2 document; it builds on every
// platform so the document can be tested anywhere. System (Windows only) is a handle to a
// running compute system.
//
// It covers LinuxKernelDirect boot, memory (with the ADR 0002 memory hints) and CPUs,
// SCSI virtual disks, an optional serial console pipe, and the HvSocket service table
// that lets the host connect to guest AF_VSOCK ports.
package vm

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Higangssh/hyperbutler/internal/guid"
)

// DenyAllSDDL is a security descriptor with a protected, empty DACL: nobody gets access.
const DenyAllSDDL = "D:P"

// Config describes a LinuxKernelDirect utility VM.
type Config struct {
	// Owner is shown by hcsdiag and in HCS events.
	Owner string
	// KernelPath and InitrdPath are absolute paths on the host.
	KernelPath string
	InitrdPath string
	// KernelCmdLine is passed to the kernel as is.
	KernelCmdLine string
	// MemoryMB is the guest memory size. AllowOvercommit is set, so guest RAM is backed
	// by host virtual memory (and the pagefile) instead of being reserved up front.
	MemoryMB int
	// ProcessorCount is the number of vCPUs.
	ProcessorCount int
	// ConsolePipe, if set, is the named pipe HCS serves COM1 on.
	ConsolePipe string
	// MemoryHints turns on the ADR 0002 memory settings: EnableDeferredCommit,
	// EnableHotHint and EnableColdDiscardHint. HCS rejects the cold discard hint without
	// deferred commit, so they are one switch.
	MemoryHints bool
	// Disks are attached to SCSI controller 0 in order: Disks[0] is LUN 0 (/dev/sda in a
	// guest with one controller).
	Disks []Disk
	// Services are the hvsock services (typically AF_VSOCK ports, see
	// transport.VsockServiceID) that host processes may connect to in the guest, or bind
	// on the host to accept connections from the guest. Every other service is closed in
	// both directions.
	Services []Service
	// Plan9 are HCS Plan9 file shares served by the VM worker process over hvsock
	// (option (a) in docs/research/bind-mounts.md). The guest reaches each one by
	// connecting to AF_VSOCK port Port on the host and mounting 9p with trans=fd.
	// hyperbutler bind mounts do not use them (ADR 0007); they exist for measurement.
	Plan9 []Plan9Share
}

// Plan9Share is one HCS Plan9 share.
type Plan9Share struct {
	// Name identifies the share in the VM document. AccessName is the 9P aname the
	// guest attaches with.
	Name, AccessName string
	// Path is the absolute host directory. The VM worker must have been granted access
	// to it (GrantVMAccess).
	Path string
	// Port is the AF_VSOCK port the VM worker listens on (LCOW uses 564).
	Port uint32
	// ReadOnly exports the share read-only.
	ReadOnly bool
	// Flags are HCS Plan9 share flags (Plan9Flag*). ReadOnly adds Plan9FlagReadOnly.
	Flags uint32
}

// HCS Plan9 share flags, as hcsshim's internal/uvm/plan9.go defines them.
const (
	Plan9FlagReadOnly      = 0x00000001
	Plan9FlagLinuxMetadata = 0x00000004
	Plan9FlagCaseSensitive = 0x00000008
)

// maxPlan9Shares bounds the share list; HCS documents no limit.
const maxPlan9Shares = 64

// Disk is a virtual disk file on the host.
type Disk struct {
	// Path is the absolute path of a VHDX file. The VM worker process must have been
	// granted access to it (GrantVMAccess).
	Path string
	// ReadOnly attaches the disk read-only.
	ReadOnly bool
	// Sparse must be set when the file is an NTFS sparse file: vhdmp refuses to open one
	// otherwise. It sets AlwaysAllowSparseFiles (ADR 0002).
	Sparse bool
}

// maxDisks bounds the SCSI attachments: one controller has 64 LUNs.
const maxDisks = 64

// Service grants access to one hvsock service. An empty descriptor denies everyone.
type Service struct {
	ID guid.GUID
	// ConnectSDDL says which host principals may connect to the guest's service, for
	// example UserOnlySDDL.
	ConnectSDDL string
	// BindSDDL says which host principals may listen on the service for connections
	// from the guest (for example the user-mode network stack of ADR 0006).
	BindSDDL string
}

const (
	minMemoryMB = 256
	maxMemoryMB = 1 << 20
	maxCPUs     = 1024
)

// Validate reports the first problem with c.
func (c *Config) Validate() error {
	if !filepath.IsAbs(c.KernelPath) || !filepath.IsAbs(c.InitrdPath) {
		return errors.New("vm: kernel and initrd paths must be absolute")
	}
	if c.MemoryMB < minMemoryMB || c.MemoryMB > maxMemoryMB {
		return fmt.Errorf("vm: memory %d MB is outside %d..%d", c.MemoryMB, minMemoryMB, maxMemoryMB)
	}
	if c.ProcessorCount < 1 || c.ProcessorCount > maxCPUs {
		return fmt.Errorf("vm: processor count %d is outside 1..%d", c.ProcessorCount, maxCPUs)
	}
	if len(c.Disks) > maxDisks {
		return fmt.Errorf("vm: %d disks, at most %d", len(c.Disks), maxDisks)
	}
	paths := make(map[string]bool, len(c.Disks))
	for _, d := range c.Disks {
		if !filepath.IsAbs(d.Path) {
			return fmt.Errorf("vm: disk path %q is not absolute", d.Path)
		}
		k := strings.ToLower(filepath.Clean(d.Path))
		if paths[k] {
			return fmt.Errorf("vm: disk %q is attached twice", d.Path)
		}
		paths[k] = true
	}
	if len(c.Plan9) > maxPlan9Shares {
		return fmt.Errorf("vm: %d Plan9 shares, at most %d", len(c.Plan9), maxPlan9Shares)
	}
	shareNames := make(map[string]bool, len(c.Plan9))
	for _, s := range c.Plan9 {
		if s.Name == "" || !filepath.IsAbs(s.Path) || s.Port == 0 {
			return fmt.Errorf("vm: Plan9 share %q needs a name, an absolute path and a port", s.Name)
		}
		if shareNames[s.Name] {
			return fmt.Errorf("vm: Plan9 share %q is listed twice", s.Name)
		}
		shareNames[s.Name] = true
	}
	seen := make(map[guid.GUID]bool, len(c.Services))
	for _, s := range c.Services {
		if s.ConnectSDDL == "" && s.BindSDDL == "" {
			return fmt.Errorf("vm: service %s has no connect or bind security descriptor", s.ID)
		}
		if seen[s.ID] {
			return fmt.Errorf("vm: service %s is listed twice", s.ID)
		}
		seen[s.ID] = true
	}
	return nil
}

func orDenyAll(sddl string) string {
	if sddl == "" {
		return DenyAllSDDL
	}
	return sddl
}

// Document renders the HCS schema 2.1 compute system document.
func (c *Config) Document() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	table := make(map[string]hvSocketService, len(c.Services))
	for _, s := range c.Services {
		table[s.ID.String()] = hvSocketService{
			BindSecurityDescriptor:    orDenyAll(s.BindSDDL),
			ConnectSecurityDescriptor: orDenyAll(s.ConnectSDDL),
		}
	}
	doc := document{
		SchemaVersion:                     schemaVersion{Major: 2, Minor: 1},
		Owner:                             c.Owner,
		ShouldTerminateOnLastHandleClosed: true,
		VirtualMachine: virtualMachine{
			StopOnReset: true,
			Chipset: chipset{LinuxKernelDirect: linuxKernelDirect{
				KernelFilePath: c.KernelPath,
				InitRdPath:     c.InitrdPath,
				KernelCmdLine:  c.KernelCmdLine,
			}},
			ComputeTopology: computeTopology{
				Memory: memory{
					SizeInMB: c.MemoryMB, AllowOvercommit: true,
					EnableDeferredCommit: c.MemoryHints, EnableHotHint: c.MemoryHints, EnableColdDiscardHint: c.MemoryHints,
				},
				Processor: processor{Count: c.ProcessorCount},
			},
			Devices: devices{
				HvSocket: &hvSocket{HvSocketConfig: hvSocketSystemConfig{
					DefaultBindSecurityDescriptor:    DenyAllSDDL,
					DefaultConnectSecurityDescriptor: DenyAllSDDL,
					ServiceTable:                     table,
				}},
			},
		},
	}
	if len(c.Disks) > 0 {
		att := make(map[string]attachment, len(c.Disks))
		for i, d := range c.Disks {
			att[strconv.Itoa(i)] = attachment{Type: "VirtualDisk", Path: d.Path, ReadOnly: d.ReadOnly, AlwaysAllowSparseFiles: d.Sparse}
		}
		doc.VirtualMachine.Devices.Scsi = map[string]scsi{"0": {Attachments: att}}
	}
	if len(c.Plan9) > 0 {
		p9 := &plan9{}
		for _, s := range c.Plan9 {
			flags := s.Flags
			if s.ReadOnly {
				flags |= Plan9FlagReadOnly
			}
			p9.Shares = append(p9.Shares, plan9Share{Name: s.Name, AccessName: s.AccessName, Path: s.Path, Port: s.Port, Flags: flags, ReadOnly: s.ReadOnly})
		}
		doc.VirtualMachine.Devices.Plan9 = p9
	}
	if c.ConsolePipe != "" {
		doc.VirtualMachine.Devices.ComPorts = map[string]comPort{"0": {NamedPipe: c.ConsolePipe}}
	}
	return json.Marshal(doc)
}

// UserOnlySDDL returns a security descriptor that grants full access to the account sid
// (a string SID such as "S-1-5-21-...") and to no one else, not even SYSTEM or
// Administrators. sid is checked so it cannot inject further SDDL clauses.
func UserOnlySDDL(sid string) (string, error) {
	if !validSID(sid) {
		return "", fmt.Errorf("vm: %q is not a string SID", sid)
	}
	return "D:P(A;;FA;;;" + sid + ")", nil
}

// maxSIDLen is far above the longest valid SID (S-1-<48-bit>-<15 x 32-bit>, 184 chars).
const maxSIDLen = 256

// validSID accepts "S-1-" followed by dash-separated decimal numbers, at least one.
func validSID(s string) bool {
	if len(s) > maxSIDLen || len(s) < 5 || s[:4] != "S-1-" {
		return false
	}
	digits := 0
	for i := 4; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			digits++
		case c == '-' && digits > 0:
			digits = 0
		default:
			return false
		}
	}
	return digits > 0
}

// HCS schema v2 subset. Field names are the schema's.
type document struct {
	SchemaVersion                     schemaVersion
	Owner                             string `json:",omitempty"`
	ShouldTerminateOnLastHandleClosed bool
	VirtualMachine                    virtualMachine
}

type schemaVersion struct{ Major, Minor int }

type virtualMachine struct {
	StopOnReset     bool
	Chipset         chipset
	ComputeTopology computeTopology
	Devices         devices
}

type chipset struct{ LinuxKernelDirect linuxKernelDirect }

type linuxKernelDirect struct {
	KernelFilePath string
	InitRdPath     string
	KernelCmdLine  string
}

type computeTopology struct {
	Memory    memory
	Processor processor
}

type memory struct {
	SizeInMB              int
	AllowOvercommit       bool
	EnableDeferredCommit  bool `json:",omitempty"`
	EnableHotHint         bool `json:",omitempty"`
	EnableColdDiscardHint bool `json:",omitempty"`
}

type processor struct{ Count int }

type devices struct {
	Scsi     map[string]scsi    `json:",omitempty"`
	ComPorts map[string]comPort `json:",omitempty"`
	HvSocket *hvSocket          `json:",omitempty"`
	Plan9    *plan9             `json:",omitempty"`
}

type plan9 struct {
	Shares []plan9Share
}

type plan9Share struct {
	Name       string
	AccessName string `json:",omitempty"`
	Path       string
	Port       uint32
	Flags      uint32 `json:",omitempty"`
	ReadOnly   bool   `json:",omitempty"`
}

type comPort struct{ NamedPipe string }

type scsi struct {
	Attachments map[string]attachment
}

type attachment struct {
	Type                   string
	Path                   string
	ReadOnly               bool `json:",omitempty"`
	AlwaysAllowSparseFiles bool `json:",omitempty"`
}

type hvSocket struct {
	HvSocketConfig hvSocketSystemConfig
}

type hvSocketSystemConfig struct {
	DefaultBindSecurityDescriptor    string
	DefaultConnectSecurityDescriptor string
	ServiceTable                     map[string]hvSocketService
}

type hvSocketService struct {
	BindSecurityDescriptor    string
	ConnectSecurityDescriptor string
}
