//go:build windows

package spikeui

import (
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Win32 messages and constants both spikes use.
const (
	WM_DESTROY      = 0x0002
	WM_MOVE         = 0x0003
	WM_SIZE         = 0x0005
	WM_SETFOCUS     = 0x0007
	WM_PAINT        = 0x000F
	WM_CLOSE        = 0x0010
	WM_ERASEBKGND   = 0x0014
	WM_COMMAND      = 0x0111
	WM_DRAWITEM     = 0x002B
	WM_KEYDOWN      = 0x0100
	WM_LBUTTONUP    = 0x0202
	WM_DPICHANGED   = 0x02E0
	WM_APP          = 0x8000
	WS_OVERLAPPED   = 0x00CF0000 // WS_OVERLAPPEDWINDOW
	WS_CHILD        = 0x40000000
	WS_VISIBLE      = 0x10000000
	WS_TABSTOP      = 0x00010000
	WS_CLIPCHILD    = 0x02000000
	CW_USEDEFAULT   = ^uintptr(0x7FFFFFFF) // 0x80000000 as a signed int
	SW_SHOW         = 5
	SWP_NOZORDER    = 0x0004
	SWP_NOACTIVATE  = 0x0010
	MB_ICONERROR    = 0x10
	IDC_ARROW       = 32512
	IDI_APPLICATION = 32512
)

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	gdi32                = windows.NewLazySystemDLL("gdi32.dll")
	procRegisterClassExW = user32.NewProc("RegisterClassExW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procShowWindow       = user32.NewProc("ShowWindow")
	procUpdateWindow     = user32.NewProc("UpdateWindow")
	procGetMessageW      = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
	procIsDialogMessageW = user32.NewProc("IsDialogMessageW")
	procPostMessageW     = user32.NewProc("PostMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procMessageBoxW      = user32.NewProc("MessageBoxW")
	procSetProcessDpiCtx = user32.NewProc("SetProcessDpiAwarenessContext")
	procGetDpiForWindow  = user32.NewProc("GetDpiForWindow")
	procGetDpiForSystem  = user32.NewProc("GetDpiForSystem")
	procSetWindowPos     = user32.NewProc("SetWindowPos")
	procLoadCursorW      = user32.NewProc("LoadCursorW")
	procLoadIconW        = user32.NewProc("LoadIconW")
	procGetClientRect    = user32.NewProc("GetClientRect")
	procInvalidateRect   = user32.NewProc("InvalidateRect")
	procCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")
)

// Rect is a Win32 RECT.
type Rect struct{ Left, Top, Right, Bottom int32 }

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

// SetDPIAware makes the process per-monitor DPI aware (v2), so text is drawn at the
// monitor's resolution instead of being stretched by Windows. It must run before the
// first window is created.
func SetDPIAware() {
	const perMonitorV2 = ^uintptr(3) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)
	_, _, _ = procSetProcessDpiCtx.Call(perMonitorV2)
}

// DPI returns the window's DPI, or the system's for hwnd 0.
func DPI(hwnd uintptr) int {
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

// RGB is a Win32 COLORREF.
func RGB(r, g, b byte) uint32 { return uint32(r) | uint32(g)<<8 | uint32(b)<<16 }

// NewWindow registers a class and creates a top-level window of w x h device-independent
// pixels at the system DPI, with bg as its background so no white frame shows before
// the first paint. It does not show the window.
func NewWindow(class, title string, w, h int, bg uint32, proc func(hwnd, msg, wp, lp uintptr) uintptr) uintptr {
	hinst, _ := getModuleHandle()
	brush, _, _ := procCreateSolidBrush.Call(uintptr(bg))
	cursor, _, _ := procLoadCursorW.Call(0, IDC_ARROW)
	icon, _, _ := procLoadIconW.Call(0, IDI_APPLICATION)
	cls, _ := windows.UTF16PtrFromString(class)
	wc := wndClassEx{
		lpfnWndProc:   windows.NewCallback(proc),
		hInstance:     hinst,
		hIcon:         windows.Handle(icon),
		hCursor:       windows.Handle(cursor),
		hbrBackground: windows.Handle(brush),
		lpszClassName: cls,
		hIconSm:       windows.Handle(icon),
	}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	_, _, _ = procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	t, _ := windows.UTF16PtrFromString(title)
	dpi := DPI(0)
	hwnd, _, _ := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(t)),
		WS_OVERLAPPED|WS_CLIPCHILD, CW_USEDEFAULT, CW_USEDEFAULT,
		uintptr(w*dpi/96), uintptr(h*dpi/96), 0, 0, uintptr(hinst), 0)
	return hwnd
}

