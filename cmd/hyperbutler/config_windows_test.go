//go:build windows

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/config"
	"github.com/Higangssh/hyperbutler/internal/engine"
)

func TestConfigListGetSet(t *testing.T) {
	home, _ := testHome(t)
	l := engine.Layout{Home: home}
	code, out, e := runCLI(t, "config", "list")
	if code != 0 || !strings.Contains(out, "memory") || !strings.Contains(out, "No settings file yet") || !strings.Contains(out, "VM restart") {
		t.Fatalf("list: %d %q %q", code, out, e)
	}
	// Flags may follow the arguments.
	code, out, e = runCLI(t, "config", "set", "memory", "8GiB")
	if code != 0 || !strings.Contains(out, "memory: 4GiB -> 8GiB (file)") || !strings.Contains(out, "next hyperbutler start") {
		t.Fatalf("set: %d %q %q", code, out, e)
	}
	s, err := engine.LoadSettings(l)
	if err != nil || s.MemoryMB != 8192 || s.CPUs != 0 || !engine.SamePath(s.DataDir, home) || s.DiskSize != engine.DefaultDiskSize {
		t.Fatalf("settings %+v %v", s, err)
	}
	if code, out, _ := runCLI(t, "config", "set", "memory", "8192"); code != 0 || !strings.Contains(out, "nothing changed") {
		t.Fatalf("same value: %d %q", code, out)
	}
	if code, out, _ := runCLI(t, "config", "get", "memory"); code != 0 || out != "8GiB\n" {
		t.Fatalf("get: %d %q", code, out)
	}
	code, out, _ = runCLI(t, "config", "get", "--json", "memory")
	var g ConfigGetJSON
	if code != 0 || json.Unmarshal([]byte(out), &g) != nil || g.Schema != 1 || g.Key != "memory" || g.Value != "8GiB" || g.Source != config.SourceFile || g.Apply != config.ApplyVM {
		t.Fatalf("get --json: %d %q %+v", code, out, g)
	}
	code, out, _ = runCLI(t, "config", "list", "--json")
	var lj ConfigListJSON
	if code != 0 || json.Unmarshal([]byte(out), &lj) != nil || lj.Schema != 1 || len(lj.Keys) != len(config.Keys) || !engine.SamePath(lj.SettingsFile, l.SettingsPath()) {
		t.Fatalf("list --json: %d %q", code, out)
	}
	if code, out, _ := runCLI(t, "config", "set", "context.manage", "false"); code != 0 || !strings.Contains(out, "at once") {
		t.Fatalf("context.manage: %d %q", code, out)
	}
	if code, out, _ := runCLI(t, "config", "set", "disk.size", "32GiB"); code != 0 || !strings.Contains(out, "first start") {
		t.Fatalf("disk.size: %d %q", code, out)
	}
	if code, _, _ := runCLI(t, "config", "unset", "memory"); code != 0 {
		t.Fatal("unset")
	}
	if s, _ := engine.LoadSettings(l); s.MemoryMB != 0 || s.ContextManaged() || s.DiskSize != 32<<30 {
		t.Fatalf("after unset: %+v", s)
	}
	// Refusals leave the file alone.
	before, _ := os.ReadFile(l.SettingsPath())
	for _, args := range [][]string{
		{"config", "set", "memory", "12"},
		{"config", "set", "cpus", "0"},
		{"config", "set", "proxy.http", "http://x"},
		{"config", "set", "nope", "1"},
		{"config", "set", "memory"},
		{"config", "get"},
		{"config", "frob"},
	} {
		if code, _, e := runCLI(t, args...); code == 0 || e == "" {
			t.Errorf("%q: exit %d %q", args, code, e)
		}
	}
	if code, _, e := runCLI(t, "config", "get", "proxy.http"); code != 1 || !strings.Contains(e, "reserved") {
		t.Errorf("reserved: %d %q", code, e)
	}
	if after, _ := os.ReadFile(l.SettingsPath()); string(after) != string(before) {
		t.Fatalf("a refused set changed settings.json:\n%s\n%s", before, after)
	}
}

