package main

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"time"

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
	// GuardPause is set while the free-space guard keeps the VM paused (REQ-DISK-010).
	GuardPause *GuardPauseJSON `json:"guardPause,omitempty"`
	// ContextCurrent is the docker context config.json selects ("default" for docker's
	// default), when it can be read (REQ-UX-018).
	ContextCurrent string `json:"dockerContextCurrent,omitempty"`
	// GuestDisk is the guest file system on the data disk, as the guest last reported
	// it while running (REQ-DISK-009).
	GuestDisk *GuestDiskJSON `json:"guestDisk,omitempty"`
	// Update is the last update check, when the check is on (REQ-UX-022).
	Update *UpdateJSON `json:"update,omitempty"`
}

// GuestDiskJSON is the guest data file system's use; the numbers are guest-reported.
type GuestDiskJSON struct {
	SizeBytes   uint64    `json:"sizeBytes"`
	UsedBytes   uint64    `json:"usedBytes"`
	FreeBytes   uint64    `json:"freeBytes"`
	UsedPercent float64   `json:"usedPercent"`
	Updated     time.Time `json:"updated"`
}

// UpdateJSON is the last update check: when it was made, and the newest release and its
// page from the last check that got an answer (empty: none, or no release).
type UpdateJSON struct {
	Checked time.Time `json:"checked"`
	Latest  string    `json:"latest,omitempty"`
	URL     string    `json:"url,omitempty"`
}

// AutostartJSON is the autostart task (REQ-UX-015) in `config list --json`.
type AutostartJSON struct {
	Task    string `json:"task"`
	Exists  bool   `json:"exists"`
	Command string `json:"command,omitempty"` // the program and arguments it runs
	// ThisExe says whether it starts this hyperbutler.exe.
	ThisExe bool   `json:"thisExe"`
	Error   string `json:"error,omitempty"`
}

// DoctorJSON is `hyperbutler doctor --json` (REQ-UX-019): every check, in order. OK is
// false when a check failed, and the command then exits 1.
type DoctorJSON struct {
	Schema int           `json:"schema"`
	Home   string        `json:"home"`
	OK     bool          `json:"ok"`
	Checks []DoctorCheck `json:"checks"`
}

// DoctorCheck is one check: ID names it ("pipe", "credential-helper", ...), Status is
// ok, info, warn or fail, and Fix says what to do when it is not ok.
type DoctorCheck struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Summary string `json:"summary"`
	Fix     string `json:"fix,omitempty"`
}

// GuardPauseJSON is a pause by the free-space guard: the host volume, its free space as
// status measured it, the free space at which the VM resumes, and since when.
type GuardPauseJSON struct {
	Volume        string    `json:"volume"`
	FreeBytes     uint64    `json:"freeBytes"`
	ResumeAtBytes uint64    `json:"resumeAtBytes"`
	Since         time.Time `json:"since"`
}

// ConfigListJSON is `hyperbutler config list --json`.
type ConfigListJSON struct {
	Schema       int           `json:"schema"`
	SettingsFile string        `json:"settingsFile"`
	Keys         []config.Item `json:"keys"`
	// Autostart is the autostart task as Task Scheduler has it.
	Autostart *AutostartJSON `json:"autostart,omitempty"`
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
