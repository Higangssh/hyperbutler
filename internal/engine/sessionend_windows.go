//go:build windows

package engine

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The engine runs detached, without a console, so it never sees CTRL_LOGOFF_EVENT or
// CTRL_SHUTDOWN_EVENT. Without this, signing out or shutting Windows down would kill it
// and its VM mid-write. A hidden top-level window receives WM_QUERYENDSESSION and
// WM_ENDSESSION instead (message-only windows do not get these broadcasts).
//
// WM_QUERYENDSESSION only asks: another application may veto it, and the Restart Manager
// (installers, updaters) sends it too, with ENDSESSION_CLOSEAPP. So the query returns
// TRUE at once and does nothing else. The decision is WM_ENDSESSION: with wParam TRUE the
// end is real and the window sets a shutdown block reason, starts the normal stop and
// waits for it; with
// FALSE the end was canceled and the VM keeps running. A confirmed ENDSESSION_CLOSEAPP
// stops too, because the caller closes the process next either way (ADR 0010).

var (
	modUser32                      = windows.NewLazySystemDLL("user32.dll")
	procRegisterClassExW           = modUser32.NewProc("RegisterClassExW")
	procCreateWindowExW            = modUser32.NewProc("CreateWindowExW")
	procDefWindowProcW             = modUser32.NewProc("DefWindowProcW")
	procGetMessageW                = modUser32.NewProc("GetMessageW")
	procTranslateMessage           = modUser32.NewProc("TranslateMessage")
	procDispatchMessageW           = modUser32.NewProc("DispatchMessageW")
	procPostMessageW               = modUser32.NewProc("PostMessageW")
	procSendMessageW               = modUser32.NewProc("SendMessageW")
	procDestroyWindow              = modUser32.NewProc("DestroyWindow")
	procShutdownBlockReasonCreate  = modUser32.NewProc("ShutdownBlockReasonCreate")
	procShutdownBlockReasonDestroy = modUser32.NewProc("ShutdownBlockReasonDestroy")
	procSetProcessShutdownParams   = modKernel32Engine.NewProc("SetProcessShutdownParameters")
	modKernel32Engine              = windows.NewLazySystemDLL("kernel32.dll")
)

const (
	wmDestroy         = 0x0002
	wmClose           = 0x0010
	wmQueryEndSession = 0x0011
	wmEndSession      = 0x0016
	wmQuit            = 0x0012

	// lParam flags of WM_QUERYENDSESSION and WM_ENDSESSION (0 is a shutdown or restart).
	endSessionCloseApp = 0x00000001 // the Restart Manager closes the application
	endSessionCritical = 0x40000000 // forced; the application may get no more time
	endSessionLogoff   = 0x80000000 // the user signs out

	// endSessionWait bounds how long WM_ENDSESSION holds the session open for the
	// guest's orderly shutdown (about 1.5 s measured).
	endSessionWait = 20 * time.Second
)

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

type msg struct {
	Hwnd    windows.HWND
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
	Private uint32
}

// sessionWatch is the hidden window.
type sessionWatch struct {
	hwnd windows.HWND
	stop func(why string)
	done <-chan struct{}
	quit chan struct{}
}

var (
	watchMu sync.Mutex
	watches = map[windows.HWND]*sessionWatch{}
	// wndProcCallback is created once: syscall.NewCallback slots are never freed.
	wndProcCallback = syscall.NewCallback(sessionWndProc)
	classOnce       sync.Once
	classErr        error
	className       = windows.StringToUTF16Ptr("hyperbutler-engine-session")
)

// stopsOnSessionEnd reports whether message m ends the session for real, so the VM must
// stop. Only WM_ENDSESSION with wParam TRUE does, whatever its lParam flags.
func stopsOnSessionEnd(m uint32, wp, _ uintptr) bool {
	return m == wmEndSession && wp != 0
}

// sessionEndWhy names the lParam flags of WM_ENDSESSION, for the log.
func sessionEndWhy(lp uintptr) string {
	var why string
	switch {
	case lp&endSessionCloseApp != 0:
		why = "an installer or updater (Restart Manager) closes the engine"
	case lp&endSessionLogoff != 0:
		why = "sign-out"
	default:
		why = "shutdown or restart"
	}
	if lp&endSessionCritical != 0 {
		why += ", forced"
	}
	return why
}

