package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

func testEnv(t *testing.T) (Env, *bool) {
	home, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	disk := new(bool)
	return Env{
		Home: home, HostCPUs: 12,
		Getenv:     func(string) string { return "" },
		DiskExists: func(string) bool { return *disk },
	}, disk
}

func item(t *testing.T, items []Item, key string) Item {
	t.Helper()
	for _, it := range items {
		if it.Key == key {
			return it
		}
	}
	t.Fatalf("no key %q", key)
	return Item{}
}

func TestListDefaults(t *testing.T) {
	e, _ := testEnv(t)
	items := List(nil, e)
	if len(items) != len(Keys) {
		t.Fatalf("%d items", len(items))
	}
	want := map[string]struct {
		value string
		apply ApplyTime
	}{
		"memory":             {"4GiB", ApplyVM},
		"cpus":               {"6", ApplyVM},
		"disk.size":          {"64GiB", ApplyFirstStart},
		"data.dir":           {e.Home, ApplyFirstStart},
		"pipe":               {`\\.\pipe\docker_engine_hyperbutler`, ApplyVM},
		"context.manage":     {"true", ApplyLive},
		"engine.memoryLimit": {"256MiB", ApplyVM},
	}
	for k, w := range want {
		it := item(t, items, k)
		if it.Value != w.value || it.Default != w.value || it.Source != SourceDefault || it.Apply != w.apply || it.Description == "" || it.Type == "" {
			t.Errorf("%s: %+v", k, it)
		}
	}
}

