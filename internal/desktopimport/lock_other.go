//go:build !windows

package desktopimport

import (
	"path/filepath"
	"sync"
)

// Off Windows (the package's tests) the lock is held within this process.
var (
	locksMu sync.Mutex
	locks   = map[string]bool{}
)

func lockInstall(home string) (func(), error) {
	p := filepath.Clean(home)
	locksMu.Lock()
	defer locksMu.Unlock()
	if locks[p] {
		return nil, ErrImportRunning
	}
	locks[p] = true
	return func() {
		locksMu.Lock()
		delete(locks, p)
		locksMu.Unlock()
	}, nil
}
