//go:build windows

package hostmon

import (
	"debug/pe"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Residency is where a process's private resident pages are, by region kind. It is
// read with VirtualQueryEx and QueryWorkingSetEx, which look at the page tables and
// never touch the pages, so taking it does not change what it measures.
type Residency struct {
	// Kinds maps a region kind to its private resident bytes and committed bytes.
	// Kinds: "exe <section>" for the executable's own sections, "exe symbol <name>"
	// for its large data symbols (also counted in their section), "go heap" for
	// the Go runtime's 4 MiB heap reservations, "private" for other private memory (thread stacks, DLL heaps,
	// Go's off-heap metadata), "dll" for other images, "mapped" for file mappings.
	Kinds map[string]Bytes
	// Stripped is set when the executable has no symbol table (a -s build, such as a
	// release), so no "exe symbol" kinds can be reported.
	Stripped bool
}

// Bytes is a resident / committed pair.
type Bytes struct{ Resident, Committed uint64 }

// String formats the kinds, largest resident first; symbols and kinds under 256 KiB
// resident are left out.
func (r Residency) String() string {
	type kv struct {
		k string
		v Bytes
	}
	var all []kv
	for k, v := range r.Kinds {
		if v.Resident >= 256<<10 {
			all = append(all, kv{k, v})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].v.Resident != all[j].v.Resident {
			return all[i].v.Resident > all[j].v.Resident
		}
		return all[i].k < all[j].k
	})
	var b strings.Builder
	if r.Stripped {
		b.WriteString("(no symbol table) ")
	}
	for i, e := range all {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s %.1f/%.1f MiB", e.k, float64(e.v.Resident)/(1<<20), float64(e.v.Committed)/(1<<20))
	}
	return b.String()
}

// heapArena is the Go runtime's arena size on 64-bit Windows (64 MiB elsewhere). The
// runtime reserves its heap in multiples of it at a randomized base, so the size of the
// reservation is what identifies it. This is a heuristic: another allocator that
// reserves 4 MiB multiples is counted as Go heap too.
const heapArena = 4 << 20

func isGoArena(size uintptr) bool {
	return size > 0 && size%heapArena == 0
}

// maxRegions bounds the address-space walk; a Go process has a few hundred regions.
const maxRegions = 1 << 16

// bigSymbol is the smallest data symbol reported on its own.
const bigSymbol = 1 << 20

type exeLayout struct {
	sections []span // RVA ranges, name = section name
	symbols  []span // large data symbols, RVA ranges
}

type span struct {
	lo, hi uint64
	name   string
}

func readExeLayout(exe string) (*exeLayout, error) {
	f, err := pe.Open(exe)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only
	l := &exeLayout{}
	for _, s := range f.Sections {
		size := uint64(max(s.VirtualSize, s.Size))
		l.sections = append(l.sections, span{uint64(s.VirtualAddress), uint64(s.VirtualAddress) + size, s.Name})
	}
	// Go writes COFF symbols (unless linked with -s). Sizes are the gap to the next
	// symbol in the same section.
	type sym struct {
		rva  uint64
		sec  int
		name string
	}
	var syms []sym
	for _, s := range f.Symbols {
		if s.SectionNumber <= 0 || int(s.SectionNumber) > len(f.Sections) {
			continue
		}
		sec := f.Sections[s.SectionNumber-1]
		if sec.Characteristics&pe.IMAGE_SCN_MEM_WRITE == 0 {
			continue
		}
		syms = append(syms, sym{uint64(sec.VirtualAddress) + uint64(s.Value), int(s.SectionNumber), s.Name})
	}
	sort.Slice(syms, func(i, j int) bool { return syms[i].rva < syms[j].rva })
	for i := 0; i < len(syms); {
		// Section markers such as runtime.noptrbss share their address with the
		// first symbol after them; name the range after the real symbols.
		j := i
		var names []string
		for ; j < len(syms) && syms[j].rva == syms[i].rva; j++ {
			if !isMarker(syms[j].name) {
				names = append(names, syms[j].name)
			}
		}
		s := syms[i]
		end := l.sections[s.sec-1].hi
		if j < len(syms) && syms[j].sec == s.sec {
			end = syms[j].rva
		}
		if end-s.rva >= bigSymbol && len(names) > 0 {
			l.symbols = append(l.symbols, span{s.rva, end, strings.Join(names, "+")})
		}
		i = j
	}
	return l, nil
}

// isMarker reports whether a symbol is one of the linker's section bounds.
func isMarker(name string) bool {
	switch name {
	case "runtime.data", "runtime.edata", "runtime.noptrdata", "runtime.enoptrdata",
		"runtime.bss", "runtime.ebss", "runtime.noptrbss", "runtime.enoptrbss", "runtime.end",
		"runtime.covctrs", "runtime.ecovctrs":
		return true
	}
	return false
}

func find(spans []span, rva uint64) string {
	i := sort.Search(len(spans), func(i int) bool { return spans[i].hi > rva })
	if i < len(spans) && spans[i].lo <= rva {
		return spans[i].name
	}
	return ""
}

