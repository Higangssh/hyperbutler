//go:build windows

package diag

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modkernel32              = windows.NewLazySystemDLL("kernel32.dll")
	procK32GetProcessMemInfo = modkernel32.NewProc("K32GetProcessMemoryInfo")
	procGetProcessHeaps      = modkernel32.NewProc("GetProcessHeaps")
	procHeapSummary          = modkernel32.NewProc("HeapSummary")
)

// processMemoryCountersEx is PROCESS_MEMORY_COUNTERS_EX.
type processMemoryCountersEx struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
	privateUsage               uintptr
}

// heapSummary is HEAP_SUMMARY.
type heapSummary struct {
	cb            uint32
	cbAllocated   uintptr
	cbCommitted   uintptr
	cbReserved    uintptr
	cbMaxReserved uintptr
}

// maxHeaps bounds the heap handles read; a process has a handful.
const maxHeaps = 256

// osMemory reads the process's committed private memory and its Windows heaps. The
// heaps hold what DLLs (HCS, Winsock, ...) allocate with HeapAlloc or malloc, which the
// Go runtime does not see.
func osMemory() *OSMemory {
	var pmc processMemoryCountersEx
	pmc.cb = uint32(unsafe.Sizeof(pmc))
	if r, _, _ := procK32GetProcessMemInfo.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.cb)); r == 0 {
		return nil
	}
	m := &OSMemory{PrivateBytes: uint64(pmc.privateUsage), WorkingSet: uint64(pmc.workingSetSize)}
	var heaps [maxHeaps]windows.Handle
	n, _, _ := procGetProcessHeaps.Call(maxHeaps, uintptr(unsafe.Pointer(&heaps[0])))
	if n == 0 || n > maxHeaps {
		// More heaps than the buffer holds: report none rather than a partial sum.
		m.Heaps = -1
		return m
	}
	m.Heaps = int(n)
	for _, h := range heaps[:n] {
		s := heapSummary{cb: uint32(unsafe.Sizeof(heapSummary{}))}
		if r, _, _ := procHeapSummary.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&s))); r == 0 {
			continue
		}
		m.HeapAllocated += uint64(s.cbAllocated)
		m.HeapCommitted += uint64(s.cbCommitted)
	}
	return m
}