func getModuleHandle() (windows.Handle, error) {
	var h windows.Handle
	err := windows.GetModuleHandleEx(0, nil, &h)
	return h, err
}

// Show shows and paints hwnd.
func Show(hwnd uintptr) {
	_, _, _ = procShowWindow.Call(hwnd, SW_SHOW)
	_, _, _ = procUpdateWindow.Call(hwnd)
}

// DefWindowProc calls DefWindowProcW.
func DefWindowProc(hwnd, msg, wp, lp uintptr) uintptr {
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

// Quit posts WM_QUIT.
func Quit() { _, _, _ = procPostQuitMessage.Call(0) }

// ClientRect returns hwnd's client rectangle.
func ClientRect(hwnd uintptr) Rect {
	var r Rect
	_, _, _ = procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	return r
}

// Invalidate schedules a repaint of hwnd.
func Invalidate(hwnd uintptr) { _, _, _ = procInvalidateRect.Call(hwnd, 0, 0) }

// ApplyDPIChange moves hwnd to the rectangle WM_DPICHANGED suggests.
func ApplyDPIChange(hwnd, lp uintptr) {
	r := *(**Rect)(unsafe.Pointer(&lp)) // lp is a *RECT; this form keeps vet quiet
	_, _, _ = procSetWindowPos.Call(hwnd, 0, uintptr(r.Left), uintptr(r.Top),
		uintptr(r.Right-r.Left), uintptr(r.Bottom-r.Top), SWP_NOZORDER|SWP_NOACTIVATE)
}

// SetBounds moves a child window.
func SetBounds(hwnd uintptr, x, y, w, h int) {
	_, _, _ = procSetWindowPos.Call(hwnd, 0, uintptr(x), uintptr(y), uintptr(w), uintptr(h), SWP_NOZORDER|SWP_NOACTIVATE)
}

// MessageBox shows a modal error box.
func MessageBox(hwnd uintptr, title, text string) {
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(text)
	_, _, _ = procMessageBoxW.Call(hwnd, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), MB_ICONERROR)
}

// Loop runs the message loop until WM_QUIT. With dialogNav, Tab and arrow keys move
// the focus between child controls (IsDialogMessage).
func Loop(top uintptr, dialogNav bool) {
	var m msg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
		if dialogNav {
			if r, _, _ := procIsDialogMessageW.Call(top, uintptr(unsafe.Pointer(&m))); r != 0 {
				continue
			}
		}
		_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// Dispatcher runs functions from other goroutines on the window's thread: Post queues
// one and posts WM_APP, and the window procedure calls Drain on WM_APP.
type Dispatcher struct {
	Hwnd uintptr
	mu   sync.Mutex
	q    []func()
}

// Post queues f for the UI thread.
func (d *Dispatcher) Post(f func()) {
	d.mu.Lock()
	d.q = append(d.q, f)
	d.mu.Unlock()
	_, _, _ = procPostMessageW.Call(d.Hwnd, WM_APP, 0, 0)
}

// Drain runs the queued functions.
func (d *Dispatcher) Drain() {
	d.mu.Lock()
	q := d.q
	d.q = nil
	d.mu.Unlock()
	for _, f := range q {
		f()
	}
}