// TestConfigNeverOverwritesInvalid: REQ-UX-006, an invalid or unreadable settings.json is
// never overwritten.
func TestConfigNeverOverwritesInvalid(t *testing.T) {
	home, _ := testHome(t)
	l := engine.Layout{Home: home}
	if err := engine.MkdirPrivate(home); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`{"version":1,"dataDir":"C:\\x","diskSizeBytes":68719476736,"memoryMB":12}`,
		`{"version":1,"dataDir":"C:\\x","diskSizeBytes":68719476736,"surprise":1}`,
		`{"version":1,`,
		"",
		`{"version":2,"dataDir":"C:\\x","diskSizeBytes":68719476736}`,
	} {
		if err := os.WriteFile(l.SettingsPath(), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{
			{"config", "set", "memory", "8GiB"},
			{"config", "unset", "cpus"},
			{"start", "--memory", "8GiB", "--image-dir", filepath.Join(home, "nope")},
		} {
			code, _, e := runCLI(t, args...)
			if code == 0 || !strings.Contains(e, "settings") {
				t.Errorf("%q with %q: %d %q", args, bad, code, e)
			}
		}
		if code, _, _ := runCLI(t, "config", "list"); code == 0 {
			t.Errorf("list with %q succeeded", bad)
		}
		// path still works: it is how the user finds the file to fix.
		if code, out, _ := runCLI(t, "config", "path"); code != 0 || !strings.Contains(out, l.SettingsPath()) {
			t.Errorf("path: %d %q", code, out)
		}
		if b, _ := os.ReadFile(l.SettingsPath()); string(b) != bad {
			t.Fatalf("invalid settings.json was rewritten to %q", b)
		}
	}
}

func TestConfigDaemon(t *testing.T) {
	home, _ := testHome(t)
	l := engine.Layout{Home: home}
	if err := engine.MkdirPrivate(home); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := runCLI(t, "config", "daemon", "validate"); code != 0 || !strings.Contains(out, "There is no") {
		t.Fatalf("validate without a file: %d %q", code, out)
	}
	write := func(s string) {
		if err := os.WriteFile(l.DaemonConfigPath(), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"insecure-registries":["192.168.1.20:5000"]}`)
	if code, out, e := runCLI(t, "config", "daemon", "validate"); code != 0 || !strings.Contains(out, "is valid") {
		t.Fatalf("validate: %d %q %q", code, out, e)
	}
	if code, out, e := runCLI(t, "config", "daemon", "apply"); code != 0 || !strings.Contains(out, "not running") {
		t.Fatalf("apply while stopped: %d %q %q", code, out, e)
	}
	write(`{"hosts":["tcp://0.0.0.0:2375"]}`)
	if code, _, e := runCLI(t, "config", "daemon", "validate"); code != 1 || !strings.Contains(e, `may not set "hosts"`) {
		t.Fatalf("owned key: %d %q", code, e)
	}
	if code, _, e := runCLI(t, "config", "daemon", "apply"); code != 1 || !strings.Contains(e, "previous daemon.json stays") {
		t.Fatalf("apply owned key: %d %q", code, e)
	}
	other := filepath.Join(home, "other.json")
	if err := os.WriteFile(other, []byte(`{"debug": tru}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, e := runCLI(t, "config", "daemon", "validate", "--file", other); code != 1 || !strings.Contains(e, "line 1") {
		t.Fatalf("validate --file: %d %q", code, e)
	}
	// The user's file is never rewritten.
	if b, _ := os.ReadFile(l.DaemonConfigPath()); string(b) != `{"hosts":["tcp://0.0.0.0:2375"]}` {
		t.Fatalf("daemon.json rewritten: %q", b)
	}
	if code, out, _ := runCLI(t, "config", "path"); code != 0 || !strings.Contains(out, l.DaemonConfigPath()) {
		t.Fatalf("path: %d %q", code, out)
	}
}

func TestStatusAndVersionJSON(t *testing.T) {
	home, _ := testHome(t)
	code, out, _ := runCLI(t, "status", "--json")
	var st StatusJSON
	if code != exitNotRunning || json.Unmarshal([]byte(out), &st) != nil || st.Schema != 1 || st.Running || st.SetUp || !engine.SamePath(st.Home, home) || st.DaemonJSON == "" {
		t.Fatalf("status --json: %d %q", code, out)
	}
	code, out, _ = runCLI(t, "version", "--json")
	var v VersionJSON
	if code != 0 || json.Unmarshal([]byte(out), &v) != nil || v.Schema != 1 || v.OS != "windows" {
		t.Fatalf("version --json: %d %q", code, out)
	}
	if code, out, _ := runCLI(t, "config", "path", "--json"); code != 0 || !strings.Contains(out, `"schema": 1`) {
		t.Fatalf("path --json: %d %q", code, out)
	}
}
