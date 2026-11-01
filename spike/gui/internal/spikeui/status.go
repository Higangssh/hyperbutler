package spikeui

import (
	"encoding/json"
	"fmt"
	"io"
)

// Status is the part of `hyperbutler status --json` (schema 1, cmd/hyperbutler
// StatusJSON) the home screen shows.
type Status struct {
	Schema   int    `json:"schema"`
	Running  bool   `json:"running"`
	SetUp    bool   `json:"setUp"`
	MemoryMB int    `json:"memoryMB"`
	CPUs     int    `json:"cpus"`
	DiskUsed uint64 `json:"diskHostBytes"`
	DiskMax  uint64 `json:"diskSizeBytes"`
	State    *struct {
		Phase string `json:"phase"`
	} `json:"state"`
}

// maxStatus bounds what ParseStatus reads; a status document is about 1 KiB.
const maxStatus = 64 << 10

// ParseStatus decodes one status document.
func ParseStatus(r io.Reader) (Status, error) {
	var s Status
	if err := json.NewDecoder(io.LimitReader(r, maxStatus)).Decode(&s); err != nil {
		return Status{}, fmt.Errorf("status --json: %w", err)
	}
	if s.Schema != 1 {
		return Status{}, fmt.Errorf("status --json: schema %d, want 1", s.Schema)
	}
	return s, nil
}

// View is what both spikes draw: already formatted, already translated.
type View struct {
	Engine  string `json:"engine"`  // "Running", "Stopped", ...
	Live    bool   `json:"live"`    // the engine is running
	Memory  string `json:"memory"`  // big number
	CPUs    string `json:"cpus"`    // big number
	Disk    string `json:"disk"`    // big number
	Note    string `json:"note"`    // a line under the cards, may be empty
	Busy    bool   `json:"busy"`    // a start is in progress
	Start   string `json:"start"`   // the Start button's label
	Loading bool   `json:"loading"` // the first status call has not returned
}

// Initial is the view before the first status call returns.
func Initial(m map[string]string) View {
	return View{Engine: m[KeyChecking], Memory: "-", CPUs: "-", Disk: "-", Start: m[KeyStart], Loading: true}
}

// MakeView formats s; err is the status call's error, if any.
func MakeView(m map[string]string, s Status, err error) View {
	v := View{Memory: "-", CPUs: "-", Disk: "-", Start: m[KeyStart]}
	switch {
	case err != nil:
		v.Engine, v.Note = m[KeyStopped], err.Error()
	case !s.SetUp:
		v.Engine = m[KeyNotSetUp]
	case s.Running:
		v.Engine, v.Live = m[KeyRunning], true
	default:
		v.Engine = m[KeyStopped]
	}
	if s.MemoryMB > 0 {
		v.Memory = fmt.Sprintf("%d %s", s.MemoryMB, m[KeyUnitMB])
	}
	if s.CPUs > 0 {
		v.CPUs = fmt.Sprint(s.CPUs)
	}
	if s.DiskMax > 0 {
		v.Disk = fmt.Sprintf("%.1f %s", float64(s.DiskUsed)/(1<<30), m[KeyUnitGB])
	}
	return v
}
