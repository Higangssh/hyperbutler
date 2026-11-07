package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/config"
	"github.com/Higangssh/hyperbutler/internal/dockerenv"
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
		ImagePinned: true, ImageManifest: "h", SandboxImagePinned: true, SandboxImageManifest: "h", SandboxMSI: "n", SandboxMSISha256: "h", SandboxMSIBytes: "1", SandboxMSIProduct: "c"},
		[]string{"schema", "version", "goVersion", "os", "arch", "revision", "modified", "imagePinned", "imageManifestSha256",
			"sandboxImagePinned", "sandboxImageManifestSha256", "sandboxMsi", "sandboxMsiSha256", "sandboxMsiBytes", "sandboxMsiProductCode"})
	checkKeys(t, "config get", ConfigGetJSON{Schema: 1, Item: item}, append([]string{"schema"}, itemKeys...))
	var listKeys []string
	for _, k := range itemKeys {
		listKeys = append(listKeys, "keys[]."+k)
	}
	checkKeys(t, "config list", ConfigListJSON{Schema: 1, SettingsFile: "f", Keys: []config.Item{item}}, append([]string{"schema", "settingsFile", "keys"}, listKeys...))
	checkKeys(t, "config path", ConfigPathJSON{Schema: 1, Home: "h", SettingsFile: "s", DaemonJSON: "d", DataDir: "x"},
		[]string{"schema", "home", "settingsFile", "daemonJson", "dataDir"})

	st := &engine.State{
		Version: 1, Phase: engine.PhaseRunning, PID: 1, PIDCreated: 2, VMID: "hyperbutler-x", RuntimeID: "r", PastRuntimeIDs: []string{"r"}, Pipe: "p", DataDir: "d",
		Started: now, ReadyMillis: 1, Updated: now, Error: "e", ErrorCode: "c", ImageDir: "i",
		Daemon: &engine.DaemonState{Generation: 1, Applies: 1, Source: engine.DaemonFile, SHA256: "h", Bytes: 2, Restarted: true, Error: "e", Updated: now,
			Env: &engine.EnvState{HTTPProxy: "http://u:***@p:1", HTTPSProxy: "http://p:1", NoProxy: "localhost", CAImport: true, CACerts: 3, CASkipped: 7,
				CAStats: dockerenv.Stats{Added: 3, Duplicate: 1, NotCA: 1, Expired: 1, NotYetValid: 1, Invalid: 1, Disallowed: 1, OverLimit: 1},
				SHA256:  "h", Restarted: true, Error: "e"}},
		HostGateway:      &engine.HostGatewayState{Address: "192.168.127.254", Enabled: true, Ports: "all", Reloads: 1, Error: "e"},
		UnpublishedPorts: []engine.UnpublishedPort{{Container: "0123456789ab", Port: 8080, Reason: "r", Since: now}},
		Executable:       "x", ExecutableVersion: "v",
		GuardPause: &engine.GuardPauseState{Volume: "C:", FreeBytes: 1, ResumeAtBytes: 2, Since: now},
	}
	checkKeys(t, "status", StatusJSON{
		Schema: 1, Running: true, SetUp: true, Home: "h", State: st, DataDir: "d", Disk: "x", DiskUsed: 1, DiskMax: 2, HostFree: 3,
		MemoryMB: 4, CPUs: 5, Context: "c", ContextSel: true, DaemonJSON: "j", DaemonJSONExists: true, DockerCLI: "d", DockerCLIBundled: "b",
		GuardPause: &GuardPauseJSON{Volume: "C:", FreeBytes: 1, ResumeAtBytes: 2, Since: now},
	}, []string{
		"guardPause", "guardPause.volume", "guardPause.freeBytes", "guardPause.resumeAtBytes", "guardPause.since",
		"state.executable", "state.executableVersion",
		"state.guardPause", "state.guardPause.volume", "state.guardPause.freeBytes", "state.guardPause.resumeAtBytes", "state.guardPause.since",
		"schema", "running", "setUp", "home", "state", "dataDir", "disk", "diskHostBytes", "diskSizeBytes", "hostFreeBytes",
		"memoryMB", "cpus", "dockerContext", "dockerContextSelected", "daemonJson", "daemonJsonExists", "dockerCli", "dockerCliBundled",
		"state.version", "state.phase", "state.pid", "state.pidCreated", "state.vmId", "state.runtimeId", "state.pastRuntimeIds", "state.pipe",
		"state.dataDir", "state.started", "state.readyMillis", "state.updated", "state.error", "state.errorCode", "state.imageDir",
		"state.daemonConfig", "state.daemonConfig.generation", "state.daemonConfig.applies", "state.daemonConfig.source",
		"state.daemonConfig.sha256", "state.daemonConfig.bytes", "state.daemonConfig.restarted", "state.daemonConfig.error",
		"state.daemonConfig.updated",
		"state.daemonConfig.env", "state.daemonConfig.env.httpProxy", "state.daemonConfig.env.httpsProxy", "state.daemonConfig.env.noProxy",
		"state.daemonConfig.env.caImport", "state.daemonConfig.env.caCerts", "state.daemonConfig.env.caSkipped", "state.daemonConfig.env.caStats",
		"state.daemonConfig.env.caStats.added", "state.daemonConfig.env.caStats.duplicate", "state.daemonConfig.env.caStats.notCA",
		"state.daemonConfig.env.caStats.expired", "state.daemonConfig.env.caStats.notYetValid", "state.daemonConfig.env.caStats.invalid",
		"state.daemonConfig.env.caStats.disallowed", "state.daemonConfig.env.caStats.overLimit",
		"state.daemonConfig.env.sha256", "state.daemonConfig.env.restarted", "state.daemonConfig.env.error",
		"state.hostGateway", "state.hostGateway.address", "state.hostGateway.enabled", "state.hostGateway.ports",
		"state.hostGateway.reloads", "state.hostGateway.error",
		"state.unpublishedPorts", "state.unpublishedPorts[].container", "state.unpublishedPorts[].port", "state.unpublishedPorts[].reason",
		"state.unpublishedPorts[].since",
	})
	checkKeys(t, "diagnose", DiagnoseJSON{Schema: 1, File: "f", Files: []string{"a"}, Missing: []DiagnoseMissing{{Name: "n", Reason: "r"}}},
		[]string{"schema", "file", "files", "missing", "missing[].name", "missing[].reason"})
	it := []UninstallItem{{Kind: "file", Path: "p", Detail: "d"}}
	checkKeys(t, "uninstall", UninstallJSON{Schema: 1, Home: "h", DryRun: true, KeepData: true, Items: it, Kept: it, Failed: it, Notes: []string{"n"}},
		[]string{"schema", "home", "dryRun", "keepData", "items", "kept", "failed", "notes",
			"items[].kind", "items[].path", "items[].detail", "kept[].kind", "kept[].path", "kept[].detail",
			"failed[].kind", "failed[].path", "failed[].detail"})
}

