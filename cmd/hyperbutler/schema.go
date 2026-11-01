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
	// ImagePinned says the build boots only the guest image it was released with (ADR
	// 0005, Decision 4): the image.json whose SHA-256 is ImageManifest. A development
	// build has no pin and accepts any image that matches its own image.json.
	ImagePinned   bool   `json:"imagePinned"`
	ImageManifest string `json:"imageManifestSha256,omitempty"`
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
	// DockerCLI is the docker.exe a plain `docker` runs (the first on PATH), when there
	// is one; DockerCLIBundled is the one next to hyperbutler.exe, when there is one.
	DockerCLI        string `json:"dockerCli,omitempty"`
	DockerCLIBundled string `json:"dockerCliBundled,omitempty"`
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

// DiagnoseJSON is `hyperbutler diagnose --json`: the zip written and what is in it.
type DiagnoseJSON struct {
	Schema int      `json:"schema"`
	File   string   `json:"file"`  // absolute path of the zip
	Files  []string `json:"files"` // the entries of the zip, in order
	// Missing names what could not be included, and why (a log not written yet, no
	// failed start).
	Missing []DiagnoseMissing `json:"missing,omitempty"`
}

// DiagnoseMissing is one file diagnose did not include.
type DiagnoseMissing struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// UninstallJSON is `hyperbutler uninstall --json` (with --dry-run: what it would do).
type UninstallJSON struct {
	Schema   int    `json:"schema"`
	Home     string `json:"home"` // the installation's state directory
	DryRun   bool   `json:"dryRun"`
	KeepData bool   `json:"keepData"`
	// Items is what was removed (or would be, in a dry run); empty when no trace is left.
	Items []UninstallItem `json:"items"`
	// Kept is what stays on purpose: hyperbutler.exe, the data disk with --keep-data,
	// a directory that holds other files, a docker context that is not this install's.
	Kept []UninstallItem `json:"kept,omitempty"`
	// Failed is what could not be removed; Detail says why.
	Failed []UninstallItem `json:"failed,omitempty"`
	Notes  []string        `json:"notes,omitempty"`
}

// Kinds of UninstallItem.
const (
	uninstallEngine  = "engine"         // the running engine process, stopped first
	uninstallVM      = "vm"             // an HCS compute system of this installation
	uninstallACL     = "acl"            // a VM account's access entry on a disk file
	uninstallContext = "docker-context" // the "hyperbutler" docker context
	uninstallPlugins = "docker-plugins" // the bundled cli-plugins entry in the docker CLI's config.json
	uninstallFile    = "file"
	uninstallDir     = "dir" // removed only when empty
)

// uninstallKinds are every Kind, in the order docs/research/settings.md lists them.
var uninstallKinds = []string{uninstallEngine, uninstallVM, uninstallACL, uninstallContext, uninstallPlugins, uninstallFile, uninstallDir}

// UninstallItem is one thing hyperbutler created. Kind is one of uninstallKinds; Path
// is the file, directory, compute system id, context name or plugin directory.
type UninstallItem struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Detail string `json:"detail,omitempty"`
}

func versionInfo() VersionJSON {
	v := VersionJSON{Schema: SchemaVersion, Version: "(devel)", GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
	v.ImageManifest, v.ImagePinned = engine.ImagePin()
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
	if v.ImagePinned {
		_, _ = fmt.Fprintln(stdout, "guest image: pinned, image.json sha256", v.ImageManifest)
	} else {
		_, _ = fmt.Fprintln(stdout, "guest image: not pinned (development build)")
	}
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
