//go:build windows

package main

import (
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/spike/gui/internal/spikeui"
)

// The WebView2 runtime's processes (msedgewebview2.exe) are children of this process
// and so are put in the same job. Closing the controller normally ends them within
// about 200 ms, but in one close out of a few dozen they stayed for 5 to 10 s, which
// breaks REQ-UX-011 ("no process left 2 s after the window closes"). The job makes the
// rule hold: on exit the GUI waits a short grace period for them to finish, then the
// kernel ends whatever is left when the last job handle closes with this process.

var (
	job                windows.Handle
	procIsProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")
)

// enterJob puts this process (and so every child it starts) in a job that is killed
// when this process exits. It fails harmlessly if the process is in a job that does
// not allow nesting.
func enterJob() {
	j, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	// BREAKAWAY_OK: the hyperbutler CLI is started outside the job, so an engine that
	// `hyperbutler start` launches outlives the GUI.
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK
	if _, err := windows.SetInformationJobObject(j, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(j)
		return
	}
	if err := windows.AssignProcessToJobObject(j, windows.CurrentProcess()); err != nil {
		_ = windows.CloseHandle(j)
		return
	}
	job = j
	spikeui.Breakaway = true
}

// waitChildren waits up to grace for every other process in the job to exit, so
// the browser processes can finish writing their profile before the job kills them.
func waitChildren(grace time.Duration) {
	if job == 0 {
		return
	}
	var acct struct {
		TotalUserTime, TotalKernelTime, ThisPeriodUserTime, ThisPeriodKernelTime int64
		TotalPageFaultCount, TotalProcesses, ActiveProcesses, TotalTerminated    uint32
	}
	for end := time.Now().Add(grace); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation,
			uintptr(unsafe.Pointer(&acct)), uint32(unsafe.Sizeof(acct)), nil); err != nil || acct.ActiveProcesses <= 1 {
			return
		}
	}
}

// reportJob prints, for each child process, whether it is in the job (-debug-job).
func reportJob() {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	self := windows.GetCurrentProcessId()
	parents := map[uint32]bool{self: true}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for i := 0; i < 3; i++ { // children and grandchildren
		for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
			if parents[e.ParentProcessID] && e.ProcessID != self {
				parents[e.ProcessID] = true
			}
		}
	}
	for pid := range parents {
		if pid == self {
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
		if err != nil {
			fmt.Printf("HB-SPIKE job pid=%d open: %v\n", pid, err)
			continue
		}
		var in int32
		r, _, err := procIsProcessInJob.Call(uintptr(h), uintptr(job), uintptr(unsafe.Pointer(&in)))
		if r != 0 {
			err = nil
		}
		_ = windows.CloseHandle(h)
		fmt.Printf("HB-SPIKE job pid=%d in_job=%v err=%v\n", pid, in, err)
	}
}