// TestUninstallKinds pins the values of uninstall's item kinds, which scripts match on,
// and checks that settings.md lists every one.
func TestUninstallKinds(t *testing.T) {
	want := []string{"engine", "vm", "acl", "docker-context", "docker-plugins", "scheduled-task", "file", "dir"}
	if !reflect.DeepEqual(uninstallKinds, want) {
		t.Fatalf("uninstall kinds %q, want %q: a kind is part of schema %d", uninstallKinds, want, SchemaVersion)
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "research", "settings.md"))
	if err != nil {
		t.Fatal(err)
	}
	var quoted []string
	for _, k := range want {
		quoted = append(quoted, "`"+k+"`")
	}
	if line := "Kinds: " + strings.Join(quoted, ", ") + "."; !strings.Contains(strings.Join(strings.Fields(string(doc)), " "), line) {
		t.Errorf("docs/research/settings.md does not list the uninstall kinds as %q", line)
	}
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
		// Items is [] (not null) when nothing is left; the commands always set it.
		"diagnose":  {DiagnoseJSON{Files: []string{}}, []string{"schema", "file", "files"}},
		"uninstall": {UninstallJSON{Items: []UninstallItem{}}, []string{"schema", "home", "dryRun", "keepData", "items"}},
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
	if v.ImagePinned || v.ImageManifest != "" || v.SandboxImagePinned || v.SandboxImageManifest != "" || v.SandboxMSI != "" || v.SandboxMSISha256 != "" || v.SandboxMSIBytes != "" || v.SandboxMSIProduct != "" {
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
