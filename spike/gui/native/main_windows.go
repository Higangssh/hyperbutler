//go:build windows

// Command native is the native half of the GUI toolkit spike (ADR 0008 "GUI shape"):
// the home screen of visual direction "B" drawn with raw Win32 and GDI+ from pure Go
// (golang.org/x/sys/windows, no cgo, no third-party module). The Start button is a real
// Win32 BUTTON (owner-drawn as a pill), so it takes keyboard focus and is in the UI
// Automation tree; everything else is painted and is not.
//
// It prints "HB-SPIKE painted ..." on stdout after the first paint, for
// spike/gui/measure.
//
//	go build -ldflags "-H windowsgui" -o native.exe ./spike/gui/native
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/spike/gui/internal/spikeui"
)

const (
	idStart     = 100
	bsOwnerDraw = 0x0000000B
	bnClicked   = 0
	odsFocus    = 0x0010
	odsDisabled = 0x0004
	srcCopy     = 0x00CC0020
)

// Colors, as GDI+ ARGB.
const (
	cGround = 0xffe6f1fd
	cInk    = 0xff1d2a3a
	cSoft   = 0xff5b6b80
	cAccent = 0xff2f7cf6
	cLive   = 0xff1f9d63
	cOff    = 0xff8a96a8
	cFocus  = 0xff9cc2ff
	cFaded  = 0xff97bdfa
)

var (
	user32  = windows.NewLazySystemDLL("user32.dll")
	gdi32   = windows.NewLazySystemDLL("gdi32.dll")
	pBegin  = user32.NewProc("BeginPaint")
	pEnd    = user32.NewProc("EndPaint")
	pCreate = user32.NewProc("CreateWindowExW")
	pSetTxt = user32.NewProc("SetWindowTextW")
	pEnable = user32.NewProc("EnableWindow")
	pInval  = user32.NewProc("InvalidateRect")
	pCDC    = gdi32.NewProc("CreateCompatibleDC")
	pCBmp   = gdi32.NewProc("CreateCompatibleBitmap")
	pSel    = gdi32.NewProc("SelectObject")
	pBlt    = gdi32.NewProc("BitBlt")
	pDelDC  = gdi32.NewProc("DeleteDC")
	pDelObj = gdi32.NewProc("DeleteObject")
)

var (
	disp      spikeui.Dispatcher
	msgs      map[string]string
	view      spikeui.View
	hbExe     string
	hbErr     error
	realRun   *bool
	family    string
	button    uintptr
	mascot    image
	mascotW   int
	mascotH   int
	mascotFit image
	fitDPI    int
	painted   bool
)

type paintStruct struct {
	hdc         uintptr
	fErase      int32
	rcPaint     spikeui.Rect
	fRestore    int32
	fIncUpdate  int32
	rgbReserved [32]byte
}

type drawItem struct {
	_, _, _, _, itemState uint32  // CtlType, CtlID, itemID, itemAction, itemState
	_, hDC                uintptr // hwndItem, hDC
	rcItem                spikeui.Rect
	_                     uintptr // itemData
}

// fontFor picks a face that has the language's script: GDI+ does not fall back to
// another font for missing glyphs the way DirectWrite and browsers do.
func fontFor(lang string) string {
	switch strings.ToLower(lang[:min(2, len(lang))]) {
	case "ko":
		return "Malgun Gothic"
	case "ja":
		return "Yu Gothic UI"
	case "zh":
		return "Microsoft YaHei UI"
	}
	return "Segoe UI"
}

func main() {
	runtime.LockOSThread()
	hbFlag := flag.String("hb", "", "hyperbutler.exe to run for status and start (default: next to this program, then PATH)")
	lang := flag.String("lang", "", "UI language, for example en or ko (default: the Windows user locale)")
	mascotPath := flag.String("mascot", "", "mascot PNG (default: assets/brand/mascot.png above the working directory)")
	realRun = flag.Bool("real-start", false, "make Start run `hyperbutler start` instead of a stub")
	flag.Parse()

	if *lang == "" {
		*lang = spikeui.UserLanguage()
	}
	msgs = spikeui.Messages(*lang)
	view = spikeui.Initial(msgs)
	family = fontFor(*lang)
	hbExe, hbErr = spikeui.FindCLI(*hbFlag)

	if err := gdiplusStartup(); err != nil {
		spikeui.MessageBox(0, msgs[spikeui.KeyTitle], err.Error())
		os.Exit(1)
	}
	p := *mascotPath
	if p == "" {
		p, _ = spikeui.FindAsset("assets/brand/mascot.png")
	}
	if p != "" {
		mascot, mascotW, mascotH, _ = loadImage(p)
	}

	spikeui.SetDPIAware()
	hwnd := spikeui.NewWindow("hbSpikeNative", msgs[spikeui.KeyTitle], 960, 620, spikeui.RGB(0xe6, 0xf1, 0xfd), wndproc)
	disp.Hwnd = hwnd
	cls, _ := windows.UTF16PtrFromString("BUTTON")
	label, _ := windows.UTF16PtrFromString(view.Start)
	hinst, _ := getModuleHandle()
	button, _, _ = pCreate.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(label)),
		spikeui.WS_CHILD|spikeui.WS_VISIBLE|spikeui.WS_TABSTOP|bsOwnerDraw, 0, 0, 10, 10, hwnd, idStart, uintptr(hinst), 0)
	layoutButton(hwnd)
	setButton()
	spikeui.Show(hwnd)
	go refresh()
	spikeui.Loop(hwnd, true)
}