// ResidencyOf walks pid's address space. exe is the process's executable on disk (for
// its section and symbol layout); it needs PROCESS_QUERY_INFORMATION, which a user has
// on their own processes. A page counts when it is in the working set and not shared.
func ResidencyOf(pid uint32, exe string) (Residency, error) {
	lay, err := readExeLayout(exe)
	if err != nil {
		return Residency{}, err
	}
	sort.Slice(lay.sections, func(i, j int) bool { return lay.sections[i].lo < lay.sections[j].lo })
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION, false, pid)
	if err != nil {
		return Residency{}, fmt.Errorf("hostmon: open PID %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	exeName := strings.ToLower(filepath.Base(exe))
	r := Residency{Kinds: map[string]Bytes{}}
	add := func(k string, res, com uint64) {
		v := r.Kinds[k]
		v.Resident += res
		v.Committed += com
		r.Kinds[k] = v
	}
	var exeBase uintptr
	imageName := map[uintptr]string{}
	const page = 4096
	var regions []windows.MemoryBasicInformation
	reserved := map[uintptr]uintptr{} // private allocation base -> size of all its regions
	for addr := uintptr(0); ; {
		var mbi windows.MemoryBasicInformation
		if err := windows.VirtualQueryEx(h, addr, &mbi, unsafe.Sizeof(mbi)); err != nil {
			if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
				break // past the last region
			}
			return r, fmt.Errorf("hostmon: VirtualQueryEx: %w", err)
		}
		next := mbi.BaseAddress + mbi.RegionSize
		if next <= addr {
			break
		}
		addr = next
		if mbi.State == memFree {
			continue
		}
		if mbi.Type == memPrivate {
			reserved[mbi.AllocationBase] += mbi.RegionSize
		}
		if mbi.State == windows.MEM_COMMIT {
			regions = append(regions, mbi)
		}
	}
	var info []wsExInfo
	for _, mbi := range regions {
		kind := ""
		switch mbi.Type {
		case memImage:
			n, ok := imageName[mbi.AllocationBase]
			if !ok {
				n = mappedName(h, mbi.AllocationBase)
				imageName[mbi.AllocationBase] = n
			}
			if n == exeName {
				exeBase = mbi.AllocationBase
				kind = "exe"
			} else {
				kind = "dll"
			}
		case memMapped:
			kind = "mapped"
		default:
			if isGoArena(reserved[mbi.AllocationBase]) {
				kind = "go heap"
			} else {
				kind = "private"
			}
		}
		pages := int(mbi.RegionSize / page)
		if cap(info) < pages {
			info = make([]wsExInfo, pages)
		}
		info = info[:pages]
		for i := range info {
			info[i] = wsExInfo{va: mbi.BaseAddress + uintptr(i)*page}
		}
		// The call takes the buffer size as a DWORD; query a huge region in chunks.
		for lo := 0; lo < pages; lo += wsExChunk {
			chunk := info[lo:min(lo+wsExChunk, pages)]
			if err := windows.QueryWorkingSetEx(h, uintptr(unsafe.Pointer(&chunk[0])), uint32(len(chunk))*wsExInfoSize); err != nil { //nolint:gosec // G115: a chunk has at most wsExChunk (2^20) entries
				return r, fmt.Errorf("hostmon: QueryWorkingSetEx: %w", err)
			}
		}
		for i := range info {
			resident := uint64(0)
			// x/sys/windows' Shared() compares a masked bit with 1, so it is always false.
			if a := info[i].attr; a.Valid() && a&(1<<15) == 0 {
				resident = page
			}
			k := kind
			if kind == "exe" {
				rva := uint64(info[i].va - exeBase)
				sec := find(lay.sections, rva)
				if sec == "" {
					sec = "headers"
				}
				k = "exe " + sec
				if s := find(lay.symbols, rva); s != "" {
					add("exe symbol "+s, resident, page)
				}
			}
			add(k, resident, page)
		}
	}
	if exeBase == 0 {
		return r, fmt.Errorf("hostmon: %s is not mapped in PID %d", exeName, pid)
	}
	if len(lay.symbols) == 0 {
		// A stripped (-s) executable, such as a release build.
		r.Stripped = true
	}
	return r, nil
}

// MEMORY_BASIC_INFORMATION.Type values.
const (
	memImage   = 0x1000000
	memMapped  = 0x40000
	memPrivate = 0x20000
	memFree    = 0x10000
)

// wsExInfo is PSAPI_WORKING_SET_EX_INFORMATION with the address as a uintptr.
type wsExInfo struct {
	va   uintptr
	attr windows.PSAPI_WORKING_SET_EX_BLOCK
}

const (
	wsExInfoSize = uint32(unsafe.Sizeof(wsExInfo{}))
	// wsExChunk entries per QueryWorkingSetEx call: 16 MiB of entries, far below the
	// DWORD size limit.
	wsExChunk = 1 << 20
)

// ImagePath is the full path of pid's executable.
func ImagePath(pid uint32) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", fmt.Errorf("hostmon: open PID %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(windows.MAX_LONG_PATH)
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return "", fmt.Errorf("hostmon: image of PID %d: %w", pid, err)
	}
	return windows.UTF16ToString(buf[:n]), nil
}

var procGetMappedFileNameW = windows.NewLazySystemDLL("psapi.dll").NewProc("GetMappedFileNameW")

// mappedName is the lower-case base name of the file mapped at base, or "".
func mappedName(h windows.Handle, base uintptr) string {
	var buf [windows.MAX_PATH + 1]uint16
	n, _, _ := procGetMappedFileNameW.Call(uintptr(h), base, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return strings.ToLower(filepath.Base(windows.UTF16ToString(buf[:n])))
}
