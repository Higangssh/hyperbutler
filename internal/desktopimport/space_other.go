//go:build !windows

package desktopimport

// installSpace knows nothing off Windows: no place is checked.
func installSpace(string) Space { return Space{} }
