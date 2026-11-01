//go:build windows

package app

import (
	"crypto/sha256"
	"encoding/hex"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procFindWindowW = user32.NewProc("FindWindowW")
	procShowWindowA = user32.NewProc("ShowWindowAsync")
)

// singleInstance takes the named mutex of key. When another process holds it, it
// brings that process's window to the front and returns first false.
func singleInstance(key string) (release func(), first bool) {
	sum := sha256.Sum256([]byte(key))
	name, _ := windows.UTF16PtrFromString(`Local\hyperbutler-gui-` + hex.EncodeToString(sum[:8]))
	h, err := windows.CreateMutex(nil, false, name)
	if err == windows.ERROR_ALREADY_EXISTS { //nolint:errorlint // CreateMutex returns the bare Errno
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
		cls, _ := windows.UTF16PtrFromString("hyperbutlerGUI")
		if hwnd, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(cls)), 0); hwnd != 0 {
			const swRestore = 9
			_, _, _ = procShowWindowA.Call(hwnd, swRestore)
			_, _, _ = procSetForegroundWin.Call(hwnd)
		}
		return func() {}, false
	}
	if h == 0 {
		return func() {}, true // no mutex (should not happen): open anyway
	}
	return func() { _ = windows.CloseHandle(h) }, true
}
