package desktopimport

import (
	"errors"
	"fmt"

	"github.com/Higangssh/hyperbutler/internal/desktopimport/source"
)

// runToken is one import run's id (Plan's or Run's), kept alive while the run goes on
// (holdRun). Every helper container the run creates, in either engine, and every volume
// it creates in hyperbutler, is labeled with the id (source.RunLabel). Another import,
// of this installation or another one, a GUI's or a CLI's, removes a helper or a
// volume only when its run is no longer alive (runAlive), so it cannot remove a running
// import's work.
type runToken struct {
	id      string
	release func()
}

// errRunTaken is a run id another run holds already (never in practice: 64 random bits).
var errRunTaken = errors.New("the import run id is taken")

func newRun() (*runToken, error) {
	var err error
	for range 3 {
		id := source.NewRunID()
		var rel func()
		if rel, err = holdRun(id); err == nil {
			return &runToken{id: id, release: rel}, nil
		}
		if !errors.Is(err, errRunTaken) {
			break
		}
	}
	return nil, fmt.Errorf("start the import run: %w", err)
}

// done ends the run: from now on its helpers and volumes count as left behind.
func (r *runToken) done() {
	if r != nil && r.release != nil {
		r.release()
		r.release = nil
	}
}
