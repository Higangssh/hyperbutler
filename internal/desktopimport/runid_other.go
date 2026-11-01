//go:build !windows

package desktopimport

import "sync"

// Off Windows (the package's tests) a run is alive while this process holds it.
var (
	runsMu sync.Mutex
	runs   = map[string]bool{}
)

func holdRun(id string) (func(), error) {
	runsMu.Lock()
	defer runsMu.Unlock()
	if runs[id] {
		return nil, errRunTaken
	}
	runs[id] = true
	return func() {
		runsMu.Lock()
		delete(runs, id)
		runsMu.Unlock()
	}, nil
}

func runAlive(id string) bool {
	runsMu.Lock()
	defer runsMu.Unlock()
	return runs[id]
}
