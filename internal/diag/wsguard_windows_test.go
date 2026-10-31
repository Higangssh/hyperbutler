//go:build windows

package diag

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// untouched is demand-zero data, like the engine's untouched data-section variables.
var untouched [16 << 20]byte

// TestEmptyWorkingSet: reading 16 MiB of untouched data, as an outside scan does, raises
// the private working set by that much without raising the commit; emptying the working
// set takes it back out.
func TestEmptyWorkingSet(t *testing.T) {
	before, c0, ok := processMemory()
	if !ok || before == 0 || c0 < before {
		t.Fatalf("processMemory = %d, %d, %v", before, c0, ok)
	}
	var sum byte
	for i := 0; i < len(untouched); i += 4096 {
		sum += untouched[i]
	}
	touched, c1, _ := processMemory()
	if touched < before+15<<20 || c1 > c0+2<<20 {
		t.Fatalf("reading 16 MiB: working set %d -> %d, commit %d -> %d (sum %d)", before, touched, c0, c1, sum)
	}
	if !emptyWorkingSet() {
		t.Fatal("emptyWorkingSet failed")
	}
	after, _, _ := processMemory()
	if after > touched-15<<20 {
		t.Fatalf("after the trim: %d bytes, %d before it", after, touched)
	}
	t.Logf("private working set %d -> %d -> %d KiB, commit %d KiB", before>>10, touched>>10, after>>10, c1>>10)
}

var (
	modkernel32t  = windows.NewLazySystemDLL("kernel32.dll")
	procHeapAlloc = modkernel32t.NewProc("HeapAlloc")
	procHeapFree  = modkernel32t.NewProc("HeapFree")
	procGetHeap   = modkernel32t.NewProc("GetProcessHeap")
	procFill      = modkernel32t.NewProc("RtlFillMemory")
)

// TestGuardNativeGrowth: 32 MiB a DLL would allocate and write (invisible to the Go
// runtime) raises the commit with the working set, so the guard's rule does not fire.
func TestGuardNativeGrowth(t *testing.T) {
	p0, c0, _ := processMemory()
	heap, _, _ := procGetHeap.Call()
	const n = 32 << 20
	m, _, _ := procHeapAlloc.Call(heap, 0, n)
	if m == 0 {
		t.Fatal("HeapAlloc failed")
	}
	defer procHeapFree.Call(heap, 0, m)
	procFill.Call(m, n, 1) // writes every page, as the DLL would
	p1, c1, _ := processMemory()
	if p1 < p0+30<<20 {
		t.Fatalf("working set %d -> %d after writing 32 MiB", p0, p1)
	}
	g := &guardState{window: GuardWindow, minGap: GuardMinGap, rise: GuardRise, slack: GuardSlack, bigRise: GuardBigRise, flat: GuardFlat}
	t0 := time.Now()
	g.observe(guardSample{at: t0, private: p0, commit: c0}, nil)
	if r := g.observe(guardSample{at: t0.Add(GuardInterval), private: p1, commit: c1}, func() (uint64, bool) { return 0, true }); r != "" {
		t.Fatalf("native growth looks like a scan: working set %d -> %d, commit %d -> %d", p0, p1, c0, c1)
	}
}

// untouchedData is demand-zero data for TestDataSectionReader only.
var untouchedData [24 << 20]byte

// TestDataSectionReader: reading 24 MiB of this executable's untouched BSS raises the
// resident bytes the reader counts by that much.
func TestDataSectionReader(t *testing.T) {
	read := dataSectionReader()
	if read == nil {
		t.Fatal("no data section reader")
	}
	before, ok := read()
	if !ok {
		t.Fatal("read failed")
	}
	var sum byte
	for i := 0; i < len(untouchedData); i += 4096 {
		sum += untouchedData[i]
	}
	after, _ := read()
	if after < before+23<<20 {
		t.Fatalf("data section resident %d -> %d bytes after reading 24 MiB (sum %d)", before, after, sum)
	}
	t.Logf("data section resident %d -> %d KiB", before>>10, after>>10)
}

func TestParseDataSectionRejects(t *testing.T) {
	base, _, _ := procGetModuleHandleW.Call(0)
	var h [4096]byte
	var n uintptr
	if err := windows.ReadProcessMemory(windows.CurrentProcess(), base, &h[0], uintptr(len(h)), &n); err != nil {
		t.Fatal(err)
	}
	if _, size, ok := parseDataSection(h[:]); !ok || size == 0 {
		t.Fatalf("own headers: size %d, ok %v", size, ok)
	}
	nt := int(h[0x3c]) | int(h[0x3d])<<8
	for name, b := range map[string][]byte{
		"empty":         nil,
		"not MZ":        append([]byte("XX"), h[2:]...),
		"truncated":     h[:nt+30],
		"bad e_lfanew":  append(append(append([]byte{}, h[:0x3c]...), 0xff, 0xff, 0xff, 0x7f), h[0x40:]...),
		"no PE":         append(append(append([]byte{}, h[:nt]...), 'X', 'X'), h[nt+2:]...),
		"sections past": h[:nt+24+240],
		"no .data":      renamed(h[:], ".data", ".dat2"),
		"no sections":   withCount(h[:], nt, 0),
	} {
		if _, _, ok := parseDataSection(b); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}

// renamed returns a copy of h with the first section called from renamed to.
func renamed(h []byte, from, to string) []byte {
	c := append([]byte{}, h...)
	if i := bytes.Index(c, append([]byte(from), 0, 0, 0)); i >= 0 {
		copy(c[i:], to)
	}
	return c
}

// withCount returns a copy of h with NumberOfSections set to n.
func withCount(h []byte, nt int, n uint16) []byte {
	c := append([]byte{}, h...)
	binary.LittleEndian.PutUint16(c[nt+6:], n)
	return c
}

// FuzzParseDataSection: no input panics, and an accepted section does not wrap.
func FuzzParseDataSection(f *testing.F) {
	base, _, _ := procGetModuleHandleW.Call(0)
	var h [4096]byte
	var n uintptr
	if err := windows.ReadProcessMemory(windows.CurrentProcess(), base, &h[0], uintptr(len(h)), &n); err == nil {
		f.Add(h[:])
	}
	f.Add([]byte("MZ"))
	f.Fuzz(func(t *testing.T, b []byte) {
		rva, size, ok := parseDataSection(b)
		if ok && uint64(rva)+uint64(size) > 1<<32 {
			t.Fatalf("rva %#x + size %#x wraps", rva, size)
		}
	})
}
