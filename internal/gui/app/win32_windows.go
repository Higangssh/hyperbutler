//go:build windows

package app

import (
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Win32 messages and constants.
const (
	wmDestroy       = 0x0002
	wmMove          = 0x0003
	wmSize          = 0x0005
	wmActivate      = 0x0006
	wmSetFocus      = 0x0007
	wmClose         = 0x0010
	wmGetMinMaxInfo = 0x0024
	wmDPIChanged    = 0x02E0
	wmApp           = 0x8000

	wsOverlappedWindow = 0x00CF0000
	wsClipChildren     = 0x02000000
	cwUseDefault       = ^uintptr(0x7FFFFFFF)
	swShow             = 5
	sizeMinimized      = 1
	waInactive         = 0
	swpNoZOrder        = 0x0004
	swpNoActivate      = 0x0010
	idcArrow           = 32512
	mbIconError        = 0x10
	mbIconInfo         = 0x40
	mbTopmost          = 0x40000
)

var (
	user32                = windows.NewLazySystemDLL("user32.dll")
	gdi32                 = windows.NewLazySystemDLL("gdi32.dll")
	procRegisterClassExW  = user32.NewProc("RegisterClassExW")
	procCreateWindowExW   = user32.NewProc("CreateWindowExW")
	procDefWindowProcW    = user32.NewProc("DefWindowProcW")
	procDestroyWindow     = user32.NewProc("DestroyWindow")
	procShowWindow        = user32.NewProc("ShowWindow")
	procUpdateWindow      = user32.NewProc("UpdateWindow")
	procGetMessageW       = user32.NewProc("GetMessageW")
	procTranslateMessage  = user32.NewProc("TranslateMessage")
	procDispatchMessageW  = user32.NewProc("DispatchMessageW")
	procPostMessageW      = user32.NewProc("PostMessageW")
	procPostQuitMessage   = user32.NewProc("PostQuitMessage")
	procMessageBoxW       = user32.NewProc("MessageBoxW")
	procSetProcessDpiCtx  = user32.NewProc("SetProcessDpiAwarenessContext")
	procGetDpiForWindow   = user32.NewProc("GetDpiForWindow")
	procGetDpiForSystem   = user32.NewProc("GetDpiForSystem")
	procSetWindowPos      = user32.NewProc("SetWindowPos")
	procLoadCursorW       = user32.NewProc("LoadCursorW")
	procLoadImageW        = user32.NewProc("LoadImageW")
	procCreateSolidBrush  = gdi32.NewProc("CreateSolidBrush")
	procSetForegroundWin  = user32.NewProc("SetForegroundWindow")
	procGetUserPrefUILang = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetUserPreferredUILanguages")
)

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     windows.Handle
	hIcon         windows.Handle
	hCursor       windows.Handle
	hbrBackground windows.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       windows.Handle
}

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
	private uint32
}

type minMaxInfo struct {
	reserved, maxSize, maxPosition, minTrackSize, maxTrackSize struct{ x, y int32 }
}

// ptrFrom turns an address Windows gave (a message parameter) into a pointer without a
// uintptr-to-Pointer conversion.
func ptrFrom(u uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&u)) }

func setDPIAware() {
	const perMonitorV2 = ^uintptr(3) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)
	_, _, _ = procSetProcessDpiCtx.Call(perMonitorV2)
}

func dpiOf(hwnd uintptr) int {
	var d uintptr
	if hwnd != 0 {
		d, _, _ = procGetDpiForWindow.Call(hwnd)
	} else {
		d, _, _ = procGetDpiForSystem.Call()
	}
	if d == 0 {
		return 96
	}
	return int(d)
}

func rgb(c uint32) uintptr { return uintptr(c>>16&0xFF | (c>>8&0xFF)<<8 | (c&0xFF)<<16) }

