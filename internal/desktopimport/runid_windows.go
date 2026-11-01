//go:build windows

package desktopimport

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// runMutexNames are the named mutexes that say a run is alive: one in the Global
// namespace (seen from every session), and in the session's Local namespace when the
// Global one cannot be created. The process holds a handle for the run's time; the
// mutex goes when the handle is closed, also when the process is killed, so a killed
// run never looks alive.
func runMutexNames(id string) []string {
	return []string{`Global\hyperbutler-import-` + id, `Local\hyperbutler-import-` + id}
}

func holdRun(id string) (func(), error) {
	var last error
	for _, n := range runMutexNames(id) {
		p, err := windows.UTF16PtrFromString(n)
		if err != nil {
			return nil, err
		}
		h, err := windows.CreateMutex(nil, false, p)
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			_ = windows.CloseHandle(h)
			return nil, errRunTaken
		}
		if err != nil {
			last = err
			continue
		}
		return func() { _ = windows.CloseHandle(h) }, nil
	}
	return nil, fmt.Errorf("create the import run's mutex: %w", last)
}

// runAlive reports whether the run with this id is going on in any process of this
// machine (any session for the Global name, this session for the Local one).
func runAlive(id string) bool {
	for _, n := range runMutexNames(id) {
		p, err := windows.UTF16PtrFromString(n)
		if err != nil {
			return false
		}
		h, err := windows.OpenMutex(windows.SYNCHRONIZE, false, p)
		if err == nil {
			_ = windows.CloseHandle(h)
			return true
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return true // it exists; another user's import
		}
	}
	return false
}
