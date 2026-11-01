//go:build windows

package main

import (
	"errors"

	"github.com/Higangssh/hyperbutler/internal/engine"
)

// errImportRunning is the refusal of a second import while one runs.
var errImportRunning = errors.New("another import is running in this hyperbutler installation; wait for it to finish (each import removes the helper containers it finds, so two at once would remove each other's)")

// importLock takes the installation's import lock without waiting: every import
// removes the helper containers it finds left over, in Docker Desktop and in
// hyperbutler, so a second one at the same time would remove the first one's.
func (in *install) importLock() (*engine.FileLock, error) {
	if err := engine.MkdirPrivate(in.l.Home); err != nil {
		return nil, err
	}
	l, err := engine.TryLock(in.l.ImportLockPath())
	if errors.Is(err, engine.ErrLocked) {
		return nil, errImportRunning
	}
	return l, err
}