func getModuleHandle() (windows.Handle, error) {
	var h windows.Handle
	err := windows.GetModuleHandleEx(0, nil, &h)
	return h, err
}

func scale(hwnd uintptr) float32 { return float32(spikeui.DPI(hwnd)) / 96 }

func layoutButton(hwnd uintptr) {
	s := scale(hwnd)
	spikeui.SetBounds(button, int(28*s), int(396*s), int(170*s), int(50*s))
}

func setButton() {
	t, _ := windows.UTF16PtrFromString(view.Start)
	_, _, _ = pSetTxt.Call(button, uintptr(unsafe.Pointer(t)))
	on := uintptr(1)
	if view.Busy || view.Live || view.Loading {
		on = 0
	}
	_, _, _ = pEnable.Call(button, on)
	_, _, _ = pInval.Call(button, 0, 1)
}

// refresh reads the engine status once. The GUI does not poll (REQ-UX-011).
func refresh() {
	var v spikeui.View
	if hbErr != nil {
		v = spikeui.MakeView(msgs, spikeui.Status{}, fmt.Errorf("%s", msgs[spikeui.KeyNoCLI]))
	} else {
		s, err := spikeui.QueryStatus(context.Background(), hbExe)
		v = spikeui.MakeView(msgs, s, err)
	}
	disp.Post(func() { render(v) })
}

func render(v spikeui.View) {
	view = v
	setButton()
	spikeui.Invalidate(disp.Hwnd)
}

func onStart() {
	if view.Busy || hbErr != nil {
		return
	}
	v := view
	v.Busy, v.Start = true, msgs[spikeui.KeyStarting]
	render(v)
	go func() {
		err := spikeui.StartEngine(context.Background(), hbExe, *realRun)
		disp.Post(func() {
			if err != nil && !*realRun {
				v := view
				v.Busy, v.Start, v.Note = false, msgs[spikeui.KeyStart], msgs[spikeui.KeyStubbed]
				render(v)
				return
			}
			go refresh()
		})
	}()
}

func paint(hwnd uintptr) {
	var ps paintStruct
	hdc, _, _ := pBegin.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	rc := spikeui.ClientRect(hwnd)
	w, h := int(rc.Right), int(rc.Bottom)
	mem, _, _ := pCDC.Call(hdc)
	bmp, _, _ := pCBmp.Call(hdc, uintptr(w), uintptr(h))
	old, _, _ := pSel.Call(mem, bmp)
	g := graphicsFromHDC(mem)
	drawHome(g, hwnd, float32(w))
	g.close()
	_, _, _ = pBlt.Call(hdc, 0, 0, uintptr(w), uintptr(h), mem, 0, 0, srcCopy)
	_, _, _ = pSel.Call(mem, old)
	_, _, _ = pDelObj.Call(bmp)
	_, _, _ = pDelDC.Call(mem)
	_, _, _ = pEnd.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	if !painted {
		painted = true
		spikeui.ReportFirstPaint("native")
	}
}

