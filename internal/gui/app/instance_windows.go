//go:build windows

package app

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procFindWindowW = user32.NewProc("FindWindowW")
	procShowWindowA = user32.NewProc("ShowWindowAsync")
)

// instanceID is the part of the mutex's and the window class's names that key gives:
// windows of other state directories have other names, so a second start finds only
// its own instance's window.
func instanceID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// instanceWait is how long a second start looks for the first one's window.
var instanceWait = 3 * time.Second

// windowClass is the window class of key's instance.
func windowClass(key string) string {
	if key == "" {
		return "hyperbutlerGUI"
	}
	return "hyperbutlerGUI-" + instanceID(key)
}

// singleInstance takes the named mutex of key. When another process holds it, it
// brings that process's window to the front and returns first false, with found false
// when no window of that instance appeared within a few seconds (the other process
// hangs, or is still starting).
func singleInstance(key string) (release func(), first, found bool) {
	name, _ := windows.UTF16PtrFromString(`Local\hyperbutler-gui-` + instanceID(key))
	h, err := windows.CreateMutex(nil, false, name)
	if err == windows.ERROR_ALREADY_EXISTS { //nolint:errorlint // CreateMutex returns the bare Errno
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
		cls, _ := windows.UTF16PtrFromString(windowClass(key))
		// The other window may be starting: its mutex comes before its window.
		for end := time.Now().Add(instanceWait); ; time.Sleep(100 * time.Millisecond) {
			if hwnd, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(cls)), 0); hwnd != 0 {
				const swRestore = 9
				_, _, _ = procShowWindowA.Call(hwnd, swRestore)
				_, _, _ = procSetForegroundWin.Call(hwnd)
				return func() {}, false, true
			}
			if time.Now().After(end) {
				return func() {}, false, false
			}
		}
	}
	if h == 0 {
		return func() {}, true, false // no mutex (should not happen): open anyway
	}
	return func() { _ = windows.CloseHandle(h) }, true, false
}
