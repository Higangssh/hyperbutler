//go:build !windows

package copyin

// isLink: outside Windows every irregular file is treated as a link.
func isLink(string) bool { return true }
