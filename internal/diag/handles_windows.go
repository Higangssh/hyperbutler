//go:build windows

package diag

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetProcessHandleCount = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessHandleCount")

// handleCount returns the number of open kernel handles in this process, or -1.
func handleCount() int64 {
	var n uint32
	r, _, _ := procGetProcessHandleCount.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		return -1
	}
	return int64(n)
}
