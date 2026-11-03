//go:build windows

package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/Higangssh/hyperbutler/internal/engine"
)

// guestFSWait is how long start waits for the guest's first file system report when it
// has none to go on. hyperbutlerd's first usable report comes about a second after it
// connects, after dockerd answers: its report at connect is made before the guard's
// first budget and is not published (REQ-DISK-009).
var guestFSWait = 3 * time.Second

// startNotes are what start says once the engine runs, after the context and the docker
// CLI: another engine and which one plain docker reaches (REQ-UX-018), credential
// helpers that are missing (REQ-UX-019), a nearly full guest file system
// (REQ-DISK-009), and a newer release (REQ-UX-022). None of them changes anything.
//
// last is the guest file system as the previous run of the same data disk last reported
// it (lastGuestFS): the disk does not change while no VM runs, so a fresh start judges
// the warning from it instead of waiting for the new VM's first report, which would add
// about 0.4 s to every start. With neither a current report nor last, start waits for
// the report only when wait is set (an existing disk with no report to go on, or an
// engine that was already running). A disk swapped for another while stopped (a backup
// restored) is judged from the old one's report this once: the warning is display only,
// and the next start has the new disk's.
func (in *install) startNotes(st *engine.State, last *engine.GuestFSState, wait bool, stdout, stderr io.Writer) {
	noteOtherEngines(stdout, st.Pipe)
	warnCredHelpers(stderr)
	g := st.GuestFS
	if g == nil {
		g = last
	}
	if g == nil && wait {
		g = in.waitGuestFS(st)
	}
	if g != nil && g.UsedPercent() > guestFullPercent {
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

// lastGuestFS is the last guest file system report of the previous run, from state.json,
// if that run used the data directory this start uses; nil otherwise. Read before the
// engine starts, which replaces state.json.
func (in *install) lastGuestFS() *engine.GuestFSState {
	st, err := engine.LoadState(in.l)
	if err != nil || st.GuestFS == nil || st.DataDir == "" || !sameDir(st.DataDir, in.l.Data) {
		return nil
	}
	return st.GuestFS
}

// sameDir reports whether a and b name the same directory, as Windows compares paths.
func sameDir(a, b string) bool { return strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) }

// autostartTip is the first start's pointer to the autostart setting (S2.3).
const autostartTip = "Tip: `hyperbutler config set autostart true` starts hyperbutler when you sign in to Windows."