// newWindow registers a class and creates the top-level window, w x h device
// independent pixels, with the icon of the executable's "APP" resource.
func newWindow(class, title string, w, h int, bg uint32, proc uintptr) (uintptr, error) {
	var hinst windows.Handle
	if err := windows.GetModuleHandleEx(0, nil, &hinst); err != nil {
		return 0, err
	}
	brush, _, _ := procCreateSolidBrush.Call(rgb(bg))
	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
	app, _ := windows.UTF16PtrFromString("APP")
	const imageIcon, lrDefaultSize, lrShared = 1, 0x40, 0x8000
	icon, _, _ := procLoadImageW.Call(uintptr(hinst), uintptr(unsafe.Pointer(app)), imageIcon, 0, 0, lrDefaultSize|lrShared)
	cls, _ := windows.UTF16PtrFromString(class)
	wc := wndClassEx{lpfnWndProc: proc, hInstance: hinst, hIcon: windows.Handle(icon), hCursor: windows.Handle(cursor),
		hbrBackground: windows.Handle(brush), lpszClassName: cls, hIconSm: windows.Handle(icon)}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return 0, err
	}
	t, _ := windows.UTF16PtrFromString(title)
	dpi := dpiOf(0)
	hwnd, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(t)),
		wsOverlappedWindow|wsClipChildren, cwUseDefault, cwUseDefault,
		uintptr(w*dpi/96), uintptr(h*dpi/96), 0, 0, uintptr(hinst), 0)
	if hwnd == 0 {
		return 0, err
	}
	return hwnd, nil
}

func defWindowProc(hwnd, m, wp, lp uintptr) uintptr {
	r, _, _ := procDefWindowProcW.Call(hwnd, m, wp, lp)
	return r
}

// messageBox shows a modal message box and waits for it.
func messageBox(hwnd uintptr, title, text string, flags uintptr) uintptr {
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(text)
	r, _, _ := procMessageBoxW.Call(hwnd, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), flags)
	return r
}

var procSetWindowTextW = user32.NewProc("SetWindowTextW")

func setWindowText(hwnd uintptr, text string) {
	if p, err := windows.UTF16PtrFromString(text); err == nil {
		_, _, _ = procSetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(p)))
	}
}

// MessageBox shows an error box with no owner window (for the runtime check and the
// console launcher's fallback).
func MessageBox(title, text string) { messageBox(0, title, text, mbIconError|mbTopmost) }

func loop() {
	var m msg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 { //nolint:gosec // G115: GetMessage returns a BOOL (-1, 0 or 1)
			return
		}
		_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// dispatcher runs functions from other goroutines on the window's thread.
type dispatcher struct {
	hwnd uintptr
	mu   sync.Mutex
	q    []func()
	done bool
}

func (d *dispatcher) post(f func()) {
	d.mu.Lock()
	if d.done {
		d.mu.Unlock()
		return
	}
	d.q = append(d.q, f)
	d.mu.Unlock()
	_, _, _ = procPostMessageW.Call(d.hwnd, wmApp, 0, 0)
}

func (d *dispatcher) drain() {
	d.mu.Lock()
	q := d.q
	d.q = nil
	d.mu.Unlock()
	for _, f := range q {
		f()
	}
}

// stop drops what is queued and refuses more: the window is closing.
func (d *dispatcher) stop() {
	d.mu.Lock()
	d.done, d.q = true, nil
	d.mu.Unlock()
}

// WindowsLanguages returns the user's Windows display languages, preferred first, as
// BCP 47 tags such as "ko-KR".
func WindowsLanguages() []string {
	const muiLanguageName = 0x8
	var num, size uint32
	if r, _, _ := procGetUserPrefUILang.Call(muiLanguageName, uintptr(unsafe.Pointer(&num)), 0, uintptr(unsafe.Pointer(&size))); r == 0 || size == 0 || size > 4096 {
		return nil
	}
	buf := make([]uint16, size)
	if r, _, _ := procGetUserPrefUILang.Call(muiLanguageName, uintptr(unsafe.Pointer(&num)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r == 0 {
		return nil
	}
	var out []string
	start := 0
	for i, c := range buf {
		if c == 0 {
			if i == start {
				break
			}
			out = append(out, windows.UTF16ToString(buf[start:i]))
			start = i + 1
		}
	}
	return out
}
