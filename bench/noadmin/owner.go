package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// OwnerSchema is the version of owner.json.
const OwnerSchema = 1

// Owner is owner.json: what the second user probes. It holds names and ids only, no
// secret (the VM secret never leaves the owner's engine and broker).
type Owner struct {
	Schema   int       `json:"schema"`
	Recorded time.Time `json:"recorded"`
	User     string    `json:"user"`
	SID      string    `json:"sid"`
	// DockerPipe is the engine's Docker API pipe, DebugPipe its pprof pipe if it serves
	// one (off by default).
	DockerPipe string `json:"dockerPipe"`
	DebugPipe  string `json:"debugPipe,omitempty"`
	// EnginePID and EnginePIDCreated identify the engine process, and EngineEvents are
	// the named events through which the CLI controls it (stop, daemon.json apply,
	// settings reload): the engine serves no control pipe.
	EnginePID        int      `json:"enginePid"`
	EnginePIDCreated int64    `json:"enginePidCreated"`
	EngineEvents     []string `json:"engineEvents"`
	// VMName is the compute system's name, VMRuntimeID the id AF_HYPERV addresses.
	VMName      string `json:"vmName"`
	VMRuntimeID string `json:"vmRuntimeId"`
	// BrokerShared is the broker's shared endpoint, BrokerPipes the owner's own
	// endpoints next to it (ADR 0004 rule 5), as full pipe paths.
	BrokerShared string   `json:"brokerShared"`
	BrokerPipes  []string `json:"brokerPipes"`
	Version      string   `json:"engineVersion,omitempty"`
}

// statusDoc is the part of `hyperbutler status --json` that record reads.
type statusDoc struct {
	Running bool `json:"running"`
	State   *struct {
		Phase             string `json:"phase"`
		PID               int    `json:"pid"`
		PIDCreated        int64  `json:"pidCreated"`
		VMID              string `json:"vmId"`
		RuntimeID         string `json:"runtimeId"`
		Pipe              string `json:"pipe"`
		ExecutableVersion string `json:"executableVersion"`
	} `json:"state"`
}

// ownerFromStatus fills the engine and VM fields of an Owner from the JSON of
// `hyperbutler status --json`. The engine must be running.
func ownerFromStatus(b []byte) (Owner, error) {
	var s statusDoc
	if err := json.Unmarshal(b, &s); err != nil {
		return Owner{}, fmt.Errorf("status --json: %w", err)
	}
	st := s.State
	switch {
	case !s.Running || st == nil:
		return Owner{}, errors.New("hyperbutler is not running for this user (status --json: running false)")
	case st.Phase != "running":
		return Owner{}, fmt.Errorf("the engine is %q, not running", st.Phase)
	case st.PID <= 0 || st.PIDCreated == 0:
		return Owner{}, errors.New("status --json has no engine pid")
	case st.RuntimeID == "":
		return Owner{}, errors.New("status --json has no VM runtime id")
	case st.Pipe == "":
		return Owner{}, errors.New("status --json has no Docker API pipe")
	}
	return Owner{
		Schema:           OwnerSchema,
		DockerPipe:       st.Pipe,
		EnginePID:        st.PID,
		EnginePIDCreated: st.PIDCreated,
		EngineEvents:     engineEventNames(st.PID, st.PIDCreated),
		VMName:           st.VMID,
		VMRuntimeID:      st.RuntimeID,
		Version:          st.ExecutableVersion,
	}, nil
}

// engineEventNames are the engine's named events, as internal/engine (proc_windows.go,
// stopEventName and its siblings) names them.
func engineEventNames(pid int, created int64) []string {
	var out []string
	for _, what := range []string{"stop", "daemon", "settings"} {
		out = append(out, fmt.Sprintf(`Local\hyperbutler-engine-%s-%d-%x`, what, pid, created))
	}
	return out
}

// check is one line of a subcommand's report.
type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}
