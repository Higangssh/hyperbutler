package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/config"
	"github.com/Higangssh/hyperbutler/internal/engine"
)

// jsonKeys returns the field names of the JSON object v encodes to, with nested objects
// as "outer.inner" and array elements as "outer[].inner".
func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&m); err != nil {
		t.Fatal(err)
	}
	var out []string
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				out = append(out, prefix+k)
				walk(prefix+k+".", e)
			}
		case []any:
			if len(x) > 0 {
				walk(strings.TrimSuffix(prefix, ".")+"[].", x[0])
			}
		}
	}
	walk("", m)
	sort.Strings(out)
	return out
}

func checkKeys(t *testing.T, name string, v any, want []string) {
	t.Helper()
	sort.Strings(want)
	if got := jsonKeys(t, v); !reflect.DeepEqual(got, want) {
		t.Errorf("%s fields changed (schema %d):\n got %q\nwant %q\nAdding a field is fine within a schema version; update this test and docs/research/settings.md. Removing or renaming one needs a new SchemaVersion.", name, SchemaVersion, got, want)
	}
}

// TestSchemaFields pins the field names of every --json document (REQ-UX-008).
func TestSchemaFields(t *testing.T) {
	if SchemaVersion != 1 {
		t.Fatalf("schema %d: review the documented fields", SchemaVersion)
	}
	now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	item := config.Item{Key: "memory", Value: "4GiB", Default: "4GiB", Source: config.SourceDefault, Type: "size", Apply: config.ApplyVM, Description: "d"}
	itemKeys := []string{"key", "value", "default", "source", "type", "apply", "description"}

	checkKeys(t, "version", VersionJSON{Schema: 1, Version: "v", GoVersion: "go", OS: "windows", Arch: "amd64", Revision: "r", Modified: true,
		ImagePinned: true, ImageManifest: "h"},
		[]string{"schema", "version", "goVersion", "os", "arch", "revision", "modified", "imagePinned", "imageManifestSha256"})
	checkKeys(t, "config get", ConfigGetJSON{Schema: 1, Item: item}, append([]string{"schema"}, itemKeys...))
	var listKeys []string
	for _, k := range itemKeys {
		listKeys = append(listKeys, "keys[]."+k)
	}
	checkKeys(t, "config list", ConfigListJSON{Schema: 1, SettingsFile: "f", Keys: []config.Item{item}}, append([]string{"schema", "settingsFile", "keys"}, listKeys...))
	checkKeys(t, "config path", ConfigPathJSON{Schema: 1, Home: "h", SettingsFile: "s", DaemonJSON: "d", DataDir: "x"},
		[]string{"schema", "home", "settingsFile", "daemonJson", "dataDir"})

	st := &engine.State{
		Version: 1, Phase: engine.PhaseRunning, PID: 1, PIDCreated: 2, VMID: "hyperbutler-x", RuntimeID: "r", Pipe: "p", DataDir: "d",
		Started: now, ReadyMillis: 1, Updated: now, Error: "e", ErrorCode: "c", ImageDir: "i",
		Daemon:      &engine.DaemonState{Generation: 1, Applies: 1, Source: engine.DaemonFile, SHA256: "h", Bytes: 2, Restarted: true, Error: "e", Updated: now},
		HostGateway: &engine.HostGatewayState{Address: "192.168.127.254", Enabled: true, Ports: "all", Reloads: 1, Error: "e"},
	}
	checkKeys(t, "status", StatusJSON{
		Schema: 1, Running: true, SetUp: true, Home: "h", State: st, DataDir: "d", Disk: "x", DiskUsed: 1, DiskMax: 2, HostFree: 3,
		MemoryMB: 4, CPUs: 5, Context: "c", ContextSel: true, DaemonJSON: "j", DaemonJSONExists: true, DockerCLI: "d", DockerCLIBundled: "b",
	}, []string{
		"schema", "running", "setUp", "home", "state", "dataDir", "disk", "diskHostBytes", "diskSizeBytes", "hostFreeBytes",
		"memoryMB", "cpus", "dockerContext", "dockerContextSelected", "daemonJson", "daemonJsonExists", "dockerCli", "dockerCliBundled",
		"state.version", "state.phase", "state.pid", "state.pidCreated", "state.vmId", "state.runtimeId", "state.pipe",
		"state.dataDir", "state.started", "state.readyMillis", "state.updated", "state.error", "state.errorCode", "state.imageDir",
		"state.daemonConfig", "state.daemonConfig.generation", "state.daemonConfig.applies", "state.daemonConfig.source",
		"state.daemonConfig.sha256", "state.daemonConfig.bytes", "state.daemonConfig.restarted", "state.daemonConfig.error",
		"state.daemonConfig.updated",
		"state.hostGateway", "state.hostGateway.address", "state.hostGateway.enabled", "state.hostGateway.ports",
		"state.hostGateway.reloads", "state.hostGateway.error",
	})
}

// TestSchemaAlwaysThere: the fields a script keys on are present even when empty.
func TestSchemaAlwaysThere(t *testing.T) {
	for name, c := range map[string]struct {
		v    any
		want []string
	}{
		"status":  {StatusJSON{}, []string{"schema", "running", "setUp", "home", "dockerContextSelected", "daemonJson", "daemonJsonExists"}},
		"version": {VersionJSON{}, []string{"schema", "version", "goVersion", "os", "arch", "imagePinned"}},
		"list":    {ConfigListJSON{}, []string{"schema", "settingsFile", "keys"}},
	} {
		got := jsonKeys(t, c.v)
		for _, w := range c.want {
			if i := sort.SearchStrings(got, w); i >= len(got) || got[i] != w {
				t.Errorf("%s: %q missing from %q", name, w, got)
			}
		}
	}
}

func TestVersionInfo(t *testing.T) {
	v := versionInfo()
	if v.Schema != SchemaVersion || v.Version == "" || !strings.HasPrefix(v.GoVersion, "go") || v.OS == "" || v.Arch == "" {
		t.Fatalf("%+v", v)
	}
	// A test binary is a development build: no image pin.
	if v.ImagePinned || v.ImageManifest != "" {
		t.Fatalf("pinned: %+v", v)
	}
}

func TestParseInterspersed(t *testing.T) {
	fs := newFlagSet("t", &bytes.Buffer{})
	apply := fs.Bool("apply", false, "")
	dir := fs.String("dir", "", "")
	pos, err := parseInterspersed(fs, []string{"memory", "--apply", "8GiB", "--dir", "x"})
	if err != nil || !reflect.DeepEqual(pos, []string{"memory", "8GiB"}) || !*apply || *dir != "x" {
		t.Fatalf("%q %v %v %q", pos, err, *apply, *dir)
	}
	fs = newFlagSet("t", &bytes.Buffer{})
	fs.Bool("apply", false, "")
	pos, err = parseInterspersed(fs, []string{"k", "--", "--apply", "-x"})
	if err != nil || !reflect.DeepEqual(pos, []string{"k", "--apply", "-x"}) {
		t.Fatalf("after --: %q %v", pos, err)
	}
	if _, err := parseInterspersed(newFlagSet("t", &bytes.Buffer{}), []string{"k", "--nope"}); err == nil {
		t.Fatal("unknown flag accepted")
	}
}
