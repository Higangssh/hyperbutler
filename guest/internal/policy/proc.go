package policy

import (
	"bytes"
	"errors"
	"math"
	"strconv"
)

// The /proc files come from the guest kernel, but they are parsed defensively anyway:
// bounded input, no panics, overflow-checked arithmetic.

// MaxProcFile bounds what the parsers read.
const MaxProcFile = 64 << 10

var errProc = errors.New("policy: malformed /proc data")

// Meminfo holds the /proc/meminfo fields the policies use, in bytes.
type Meminfo struct {
	MemTotal, MemFree, MemAvailable uint64
	ActiveFile, InactiveFile        uint64
	Shmem, Cached                   uint64
}

// FileCache is the reclaimable file page cache: Active(file)+Inactive(file). It
// excludes Shmem (tmpfs, the initramfs), which is on the anon LRU and cannot be
// dropped.
func (m Meminfo) FileCache() uint64 { return satAdd(m.ActiveFile, m.InactiveFile) }

// ParseMeminfo parses /proc/meminfo. Unknown lines are ignored; MemTotal is required.
func ParseMeminfo(b []byte) (Meminfo, error) {
	if len(b) > MaxProcFile {
		return Meminfo{}, errProc
	}
	var m Meminfo
	var seenTotal bool
	for len(b) > 0 {
		var line []byte
		line, b, _ = bytes.Cut(b, []byte{'\n'})
		key, rest, ok := bytes.Cut(line, []byte{':'})
		if !ok {
			continue
		}
		var dst *uint64
		switch string(key) {
		case "MemTotal":
			dst, seenTotal = &m.MemTotal, true
		case "MemFree":
			dst = &m.MemFree
		case "MemAvailable":
			dst = &m.MemAvailable
		case "Active(file)":
			dst = &m.ActiveFile
		case "Inactive(file)":
			dst = &m.InactiveFile
		case "Shmem":
			dst = &m.Shmem
		case "Cached":
			dst = &m.Cached
		default:
			continue
		}
		f := bytes.Fields(rest)
		if len(f) != 2 || string(f[1]) != "kB" {
			return Meminfo{}, errProc
		}
		v, err := strconv.ParseUint(string(f[0]), 10, 64)
		if err != nil || v > math.MaxUint64/1024 {
			return Meminfo{}, errProc
		}
		*dst = v * 1024
	}
	if !seenTotal {
		return Meminfo{}, errProc
	}
	return m, nil
}

// ParseStatCPU returns the busy and total jiffies from the aggregate "cpu " line of
// /proc/stat. Busy is everything except idle (4th field) and iowait (5th).
func ParseStatCPU(b []byte) (busy, total uint64, err error) {
	if len(b) > MaxProcFile {
		return 0, 0, errProc
	}
	for len(b) > 0 {
		var line []byte
		line, b, _ = bytes.Cut(b, []byte{'\n'})
		f := bytes.Fields(line)
		if len(f) == 0 || string(f[0]) != "cpu" {
			continue
		}
		if len(f) < 5 || len(f) > 11 {
			return 0, 0, errProc
		}
		var idle uint64
		for i, s := range f[1:] {
			v, err := strconv.ParseUint(string(s), 10, 64)
			if err != nil {
				return 0, 0, errProc
			}
			total = satAdd(total, v)
			if i == 3 || i == 4 {
				idle = satAdd(idle, v)
			}
		}
		return total - min(idle, total), total, nil
	}
	return 0, 0, errProc
}

// ParseVmstatPageIO returns pgpgin+pgpgout from /proc/vmstat, in bytes.
func ParseVmstatPageIO(b []byte) (uint64, error) {
	if len(b) > MaxProcFile {
		return 0, errProc
	}
	var sum uint64
	seen := 0
	for len(b) > 0 {
		var line []byte
		line, b, _ = bytes.Cut(b, []byte{'\n'})
		k, v, ok := bytes.Cut(line, []byte{' '})
		if !ok || (string(k) != "pgpgin" && string(k) != "pgpgout") {
			continue
		}
		n, err := strconv.ParseUint(string(v), 10, 64)
		if err != nil {
			return 0, errProc
		}
		sum = satAdd(sum, n)
		seen++
	}
	if seen != 2 {
		return 0, errProc
	}
	if sum > math.MaxUint64/1024 {
		return math.MaxUint64, nil
	}
	return sum * 1024, nil
}

func satAdd(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}