func TestSetUnset(t *testing.T) {
	e, disk := testEnv(t)
	var s *engine.Settings
	set := func(k, v string) {
		t.Helper()
		key, err := Lookup(k)
		if err != nil {
			t.Fatal(err)
		}
		n, err := key.Set(s, v, e)
		if err != nil {
			t.Fatalf("set %s %s: %v", k, v, err)
		}
		s = n
	}
	set("memory", "8GiB")
	set("CPUS", "3") // names are case-insensitive
	set("context.manage", "off")
	set("pipe", `\\.\pipe\docker_engine_hb_test`)
	set("engine.memoryLimit", "off")
	set("disk.size", "128g")
	set("data.dir", filepath.Join(e.Home, "data"))
	if s.MemoryMB != 8192 || s.CPUs != 3 || s.ContextManaged() || s.Pipe != `\\.\pipe\docker_engine_hb_test` ||
		s.MemoryLimit != "off" || s.DiskSize != 128<<30 || !engine.SamePath(s.DataDir, filepath.Join(e.Home, "data")) {
		t.Fatalf("settings %+v", s)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	items := List(s, e)
	for _, k := range []string{"memory", "cpus", "context.manage", "pipe", "engine.memoryLimit", "disk.size", "data.dir"} {
		if it := item(t, items, k); it.Source != SourceFile {
			t.Errorf("%s source %s", k, it.Source)
		}
	}
	// The default pipe and memory limit are stored as "unset".
	set("pipe", `\\.\pipe\docker_engine_hyperbutler`)
	set("engine.memoryLimit", "256MiB")
	if s.Pipe != "" || s.MemoryLimit != "" {
		t.Fatalf("defaults stored: %+v", s)
	}
	for _, k := range Keys {
		n, err := k.Unset(s, e)
		if err != nil {
			t.Fatalf("unset %s: %v", k.Name, err)
		}
		s = n
	}
	if s.MemoryMB != 0 || s.CPUs != 0 || s.ManageContext != nil || s.DiskSize != engine.DefaultDiskSize || !engine.SamePath(s.DataDir, e.Home) {
		t.Fatalf("after unset: %+v", s)
	}
	// Once the disk exists, its location and size are fixed.
	*disk = true
	for _, kv := range [][2]string{{"disk.size", "32GiB"}, {"data.dir", filepath.Join(e.Home, "x")}} {
		k, _ := Lookup(kv[0])
		if _, err := k.Set(s, kv[1], e); err == nil {
			t.Errorf("%s changed with a disk", kv[0])
		}
	}
	k, _ := Lookup("disk.size")
	if _, err := k.Set(s, "64GiB", e); err != nil {
		t.Errorf("the same disk size refused: %v", err)
	}
}

func TestSetRefuses(t *testing.T) {
	e, _ := testEnv(t)
	for _, c := range [][2]string{
		{"memory", "512MiB"}, {"memory", "4.5GiB"}, {"memory", "lots"}, {"memory", ""}, {"memory", "2000000MiB"},
		{"cpus", "0"}, {"cpus", "257"}, {"cpus", "two"}, {"cpus", "1.5"},
		{"disk.size", "1GiB"}, {"disk.size", "65GiB1"}, {"disk.size", "17TiB"}, {"disk.size", "4294967297"},
		{"data.dir", ""}, {"data.dir", `\\server\share`}, {"data.dir", "a\x00b"},
		{"pipe", `C:\pipe`}, {"pipe", `\\.\pipe\`}, {"pipe", `\\server\pipe\x`},
		{"context.manage", "maybe"},
		{"engine.memoryLimit", "1KiB"}, {"engine.memoryLimit", "lots"},
	} {
		k, err := Lookup(c[0])
		if err != nil {
			t.Fatal(err)
		}
		if n, err := k.Set(nil, c[1], e); err == nil {
			t.Errorf("set %s %q accepted: %+v", c[0], c[1], n)
		} else if !strings.HasPrefix(err.Error(), k.Name+": ") {
			t.Errorf("error %q does not name the key", err)
		}
	}
}

func TestSetDoesNotChangeInput(t *testing.T) {
	e, _ := testEnv(t)
	no := false
	s := &engine.Settings{Version: 1, DataDir: e.Home, DiskSize: engine.DefaultDiskSize, ManageContext: &no}
	k, _ := Lookup("context.manage")
	n, err := k.Set(s, "true", e)
	if err != nil || !n.ContextManaged() || s.ContextManaged() {
		t.Fatalf("input changed: %+v %+v %v", s, n, err)
	}
	if Equal(s, n) || !Equal(s, s) || Equal(nil, s) || !Equal(nil, nil) {
		t.Fatal("Equal")
	}
}

func TestLookup(t *testing.T) {
	if _, err := Lookup("proxy.http"); !errors.Is(err, ErrReserved) {
		t.Errorf("reserved: %v", err)
	}
	if _, err := Lookup("nope"); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("unknown: %v", err)
	}
	seen := map[string]bool{}
	for _, k := range Keys {
		if seen[strings.ToLower(k.Name)] {
			t.Errorf("duplicate key %s", k.Name)
		}
		seen[strings.ToLower(k.Name)] = true
		switch k.Apply {
		case ApplyLive, ApplyDockerd, ApplyVM, ApplyFirstStart:
		default:
			t.Errorf("%s: apply time %q", k.Name, k.Apply)
		}
	}
	for _, r := range Reserved {
		if seen[strings.ToLower(r)] {
			t.Errorf("%s is both a key and reserved", r)
		}
	}
}

func TestEnvOverride(t *testing.T) {
	e, _ := testEnv(t)
	e.Getenv = func(k string) string {
		if k == "GOMEMLIMIT" {
			return "1GiB"
		}
		return ""
	}
	k, _ := Lookup("engine.memoryLimit")
	if it := k.Get(nil, e); it.Value != "1GiB" || it.Source != SourceEnv || it.Default != "256MiB" {
		t.Fatalf("%+v", it)
	}
}

func TestParseBool(t *testing.T) {
	for v, want := range map[string]bool{"true": true, "YES": true, " on ": true, "1": true, "false": false, "No": false, "off": false, "0": false} {
		if got, err := ParseBool(v); err != nil || got != want {
			t.Errorf("ParseBool(%q) = %v %v", v, got, err)
		}
	}
	for _, v := range []string{"", "t", "2", "truee"} {
		if _, err := ParseBool(v); err == nil {
			t.Errorf("ParseBool(%q) accepted", v)
		}
	}
}
