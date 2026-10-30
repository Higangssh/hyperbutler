// Package vm creates and runs the winbox utility VM through the Host Compute Service
// (HCS, computecore.dll), the primary backend chosen in ADR 0002.
//
// Config describes the VM and renders the HCS schema v2 document; it builds on every
// platform so the document can be tested anywhere. System (Windows only) is a handle to a
// running compute system.
//
// Only what the hvsock milestone needs is here: LinuxKernelDirect boot, memory and CPUs,
// an optional serial console pipe, and the HvSocket service table that lets the host
// connect to guest AF_VSOCK ports. Disks and networking come later.
package vm

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/Microsoft/go-winio/pkg/guid"
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
	// Services are the guest hvsock services (typically AF_VSOCK ports, see
	// transport.VsockServiceID) that host processes may connect to. Every other service
	// is closed to host connections, and no host process may bind (listen for
	// connections from the guest).
	Services []Service
}

// Service grants connect access to one guest hvsock service.
type Service struct {
	ID guid.GUID
	// ConnectSDDL says which host principals may connect, for example UserOnlySDDL.
	ConnectSDDL string
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
	seen := make(map[guid.GUID]bool, len(c.Services))
	for _, s := range c.Services {
		if s.ConnectSDDL == "" {
			return fmt.Errorf("vm: service %s has no connect security descriptor", s.ID)
		}
		if seen[s.ID] {
			return fmt.Errorf("vm: service %s is listed twice", s.ID)
		}
		seen[s.ID] = true
	}
	return nil
}

// Document renders the HCS schema 2.1 compute system document.
func (c *Config) Document() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	table := make(map[string]hvSocketService, len(c.Services))
	for _, s := range c.Services {
		table[s.ID.String()] = hvSocketService{
			BindSecurityDescriptor:    DenyAllSDDL,
			ConnectSecurityDescriptor: s.ConnectSDDL,
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
				Memory:    memory{SizeInMB: c.MemoryMB, AllowOvercommit: true},
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
	SizeInMB        int
	AllowOvercommit bool
}

type processor struct{ Count int }

type devices struct {
	ComPorts map[string]comPort `json:",omitempty"`
	HvSocket *hvSocket          `json:",omitempty"`
}

type comPort struct{ NamedPipe string }

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
