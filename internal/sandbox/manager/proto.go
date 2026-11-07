// Package manager runs a user's agent sandbox sessions (ADR 0009, "Sessions and disks"):
// one background process, apart from the Docker engine, that holds each session's VM,
// network and review cache, and that the `hyperbutler sandbox` commands talk to over a
// pipe only this user can open. It starts with the first session and ends some minutes
// after the last one stopped; a session's disk and record stay until it is removed.
//
// Keeping it out of the Docker engine's process means a session needs no Docker VM and
// the engine's memory (REQ-MEM-001) does not carry a review of a thousand files.
package manager

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Op names a request.
type Op string

// The requests. OpTerm is the only one that goes on after its reply: the connection then
// carries a terminal's ctl frames, unread here.
const (
	OpRun     Op = "run"
	OpStart   Op = "start"
	OpStop    Op = "stop"
	OpRemove  Op = "rm"
	OpList    Op = "ls"
	OpDiff    Op = "diff"
	OpApply   Op = "apply"
	OpCommits Op = "commits"
	OpAllow   Op = "allow"
	OpBlocked Op = "blocked"
	OpTerm    Op = "term"
	OpPing    Op = "ping"
	// OpQuit ends the manager and every running session (uninstall).
	OpQuit Op = "quit"
)

// Request is one JSON line from the client.
type Request struct {
	Op   Op     `json:"op"`
	Name string `json:"name,omitempty"`

	// OpRun.
	Dir      string   `json:"dir,omitempty"`
	Kept     bool     `json:"kept,omitempty"`
	Mode     string   `json:"mode,omitempty"`
	Allow    []string `json:"allow,omitempty"` // and OpAllow
	Include  []string `json:"include,omitempty"`
	MemoryMB int      `json:"memoryMB,omitempty"`
	CPUs     int      `json:"cpus,omitempty"`
	DiskSize uint64   `json:"diskSize,omitempty"`
	// LargeOK copies a project over the size limits.
	LargeOK bool `json:"largeOK,omitempty"`

	// OpStop and OpRemove: end it even though it has changes nobody applied.
	Discard bool `json:"discard,omitempty"`

	// OpDiff.
	StatOnly bool `json:"statOnly,omitempty"`

	// OpApply: the paths from the last review, or All of them but the risky ones.
	Paths []string `json:"paths,omitempty"`
	All   bool     `json:"all,omitempty"`

	// OpCommits.
	Branch string `json:"branch,omitempty"`

	// OpTerm.
	Attach string   `json:"attach,omitempty"`
	Argv   []string `json:"argv,omitempty"`
	Env    []string `json:"env,omitempty"`
	Cols   uint16   `json:"cols,omitempty"`
	Rows   uint16   `json:"rows,omitempty"`
}

// Reply is one JSON line to the client: any number with Progress, then one without. Text
// that came from the guest is escaped before it gets here.
type Reply struct {
	Progress string `json:"progress,omitempty"`
	Error    string `json:"error,omitempty"`

	Session  *Info     `json:"session,omitempty"` // OpRun, OpStart
	Sessions []Info    `json:"sessions,omitempty"`
	Skipped  []Skipped `json:"skipped,omitempty"` // OpRun
	Files    int       `json:"files,omitempty"`   // OpRun
	Bytes    int64     `json:"bytes,omitempty"`   // OpRun

	Items    []DiffItem `json:"items,omitempty"`    // OpDiff
	Outcomes []Outcome  `json:"outcomes,omitempty"` // OpApply

	Branch  string `json:"branch,omitempty"`  // OpCommits
	Commits string `json:"commits,omitempty"` // OpCommits: git's words

	Blocked     []Blocked `json:"blocked,omitempty"`
	BlockedLost uint64    `json:"blockedLost,omitempty"`
}

// Info describes one session.
type Info struct {
	Name     string    `json:"name"`
	Dir      string    `json:"dir"`
	Running  bool      `json:"running"`
	Kept     bool      `json:"kept"`
	Mode     string    `json:"mode"`
	MemoryMB int       `json:"memoryMB"`
	Created  time.Time `json:"created"`
}

// Skipped is a project file left out of the copy.
type Skipped struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// DiffItem is one change in a review. Shown and Text are escaped.
type DiffItem struct {
	Path    string `json:"path"` // what `sandbox apply --path` takes; escaped when not plain
	Shown   string `json:"shown"`
	Kind    string `json:"kind"` // added, modified, deleted
	Risk    string `json:"risk,omitempty"`
	Refused string `json:"refused,omitempty"`
	Added   int    `json:"added,omitempty"`
	Removed int    `json:"removed,omitempty"`
	Binary  bool   `json:"binary,omitempty"`
	Text    string `json:"text,omitempty"`
	// Unshown says Text does not hold the whole change, so `apply --all` leaves it out.
	Unshown bool `json:"unshown,omitempty"`
	// Conflict says the file changed on this computer since the copy.
	Conflict bool `json:"conflict,omitempty"`
}

// Outcome is what apply did to one path.
type Outcome struct {
	Path  string `json:"path"`
	Error string `json:"error,omitempty"`
}

// Blocked is a name or address the session asked for and did not get; Name is escaped.
type Blocked struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Count uint64 `json:"count"`
}

// A session's name goes on the command line, in a file name and in a git branch.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// CheckName checks a session's name.
func CheckName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("the session name %q must be 1 to 32 lower-case letters, digits and hyphens, starting with a letter or digit", name)
	}
	return nil
}

// ErrNoSession is a request for a session that does not exist.
var ErrNoSession = errors.New("no such session")
