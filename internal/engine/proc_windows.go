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
	ms := uint32(timeout.Milliseconds())
	if timeout < 0 {
		ms = windows.INFINITE
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
	return fmt.Sprintf(`Local\winbox-engine-stop-%d-%x`, pid, created)
}

// StopEvent is the engine's end of the stop request.
type StopEvent struct{ h windows.Handle }

// CreateStopEvent creates this process's stop event. Only the current user (and
// SYSTEM) may set it. A name that already exists is refused: someone else made it.
func CreateStopEvent() (*StopEvent, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;" + tu.User.Sid.String() + ")(A;;GA;;;SY)")
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	name, err := windows.UTF16PtrFromString(stopEventName(windowsPID(), SelfCreated()))
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateEvent(sa, 1, 0, name)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return nil, fmt.Errorf("engine: create stop event: %w", err)
	}
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		_ = windows.CloseHandle(h)
		return nil, errors.New("engine: the stop event already exists; another process created it")
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
func RequestStop(pid int, created int64) error {
	name, err := windows.UTF16PtrFromString(stopEventName(pid, created))
	if err != nil {
		return err
	}
	h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if err != nil {
		return fmt.Errorf("engine: open stop event: %w", err)
	}
	defer windows.CloseHandle(h)
	return windows.SetEvent(h)
}
