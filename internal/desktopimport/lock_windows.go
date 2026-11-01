//go:build windows

package desktopimport

import (
	"errors"

	"github.com/Higangssh/hyperbutler/internal/engine"
)

// lockInstall takes the installation's import lock (import.lock in home) without
// waiting; ErrImportRunning when another process holds it. The OS drops it when the
// process ends, however it ends.
func lockInstall(home string) (func(), error) {
	if err := engine.MkdirPrivate(home); err != nil {
		return nil, err
	}
	l, err := engine.TryLock(engine.Layout{Home: home}.ImportLockPath())
	if errors.Is(err, engine.ErrLocked) {
		return nil, ErrImportRunning
	}
	if err != nil {
		return nil, err
	}
	return func() { _ = l.Unlock() }, nil
}
