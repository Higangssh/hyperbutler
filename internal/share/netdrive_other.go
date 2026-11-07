//go:build !windows

package share

// checkDriveOf has no drive letters to look at away from Windows.
func checkDriveOf(string) error { return nil }
