// Package bridge is the GUI's only way from the page to hyperbutler (ADR 0008,
// docs/gui.md): a small, fixed set of typed requests that the page posts as JSON and
// the host answers.
//
// Every message from the page is untrusted input. The page is our own code, but it
// shows text that containers, dockerd and the guest wrote, so the bridge is built for
// a page that has been made to misbehave: Decode refuses anything but one known
// operation with its exact arguments, every argument is checked and bounded, no
// operation takes a path, a command or a URL, nothing deletes, and the settings the
// page may change are a reviewed list. What a compromised page could do is at most
// what the visible buttons do, and the destructive ones are behind a confirmation
// in the page; see docs/gui.md for the review.
package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"unicode/utf8"
)

// MaxMessage bounds one message from the page, in bytes (a daemon.json, at most
// MaxDaemonJSON, escaped, is the largest legitimate one).
const MaxMessage = 512 << 10

// MaxDaemonJSON bounds a daemon.json the page sends: the engine reads at most 64 KiB.
const MaxDaemonJSON = 64 << 10

// Request is one message from the page: {"id": 7, "op": "status", "args": {...}}.
type Request struct {
	ID   int64           `json:"id"`
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

// maxID keeps ids exact in JavaScript numbers.
const maxID = 1<<53 - 1

// The operations. Each has an argument type; ops without arguments take {} or none.
const (
	OpInit           = "init"
	OpReady          = "ready"
	OpLog            = "log"
	OpStatus         = "status"
	OpContainers     = "containers"
	OpContainerDo    = "container.action"
	OpContainerLogs  = "container.logs"
	OpEnginePreview  = "engine.preview"
	OpEngineStart    = "engine.start"
	OpEngineStop     = "engine.stop"
	OpEngineRestart  = "engine.restart"
	OpDockerdRestart = "engine.restartDockerd"
	OpConfigList     = "config.list"
	OpConfigSet      = "config.set"
	OpDaemonGet      = "daemon.get"
	OpDaemonValidate = "daemon.validate"
	OpDaemonSave     = "daemon.save"
	OpStorage        = "storage"
	OpDoctor         = "doctor"
	OpDiagnose       = "diagnose"
	OpDiagnoseReveal = "diagnose.reveal"
	OpErrorCopy      = "error.copy"
	OpLayout         = "layout"
	OpImportPlan     = "import.plan"
	OpImportRun      = "import.run"
	OpImportCancel   = "import.cancel"
)

// RefreshArgs: auto marks a timer's refresh, which the host refuses while the window
// is hidden or minimized (REQ-UX-011: no polling while hidden).
type RefreshArgs struct {
	Auto bool `json:"auto"`
}

// ContainerArgs names a container and what to do with it.
type ContainerArgs struct {
	ID     string `json:"id"`
	Action string `json:"action"`
}

// LogsArgs asks for the last Tail lines of a container's output.
type LogsArgs struct {
	ID   string `json:"id"`
	Tail int    `json:"tail"`
}

// ConfigSetArgs sets one setting.
type ConfigSetArgs struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// DaemonArgs carries a daemon.json text.
type DaemonArgs struct {
	Text string `json:"text"`
}

// LogArgs is a line for the host's log (a script error in the page).
type LogArgs struct {
	Text string `json:"text"`
}

// RefArgs names an earlier error by the ref its response carried.
type RefArgs struct {
	Ref int64 `json:"ref"`
}

// noArgs is the argument type of operations without arguments.
type noArgs struct{}

// argTypes maps each operation to a new value of its argument type.
var argTypes = map[string]func() any{
	OpInit:           func() any { return &noArgs{} },
	OpReady:          func() any { return &noArgs{} },
	OpLog:            func() any { return &LogArgs{} },
	OpStatus:         func() any { return &RefreshArgs{} },
	OpContainers:     func() any { return &RefreshArgs{} },
	OpContainerDo:    func() any { return &ContainerArgs{} },
	OpContainerLogs:  func() any { return &LogsArgs{} },
	OpEnginePreview:  func() any { return &noArgs{} },
	OpEngineStart:    func() any { return &noArgs{} },
	OpEngineStop:     func() any { return &noArgs{} },
	OpEngineRestart:  func() any { return &noArgs{} },
	OpDockerdRestart: func() any { return &noArgs{} },
	OpConfigList:     func() any { return &noArgs{} },
	OpConfigSet:      func() any { return &ConfigSetArgs{} },
	OpDaemonGet:      func() any { return &noArgs{} },
	OpDaemonValidate: func() any { return &DaemonArgs{} },
	OpDaemonSave:     func() any { return &DaemonArgs{} },
	OpStorage:        func() any { return &noArgs{} },
	OpDoctor:         func() any { return &noArgs{} },
	OpDiagnose:       func() any { return &noArgs{} },
	OpDiagnoseReveal: func() any { return &noArgs{} },
	OpErrorCopy:      func() any { return &RefArgs{} },
	OpLayout:         func() any { return &LogArgs{} },
	OpImportPlan:     func() any { return &noArgs{} },
	OpImportRun:      func() any { return &ImportArgs{} },
	OpImportCancel:   func() any { return &noArgs{} },
}

// Bounds of arguments.
const (
	DefaultTail = 200
	MaxTail     = 1000
	maxLogLine  = 2 << 10
	maxKey      = 64
	maxValue    = 1024
)

var reKey = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9.]{0,63}$`)

// ErrBadRequest is a message the bridge refuses.
var ErrBadRequest = errors.New("bad request")

func badf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrBadRequest, fmt.Sprintf(format, a...))
}

// Decode parses and checks one message. It returns the request and its decoded,
// checked arguments. On error the request's id is still set when it could be read, so
// the page can be answered.
func Decode(msg string) (Request, any, error) {
	var r Request
	if len(msg) > MaxMessage {
		return r, nil, badf("message of %d bytes is over %d", len(msg), MaxMessage)
	}
	if !utf8.ValidString(msg) {
		return r, nil, badf("message is not UTF-8")
	}
	if err := strictDecode([]byte(msg), &r); err != nil {
		// Read the id alone, to answer.
		var idOnly struct {
			ID int64 `json:"id"`
		}
		if json.Unmarshal([]byte(msg), &idOnly) == nil && idOnly.ID > 0 && idOnly.ID <= maxID {
			r = Request{ID: idOnly.ID}
		} else {
			r = Request{}
		}
		return r, nil, badf("%v", err)
	}
	if r.ID <= 0 || r.ID > maxID {
		return Request{}, nil, badf("id %d is outside 1..2^53-1", r.ID)
	}
	mk, ok := argTypes[r.Op]
	if !ok {
		return r, nil, badf("unknown operation %.40q", r.Op)
	}
	args := mk()
	if len(bytes.TrimSpace(r.Args)) > 0 && !bytes.Equal(bytes.TrimSpace(r.Args), []byte("null")) {
		if err := strictDecode(r.Args, args); err != nil {
			return r, nil, badf("%s: arguments: %v", r.Op, err)
		}
	}
	if err := checkArgs(r.Op, args); err != nil {
		return r, nil, badf("%s: %v", r.Op, err)
	}
	return r, args, nil
}

// strictDecode decodes exactly one JSON value into v, refusing unknown fields and
// trailing data.
func strictDecode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}

// containerActions are the actions container.action takes. There is no remove.
var containerActions = map[string]bool{"start": true, "stop": true, "restart": true}

// reContainerID is a container id as the containers operation returns it: 64 hex
// digits. The page names a container only by an id it was given.
var reContainerID = regexp.MustCompile(`^[0-9a-f]{64}$`)

func checkArgs(op string, args any) error {
	switch a := args.(type) {
	case *ContainerArgs:
		if !reContainerID.MatchString(a.ID) {
			return errors.New("id: want a container id (64 hex digits)")
		}
		if !containerActions[a.Action] {
			return fmt.Errorf("action %.20q: want start, stop or restart", a.Action)
		}
	case *LogsArgs:
		if !reContainerID.MatchString(a.ID) {
			return errors.New("id: want a container id (64 hex digits)")
		}
		if a.Tail == 0 {
			a.Tail = DefaultTail
		}
		if a.Tail < 1 || a.Tail > MaxTail {
			return fmt.Errorf("tail %d is outside 1..%d", a.Tail, MaxTail)
		}
	case *ConfigSetArgs:
		if !reKey.MatchString(a.Key) {
			return errors.New("key: not a setting name")
		}
		if len(a.Value) > maxValue || !printableValue(a.Value) {
			return fmt.Errorf("value: at most %d bytes of printable text", maxValue)
		}
	case *DaemonArgs:
		if len(a.Text) > MaxDaemonJSON {
			return fmt.Errorf("daemon.json of %d bytes is over %d", len(a.Text), MaxDaemonJSON)
		}
	case *LogArgs:
		if len(a.Text) > maxLogLine {
			a.Text = a.Text[:maxLogLine]
		}
	case *ImportArgs:
		return checkImportArgs(a)
	case *RefArgs:
		if a.Ref <= 0 || a.Ref > maxID {
			return errors.New("ref: not an error reference")
		}
	}
	return nil
}

// printableValue refuses control characters (a newline or NUL in a setting value).
func printableValue(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
