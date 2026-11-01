package engine

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testDir(t testing.TB) string {
	t.Helper()
	d, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func validState(dir string) *State {
	return &State{
		Version: stateVersion, Phase: PhaseRunning, PID: 1234, PIDCreated: 133000000000000000,
		VMID: "hyperbutler-65fd2a1a-c01f6278e7ec", RuntimeID: "6cf7ba4c-5e89-5915-bea5-3642d57a897b",
		Pipe: `\\.\pipe\docker_engine_hyperbutler`, DataDir: dir,
		Started: time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC), ReadyMillis: 1700,
	}
}

func TestStateRoundTrip(t *testing.T) {
	l := Layout{Home: testDir(t)}
	s := validState(l.Home)
	s.HostGateway = &HostGatewayState{Address: "192.168.127.254", Enabled: true, Ports: "80,8000-8100", Reloads: 2}
	if err := SaveState(l, s); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(l)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, s)
	}
	// No temporary files are left behind.
	ents, _ := os.ReadDir(l.Home)
	if len(ents) != 1 {
		t.Fatalf("files after save: %v", ents)
	}
}

func TestStateRejects(t *testing.T) {
	dir := testDir(t)
	for name, mut := range map[string]func(*State){
		"version":       func(s *State) { s.Version = 2 },
		"phase":         func(s *State) { s.Phase = "dancing" },
		"negative pid":  func(s *State) { s.PID = -1 },
		"live no pid":   func(s *State) { s.PID = 0 },
		"vm id":         func(s *State) { s.VMID = "other-vm" },
		"vm id chars":   func(s *State) { s.VMID = "hyperbutler-A;B" },
		"runtime id":    func(s *State) { s.RuntimeID = "{x}" },
		"pipe":          func(s *State) { s.Pipe = `\\server\pipe\x` },
		"error code":    func(s *State) { s.ErrorCode = "Bad Code" },
		"relative data": func(s *State) { s.DataDir = "rel" },
		"long error":    func(s *State) { s.Error = strings.Repeat("x", maxErrorLen+1) },
		"ready":         func(s *State) { s.ReadyMillis = -1 },
		"host gw addr":  func(s *State) { s.HostGateway = &HostGatewayState{Address: "::1", Ports: "all"} },
		"host gw ports": func(s *State) { s.HostGateway = &HostGatewayState{Address: "192.168.127.254", Ports: "0"} },
	} {
		s := validState(dir)
		mut(s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, s)
		}
	}
	for name, doc := range map[string]string{
		"unknown field": `{"version":1,"phase":"stopped","pid":0,"pidCreated":0,"pipe":"","dataDir":"","started":"2026-10-30T08:00:00Z","updated":"2026-10-30T08:00:00Z","extra":1}`,
		"trailing":      `{"version":1,"phase":"stopped","pid":0,"pidCreated":0,"pipe":"","dataDir":"","started":"2026-10-30T08:00:00Z","updated":"2026-10-30T08:00:00Z"} {}`,
		"array":         `[]`,
		"not utf8":      "{\"version\":1,\"error\":\"\xff\"}",
		"empty":         ``,
	} {
		if _, err := ParseState([]byte(doc)); err == nil {
			t.Errorf("%s: ParseState accepted %q", name, doc)
		}
	}
}

func TestSaveStateTruncatesError(t *testing.T) {
	l := Layout{Home: testDir(t)}
	s := validState(l.Home)
	s.Phase, s.Error = PhaseFailed, strings.Repeat("e", 2*maxErrorLen)+"\xff"
	if err := SaveState(l, s); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(l)
	if err != nil || len(got.Error) > maxErrorLen {
		t.Fatalf("got %d bytes, %v", len(got.Error), err)
	}
}

