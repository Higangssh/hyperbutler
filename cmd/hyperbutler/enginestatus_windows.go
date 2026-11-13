//go:build windows

package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/socktable"
)

// guardPauseStatus is the free-space guard's pause of the running engine (REQ-DISK-010),
// with the free space measured now (hostFree, 0 if unknown: then the engine's last
// figure), or nil when the VM is not paused.
func guardPauseStatus(st *engine.State, running bool, hostFree uint64) *GuardPauseJSON {
	if st == nil || !running || !st.Phase.Live() || st.GuardPause == nil {
		return nil
	}
	p := st.GuardPause
	free := hostFree
	if free == 0 {
		free = p.FreeBytes
	}
	return &GuardPauseJSON{Volume: p.Volume, FreeBytes: free, ResumeAtBytes: p.ResumeAtBytes, Since: p.Since}
}

// writeEngineStatus prints the status lines about the running engine itself: a pause by
// the free-space guard, and which hyperbutler.exe runs (REQ-PLAT-008).
func writeEngineStatus(w io.Writer, r StatusJSON) {
	if p := r.GuardPause; p != nil {
		_, _ = fmt.Fprintf(w, "  paused:  %s\n", pauseText(p))
	}
	if st := r.State; st != nil && st.Executable != "" {
		v := st.ExecutableVersion
		if v == "" {
			v = "unknown version"
		}
		_, _ = fmt.Fprintf(w, "  engine:  %s (%s)\n", printable([]byte(st.Executable)), printable([]byte(v)))
	}
}

// pauseText says why the VM is paused and when it resumes: "C: has 900.0 MiB free; it
// resumes at 1.2 GiB free".
func pauseText(p *GuardPauseJSON) string {
	s := fmt.Sprintf("%s has %s free; it resumes at %s free", printable([]byte(p.Volume)), engine.HumanSize(p.FreeBytes), engine.HumanSize(p.ResumeAtBytes))
	if !p.Since.IsZero() {
		s += fmt.Sprintf(" (the free-space guard paused the VM at %s; docker commands wait until then)", p.Since.Local().Format(time.DateTime))
	}
	return s
}

// sharedPorts lists other programs' listeners on the ports the running engine of st
// publishes (issue #2), and how many more there were. It lists nothing when st's PID is
// no longer that engine (PIDCreated) or the socket table cannot be read.
func sharedPorts(st *engine.State) ([]SharedPortJSON, int) {
	if st == nil || st.PID <= 0 || len(st.PublishedPorts) == 0 {
		return nil, 0
	}
	if t, err := engine.CreatedTime(st.PID); err != nil || t != st.PIDCreated {
		return nil, 0
	}
	var tr socktable.Reader
	ss, err := tr.TCPListeners()
	if err != nil {
		return nil, 0
	}
	found, more := engine.SharedPorts(ss, uint32(st.PID), st.PublishedPorts) //nolint:gosec // G115: Windows process IDs are DWORDs
	var out []SharedPortJSON
	for _, s := range found {
		j := SharedPortJSON{Port: s.Port, PID: s.PID, Process: engine.ProcessName(s.PID)}
		for _, a := range s.Listen {
			j.Listen = append(j.Listen, a.String())
		}
		out = append(out, j)
	}
	return out, more
}

// maxProcessName bounds the process name in a status line.
const maxProcessName = 64

// sharedPortText is the status line for s.
func sharedPortText(s SharedPortJSON) string {
	who := fmt.Sprintf("process %d (tasklist /fi \"PID eq %d\" names it)", s.PID, s.PID)
	if s.Process != "" {
		name := []rune(string(printable([]byte(s.Process))))
		if len(name) > maxProcessName {
			name = append(name[:maxProcessName-1], '…')
		}
		who = fmt.Sprintf("%s (process %d)", string(name), s.PID)
	}
	addrs := make([]string, len(s.Listen))
	for i, a := range s.Listen {
		addrs[i] = string(printable([]byte(a)))
	}
	return fmt.Sprintf("%d is also listened on by %s on %s; localhost reaches hyperbutler's container, not that program",
		s.Port, who, strings.Join(addrs, " and "))
}
