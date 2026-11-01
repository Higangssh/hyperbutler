//go:build windows

package main

import (
	"fmt"
	"io"
	"time"

	"github.com/Higangssh/hyperbutler/internal/engine"
)

// guestFSWait is how long start waits for the guest's first file system report, which
// normally arrives before dockerd answers.
var guestFSWait = 3 * time.Second

// startNotes are what start says once the engine runs, after the context and the docker
// CLI: another engine and which one plain docker reaches (REQ-UX-018), credential
// helpers that are missing (REQ-UX-019), a nearly full guest file system
// (REQ-DISK-009), and a newer release (REQ-UX-022). None of them changes anything.
func (in *install) startNotes(st *engine.State, stdout, stderr io.Writer) {
	noteOtherEngines(stdout, st.Pipe)
	warnCredHelpers(stderr)
	if g := in.waitGuestFS(st); g != nil && g.UsedPercent() > guestFullPercent {
		_, _ = fmt.Fprintf(stderr, "hyperbutler: warning: the data disk is nearly full: %s. Builds and pulls fail with \"no space left on device\" when it is.\n"+
			"        docker system prune (and docker builder prune) frees space; growing the disk is not supported yet.\n", guestFSLine(g))
	}
	noteUpdate(stdout, in.checkUpdate(stderr))
}

// waitGuestFS returns the guest file system report of the engine st describes, waiting
// up to guestFSWait for it.
func (in *install) waitGuestFS(st *engine.State) *engine.GuestFSState {
	deadline := time.Now().Add(guestFSWait)
	for {
		if st != nil && st.GuestFS != nil {
			return st.GuestFS
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
		cur, err := engine.LoadState(in.l)
		if err != nil || st == nil || cur.PID != st.PID || cur.PIDCreated != st.PIDCreated {
			continue
		}
		st = cur
	}
}

// autostartTip is the first start's pointer to the autostart setting (S2.3).
const autostartTip = "Tip: `hyperbutler config set autostart true` starts hyperbutler when you sign in to Windows."
