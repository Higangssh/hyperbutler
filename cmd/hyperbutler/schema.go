package main

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"

	"github.com/Higangssh/hyperbutler/internal/config"
	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/imageset"
	"github.com/Higangssh/hyperbutler/internal/schema"
)

// The --json outputs (REQ-UX-008). Every document is one JSON object whose "schema"
// field is SchemaVersion. Within a schema version fields are only ever added; a field is
// removed or changes meaning only with a new version. docs/research/settings.md
// documents them, and schema_test.go pins their field names.
const SchemaVersion = schema.Version

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
	// SandboxImagePinned is the same for the agent sandbox image (ADR 0009), and the
	// SandboxMSI fields name the release's installer of it, which `sandbox install` fetches
	// and checks. A development build has none of them.
	SandboxImagePinned   bool   `json:"sandboxImagePinned"`
	SandboxImageManifest string `json:"sandboxImageManifestSha256,omitempty"`
	SandboxMSI           string `json:"sandboxMsi,omitempty"`
	SandboxMSISha256     string `json:"sandboxMsiSha256,omitempty"`
	SandboxMSIBytes      string `json:"sandboxMsiBytes,omitempty"`
	SandboxMSIProduct    string `json:"sandboxMsiProductCode,omitempty"`
}

// The documents the GUI reads too live in internal/schema; these names keep the CLI's.
type (
	StatusJSON      = schema.Status
	GuestDiskJSON   = schema.GuestDisk
	UpdateJSON      = schema.Update
	AutostartJSON   = schema.Autostart
	DoctorJSON      = schema.Doctor
	DoctorCheck     = schema.DoctorCheck
	GuardPauseJSON  = schema.GuardPause
	ConfigListJSON  = schema.ConfigList
	DiagnoseJSON    = schema.Diagnose
	DiagnoseMissing = schema.DiagnoseMissing
)

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
	uninstallTask    = "scheduled-task" // the autostart task in Task Scheduler (REQ-UX-015)
	uninstallFile    = "file"
	uninstallDir     = "dir" // removed only when empty
)

// uninstallKinds are every Kind, in the order docs/research/settings.md lists them.
var uninstallKinds = []string{uninstallEngine, uninstallVM, uninstallACL, uninstallContext, uninstallPlugins, uninstallTask, uninstallFile, uninstallDir}

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
	v.SandboxImageManifest, v.SandboxImagePinned = imageset.SandboxPin()
	v.SandboxMSISha256, v.SandboxMSI, v.SandboxMSIBytes, v.SandboxMSIProduct = imageset.SandboxMSIPin()
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

// buildVersion is this build's version as state.json records it and start compares it
// (REQ-PLAT-008): the release version, or for a source build "(devel)" with the VCS
// revision, so two development builds from different commits differ.
func buildVersion() string {
	v := versionInfo()
	s := v.Version
	if s == "(devel)" && v.Revision != "" {
		s += " " + v.Revision[:min(12, len(v.Revision))]
		if v.Modified {
			s += "+modified"
		}
	}
	if len(s) > 128 {
		s = s[:128]
	}
	return s
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
	if v.SandboxImagePinned {
		_, _ = fmt.Fprintln(stdout, "sandbox image: pinned, image.json sha256", v.SandboxImageManifest)
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