func drawHome(g graphics, hwnd uintptr, width float32) {
	s := scale(hwnd)
	g.clear(cGround)

	// Pill tabs, sized to their text so longer translations fit.
	tab := newFont(family, 15*s, false)
	tabB := newFont(family, 15*s, true)
	x := 28 * s
	for i, k := range []string{spikeui.KeyTabHome, spikeui.KeyTabContainer, spikeui.KeyTabStorage, spikeui.KeyTabSettings} {
		f, ink := tab, uint32(cSoft)
		if i == 0 {
			f, ink = tabB, cInk
		}
		tw := g.measure(msgs[k], f) + 36*s
		if i == 0 {
			g.fillRound(x, 20*s, tw, 36*s, 18*s, 0xffffffff)
		}
		g.text(msgs[k], f, rectF{x, 20 * s, tw, 36 * s}, ink, alignCenter, alignCenter)
		x += tw + 8*s
	}
	tab.close()
	tabB.close()

	// Greeting card with the mascot.
	cw := width - 56*s
	g.card(28*s, 76*s, cw, 174*s, 24*s, s)
	if mascot != 0 {
		mh := int(130 * s)
		mw := mh * mascotW / mascotH
		if mascotFit == 0 || fitDPI != spikeui.DPI(hwnd) {
			if mascotFit != 0 {
				mascotFit.close()
			}
			mascotFit, fitDPI = scaled(mascot, mw, mh), spikeui.DPI(hwnd)
		}
		g.draw(mascotFit, int(54*s), int(98*s), mw, mh)
	}
	h1 := newFont(family, 26*s, true)
	body := newFont(family, 15*s, false)
	tx := 54*s + 172*s
	g.text(msgs[spikeui.KeyGreeting], h1, rectF{tx, 104 * s, cw - tx, 40 * s}, cInk, alignNear, alignNear)
	g.text(msgs[spikeui.KeyGreetingSub], body, rectF{tx, 148 * s, cw - tx - 20*s, 70 * s}, cSoft, alignNear, alignNear)
	h1.close()

	// Four stat cards with big numbers.
	label := newFont(family, 14*s, false)
	big := newFont(family, 34*s, true)
	gap := 14 * s
	sw := (cw - 3*gap) / 4
	stats := []struct {
		k, v string
		ink  uint32
	}{
		{spikeui.KeyEngine, view.Engine, map[bool]uint32{true: cLive, false: cOff}[view.Live]},
		{spikeui.KeyMemory, view.Memory, cInk},
		{spikeui.KeyCPUs, view.CPUs, cInk},
		{spikeui.KeyDisk, view.Disk, cInk},
	}
	for i, st := range stats {
		cx := 28*s + float32(i)*(sw+gap)
		g.card(cx, 268*s, sw, 110*s, 24*s, s)
		g.text(msgs[st.k], label, rectF{cx + 24*s, 286 * s, sw - 36*s, 22 * s}, cSoft, alignNear, alignNear)
		// Shrink a value that does not fit (a longer translation) instead of clipping it;
		// CSS layout does this kind of thing on its own, here it is code per element.
		f, px := big, float32(34)
		for px > 18 && g.measure(st.v, f) > sw-34*s {
			if f != big {
				f.close()
			}
			px -= 2
			f = newFont(family, px*s, true)
		}
		g.text(st.v, f, rectF{cx + 22*s, (312 + (34-px)/2) * s, sw - 30*s, 50 * s}, st.ink, alignNear, alignNear)
		if f != big {
			f.close()
		}
	}
	label.close()
	big.close()

	note := newFont(family, 13*s, false)
	g.text(view.Note, note, rectF{214 * s, 396 * s, cw - 200*s, 50 * s}, cSoft, alignNear, alignCenter)
	note.close()
	body.close()
}

func drawButton(di *drawItem) {
	w := float32(di.rcItem.Right - di.rcItem.Left)
	h := float32(di.rcItem.Bottom - di.rcItem.Top)
	s := scale(disp.Hwnd)
	g := graphicsFromHDC(di.hDC)
	g.clear(cGround)
	fill := uint32(cAccent)
	if di.itemState&odsDisabled != 0 {
		fill = cFaded
	}
	g.fillRound(3*s, 3*s, w-6*s, h-6*s, (h-6*s)/2, fill)
	if di.itemState&odsFocus != 0 {
		g.strokeRound(1.5*s, 1.5*s, w-3*s, h-3*s, (h-3*s)/2, 3*s, cFocus)
	}
	f := newFont(family, 16*s, true)
	g.text(view.Start, f, rectF{0, 0, w, h}, 0xffffffff, alignCenter, alignCenter)
	f.close()
	g.close()
}

func wndproc(hwnd, msg, wp, lp uintptr) uintptr {
	switch msg {
	case spikeui.WM_ERASEBKGND:
		return 1
	case spikeui.WM_PAINT:
		paint(hwnd)
		return 0
	case spikeui.WM_DRAWITEM:
		drawButton(*(**drawItem)(unsafe.Pointer(&lp))) // lp is a *DRAWITEMSTRUCT
		return 1
	case spikeui.WM_COMMAND:
		if wp&0xffff == idStart && wp>>16 == bnClicked {
			onStart()
		}
		return 0
	case spikeui.WM_DPICHANGED:
		spikeui.ApplyDPIChange(hwnd, lp)
		layoutButton(hwnd)
		spikeui.Invalidate(hwnd)
		return 0
	case spikeui.WM_SIZE:
		spikeui.Invalidate(hwnd)
		return 0
	case spikeui.WM_APP:
		disp.Drain()
		return 0
	case spikeui.WM_DESTROY:
		spikeui.Quit()
		return 0
	}
	return spikeui.DefWindowProc(hwnd, msg, wp, lp)
}
