//go:build windows

package config

import "golang.org/x/sys/windows"

// remoteDrive reports whether root (`X:\`) is a network drive (a mapped share).
func remoteDrive(root string) bool {
	p, err := windows.UTF16PtrFromString(root)
	return err == nil && windows.GetDriveType(p) == windows.DRIVE_REMOTE
}
