//go:build windows

package netbench

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	procQPC  = kernel32.NewProc("QueryPerformanceCounter")
	procQPF  = kernel32.NewProc("QueryPerformanceFrequency")
)

// Go's monotonic clock on Windows follows the interrupt time, which advances in steps
// of about 0.5 ms: too coarse for round trips of tens of microseconds. Use the
// performance counter instead.
func init() {
	var freq int64
	if r, _, _ := procQPF.Call(uintptr(unsafe.Pointer(&freq))); r == 0 || freq <= 0 {
		return
	}
	Clock = func() int64 {
		var v int64
		_, _, _ = procQPC.Call(uintptr(unsafe.Pointer(&v)))
		return int64(float64(v) * 1e9 / float64(freq))
	}
}
