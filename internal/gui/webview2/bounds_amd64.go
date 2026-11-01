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

// putBounds calls ICoreWebView2Controller::put_Bounds(RECT). On x64 a 16-byte struct
// passed by value goes as a pointer to a copy.
func putBounds(ctl obj, rc *windows.Rect) {
	c := *rc
	ctl.call(ctlPutBounds, uintptr(unsafe.Pointer(&c)))
}
