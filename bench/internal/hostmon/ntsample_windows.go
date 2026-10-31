//go:build windows

package hostmon

import (
	"context"
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Sampler reads per-process counters for a set of PIDs.
type Sampler func(ctx context.Context, pids []uint32) (map[uint32]ProcSample, error)

// SamplerByName returns the sampler for a -sampler flag value: "nt" (SampleNT, in
// process) or "cim" (SampleCIM, one PowerShell per call).
func SamplerByName(name string) (Sampler, error) {
	switch name {
	case "nt":
		return SampleNT, nil
	case "cim":
		return SampleCIM, nil
	}
	return nil, fmt.Errorf("hostmon: unknown sampler %q (want nt or cim)", name)
}

// maxProcInfo bounds the buffer SampleNT grows to; a desktop with a few hundred
// processes needs well under 1 MiB.
const maxProcInfo = 64 << 20

// SampleNT reads the same counters as SampleCIM from NtQuerySystemInformation
// (SystemProcessInformation), in this process. It opens no process and starts no
// program: the perf counters SampleCIM reads (Win32_PerfFormattedData_PerfProc_Process)
// come from this same call, so the values match, but the bench no longer launches a
// PowerShell per sample. vmmem and vmwp, which a non-elevated user cannot open, are
// listed like every other process. A PID that has exited is missing from the result.
func SampleNT(_ context.Context, pids []uint32) (map[uint32]ProcSample, error) {
	want := make(map[uint32]bool, len(pids))
	for _, p := range pids {
		want[p] = true
	}
	res := make(map[uint32]ProcSample, len(pids))
	if len(pids) == 0 {
		return res, nil
	}
	buf, err := querySystemProcesses()
	if err != nil {
		return nil, err
	}
	for off := uintptr(0); ; {
		if off+unsafe.Sizeof(windows.SYSTEM_PROCESS_INFORMATION{}) > uintptr(len(buf)) {
			return nil, errors.New("hostmon: truncated process information")
		}
		pi := (*windows.SYSTEM_PROCESS_INFORMATION)(unsafe.Pointer(&buf[off]))
		pid := uint32(pi.UniqueProcessID) //nolint:gosec // G115: Windows process IDs are DWORDs
		if want[pid] {
			res[pid] = ProcSample{
				PID:            pid,
				WorkingSetPriv: uint64(max(pi.WorkingSetPrivateSize, 0)),
				HandleCount:    uint64(pi.HandleCount),
				ThreadCount:    uint64(pi.NumberOfThreads),
				PrivateBytes:   uint64(pi.PagefileUsage),
				PageFaults:     uint64(pi.PageFaultCount),
			}
		}
		if pi.NextEntryOffset == 0 {
			return res, nil
		}
		off += uintptr(pi.NextEntryOffset)
	}
}

func querySystemProcesses() ([]byte, error) {
	n := uint32(256 << 10)
	for {
		// SYSTEM_PROCESS_INFORMATION has 8-byte fields: the buffer must be 8-byte aligned.
		words := make([]uint64, (n+7)/8)
		buf := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), n)
		var ret uint32
		err := windows.NtQuerySystemInformation(windows.SystemProcessInformation, unsafe.Pointer(&buf[0]), n, &ret)
		if err == nil {
			if ret > n {
				return nil, errors.New("hostmon: process information longer than its buffer")
			}
			return buf[:ret], nil
		}
		if !errors.Is(err, windows.STATUS_INFO_LENGTH_MISMATCH) {
			return nil, fmt.Errorf("hostmon: NtQuerySystemInformation: %w", err)
		}
		// Processes may start between the calls; leave room.
		next := max(ret+ret/4, 2*n)
		if next > maxProcInfo {
			return nil, errors.New("hostmon: process information larger than 64 MiB")
		}
		n = next
	}
}
