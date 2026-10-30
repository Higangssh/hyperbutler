//go:build !windows

package diag

// osMemory is Windows only.
func osMemory() *OSMemory { return nil }
