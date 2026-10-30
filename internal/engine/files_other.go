//go:build !windows

package engine

func isSharingViolation(error) bool { return false }