func sessionWndProc(hwnd windows.HWND, m uint32, wp, lp uintptr) uintptr {
	watchMu.Lock()
	w := watches[hwnd]
	watchMu.Unlock()
	switch {
	case w != nil && m == wmQueryEndSession:
		// Never veto, and return at once: the end may still be canceled, and a Restart
		// Manager session another application refuses may send no WM_ENDSESSION at all,
		// so nothing stops here.
		return 1
	case w != nil && m == wmEndSession:
		if stopsOnSessionEnd(m, wp, lp) {
			// Shown on the blocking screen while this handler waits for the stop.
			reason, _ := windows.UTF16PtrFromString("Stopping the hyperbutler VM")
			procShutdownBlockReasonCreate.Call(uintptr(hwnd), uintptr(unsafe.Pointer(reason)))
			w.stop(sessionEndWhy(lp))
			select {
			case <-w.done:
			case <-time.After(endSessionWait):
			}
		}
		procShutdownBlockReasonDestroy.Call(uintptr(hwnd))
		return 0
	case m == wmClose:
		procDestroyWindow.Call(uintptr(hwnd))
		return 0
	case m == wmDestroy:
		procPostMessageW.Call(0, wmQuit, 0, 0) // ends this thread's GetMessage loop
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(m), wp, lp)
	return r
}

// WatchSessionEnd calls stop (which must not block) when the user signs out, Windows
// shuts down or the Restart Manager closes the engine, with a short description of
// which, and holds the session end until done is closed or endSessionWait passes. A
// session end that is queried and then canceled calls nothing.
// The returned function removes the window.
func WatchSessionEnd(stop func(why string), done <-chan struct{}) (func(), error) {
	if err := modUser32.Load(); err != nil {
		return nil, err
	}
	// Ask to be told early, before the default application level (0x280).
	procSetProcessShutdownParams.Call(0x3FF, 0)
	ready := make(chan error, 1)
	w := &sessionWatch{stop: stop, done: done, quit: make(chan struct{})}
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(w.quit)
		classOnce.Do(func() {
			wc := wndClassEx{WndProc: wndProcCallback, ClassName: className}
			wc.Size = uint32(unsafe.Sizeof(wc))
			if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
				classErr = fmt.Errorf("engine: RegisterClassEx: %w", err)
			}
		})
		if classErr != nil {
			ready <- classErr
			return
		}
		// A hidden, zero-size top-level window (no WS_VISIBLE).
		h, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(className)), 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
		if h == 0 {
			ready <- fmt.Errorf("engine: CreateWindowEx: %w", err)
			return
		}
		w.hwnd = windows.HWND(h)
		watchMu.Lock()
		watches[w.hwnd] = w
		watchMu.Unlock()
		ready <- nil
		var m msg
		for {
			r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if int32(r) <= 0 { //nolint:gosec // G115: GetMessageW returns a BOOL (int32, -1 on error) in the low half
				break
			}
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
		watchMu.Lock()
		delete(watches, w.hwnd)
		watchMu.Unlock()
	}()
	if err := <-ready; err != nil {
		<-w.quit
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			procPostMessageW.Call(uintptr(w.hwnd), wmClose, 0, 0)
			select {
			case <-w.quit:
			case <-time.After(5 * time.Second):
			}
		})
	}, nil
}

// sendSessionMessage delivers m to the watch windows, for tests.
func sendSessionMessage(m uint32, wp, lp uintptr) ([]uintptr, error) {
	watchMu.Lock()
	var hs []windows.HWND
	for h := range watches {
		hs = append(hs, h)
	}
	watchMu.Unlock()
	if len(hs) == 0 {
		return nil, errors.New("no session watch window")
	}
	var out []uintptr
	for _, h := range hs {
		r, _, _ := procSendMessageW.Call(uintptr(h), uintptr(m), wp, lp)
		out = append(out, r)
	}
	return out, nil
}
