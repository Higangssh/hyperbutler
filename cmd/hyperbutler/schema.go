package main

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"

	"github.com/Higangssh/hyperbutler/internal/config"
	"github.com/Higangssh/hyperbutler/internal/engine"
)

// The --json outputs (REQ-UX-008). Every document is one JSON object whose "schema"
// field is SchemaVersion. Within a schema version fields are only ever added; a field is
// removed or changes meaning only with a new version. docs/research/settings.md
// documents them, and schema_test.go pins their field names.
const SchemaVersion = 1

// VersionJSON is `hyperbutler version --json`.
type VersionJSON struct {
	Schema    int    `json:"schema"`
	Version   string `json:"version"` // the module version, "(devel)" for a source build
	GoVersion string `json:"goVersion"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Revision  string `json:"revision,omitempty"` // VCS revision of the build, if known
	Modified  bool   `json:"modified,omitempty"` // the build had uncommitted changes
}

// StatusJSON is `hyperbutler status --json`.
type StatusJSON struct {
	Schema  int    `json:"schema"`
	Running bool   `json:"running"`
	SetUp   bool   `json:"setUp"` // settings.json exists
	Home    string `json:"home"`
	// State is the engine's state.json as it is, when there is one.
	State      *engine.State `json:"state,omitempty"`
	DataDir    string        `json:"dataDir,omitempty"`
	Disk       string        `json:"disk,omitempty"`
	DiskUsed   uint64        `json:"diskHostBytes,omitempty"`
	DiskMax    uint64        `json:"diskSizeBytes,omitempty"`
	HostFree   uint64        `json:"hostFreeBytes,omitempty"`
	MemoryMB   int           `json:"memoryMB,omitempty"`
	CPUs       int           `json:"cpus,omitempty"`
	Context    string        `json:"dockerContext,omitempty"`
	ContextSel bool          `json:"dockerContextSelected"`
	// DaemonJSON is the user's daemon.json path; DaemonJSONExists says whether it exists.
	DaemonJSON       string `json:"daemonJson"`
	DaemonJSONExists bool   `json:"daemonJsonExists"`
}

// ConfigListJSON is `hyperbutler config list --json`.
type ConfigListJSON struct {
	Schema       int           `json:"schema"`
	SettingsFile string        `json:"settingsFile"`
	Keys         []config.Item `json:"keys"`
}

// ConfigGetJSON is `hyperbutler config get KEY --json`: one key, as in list.
type ConfigGetJSON struct {
	Schema int `json:"schema"`
	config.Item
}

// ConfigPathJSON is `hyperbutler config path --json`.
type ConfigPathJSON struct {
	Schema       int    `json:"schema"`
	Home         string `json:"home"`
	SettingsFile string `json:"settingsFile"`
	DaemonJSON   string `json:"daemonJson"`
	DataDir      string `json:"dataDir"`
}

func versionInfo() VersionJSON {
	v := VersionJSON{Schema: SchemaVersion, Version: "(devel)", GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if bi.Main.Version != "" {
			v.Version = bi.Main.Version
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				v.Revision = s.Value
			case "vcs.modified":
				v.Modified = s.Value == "true"
			}
		}
	}
	return v
}

func cmdVersion(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("version", stderr)
	asJSON := fs.Bool("json", false, "print JSON (schema "+fmt.Sprint(SchemaVersion)+")")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	v := versionInfo()
	if *asJSON {
		return writeJSON(stdout, v)
	}
	_, _ = fmt.Fprintln(stdout, "hyperbutler", v.Version)
	return nil
}

// writeJSON prints v as indented JSON.
func writeJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}