func TestSettings(t *testing.T) {
	l := Layout{Home: testDir(t)}
	if _, err := LoadSettings(l); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing settings: %v", err)
	}
	s := &Settings{DataDir: l.Home, DiskSize: 64 << 30, MemoryMB: 4096, CPUs: 4}
	if err := SaveSettings(l, s); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSettings(l)
	if err != nil || *got != *s {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got.PipeName() != `\\.\pipe\docker_engine_hyperbutler` {
		t.Fatalf("default pipe %q", got.PipeName())
	}
	// Memory, CPUs and the context are optional and have defaults.
	opt, err := ParseSettings([]byte(`{"version":1,"dataDir":"C:\\x","diskSizeBytes":68719476736}`))
	if err != nil || opt.Memory() != DefaultMemoryMB || opt.CPUCount(16) != 8 || opt.CPUCount(2) != 2 || !opt.ContextManaged() {
		t.Fatalf("optional fields: %+v %v", opt, err)
	}
	no, err := ParseSettings([]byte(`{"version":1,"dataDir":"C:\\x","diskSizeBytes":68719476736,"cpus":3,"manageContext":false}`))
	if err != nil || no.CPUCount(16) != 3 || no.ContextManaged() {
		t.Fatalf("set fields: %+v %v", no, err)
	}
	// REQ-NET-004: the host gateway is on for every port unless settings.json says otherwise.
	if hg := opt.HostGateway(); !hg.Enabled || len(hg.Ports) != 0 {
		t.Fatalf("default host gateway %+v", hg)
	}
	hgs, err := ParseSettings([]byte(`{"version":1,"dataDir":"C:\\x","diskSizeBytes":68719476736,"hostGatewayEnabled":false,"hostGatewayPorts":"5432"}`))
	if hg := hgs.HostGateway(); err != nil || hg.Enabled || hg.Ports.String() != "5432" {
		t.Fatalf("host gateway %+v %v", hg, err)
	}
	for name, mut := range map[string]func(*Settings){
		"relative":   func(s *Settings) { s.DataDir = "hyperbutler" },
		"disk small": func(s *Settings) { s.DiskSize = 1 << 30 },
		"disk odd":   func(s *Settings) { s.DiskSize = 64<<30 + 1 },
		"memory":     func(s *Settings) { s.MemoryMB = 512 },
		"cpus":       func(s *Settings) { s.CPUs = -1 },
		"cpus big":   func(s *Settings) { s.CPUs = MaxCPUs + 1 },
		"pipe":       func(s *Settings) { s.Pipe = `C:\pipe` },
		"nul":        func(s *Settings) { s.DataDir += "\x00" },
		"hg ports":   func(s *Settings) { s.HostGatewayPorts = "80-" },
	} {
		c := *s
		c.Version = SettingsVersion
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted %+v", name, c)
		}
	}
}

func TestDiskMeta(t *testing.T) {
	l := Layout{Home: testDir(t)}
	l.Data = l.Home
	d := &DiskMeta{SizeBytes: 64 << 30, BlockSize: 1 << 20, MetadataBytes: 4 << 20, Created: time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)}
	if err := saveDiskMeta(l, d); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDiskMeta(l)
	if err != nil || *got != *d {
		t.Fatalf("got %+v, %v", got, err)
	}
	bad := *d
	bad.BlockSize = 32 << 20
	if bad.Validate() == nil {
		t.Fatal("accepted 32 MiB blocks")
	}
}

func TestReadFileBounded(t *testing.T) {
	p := filepath.Join(testDir(t), "big")
	if err := os.WriteFile(p, make([]byte, maxFileSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFileBounded(p); err == nil {
		t.Fatal("read an oversized file")
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]uint64{
		"0": 0, "1": 1, "512b": 512, "4k": 4 << 10, "64GiB": 64 << 30, "64g": 64 << 30, "64 GB": 64 << 30,
		"2048MiB": 2 << 30, "1T": 1 << 40, " 3mib ": 3 << 20, "16777215TiB": 16777215 << 40,
	} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "GiB", "-1G", "1.5G", "1e3", "0x10", "16777216TiB", "18446744073709551616", "1 G B", strings.Repeat("1", 40), "+5"} {
		if n, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) = %d, want an error", in, n)
		}
	}
	for _, n := range []uint64{0, 1, 1023, 1 << 10, 3 << 20, 64 << 30, 5 << 40, 1<<64 - 1} {
		if got, err := ParseSize(FormatSize(n)); err != nil || got != n {
			t.Errorf("FormatSize(%d) = %q parses as %d, %v", n, FormatSize(n), got, err)
		}
	}
	if s := HumanSize(1536 << 20); s != "1.5 GiB" {
		t.Errorf("HumanSize = %q", s)
	}
}

func TestLayout(t *testing.T) {
	a := Layout{Home: `C:\Users\x\AppData\Local\hyperbutler`}
	b := Layout{Home: `c:\users\X\appdata\local\HYPERBUTLER\`}
	if a.Tag() != b.Tag() || len(a.Tag()) != 8 {
		t.Fatalf("tags %q %q", a.Tag(), b.Tag())
	}
	if !strings.HasPrefix(a.VMPrefix(), "hyperbutler-") || !validToken(a.VMPrefix()+"0", maxVMIDLen, true) {
		t.Fatalf("prefix %q", a.VMPrefix())
	}
	t.Setenv(HomeEnv, "")
	t.Setenv("LOCALAPPDATA", filepath.Join(testDir(t), "Local"))
	h, err := DefaultHome()
	if err != nil || filepath.Base(h) != "hyperbutler" || filepath.Base(filepath.Dir(h)) != "Local" {
		t.Fatalf("DefaultHome = %q, %v", h, err)
	}
	// A relative HYPERBUTLER_HOME would name another installation in every directory.
	for _, rel := range []string{"rel", `\no-volume`, `C:drive-relative`} {
		t.Setenv(HomeEnv, rel)
		if h, err := DefaultHome(); err == nil {
			t.Fatalf("HYPERBUTLER_HOME %q accepted: %q", rel, h)
		}
	}
	abs := filepath.Join(testDir(t), "h")
	t.Setenv(HomeEnv, abs+`\`)
	if h, err := DefaultHome(); err != nil || h != abs {
		t.Fatalf("HYPERBUTLER_HOME absolute: %q, %v", h, err)
	}
}
