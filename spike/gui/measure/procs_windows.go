//go:build windows

package main

import (
	"errors"
	"fmt"
	"sort"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A copy of the part of bench/internal/hostmon this harness needs (an internal
// package cannot be imported from spike/): one NtQuerySystemInformation call gives
// every process with its parent, image name, private working set and commit, in this
// process, without opening the processes.

type proc struct {
	PID, PPID  uint32
	Name       string
	WSPrivate  uint64 // WorkingSetPrivateSize, bytes
	CommitPriv uint64 // PagefileUsage (private bytes), bytes
	Threads    uint32
}

const maxProcInfo = 64 << 20

func snapshot() ([]proc, error) {
	n := uint32(512 << 10)
	var buf []byte
	for {
		words := make([]uint64, (n+7)/8)
		buf = unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), n)
		var ret uint32
		err := windows.NtQuerySystemInformation(windows.SystemProcessInformation, unsafe.Pointer(&buf[0]), n, &ret)
		if err == nil {
			if ret > n {
				return nil, errors.New("process information longer than its buffer")
			}
			buf = buf[:ret]
			break
		}
		if !errors.Is(err, windows.STATUS_INFO_LENGTH_MISMATCH) {
			return nil, fmt.Errorf("NtQuerySystemInformation: %w", err)
		}
		if n = max(ret+ret/4, 2*n); n > maxProcInfo {
			return nil, errors.New("process information larger than 64 MiB")
		}
	}
	var ps []proc
	for off := uintptr(0); ; {
		if off+unsafe.Sizeof(windows.SYSTEM_PROCESS_INFORMATION{}) > uintptr(len(buf)) {
			return nil, errors.New("truncated process information")
		}
		pi := (*windows.SYSTEM_PROCESS_INFORMATION)(unsafe.Pointer(&buf[off]))
		name := ""
		if pi.ImageName.Buffer != nil {
			name = windows.UTF16PtrToString(pi.ImageName.Buffer)
		}
		ps = append(ps, proc{
			PID:        uint32(pi.UniqueProcessID),
			PPID:       uint32(pi.InheritedFromUniqueProcessID),
			Name:       name,
			WSPrivate:  uint64(max(pi.WorkingSetPrivateSize, 0)),
			CommitPriv: uint64(pi.PagefileUsage),
			Threads:    pi.NumberOfThreads,
		})
		if pi.NextEntryOffset == 0 {
			return ps, nil
		}
		off += uintptr(pi.NextEntryOffset)
	}
}

// descendants returns root and every process below it. A parent PID can name a parent
// that exited and whose PID was reused, so this over-approximates only through reuse.
func descendants(ps []proc, root uint32) map[uint32]bool {
	kids := map[uint32][]uint32{}
	for _, p := range ps {
		if p.PID != p.PPID {
			kids[p.PPID] = append(kids[p.PPID], p.PID)
		}
	}
	out := map[uint32]bool{root: true}
	for q := []uint32{root}; len(q) > 0; q = q[1:] {
		for _, k := range kids[q[0]] {
			if !out[k] {
				out[k] = true
				q = append(q, k)
			}
		}
	}
	return out
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}
