// Package vmtemplate builds the hyperbutler VM's HCS document (vm.Config) from typed
// inputs. It is the one place that knows the kernel command line, the order and LUNs of
// the disks, and the fixed set of hvsock services with their security descriptors, so
// the in-process engine (developer mode, ADR 0004 option A) and the HCS access broker
// boot the same VM.
//
// Every kernel argument is rendered here from a typed field: an IPv4 subnet, an MTU, a
// port number, a flag, the VM secret's hex (a typed [32]byte). Spec has no free-form text
// that reaches the command line or the service table, except Dev, which only the
// in-process developer harness sets and the broker never does (ADR 0004 rule 1,
// Implementation decision 4). The paths in Spec are those of files the caller has opened
// and verified itself.
//
// The package builds on every platform and depends only on the standard library, vm
// and hvaddr, so the broker can use it within its surface budget (rule 10).
package vmtemplate

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/Higangssh/hyperbutler/internal/hvaddr"
	"github.com/Higangssh/hyperbutler/internal/vm"
	"github.com/Higangssh/hyperbutler/internal/vmsecret"
)

// DefaultCmdLine is the base kernel command line.
//
// swiotlb=noforce: a guest with RAM above 4 GiB of physical address space (any VM of
// 4 GiB, because of the MMIO gap below 4 GiB) otherwise reserves and zeroes a 64 MiB
// bounce-buffer pool at boot, which then stays committed on the host. Nothing here
// needs it: the Hyper-V synthetic devices (vmbus storage, network, hvsock) are not
// DMA-limited, and this is not an isolated (confidential) VM, where Linux forces bounce
// buffers regardless. See docs/research/idle-memory.md.
//
// page_reporting.page_reporting_order=0: hv_balloon's free page reporting hands free
// memory back to the host (the cold discard hint), by default only in free blocks of
// 2 MiB (order 9) or more. After a workload has touched most of the guest's RAM, the
// slab and kernel pages it left behind split many 2 MiB blocks, and the free pieces
// around them stay committed on the host: 50 to 90 MiB after a 4 GiB + 4 GiB workload
// in a 12 GiB guest, growing with each workload. Reporting single pages returns them.
// Idle guest CPU did not change measurably. See docs/research/mem-reclaim.md
// (REQ-MEM-002).
//
// The dentry and inode hash table sizes are added per VM size (HashTableArgs).
const DefaultCmdLine = consoleArg + "quiet swiotlb=noforce page_reporting.page_reporting_order=0"

// consoleArg starts DefaultCmdLine; Build drops it from a VM without a COM port.
const consoleArg = "console=ttyS0 "

// The guest's AF_VSOCK ports: the services of the VM's hvsock table.
const (
	// DockerPort is the guest agent's Docker API relay.
	DockerPort = 2375
	// NetPort is the default port of the guest network link (ADR 0006).
	NetPort = 1024
	// SharePort is the port the guest's share relay connects to (ADR 0007).
	SharePort = 2385
	// ControlPort is the guest control channel's (guestctl.ControlPort, which a test
	// holds equal: guestctl dials, and the broker may not link it).
	ControlPort = 2380
)

// Network limits, the ones netstack enforces (netstack.Config.Validate and
// frame.MinMTU..MaxMTU); the engine's tests check that they agree.
const (
	MinMTU        = 576
	MaxMTU        = 65520
	MinSubnetBits = 8
	MaxSubnetBits = 29
)

// CRC is the link CRC mode of the offload link (netstack.CRCMode).
type CRC uint8

const (
	// CRCOn is the default: every frame carries a CRC-32C. The guest is told nothing.
	CRCOn CRC = iota
	// CRCOff is the link without a CRC, only for measuring what the CRC costs.
	CRCOff
	// CRCStrict makes a mismatch end the link (tests and benches).
	CRCStrict
)

func (c CRC) kernelArg() string {
	switch c {
	case CRCOff:
		return " hyperbutler.netcrc=0"
	case CRCStrict:
		return " hyperbutler.netcrc=strict"
	}
	return ""
}

// Net is the guest network, as the user-mode stack of ADR 0006 runs it.
type Net struct {
	// Subnet is the guest subnet: gateway .1 (also the DNS server), guest .2, and the
	// host gateway at the last address before the broadcast address.
	Subnet netip.Prefix
	// MTU is the link MTU, MinMTU..MaxMTU.
	MTU int
	// Offload is the offload link: the host dials the guest's netfwd -vnet, so the user
	// may connect to the link's service as well as listen on it.
	Offload bool
	// HostGateway tells the guest the host gateway's address (host.docker.internal).
	HostGateway bool
	// CRC is the offload link's CRC mode; it is told to the guest only with Offload.
	CRC CRC
	// Port is the link's AF_VSOCK port; 0 means NetPort. Only the developer harness sets
	// another one.
	Port uint32
}

