package main

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"
)

var procK32GetProcessMemoryInfo = modKernel32.NewProc("K32GetProcessMemoryInfo")

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

// vmmemUsage sums the working set and private bytes of every vmmem process (the host-side
// view of HCS guest memory). Readable without elevation for a Hyper-V Administrators member.
func vmmemUsage() (ws, private uint64, n int, err error) {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, 0, 0, err
	}
	defer syscall.CloseHandle(snap)
	var pe syscall.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = syscall.Process32First(snap, &pe); err == nil; err = syscall.Process32Next(snap, &pe) {
		name := syscall.UTF16ToString(pe.ExeFile[:])
		if !strings.HasPrefix(strings.ToLower(name), "vmmem") {
			continue
		}
		h, oerr := syscall.OpenProcess(0x1000, false, pe.ProcessID) // PROCESS_QUERY_LIMITED_INFORMATION
		if oerr != nil {
			continue
		}
		var c processMemoryCountersEx
		c.cb = uint32(unsafe.Sizeof(c))
		if r, _, _ := procK32GetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&c)), uintptr(c.cb)); r != 0 {
			ws += uint64(c.workingSetSize)
			private += uint64(c.privateUsage)
			n++
		}
		syscall.CloseHandle(h)
	}
	return ws, private, n, nil
}

// printVmmem asks Get-Process, which reads process memory without opening a handle:
// OpenProcess on vmmem fails for a non-elevated caller, so vmmemUsage finds nothing there.
func printVmmem() {
	out, err := exec.Command("powershell.exe", "-NoProfile", "-Command",
		`$p = Get-Process vmmem* -ErrorAction SilentlyContinue; "{0} {1} {2}" -f @($p).Count, ($p | Measure-Object WorkingSet64 -Sum).Sum, ($p | Measure-Object PrivateMemorySize64 -Sum).Sum`).Output()
	if err == nil {
		var n int
		var ws, priv uint64
		if _, err := fmt.Sscan(string(out), &n, &ws, &priv); err == nil {
			fmt.Printf("  vmmem (%d proc): working set %d MiB, private %d MiB\n", n, ws>>20, priv>>20)
			return
		}
	}
	ws, priv, n, err := vmmemUsage()
	if err != nil {
		fmt.Println("  vmmem:", err)
		return
	}
	fmt.Printf("  vmmem (%d proc): working set %d MiB, private %d MiB\n", n, ws>>20, priv>>20)
}
