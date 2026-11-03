package hvaddr

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Higangssh/hyperbutler/internal/guid"
	"github.com/Higangssh/hyperbutler/internal/vmsecret"
)

// vsockTemplate is the hvsock service GUID that maps to AF_VSOCK ports in a Linux guest:
// the port number replaces Data1. See the Hyper-V sockets documentation and go-winio's
// VsockServiceID.
var vsockTemplate = guid.GUID{
	Data1: 0x00000000,
	Data2: 0xfacb,
	Data3: 0x11e6,
	Data4: [8]byte{0xbd, 0x58, 0x64, 0x00, 0x6a, 0x79, 0x86, 0xd3},
}

// VsockServiceID returns the hvsock service GUID that a Linux guest sees as AF_VSOCK port
// port.
func VsockServiceID(port uint32) guid.GUID {
	g := vsockTemplate
	g.Data1 = port
	return g
}

// HvsockTarget names an AF_HYPERV endpoint: a VM and a service in it.
type HvsockTarget struct {
	VMID      guid.GUID
	ServiceID guid.GUID
	// Secret, if set, is the VM's secret (ADR 0004 rule 6): HvsockDialer sends it as the
	// first bytes of every connection, which the guest's services require. It never
	// prints, and String leaves it out.
	Secret vmsecret.Secret
}

// String formats t as "<vm-guid>:<service-guid>", which ParseHvsockTarget accepts.
func (t HvsockTarget) String() string {
	return t.VMID.String() + ":" + t.ServiceID.String()
}

// maxHvsockTargetLen bounds the input to ParseHvsockTarget. Two GUIDs with braces and a
// separator fit in 80 bytes, so anything far longer is rejected before any parsing.
const maxHvsockTargetLen = 128

// ParseHvsockTarget parses "<vm-guid>:<service>", where service is either a GUID or
// "vsock:<port>" with a decimal AF_VSOCK port (0 to 4294967295). GUIDs are accepted with
// or without braces, in any letter case.
//
// The input typically comes from a config file or the command line, so it is treated as
// untrusted.
func ParseHvsockTarget(s string) (HvsockTarget, error) {
	if len(s) > maxHvsockTargetLen {
		return HvsockTarget{}, errors.New("transport: hvsock target is too long")
	}
	vm, svc, ok := strings.Cut(s, ":")
	if !ok {
		return HvsockTarget{}, fmt.Errorf("transport: hvsock target %q: want <vm-guid>:<service>", s)
	}
	vmID, err := parseGUID(vm)
	if err != nil {
		return HvsockTarget{}, fmt.Errorf("transport: hvsock target %q: vm id: %w", s, err)
	}
	var serviceID guid.GUID
	if port, isVsock := strings.CutPrefix(svc, "vsock:"); isVsock {
		p, err := parsePort(port)
		if err != nil {
			return HvsockTarget{}, fmt.Errorf("transport: hvsock target %q: %w", s, err)
		}
		serviceID = VsockServiceID(p)
	} else {
		serviceID, err = parseGUID(svc)
		if err != nil {
			return HvsockTarget{}, fmt.Errorf("transport: hvsock target %q: service id: %w", s, err)
		}
	}
	return HvsockTarget{VMID: vmID, ServiceID: serviceID}, nil
}

func parseGUID(s string) (guid.GUID, error) {
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") && len(s) >= 2 {
		s = s[1 : len(s)-1]
	}
	// guid.FromString checks only length and dashes; it relies on hex.Decode for the rest.
	// Reject anything that is not exactly the 8-4-4-4-12 hex form so that inputs such as
	// a leading "+" never slip through.
	if len(s) != 36 {
		return guid.GUID{}, errors.New("malformed GUID")
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return guid.GUID{}, errors.New("malformed GUID")
			}
		default:
			if !isHex(c) {
				return guid.GUID{}, errors.New("malformed GUID")
			}
		}
	}
	return guid.FromString(s)
}

func isHex(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
}

func parsePort(s string) (uint32, error) {
	if s == "" {
		return 0, errors.New("empty vsock port")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("vsock port %q is not a decimal number", s)
		}
	}
	p, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("vsock port %q is out of range", s)
	}
	return uint32(p), nil
}
