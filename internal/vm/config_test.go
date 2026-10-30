package vm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Microsoft/go-winio/pkg/guid"
)

const testSID = "S-1-5-21-1004336348-1177238915-682003330-1001"

func testConfig(t *testing.T) *Config {
	t.Helper()
	sddl, err := UserOnlySDDL(testSID)
	if err != nil {
		t.Fatal(err)
	}
	return &Config{
		Owner:          "test",
		KernelPath:     `C:\k\vmlinuz`,
		InitrdPath:     `C:\k\initrd.gz`,
		KernelCmdLine:  "console=ttyS0",
		MemoryMB:       1024,
		ProcessorCount: 2,
		ConsolePipe:    `\\.\pipe\x-com1`,
		Services:       []Service{{ID: mustGUID(t, "00000947-facb-11e6-bd58-64006a7986d3"), ConnectSDDL: sddl}},
	}
}

func mustGUID(t *testing.T, s string) guid.GUID {
	t.Helper()
	g, err := guid.FromString(s)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestDocumentHvSocket(t *testing.T) {
	b, err := testConfig(t).Document()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		ShouldTerminateOnLastHandleClosed bool
		VirtualMachine                    struct {
			Devices struct {
				ComPorts map[string]struct{ NamedPipe string }
				HvSocket struct {
					HvSocketConfig struct {
						DefaultBindSecurityDescriptor    string
						DefaultConnectSecurityDescriptor string
						ServiceTable                     map[string]struct {
							BindSecurityDescriptor    string
							ConnectSecurityDescriptor string
						}
					}
				}
			}
		}
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.ShouldTerminateOnLastHandleClosed {
		t.Error("VM would outlive its owner")
	}
	hv := doc.VirtualMachine.Devices.HvSocket.HvSocketConfig
	if hv.DefaultBindSecurityDescriptor != DenyAllSDDL || hv.DefaultConnectSecurityDescriptor != DenyAllSDDL {
		t.Errorf("defaults are not deny-all: %+v", hv)
	}
	svc, ok := hv.ServiceTable["00000947-facb-11e6-bd58-64006a7986d3"]
	if !ok || len(hv.ServiceTable) != 1 {
		t.Fatalf("service table %+v", hv.ServiceTable)
	}
	if want := "D:P(A;;FA;;;" + testSID + ")"; svc.ConnectSecurityDescriptor != want {
		t.Errorf("connect SDDL %q, want %q", svc.ConnectSecurityDescriptor, want)
	}
	if svc.BindSecurityDescriptor != DenyAllSDDL {
		t.Errorf("bind SDDL %q, want deny-all", svc.BindSecurityDescriptor)
	}
	if doc.VirtualMachine.Devices.ComPorts["0"].NamedPipe != `\\.\pipe\x-com1` {
		t.Errorf("com ports %+v", doc.VirtualMachine.Devices.ComPorts)
	}
}

func TestDocumentBindService(t *testing.T) {
	c := testConfig(t)
	sddl := c.Services[0].ConnectSDDL
	c.Services = append(c.Services, Service{ID: mustGUID(t, "00000400-facb-11e6-bd58-64006a7986d3"), BindSDDL: sddl})
	b, err := c.Document()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		VirtualMachine struct {
			Devices struct {
				HvSocket struct {
					HvSocketConfig struct {
						ServiceTable map[string]struct {
							BindSecurityDescriptor    string
							ConnectSecurityDescriptor string
						}
					}
				}
			}
		}
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	svc := doc.VirtualMachine.Devices.HvSocket.HvSocketConfig.ServiceTable["00000400-facb-11e6-bd58-64006a7986d3"]
	if svc.BindSecurityDescriptor != sddl || svc.ConnectSecurityDescriptor != DenyAllSDDL {
		t.Fatalf("bind-only service rendered as %+v", svc)
	}
}

func TestDocumentNoConsole(t *testing.T) {
	c := testConfig(t)
	c.ConsolePipe = ""
	b, err := c.Document()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "ComPorts") {
		t.Errorf("document has a COM port: %s", b)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"relative kernel", func(c *Config) { c.KernelPath = "vmlinuz" }},
		{"relative initrd", func(c *Config) { c.InitrdPath = `k\initrd` }},
		{"memory too small", func(c *Config) { c.MemoryMB = 128 }},
		{"no cpus", func(c *Config) { c.ProcessorCount = 0 }},
		{"service without sddl", func(c *Config) { c.Services[0].ConnectSDDL = "" }},
		{"service without any sddl", func(c *Config) { c.Services[0] = Service{ID: c.Services[0].ID} }},
		{"duplicate service", func(c *Config) { c.Services = append(c.Services, c.Services[0]) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig(t)
			tt.mutate(c)
			if _, err := c.Document(); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestUserOnlySDDL(t *testing.T) {
	for _, bad := range []string{"", "S-1-", "S-1-5-", "S-1--5", "S-1-5)(A;;FA;;;WD", "s-1-5-18", "S-1-5-18-", "WD", "S-1-5-1\x008"} {
		if _, err := UserOnlySDDL(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	for _, good := range []string{"S-1-5-18", testSID} {
		if _, err := UserOnlySDDL(good); err != nil {
			t.Errorf("rejected %q: %v", good, err)
		}
	}
}

func TestParseRuntimeID(t *testing.T) {
	id, err := parseRuntimeID(`{"Id":"winbox-dev-1","SystemType":"VirtualMachine","RuntimeId":"7d757fea-5532-5530-aa44-69bf7ad959da","State":"Running"}`)
	if err != nil {
		t.Fatal(err)
	}
	if id.String() != "7d757fea-5532-5530-aa44-69bf7ad959da" {
		t.Fatalf("got %s", id)
	}
	for _, bad := range []string{"", "{}", `{"RuntimeId":""}`, `{"RuntimeId":"00000000-0000-0000-0000-000000000000"}`, `{"RuntimeId":"zz"}`, `[`} {
		if _, err := parseRuntimeID(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
