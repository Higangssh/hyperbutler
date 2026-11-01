// Package schema holds the --json documents (REQ-UX-008) that more than one front end
// reads: `hyperbutler status --json`, `config list --json`, `doctor --json` and
// `diagnose --json`. The CLI prints them, and the GUI (ADR 0008) builds and reads the
// same values in process, so both see one definition.
//
// Every document is one JSON object whose "schema" field is Version. Within a schema
// version fields are only ever added; a field is removed or changes meaning only with a
// new version. docs/research/settings.md documents them, and cmd/hyperbutler's
// schema_test.go pins their field names.
package schema

import (
	"time"

	"github.com/Higangssh/hyperbutler/internal/config"
	"github.com/Higangssh/hyperbutler/internal/engine"
)

// Version is the schema version of every document.
const Version = 1

// Status is `hyperbutler status --json`.
type Status struct {
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
	GuardPause *GuardPause `json:"guardPause,omitempty"`
	// ContextCurrent is the docker context config.json selects ("default" for docker's
	// default), when it can be read (REQ-UX-018).
	ContextCurrent string `json:"dockerContextCurrent,omitempty"`
	// GuestDisk is the guest file system on the data disk, as the guest last reported
	// it while running (REQ-DISK-009).
	GuestDisk *GuestDisk `json:"guestDisk,omitempty"`
	// Update is the last update check, when the check is on (REQ-UX-022).
	Update *Update `json:"update,omitempty"`
}

// GuestDisk is the guest data file system's use; the numbers are guest-reported.
type GuestDisk struct {
	SizeBytes   uint64    `json:"sizeBytes"`
	UsedBytes   uint64    `json:"usedBytes"`
	FreeBytes   uint64    `json:"freeBytes"`
	UsedPercent float64   `json:"usedPercent"`
	Updated     time.Time `json:"updated"`
}

// Update is the last update check: when it was made, and the newest release and its
// page from the last check that got an answer (empty: none, or no release).
type Update struct {
	Checked time.Time `json:"checked"`
	Latest  string    `json:"latest,omitempty"`
	URL     string    `json:"url,omitempty"`
}

// Autostart is the autostart task (REQ-UX-015) in `config list --json`.
type Autostart struct {
	Task    string `json:"task"`
	Exists  bool   `json:"exists"`
	Command string `json:"command,omitempty"` // the program and arguments it runs
	// ThisExe says whether it starts this hyperbutler.exe.
	ThisExe bool   `json:"thisExe"`
	Error   string `json:"error,omitempty"`
}

// Doctor is `hyperbutler doctor --json` (REQ-UX-019): every check, in order. OK is
// false when a check failed, and the command then exits 1.
type Doctor struct {
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

// GuardPause is a pause by the free-space guard: the host volume, its free space as
// status measured it, the free space at which the VM resumes, and since when.
type GuardPause struct {
	Volume        string    `json:"volume"`
	FreeBytes     uint64    `json:"freeBytes"`
	ResumeAtBytes uint64    `json:"resumeAtBytes"`
	Since         time.Time `json:"since"`
}

// ConfigList is `hyperbutler config list --json`.
type ConfigList struct {
	Schema       int           `json:"schema"`
	SettingsFile string        `json:"settingsFile"`
	Keys         []config.Item `json:"keys"`
	// Autostart is the autostart task as Task Scheduler has it.
	Autostart *Autostart `json:"autostart,omitempty"`
}

// Diagnose is `hyperbutler diagnose --json`: the zip written and what is in it.
type Diagnose struct {
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
