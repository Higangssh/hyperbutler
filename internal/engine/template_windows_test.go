//go:build windows

package engine

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/netip"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/guestctl"
	"github.com/Higangssh/hyperbutler/internal/netstack"
	"github.com/Higangssh/hyperbutler/internal/netstack/frame"
	"github.com/Higangssh/hyperbutler/internal/transport"
	"github.com/Higangssh/hyperbutler/internal/vm"
	"github.com/Higangssh/hyperbutler/internal/vmtemplate"
)

// legacyInput is what Run assembled the VM document from before vmtemplate existed.
type legacyInput struct {
	cmdline       string // cfg.CmdLine after defaults
	memoryMB      int
	cpus          int
	net           *NetConfig // nil: off
	share         bool
	daemonCfg     bool
	data          string
	blank         bool
	system        string
	kernel        string
	initrd        string
	vmID          string
	extraCmdLine  string
	extraServices []vm.Service
}

// legacyConfig is Run's VM document assembly as it was before the template (with
// setupNet's and setupShares' parts), kept verbatim to prove the template renders the
// same document.
func legacyConfig(t *testing.T, in legacyInput, sddl string) *vm.Config {
	t.Helper()
	var netSvcs []vm.Service
	var netArgs string
	if in.net != nil {
		nc := *in.net
		if nc.Port == 0 {
			nc.Port = DefaultNetPort
		}
		subnet := nc.Subnet
		if !subnet.IsValid() {
			subnet = netstack.DefaultSubnet
		}
		st, err := netstack.New(netstack.Config{Subnet: subnet, MTU: nc.MTU, HostForwards: nc.HostForwards, HostGateway: nc.HostGateway, LinkCRC: nc.CRC}, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		cfg := st.Config()
		st.Close()
		svc := vm.Service{ID: transport.VsockServiceID(nc.Port), BindSDDL: sddl}
		netArgs = fmt.Sprintf(" hyperbutler.net=%d hyperbutler.mtu=%d hyperbutler.ip=%s/%d hyperbutler.gw=%s hyperbutler.dns=%s",
			nc.Port, cfg.MTU, cfg.GuestIP(), cfg.Subnet.Bits(), cfg.Gateway(), cfg.Gateway())
		if cfg.HostGateway != nil {
			netArgs += " hyperbutler.hostgw=" + cfg.HostGatewayAddr().String()
		}
		if !nc.NoOffload {
			svc.ConnectSDDL = sddl
			netArgs += " hyperbutler.netvnet=1"
			netArgs += cfg.LinkCRC.KernelArg()
		}
		netSvcs = []vm.Service{svc}
	}
	var shareSvcs []vm.Service
	var shareArgs string
	if in.share {
		shareSvcs = []vm.Service{{ID: transport.VsockServiceID(SharePort), BindSDDL: sddl}}
		shareArgs = fmt.Sprintf(" hyperbutler.share=%d", SharePort)
	}
	cmdline := in.cmdline + vmtemplate.HashTableArgs(in.memoryMB) + netArgs + shareArgs + in.extraCmdLine
	if in.daemonCfg {
		cmdline += " hyperbutler.daemoncfg=1"
	}
	var disks []vm.Disk
	if in.data != "" {
		disks = []vm.Disk{{Path: in.data, Sparse: true}}
		if in.system != "" {
			cmdline += " hyperbutler.data=lun:0"
		} else {
			cmdline += " hyperbutler.data=sda"
		}
		if in.blank {
			cmdline += " hyperbutler.newdisk=1"
		}
	}
	if in.system != "" {
		cmdline += fmt.Sprintf(" hyperbutler.system=lun:%d", len(disks))
		disks = append(disks, vm.Disk{Path: in.system, ReadOnly: true})
	}
	dockerSvc := transport.VsockServiceID(DockerPort)
	consolePipe := `\\.\pipe\` + in.vmID + "-com1"
	svcs := []vm.Service{{ID: dockerSvc, ConnectSDDL: sddl}, {ID: guestctl.ServiceID, ConnectSDDL: sddl}}
	svcs = append(append(append(svcs, netSvcs...), shareSvcs...), in.extraServices...)
	return &vm.Config{
		Owner:          VMOwner,
		KernelPath:     in.kernel,
		InitrdPath:     in.initrd,
		KernelCmdLine:  cmdline,
		MemoryMB:       in.memoryMB,
		ProcessorCount: in.cpus,
		MemoryHints:    true,
		Disks:          disks,
		ConsolePipe:    consolePipe,
		Services:       svcs,
	}
}

// templateConfig is what Run now builds from the same input.
func templateConfig(t *testing.T, in legacyInput, sid string) *vm.Config {
	t.Helper()
	spec := &vmtemplate.Spec{
		Owner: VMOwner, UserSID: sid, MemoryMB: in.memoryMB, CPUs: in.cpus,
		Kernel: in.kernel, Initrd: in.initrd, System: in.system, Data: in.data, BlankData: in.blank,
		Share: in.share, DaemonConfig: in.daemonCfg, ConsolePipe: `\\.\pipe\` + in.vmID + "-com1",
		Dev: vmtemplate.Dev{CmdLine: in.cmdline, ExtraCmdLine: in.extraCmdLine, ExtraServices: in.extraServices},
	}
	if in.net != nil {
		g, n, err := setupNet(*in.net, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		g.close()
		spec.Net = n
	}
	c, err := vmtemplate.Build(spec)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestTemplateMatchesLegacyDocument: the VM document Run builds through vmtemplate is
// byte for byte the one it built before (M1a of the broker work is a pure refactor).
func TestTemplateMatchesLegacyDocument(t *testing.T) {
	sid, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	sddl, err := CurrentUserSDDL()
	if err != nil {
		t.Fatal(err)
	}
	hg := &netstack.HostGateway{Enabled: true}
	nets := map[string]*NetConfig{
		"off":       nil,
		"default":   {},
		"plain":     {NoOffload: true},
		"hostgw":    {HostGateway: hg},
		"crc off":   {CRC: netstack.CRCOff},
		"crc plain": {CRC: netstack.CRCStrict, NoOffload: true},
		"strict":    {CRC: netstack.CRCStrict, HostGateway: &netstack.HostGateway{}},
		"subnet":    {Subnet: netip.MustParsePrefix("10.20.30.0/26"), MTU: 1500, HostGateway: hg},
		"port":      {Port: 4000, MTU: frame.MinMTU},
	}
	type disks struct{ data, system string }
	images := map[string]disks{
		"initramfs":         {},
		"initramfs + data":  {data: `C:\d\data.vhdx`},
		"image":             {system: `C:\img\system.vhd`},
		"image + data":      {data: `D:\hb data\data.vhdx`, system: `C:\Program Files\hyperbutler\image\system.vhd`},
		"image + long data": {data: `\\server\share\data.vhdx`, system: `C:\img\system.vhd`},
	}
	n := 0
	for nn, nc := range nets {
		for in, d := range images {
			for _, v := range []struct{ share, daemon, blank, extra bool }{
				{}, {share: true}, {daemon: true}, {share: true, daemon: true, blank: true}, {extra: true, share: true},
			} {
				if v.blank && d.data == "" {
					continue
				}
				li := legacyInput{
					cmdline: DefaultCmdLine, memoryMB: 4096, cpus: 2, net: nc, share: v.share, daemonCfg: v.daemon,
					data: d.data, blank: v.blank, system: d.system,
					kernel: `C:\img\vmlinuz-virt`, initrd: `C:\img\initramfs-stage1.gz`, vmID: "hyperbutler-0a1b2c3d-112233445566",
				}
				if v.extra {
					li.cmdline, li.memoryMB, li.cpus = "console=ttyS0", 12288, 8
					li.extraCmdLine = " rdinit=/sharebench sb.port=9"
					li.extraServices = []vm.Service{{ID: transport.VsockServiceID(9), BindSDDL: sddl}}
				}
				name := fmt.Sprintf("%s/%s/%+v", nn, in, v)
				want, err := legacyConfig(t, li, sddl).Document()
				if err != nil {
					t.Fatalf("%s: legacy: %v", name, err)
				}
				got, err := templateConfig(t, li, sid).Document()
				if err != nil {
					t.Fatalf("%s: template: %v", name, err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("%s: documents differ\n got %s\nwant %s", name, got, want)
				}
				n++
			}
		}
	}
	if n < 100 {
		t.Fatalf("only %d configurations compared", n)
	}
}

// The template's network limits and addresses are the stack's.
func TestTemplateNetMatchesStack(t *testing.T) {
	if vmtemplate.MinMTU != frame.MinMTU || vmtemplate.MaxMTU != frame.MaxMTU {
		t.Fatalf("template MTU range %d..%d, stack %d..%d", vmtemplate.MinMTU, vmtemplate.MaxMTU, frame.MinMTU, frame.MaxMTU)
	}
	if vmtemplate.NetPort != DefaultNetPort {
		t.Fatal("network port differs")
	}
	for _, s := range []string{"192.168.127.0/24", "10.0.0.0/8", "172.16.4.0/29", "10.20.30.64/26"} {
		p := netip.MustParsePrefix(s)
		c := netstack.Config{Subnet: p}
		n := vmtemplate.Net{Subnet: p}
		if c.Gateway() != n.Gateway() || c.GuestIP() != n.GuestIP() || c.HostGatewayAddr() != n.HostGatewayAddr() {
			t.Errorf("%s: stack %s %s %s, template %s %s %s", s, c.Gateway(), c.GuestIP(), c.HostGatewayAddr(),
				n.Gateway(), n.GuestIP(), n.HostGatewayAddr())
		}
	}
	for m, want := range map[netstack.CRCMode]vmtemplate.CRC{netstack.CRCOn: vmtemplate.CRCOn, netstack.CRCOff: vmtemplate.CRCOff, netstack.CRCStrict: vmtemplate.CRCStrict} {
		if got := templateCRC(m); got != want {
			t.Errorf("CRC %v maps to %d", m, got)
		}
	}
}
