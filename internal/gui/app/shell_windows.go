//go:build windows

package app

import (
	"errors"
	"fmt"
	"runtime"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32              = windows.NewLazySystemDLL("kernel32.dll")
	shell32               = windows.NewLazySystemDLL("shell32.dll")
	procGlobalAlloc       = kernel32.NewProc("GlobalAlloc")
	procGlobalFree        = kernel32.NewProc("GlobalFree")
	procGlobalLock        = kernel32.NewProc("GlobalLock")
	procGlobalUnlock      = kernel32.NewProc("GlobalUnlock")
	procOpenClipboard     = user32.NewProc("OpenClipboard")
	procCloseClipboard    = user32.NewProc("CloseClipboard")
	procEmptyClipboard    = user32.NewProc("EmptyClipboard")
	procSetClipboardData  = user32.NewProc("SetClipboardData")
	procILCreateFromPathW = shell32.NewProc("ILCreateFromPathW")
	procILFree            = shell32.NewProc("ILFree")
	procSHOpenFolderAndSI = shell32.NewProc("SHOpenFolderAndSelectItems")
)

// maxClipboard bounds what CopyText puts on the clipboard.
const maxClipboard = 64 << 10

// CopyText puts s on the clipboard as Unicode text. The bridge calls it only with an
// error's English original that the host kept, never with text the page sends.
func CopyText(s string) error {
	if len(s) > maxClipboard {
		s = s[:maxClipboard]
	}
	u := append(utf16.Encode([]rune(s)), 0)
	const gmemMoveable, cfUnicodeText = 0x0002, 13
	h, _, err := procGlobalAlloc.Call(gmemMoveable, uintptr(len(u))*2)
	if h == 0 {
		return fmt.Errorf("GlobalAlloc: %w", err)
	}
	p, _, err := procGlobalLock.Call(h)
	if p == 0 {
		_, _, _ = procGlobalFree.Call(h)
		return fmt.Errorf("GlobalLock: %w", err)
	}
	copy(unsafe.Slice((*uint16)(ptrFrom(p)), len(u)), u)
	_, _, _ = procGlobalUnlock.Call(h)
	if r, _, err := procOpenClipboard.Call(0); r == 0 {
		_, _, _ = procGlobalFree.Call(h)
		return fmt.Errorf("OpenClipboard: %w", err)
	}
	defer func() { _, _, _ = procCloseClipboard.Call() }()
	_, _, _ = procEmptyClipboard.Call()
	if r, _, err := procSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		_, _, _ = procGlobalFree.Call(h)
		return fmt.Errorf("SetClipboardData: %w", err)
	}
	return nil // the clipboard owns h now
}

// Reveal opens an Explorer window on the folder of path with the file selected. It
// starts no process of its own: Explorer (the shell) opens the window.
func Reveal(path string) error {
	if path == "" {
		return errors.New("no path")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); err == nil {
		defer windows.CoUninitialize()
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	pidl, _, _ := procILCreateFromPathW.Call(uintptr(unsafe.Pointer(p)))
	if pidl == 0 {
		return fmt.Errorf("%s: not found", path)
	}
	defer func() { _, _, _ = procILFree.Call(pidl) }()
	if hr, _, _ := procSHOpenFolderAndSI.Call(pidl, 0, 0, 0); int32(uint32(hr)) < 0 { //nolint:gosec // G115: an HRESULT is the low 32 bits, signed
		return fmt.Errorf("SHOpenFolderAndSelectItems: HRESULT 0x%08X", uint32(hr)) //nolint:gosec // G115: an HRESULT is the low 32 bits
	}
	return nil
}
