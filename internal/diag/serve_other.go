//go:build !windows

package diag

import (
	"errors"
	"runtime"
)

// Serve is only implemented on Windows, where the endpoint is a current-user-only named
// pipe. Guest daemons get ApplyMemoryLimit and Snapshot but no endpoint yet.
func Serve(string) (*Server, error) {
	return nil, errors.New("diag: debug endpoint is not supported on " + runtime.GOOS)
}
