//go:build windows

package app

import (
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The WebView2 runtime's processes (msedgewebview2.exe) are children of this process
// and so join its job. Closing the controller normally ends them within about 300 ms,
// but the spike saw them stay for 5 to 10 s now and then, which would break REQ-UX-011
// ("no process left 2 s after the window closes"). A kill-on-close job makes the rule
// hold: after the window closes the GUI waits a short grace period for them, then the
// kernel ends whatever is left when the job's last handle closes with this process.
//
// The job allows breakaway, and the CLI the GUI runs for engine operations is started
// with CREATE_BREAKAWAY_FROM_JOB, so an engine started from the GUI outlives it
// (closing the GUI never stops the engine, REQ-UX-016).

var (
	job       windows.Handle
	breakaway atomic.Bool

	procIsProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")
)

const (
	jobLimitBreakawayOK       = 0x00000800
	jobLimitSilentBreakawayOK = 0x00001000
	jobLimitKillOnJobClose    = 0x00002000
)

// Breakaway reports whether the GUI runs in its kill-on-close job, so a process that
// must outlive it has to be started with CREATE_BREAKAWAY_FROM_JOB.
func Breakaway() bool { return breakaway.Load() }

// inJob reports whether this process is already in a job, and whether that job lets a
// child break away from it.
func inJob() (in, canBreakAway bool) {
	var r int32
	if ok, _, _ := procIsProcessInJob.Call(uintptr(windows.CurrentProcess()), 0, uintptr(unsafe.Pointer(&r))); ok == 0 || r == 0 {
		return false, true
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	// Handle 0: the job of this process.
	if err := windows.QueryInformationJobObject(0, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		return true, false
	}
	f := info.BasicLimitInformation.LimitFlags
	return true, f&(jobLimitBreakawayOK|jobLimitSilentBreakawayOK) != 0
}

// enterJob puts this process (and so every child it starts) in a job that is killed
// when this process exits. When this process is already in a job that forbids
// breakaway, it does not: the engine started from the GUI could then not leave our
// job and would die with the window. The controller's Close still ends the browser
// processes; only the backstop is missing, and the log says so.
func enterJob(log func(string)) {
	if in, ok := inJob(); in && !ok {
		log("the GUI runs in a job that forbids breakaway; not using a kill-on-close job of its own")
		return
	}
	j, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		log("create job: " + err.Error())
		return
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = jobLimitKillOnJobClose | jobLimitBreakawayOK
	if _, err := windows.SetInformationJobObject(j, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(j)
		log("set job limits: " + err.Error())
		return
	}
	if err := windows.AssignProcessToJobObject(j, windows.CurrentProcess()); err != nil {
		_ = windows.CloseHandle(j)
		log("assign job: " + err.Error())
		return
	}
	job = j
	breakaway.Store(true)
}

// waitChildren waits up to grace for every other process in the job to exit, so the
// browser processes can finish writing their profile before the job ends them.
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
