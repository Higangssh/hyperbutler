//go:build !windows

package diag

const guardSupported = false

func processMemory() (private, commit uint64, ok bool) { return 0, 0, false }

func emptyWorkingSet() bool { return false }