func (n *Net) port() uint32 {
	if n.Port == 0 {
		return NetPort
	}
	return n.Port
}

// Gateway is the stack's own address in the subnet (netstack.Config.Gateway).
func (n *Net) Gateway() netip.Addr { return n.Subnet.Masked().Addr().Next() }

// GuestIP is the guest's static address (netstack.Config.GuestIP).
func (n *Net) GuestIP() netip.Addr { return n.Gateway().Next() }

// HostGatewayAddr is the host gateway's address: the last one before the subnet's
// broadcast address (netstack.Config.HostGatewayAddr).
func (n *Net) HostGatewayAddr() netip.Addr {
	p := n.Subnet.Masked()
	a := p.Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	binary.BigEndian.PutUint32(a[:], (binary.BigEndian.Uint32(a[:])|host)-1)
	return netip.AddrFrom4(a)
}

// Validate reports the first problem with n.
func (n *Net) Validate() error {
	if !n.Subnet.IsValid() || !n.Subnet.Addr().Is4() || n.Subnet.Bits() < MinSubnetBits || n.Subnet.Bits() > MaxSubnetBits {
		return fmt.Errorf("vmtemplate: subnet %s must be IPv4 with a prefix length of %d..%d", n.Subnet, MinSubnetBits, MaxSubnetBits)
	}
	if !n.Subnet.Addr().IsPrivate() {
		return fmt.Errorf("vmtemplate: subnet %s is not a private (RFC 1918) range", n.Subnet)
	}
	if n.MTU < MinMTU || n.MTU > MaxMTU {
		return fmt.Errorf("vmtemplate: MTU %d is outside %d..%d", n.MTU, MinMTU, MaxMTU)
	}
	if n.CRC > CRCStrict {
		return fmt.Errorf("vmtemplate: link CRC mode %d is unknown", n.CRC)
	}
	if n.Port == 0xFFFFFFFF {
		return errors.New("vmtemplate: network vsock port is out of range")
	}
	return nil
}

// Spec is one VM.
type Spec struct {
	// Owner is the HCS Owner of the compute system.
	Owner string
	// UserSID is the only principal that may connect to or listen on any of the VM's
	// hvsock services (vm.UserOnlySDDL, ADR 0004 rule 6).
	UserSID string
	// Secret is the VM's secret (ADR 0004 rule 6), made fresh for each start
	// (vmsecret.New). It reaches the guest as vmsecret.CmdlineKey on the kernel command
	// line, the only text there that is not rendered from a fixed set; its hex form is
	// the only thing a Secret can render to. Build refuses a zero secret.
	Secret vmsecret.Secret
	// MemoryMB and CPUs size the VM.
	MemoryMB, CPUs int
	// Kernel and Initrd are the absolute paths of the guest kernel and initramfs.
	Kernel, Initrd string
	// System is the read-only system disk of an image set (ADR 0005), or empty for an
	// initramfs-only image.
	System string
	// Data is the data disk (a sparse VHDX), or empty for a guest that keeps
	// /var/lib/docker in RAM. BlankData says the guest may format it.
	Data      string
	BlankData bool
	// Net is the guest network, nil for none.
	Net *Net
	// Share adds the bind-mount file server's service (ADR 0007).
	Share bool
	// DaemonConfig tells the guest's init to wait for daemon.json on the control channel.
	DaemonConfig bool
	// ConsolePipe, if set, is the named pipe HCS serves COM1 on (developer mode only;
	// ADR 0004 Implementation decision 7).
	ConsolePipe string
	// Dev are the developer harness's additions. The broker never sets them.
	Dev Dev
}

// Dev are additions to the VM for development harnesses (cmd/hyperbutler-dev, spikes).
type Dev struct {
	// CmdLine replaces DefaultCmdLine when set.
	CmdLine string
	// ExtraCmdLine is appended to the kernel command line, and ExtraServices to the
	// service table.
	ExtraCmdLine  string
	ExtraServices []vm.Service
}

// HashTableArgs returns the kernel arguments that cap the dentry and inode hash tables
// of a guest with memoryMB of RAM ("" or " dhash_entries=... ihash_entries=...").
//
// The kernel sizes both tables from RAM (one dentry bucket per 2 pages, one inode
// bucket per 4, rounded up to a power of two), zeroes them at boot, and so they stay
// committed on the host: 16 + 8 MiB in a 12 GiB guest. Above 4 GiB they are capped at
// the sizes a 4 GiB guest gets by default, 2^19 and 2^18 buckets (4 + 2 MiB); at 4 GiB
// and below the kernel's own sizes stay. See docs/research/mem-reclaim.md (REQ-MEM-002).
func HashTableArgs(memoryMB int) string {
	if memoryMB <= hashCapMB {
		return ""
	}
	return " dhash_entries=524288 ihash_entries=262144"
}

