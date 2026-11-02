package vmtemplate

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/transport"
	"github.com/Higangssh/hyperbutler/internal/vm"
)

var update = flag.Bool("update", false, "rewrite the golden documents in testdata")

const sid = "S-1-5-21-1000-2000-3000-1001"

func defaultNet() *Net {
	return &Net{Subnet: netip.MustParsePrefix("192.168.127.0/24"), MTU: 9000, Offload: true, HostGateway: true}
}

// golden are representative VMs: the release image with a data disk as the engine and
// the broker boot it, an initramfs-only development image, and a bare VM.
var golden = map[string]*Spec{
	"release": {
		Owner: "hyperbutler", UserSID: sid, MemoryMB: 4096, CPUs: 2,
		Kernel: `C:\Program Files\hyperbutler\image\vmlinuz-virt`, Initrd: `C:\Program Files\hyperbutler\image\initramfs-stage1.gz`,
		System: `C:\Program Files\hyperbutler\image\system.vhd`, Data: `C:\Users\u\AppData\Local\hyperbutler\data.vhdx`,
		Net: defaultNet(), Share: true, DaemonConfig: true,
		ConsolePipe: `\\.\pipe\hyperbutler-0a1b2c3d-112233445566-com1`,
	},
	"release-blank-12g": {
		Owner: "hyperbutler", UserSID: sid, MemoryMB: 12288, CPUs: 8,
		Kernel: `C:\img\vmlinuz-virt`, Initrd: `C:\img\initramfs-stage1.gz`, System: `C:\img\system.vhd`,
		Data: `D:\hb\data.vhdx`, BlankData: true,
		Net:   &Net{Subnet: netip.MustParsePrefix("10.20.30.0/26"), MTU: 1500, CRC: CRCStrict, Offload: true},
		Share: true,
	},
	"initramfs-plain-net": {
		Owner: "hyperbutler", UserSID: sid, MemoryMB: 2048, CPUs: 1,
		Kernel: `C:\img\vmlinuz-virt`, Initrd: `C:\img\initramfs-hyperbutler.gz`, Data: `C:\d\data.vhdx`,
		Net: &Net{Subnet: netip.MustParsePrefix("192.168.127.0/24"), MTU: 9000, CRC: CRCOff},
	},
	"bare-dev": {
		UserSID: sid, MemoryMB: 1024, CPUs: 1, Kernel: `C:\k`, Initrd: `C:\i`,
		Dev: Dev{CmdLine: "console=ttyS0", ExtraCmdLine: " rdinit=/bench", ExtraServices: []vm.Service{{ID: transport.VsockServiceID(9), BindSDDL: "D:P"}}},
	},
}

// TestGolden: the documents of the representative VMs do not change unnoticed. Run
// with -update to rewrite them after a deliberate change, and review the diff.
func TestGolden(t *testing.T) {
	for name, s := range golden {
		t.Run(name, func(t *testing.T) {
			c, err := Build(s)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := c.Document()
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := json.Indent(&out, doc, "", "  "); err != nil {
				t.Fatal(err)
			}
			out.WriteByte('\n')
			p := filepath.Join("testdata", name+".json")
			if *update {
				if err := os.WriteFile(p, out.Bytes(), 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
			if !bytes.Equal(out.Bytes(), want) {
				t.Errorf("document differs from %s:\n%s", p, out.Bytes())
			}
		})
	}
}

func TestBuildCmdLine(t *testing.T) {
	c, err := Build(golden["release"])
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultCmdLine +
		" hyperbutler.net=1024 hyperbutler.mtu=9000 hyperbutler.ip=192.168.127.2/24 hyperbutler.gw=192.168.127.1 hyperbutler.dns=192.168.127.1" +
		" hyperbutler.hostgw=192.168.127.254 hyperbutler.netvnet=1 hyperbutler.share=2385 hyperbutler.daemoncfg=1" +
		" hyperbutler.data=lun:0 hyperbutler.system=lun:1"
	if c.KernelCmdLine != want {
		t.Fatalf("command line\n got %q\nwant %q", c.KernelCmdLine, want)
	}
	if len(c.Disks) != 2 || !c.Disks[0].Sparse || c.Disks[0].ReadOnly || !c.Disks[1].ReadOnly {
		t.Fatalf("disks %+v", c.Disks)
	}
	userOnly, _ := vm.UserOnlySDDL(sid)
	for _, s := range c.Services {
		for _, d := range []string{s.ConnectSDDL, s.BindSDDL} {
			if d != "" && d != userOnly {
				t.Errorf("service %s has descriptor %q", s.ID, d)
			}
		}
	}
}

func TestBuildRejects(t *testing.T) {
	base := func() *Spec {
		s := *golden["release"]
		n := *s.Net
		s.Net = &n
		return &s
	}
	for name, mut := range map[string]func(*Spec){
		"bad sid":         func(s *Spec) { s.UserSID = "S-1-5-21-1)(A;;FA;;;WD" },
		"no sid":          func(s *Spec) { s.UserSID = "" },
		"blank without":   func(s *Spec) { s.Data, s.System = "", ""; s.BlankData = true },
		"public subnet":   func(s *Spec) { s.Net.Subnet = netip.MustParsePrefix("8.8.8.0/24") },
		"ipv6 subnet":     func(s *Spec) { s.Net.Subnet = netip.MustParsePrefix("fd00::/64") },
		"prefix 30":       func(s *Spec) { s.Net.Subnet = netip.MustParsePrefix("10.0.0.0/30") },
		"prefix 7":        func(s *Spec) { s.Net.Subnet = netip.MustParsePrefix("10.0.0.0/7") },
		"zero subnet":     func(s *Spec) { s.Net.Subnet = netip.Prefix{} },
		"mtu low":         func(s *Spec) { s.Net.MTU = MinMTU - 1 },
		"mtu high":        func(s *Spec) { s.Net.MTU = MaxMTU + 1 },
		"crc":             func(s *Spec) { s.Net.CRC = 3 },
		"port":            func(s *Spec) { s.Net.Port = 0xFFFFFFFF },
		"memory":          func(s *Spec) { s.MemoryMB = 0 },
		"cpus":            func(s *Spec) { s.CPUs = 0 },
		"relative kernel": func(s *Spec) { s.Kernel = "vmlinuz" },
		"same disk twice": func(s *Spec) { s.System = s.Data },
		"duplicate service": func(s *Spec) {
			s.Dev.ExtraServices = []vm.Service{{ID: transport.VsockServiceID(DockerPort), ConnectSDDL: "D:P"}}
		},
	} {
		s := base()
		mut(s)
		if _, err := Build(s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Build(base()); err != nil {
		t.Fatal(err)
	}
}

func TestHashTableArgs(t *testing.T) {
	for _, mb := range []int{512, 2048, 4096} {
		if a := HashTableArgs(mb); a != "" {
			t.Errorf("%d MiB: %q, want the kernel's own sizes", mb, a)
		}
	}
	for _, mb := range []int{4097, 12288, 65536, 1 << 20} {
		a := HashTableArgs(mb)
		if !strings.HasPrefix(a, " ") || !strings.Contains(a, "dhash_entries=524288") || !strings.Contains(a, "ihash_entries=262144") {
			t.Errorf("%d MiB: %q", mb, a)
		}
	}
}
