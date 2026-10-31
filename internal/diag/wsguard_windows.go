//go:build windows

package diag

import (
	"encoding/binary"
	"unsafe"

	"golang.org/x/sys/windows"
)

const guardSupported = true

var (
	procSetProcessWorkingSetSizeEx = modkernel32.NewProc("SetProcessWorkingSetSizeEx")
	procGetModuleHandleW           = modkernel32.NewProc("GetModuleHandleW")
	procQueryWorkingSetEx          = windows.NewLazySystemDLL("psapi.dll").NewProc("QueryWorkingSetEx")
)

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

// maxDataPages bounds the data section the reader walks (256 MiB).
const maxDataPages = 1 << 16

// wsExEntry is PSAPI_WORKING_SET_EX_INFORMATION with the address as a uintptr.
type wsExEntry struct {
	va   uintptr
	attr uintptr // ULONG_PTR
}

// dataSectionReader returns a function that counts the resident private bytes of this
// executable's .data section (initialized data and BSS), or nil if the section cannot
// be found. The section table is read from the loaded image's own headers (no file
// I/O, no symbol table). QueryWorkingSetEx reads the page tables and touches no page.
func dataSectionReader() func() (uint64, bool) {
	base, _, _ := procGetModuleHandleW.Call(0)
	if base == 0 {
		return nil
	}
	rva, size, ok := dataSection(base)
	if !ok {
		return nil
	}
	const page = 4096
	start := (base + uintptr(rva)) &^ (page - 1)
	pages := (base + uintptr(rva) + uintptr(size) - start + page - 1) / page
	if pages > maxDataPages {
		return nil
	}
	buf := make([]wsExEntry, pages)
	return func() (uint64, bool) {
		for i := range buf {
			buf[i] = wsExEntry{va: start + uintptr(i)*page}
		}
		r, _, _ := procQueryWorkingSetEx.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))*unsafe.Sizeof(buf[0]))
		if r == 0 {
			return 0, false
		}
		var n uint64
		for _, e := range buf {
			// Valid and not shared (bit 15): a private resident page.
			if e.attr&1 == 1 && e.attr&(1<<15) == 0 {
				n += page
			}
		}
		return n, true
	}
}

// dataSection finds .data in the PE headers of the image loaded at base.
func dataSection(base uintptr) (rva, size uint32, ok bool) {
	var h [4096]byte
	var n uintptr
	if windows.ReadProcessMemory(windows.CurrentProcess(), base, &h[0], uintptr(len(h)), &n) != nil || n != uintptr(len(h)) {
		return 0, 0, false
	}
	return parseDataSection(h[:])
}

// parseDataSection reads the .data section's RVA and size from PE headers.
func parseDataSection(h []byte) (rva, size uint32, ok bool) {
	le := binary.LittleEndian
	if len(h) < 0x40 || h[0] != 'M' || h[1] != 'Z' {
		return 0, 0, false
	}
	nt := int(le.Uint32(h[0x3c:]))
	if nt < 0 || nt+24 > len(h) || le.Uint32(h[nt:]) != 0x4550 {
		return 0, 0, false
	}
	count := int(le.Uint16(h[nt+6:]))
	sec := nt + 24 + int(le.Uint16(h[nt+20:]))
	for i := 0; i < count; i++ {
		o := sec + 40*i
		if o+40 > len(h) {
			return 0, 0, false
		}
		if string(h[o:o+8]) == ".data\x00\x00\x00" {
			vs, raw := le.Uint32(h[o+8:]), le.Uint32(h[o+16:])
			return le.Uint32(h[o+12:]), max(vs, raw), true
		}
	}
	return 0, 0, false
}
