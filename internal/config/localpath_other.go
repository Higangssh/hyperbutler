//go:build !windows

package config

// remoteDrive is false: there are no drive letters.
func remoteDrive(string) bool { return false }
