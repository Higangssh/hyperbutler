//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/Microsoft/go-winio"

	"github.com/Higangssh/hyperbutler/internal/dockerapi"
	"github.com/Higangssh/hyperbutler/internal/engine"
)

// REQ-UX-017: before a stop or restart of the VM or of dockerd, say which running
// containers stop and which of them come back.

// RunningContainer is a running container that a stop or restart is about to stop.
type RunningContainer = dockerapi.RunningContainer

// comesBack reports whether a container with restart policy p is started again when
// dockerd starts after a stop.
func comesBack(p string) bool { return dockerapi.ComesBack(p) }

// afterText says what happens to c, for the list.
func afterText(c RunningContainer) string {
	switch {
	case c.ComesBack:
		return "comes back (restart " + c.RestartPolicy + ")"
	case c.MayComeBack():
		return "comes back only if it stops with an error, as when killed (restart on-failure)"
	case c.RestartPolicy == "" || c.RestartPolicy == "no":
		return "stays stopped (no restart policy)"
	}
	return "stays stopped (restart " + c.RestartPolicy + ")"
}

// containerAPITimeout bounds the whole query: a paused or hung VM must not hold up a stop.
const containerAPITimeout = 5 * time.Second

// listRunningContainers asks the engine's Docker API on pipe (this machine's named pipe,
// which the engine relays to the guest's dockerd) for the running containers and their
// restart policies.
func listRunningContainers(ctx context.Context, pipe string) ([]RunningContainer, error) {
	ctx, cancel := context.WithTimeout(ctx, containerAPITimeout)
	defer cancel()
	d := dockerapi.New(func(ctx context.Context) (net.Conn, error) { return winio.DialPipeContext(ctx, pipe) })
	defer d.Close()
	return dockerapi.ListRunning(ctx, d)
}

// containerName is a container's name without the leading "/", or its short id.
func containerName(id string, names []string) string { return dockerapi.ContainerName(id, names) }

// writeContainerList prints what a stop or restart does to the running containers: what
// is the event ("hyperbutler stop", "dockerd restarts"), later when they come back ("at
// the next start", "after the restart"). err is why the list is unknown.
func writeContainerList(w io.Writer, what, later string, list []RunningContainer, err error) {
	if err != nil {
		_, _ = fmt.Fprintf(w, "%s: could not list the running containers (%v); any that run stop too.\n", what, err)
		return
	}
	if len(list) == 0 {
		return
	}
	back, maybe := 0, 0
	for _, c := range list {
		switch {
		case c.ComesBack:
			back++
		case c.MayComeBack():
			maybe++
		}
	}
	stay := len(list) - back - maybe
	s := fmt.Sprintf("%s: %d %s stop; %d come%s back %s", what,
		len(list), plural(len(list), "running container", "running containers"), back, verbS(back), later)
	if maybe > 0 {
		s += fmt.Sprintf(", %d only if %s with an error (on-failure)", maybe, plural(maybe, "it stops", "they stop"))
	}
	_, _ = fmt.Fprintf(w, "%s, %d stay%s stopped.\n", s, stay, verbS(stay))
	t := newTable(w, "  CONTAINER", "IMAGE", "THEN")
	for _, c := range list {
		t.row("  "+c.Name, c.Image, afterText(c))
	}
	t.flush()
}

// verbS is the third-person "s" for a count of n.
func verbS(n int) string {
	if n == 1 {
		return "s"
	}
	return ""
}

// noteContainers lists the running containers of the engine described by st before a
// stop or restart, and returns the list (nil when unknown or none). It never blocks the
// stop: an engine whose API does not answer is said so.
func noteContainers(st *engine.State, w io.Writer, what, later string) []RunningContainer {
	if st == nil || st.Phase != engine.PhaseRunning || st.Pipe == "" {
		return nil
	}
	list, err := listRunningContainers(context.Background(), st.Pipe)
	if errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("the Docker API did not answer within %v", containerAPITimeout)
	}
	writeContainerList(w, what, later, list, err)
	return list
}