// hashCapMB is the guest size whose default dentry and inode hash tables HashTableArgs
// keeps for larger guests.
const hashCapMB = 4096

// Build renders s into the VM's configuration. The result passes vm.Config.Validate.
//
// The kernel command line is the base, the hash table sizes, the secret, the network, the share,
// the developer additions, daemon.json, then the disks. The data disk is LUN 0 and the
// system disk follows it. The services are the Docker API, the guest control channel,
// the network link and the share, in that order.
func Build(s *Spec) (*vm.Config, error) {
	sddl, err := vm.UserOnlySDDL(s.UserSID)
	if err != nil {
		return nil, err
	}
	if s.BlankData && s.Data == "" {
		return nil, errors.New("vmtemplate: a blank data disk without a data disk")
	}
	if s.Secret.IsZero() {
		return nil, errors.New("vmtemplate: the VM has no secret")
	}
	// The guest refuses a command line with two secrets; refuse it here already.
	if strings.Contains(s.Dev.CmdLine, vmsecret.CmdlineKey) || strings.Contains(s.Dev.ExtraCmdLine, vmsecret.CmdlineKey) {
		return nil, fmt.Errorf("vmtemplate: the developer command line must not set %s", vmsecret.CmdlineKey)
	}
	cmdline := s.Dev.CmdLine
	if cmdline == "" {
		cmdline = DefaultCmdLine
		// Without a COM port (the broker's VMs) console=ttyS0 names a console that is not
		// there, and the guest powers off within a second (seen 2026-11-02); without it the
		// kernel has no console and the guest's init logs to /dev/kmsg.
		if s.ConsolePipe == "" {
			cmdline = cmdline[len(consoleArg):]
		}
	}
	cmdline += HashTableArgs(s.MemoryMB)
	cmdline += " " + vmsecret.CmdlineKey + "=" + s.Secret.Hex()
	svcs := []vm.Service{
		{ID: hvaddr.VsockServiceID(DockerPort), ConnectSDDL: sddl},
		{ID: hvaddr.VsockServiceID(ControlPort), ConnectSDDL: sddl},
	}
	if n := s.Net; n != nil {
		if err := n.Validate(); err != nil {
			return nil, err
		}
		gw := n.Gateway()
		cmdline += fmt.Sprintf(" hyperbutler.net=%d hyperbutler.mtu=%d hyperbutler.ip=%s/%d hyperbutler.gw=%s hyperbutler.dns=%s",
			n.port(), n.MTU, n.GuestIP(), n.Subnet.Bits(), gw, gw)
		if n.HostGateway {
			// Always passed while the stack has a host gateway, on or off: turning it off
			// refuses connections in the stack, so dockerd need not restart for it.
			cmdline += " hyperbutler.hostgw=" + n.HostGatewayAddr().String()
		}
		// Only this user may listen on the link's service (for a guest forwarder that
		// dials the host) and, in offload mode, connect to it (netfwd -vnet listens in
		// the guest).
		svc := vm.Service{ID: hvaddr.VsockServiceID(n.port()), BindSDDL: sddl}
		if n.Offload {
			svc.ConnectSDDL = sddl
			cmdline += " hyperbutler.netvnet=1" + n.CRC.kernelArg()
		}
		svcs = append(svcs, svc)
	}
	if s.Share {
		// The guest connects to the host on this service; only this user may listen on it.
		svcs = append(svcs, vm.Service{ID: hvaddr.VsockServiceID(SharePort), BindSDDL: sddl})
		cmdline += fmt.Sprintf(" hyperbutler.share=%d", SharePort)
	}
	cmdline += s.Dev.ExtraCmdLine
	svcs = append(svcs, s.Dev.ExtraServices...)
	if s.DaemonConfig {
		cmdline += " hyperbutler.daemoncfg=1"
	}
	var disks []vm.Disk
	if s.Data != "" {
		disks = []vm.Disk{{Path: s.Data, Sparse: true}}
		if s.System != "" {
			cmdline += " hyperbutler.data=lun:0"
		} else {
			cmdline += " hyperbutler.data=sda" // what the initramfs-only image understands
		}
		if s.BlankData {
			cmdline += " hyperbutler.newdisk=1"
		}
	}
	if s.System != "" {
		// The system disk, read-only at the hypervisor: the guest cannot change it.
		cmdline += fmt.Sprintf(" hyperbutler.system=lun:%d", len(disks))
		disks = append(disks, vm.Disk{Path: s.System, ReadOnly: true})
	}
	c := &vm.Config{
		Owner:          s.Owner,
		KernelPath:     s.Kernel,
		InitrdPath:     s.Initrd,
		KernelCmdLine:  cmdline,
		MemoryMB:       s.MemoryMB,
		ProcessorCount: s.CPUs,
		MemoryHints:    true,
		Disks:          disks,
		ConsolePipe:    s.ConsolePipe,
		Services:       svcs,
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}
