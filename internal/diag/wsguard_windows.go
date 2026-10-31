//go:build windows

package diag

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

const guardSupported = true

var procSetProcessWorkingSetSizeEx = modkernel32.NewProc("SetProcessWorkingSetSizeEx")

// processMemoryCountersEx2 is PROCESS_MEMORY_COUNTERS_EX2 (Windows 10 1809 and later).
type processMemoryCountersEx2 struct {
	processMemoryCountersEx
	privateWorkingSetSize uintptr
	sharedCommitUsage     uint64
}

// processMemory returns the private working set (the number REQ-MEM-001 counts) and the
// private bytes (commit).
func processMemory() (private, commit uint64, ok bool) {
	var pmc processMemoryCountersEx2
	pmc.cb = uint32(unsafe.Sizeof(pmc))
	if r, _, _ := procK32GetProcessMemInfo.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.cb)); r == 0 {
		return 0, 0, false
	}
	return uint64(pmc.privateWorkingSetSize), uint64(pmc.privateUsage), true
}

// emptyWorkingSet removes every page from the working set, as EmptyWorkingSet does.
func emptyWorkingSet() bool {
	all := ^uintptr(0) // (SIZE_T)-1 for both limits: trim
	r, _, _ := procSetProcessWorkingSetSizeEx.Call(uintptr(windows.CurrentProcess()), all, all, 0)
	return r != 0
}
