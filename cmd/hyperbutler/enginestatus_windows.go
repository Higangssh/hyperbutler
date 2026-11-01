//go:build windows

package main

import (
	"fmt"
	"io"
	"time"

	"github.com/Higangssh/hyperbutler/internal/engine"
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
