//go:build windows

package broker

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// memoryStatusEx is MEMORYSTATUSEX.
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

var procGlobalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

// physicalMemoryMB is the host's physical memory in MiB, or 0 if it cannot be read.
func physicalMemoryMB() uint64 {
	m := memoryStatusEx{length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return 0
	}
	return m.totalPhys >> 20
}

// memoryCapShare is the share of physical memory one user's running VMs may take
// together (ADR 0009: sessions cannot starve the host), in percent.
const memoryCapShare = 75

// totalMemoryCap is the service's Config.MaxTotalMemoryMB: memoryCapShare of physical
// memory, never below the Docker VM's default (so a small host still starts it) nor
// above the default per-VM maximum. 0 (the server's default) if memory cannot be read.
func totalMemoryCap(infof func(string, ...any)) uint32 {
	return memoryCap(physicalMemoryMB(), infof)
}

func memoryCap(physMB uint64, infof func(string, ...any)) uint32 {
	const floor, ceiling = 2048, 16 << 10 // New's DefaultMemory and MaxMemoryMB defaults
	if physMB == 0 {
		infof("broker: physical memory unknown; one user's VMs are capped at %d MB together", ceiling)
		return 0
	}
	c := physMB * memoryCapShare / 100
	c = max(c, floor)
	c = min(c, ceiling)
	infof("broker: one user's VMs are capped at %d MB together (%d MB physical)", c, physMB)
	return uint32(c) //nolint:gosec // G115: c <= ceiling
}
