//go:build !windows

package diag

import "os"

// handleCount returns the number of open file descriptors in this process (Linux), or
// -1 where /proc/self/fd does not exist.
func handleCount() int64 {
	f, err := os.Open("/proc/self/fd")
	if err != nil {
		return -1
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return -1
	}
	// One of the entries is the descriptor opened above to read the directory.
	return int64(len(names)) - 1
}
