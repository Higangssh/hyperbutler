//go:build windows

package engine

import (
	"errors"
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ErrProcessGone means the process is not running, or the PID now belongs to another
// process than the one recorded.
var ErrProcessGone = errors.New("engine: process is not running")

func filetime(f windows.Filetime) int64 { return int64(f.HighDateTime)<<32 | int64(f.LowDateTime) }

func processCreated(h windows.Handle) (int64, error) {
	var c, e, k, u windows.Filetime
	if err := windows.GetProcessTimes(h, &c, &e, &k, &u); err != nil {
		return 0, err
	}
	return filetime(c), nil
}

// SelfCreated returns this process's creation time, for State.PIDCreated.
func SelfCreated() int64 {
	t, _ := processCreated(windows.CurrentProcess())
	return t
}

// CreatedTime returns the creation time of process pid, for comparison with
// State.PIDCreated. The caller must hold a handle to the process (such as an
// os.Process it started), or the PID could be reused in between.
func CreatedTime(pid int) (int64, error) {
	if pid <= 0 || pid >= maxPID {
		return 0, ErrProcessGone
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, fmt.Errorf("engine: open process %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	return processCreated(h)
}

// Process is a handle to another process, checked against its creation time.
type Process struct {
	PID int
	h   windows.Handle
}

// OpenProcess opens pid for waiting and termination, and checks that it is the process
// created at created (State.PIDCreated). A process that exited, or a PID reused by
// another process, gives ErrProcessGone.
func OpenProcess(pid int, created int64) (*Process, error) {
	if pid <= 0 || pid >= maxPID {
		return nil, ErrProcessGone
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil, ErrProcessGone
		}
		return nil, fmt.Errorf("engine: open process %d: %w", pid, err)
	}
	p := &Process{PID: pid, h: h}
	t, err := processCreated(h)
	if err != nil {
		p.Close()
		return nil, err
	}
	if t != created {
		p.Close()
		return nil, ErrProcessGone
	}
	if exited, _ := p.Wait(0); exited {
		p.Close()
		return nil, ErrProcessGone
	}
	return p, nil
}

// Wait waits up to timeout for the process to exit and reports whether it did.
func (p *Process) Wait(timeout time.Duration) (bool, error) {
	ms := uint32(windows.INFINITE)
	if timeout >= 0 {
		// Clamp: a longer wait would wrap, and INFINITE itself means forever.
		d := timeout.Milliseconds()
		if d > windows.INFINITE-1 {
			d = windows.INFINITE - 1
		}
		ms = uint32(d) //nolint:gosec // G115: d is 0..INFINITE-1 (timeout >= 0, clamped above)
	}
	ev, err := windows.WaitForSingleObject(p.h, ms)
	switch {
	case err != nil:
		return false, err
	case ev == windows.WAIT_OBJECT_0:
		return true, nil
	case ev == uint32(windows.WAIT_TIMEOUT):
		return false, nil
	}
	return false, fmt.Errorf("engine: wait for process %d: %#x", p.PID, ev)
}

// Kill terminates the process. Its VM goes with it: the VM is created with
// ShouldTerminateOnLastHandleClosed and the engine holds the only handle.
func (p *Process) Kill() error { return windows.TerminateProcess(p.h, 1) }

// Close releases the handle.
func (p *Process) Close() {
	if p.h != 0 {
		_ = windows.CloseHandle(p.h)
		p.h = 0
	}
}

// stopEventName is the named event the CLI sets to ask the engine with this PID to stop.
func stopEventName(pid int, created int64) string {
	return fmt.Sprintf(`Local\hyperbutler-engine-stop-%d-%x`, pid, created)
}

// daemonEventName is the named event the CLI sets to ask the engine to apply
// daemon.json again (`hyperbutler config daemon apply`).
func daemonEventName(pid int, created int64) string {
	return fmt.Sprintf(`Local\hyperbutler-engine-daemon-%d-%x`, pid, created)
}

// settingsEventName is the named event the CLI sets to ask the engine to apply the live
// settings again (`hyperbutler config set hostGateway.enabled false`).
func settingsEventName(pid int, created int64) string {
	return fmt.Sprintf(`Local\hyperbutler-engine-settings-%d-%x`, pid, created)
}

// StopEvent is the engine's end of the stop request.
type StopEvent struct{ h windows.Handle }

// CreateStopEvent creates this process's stop event. Only the current user (and
// SYSTEM) may set it. A name that already exists is refused: someone else made it.
func CreateStopEvent() (*StopEvent, error) {
	return createEvent(stopEventName(windowsPID(), SelfCreated()), true)
}

// CreateDaemonEvent creates this process's daemon.json apply event, with the stop
// event's access rules. It resets when Wait returns, so each request is seen once.
func CreateDaemonEvent() (*StopEvent, error) {
	return createEvent(daemonEventName(windowsPID(), SelfCreated()), false)
}

// CreateSettingsEvent creates this process's settings reload event, with the stop
// event's access rules: `hyperbutler config set` of a live key sets it, and the engine
// reads settings.json again. It resets when Wait returns.
func CreateSettingsEvent() (*StopEvent, error) {
	return createEvent(settingsEventName(windowsPID(), SelfCreated()), false)
}

// RequestSettingsReload asks the engine process pid created at created to read
// settings.json again and apply its live keys. The result shows in its state.json
// (State.HostGateway.Reloads).
func RequestSettingsReload(pid int, created int64) error {
	return setEvent(settingsEventName(pid, created))
}

func createEvent(eventName string, manualReset bool) (*StopEvent, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;" + tu.User.Sid.String() + ")(A;;GA;;;SY)")
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	name, err := windows.UTF16PtrFromString(eventName)
	if err != nil {
		return nil, err
	}
	var manual uint32
	if manualReset {
		manual = 1
	}
	h, err := windows.CreateEvent(sa, manual, 0, name)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return nil, fmt.Errorf("engine: create event %s: %w", eventName, err)
	}
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("engine: the event %s already exists; another process created it", eventName)
	}
	return &StopEvent{h: h}, nil
}

func windowsPID() int { return int(windows.GetCurrentProcessId()) }

// Wait blocks until the event is set or done is closed. It reports whether a stop was
// requested.
func (e *StopEvent) Wait(done <-chan struct{}) bool {
	for {
		ev, err := windows.WaitForSingleObject(e.h, 200)
		if err == nil && ev == windows.WAIT_OBJECT_0 {
			return true
		}
		select {
		case <-done:
			return false
		default:
		}
	}
}

// Close releases the event.
func (e *StopEvent) Close() { _ = windows.CloseHandle(e.h) }

// RequestStop sets the stop event of the engine process pid created at created.
func RequestStop(pid int, created int64) error { return setEvent(stopEventName(pid, created)) }

// RequestDaemonApply asks the engine process pid created at created to apply
// daemon.json again. The result shows in its state.json (State.Daemon.Generation).
func RequestDaemonApply(pid int, created int64) error {
	return setEvent(daemonEventName(pid, created))
}

func setEvent(eventName string) error {
	name, err := windows.UTF16PtrFromString(eventName)
	if err != nil {
		return err
	}
	h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if err != nil {
		return fmt.Errorf("engine: open event %s: %w", eventName, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	return windows.SetEvent(h)
}
