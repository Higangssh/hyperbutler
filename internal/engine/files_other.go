//go:build !windows

package engine

func isSharingViolation(error) bool { return false }

// restrictToUser does nothing more: the file was created 0600.
func restrictToUser(string) error { return nil }
