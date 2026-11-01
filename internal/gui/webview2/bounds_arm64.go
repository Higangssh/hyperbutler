//go:build windows

package webview2

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetClientRect = windows.NewLazySystemDLL("user32.dll").NewProc("GetClientRect")

func getClientRect(hwnd uintptr, rc *windows.Rect) error {
	if r, _, err := procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(rc))); r == 0 {
		return err
	}
	return nil
}

// putBounds calls ICoreWebView2Controller::put_Bounds(RECT). On ARM64 a 16-byte struct
// passed by value goes in two registers.
func putBounds(ctl obj, rc *windows.Rect) {
	words := *(*[2]uintptr)(unsafe.Pointer(rc))
	ctl.call(ctlPutBounds, words[0], words[1])
}
