//go:build windows

package main

import (
	"fmt"
	"math"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A minimal binding to the GDI+ flat API (gdiplus.dll, part of Windows): anti-aliased
// round rectangles, PNG decoding and text. GDI+ takes float32 arguments; the Go
// syscall path on windows/amd64 copies the first four arguments into XMM0-3 as well, so
// a float32 is passed as its bits in a uintptr.

var (
	gdiplus = windows.NewLazySystemDLL("gdiplus.dll")

	pGdiplusStartup           = gdiplus.NewProc("GdiplusStartup")
	pGdipCreateFromHDC        = gdiplus.NewProc("GdipCreateFromHDC")
	pGdipDeleteGraphics       = gdiplus.NewProc("GdipDeleteGraphics")
	pGdipSetSmoothingMode     = gdiplus.NewProc("GdipSetSmoothingMode")
	pGdipSetTextRenderingHint = gdiplus.NewProc("GdipSetTextRenderingHint")
	pGdipSetInterpolationMode = gdiplus.NewProc("GdipSetInterpolationMode")
	pGdipGraphicsClear        = gdiplus.NewProc("GdipGraphicsClear")
	pGdipCreateSolidFill      = gdiplus.NewProc("GdipCreateSolidFill")
	pGdipDeleteBrush          = gdiplus.NewProc("GdipDeleteBrush")
	pGdipCreatePath           = gdiplus.NewProc("GdipCreatePath")
	pGdipDeletePath           = gdiplus.NewProc("GdipDeletePath")
	pGdipAddPathArc           = gdiplus.NewProc("GdipAddPathArc")
	pGdipClosePathFigure      = gdiplus.NewProc("GdipClosePathFigure")
	pGdipFillPath             = gdiplus.NewProc("GdipFillPath")
	pGdipDrawPath             = gdiplus.NewProc("GdipDrawPath")
	pGdipCreatePen1           = gdiplus.NewProc("GdipCreatePen1")
	pGdipDeletePen            = gdiplus.NewProc("GdipDeletePen")
	pGdipCreateFontFamily     = gdiplus.NewProc("GdipCreateFontFamilyFromName")
	pGdipDeleteFontFamily     = gdiplus.NewProc("GdipDeleteFontFamily")
	pGdipCreateFont           = gdiplus.NewProc("GdipCreateFont")
	pGdipDeleteFont           = gdiplus.NewProc("GdipDeleteFont")
	pGdipCreateStringFormat   = gdiplus.NewProc("GdipCreateStringFormat")
	pGdipDeleteStringFormat   = gdiplus.NewProc("GdipDeleteStringFormat")
	pGdipSetStringFormatAlign = gdiplus.NewProc("GdipSetStringFormatAlign")
	pGdipSetStringFormatLine  = gdiplus.NewProc("GdipSetStringFormatLineAlign")
	pGdipDrawString           = gdiplus.NewProc("GdipDrawString")
	pGdipMeasureString        = gdiplus.NewProc("GdipMeasureString")
	pGdipLoadImageFromFile    = gdiplus.NewProc("GdipLoadImageFromFile")
	pGdipGetImageWidth        = gdiplus.NewProc("GdipGetImageWidth")
	pGdipGetImageHeight       = gdiplus.NewProc("GdipGetImageHeight")
	pGdipDrawImageRectI       = gdiplus.NewProc("GdipDrawImageRectI")
	pGdipDisposeImage         = gdiplus.NewProc("GdipDisposeImage")
	pGdipCreateBitmapFromScan = gdiplus.NewProc("GdipCreateBitmapFromScan0")
	pGdipGetImageGraphics     = gdiplus.NewProc("GdipGetImageGraphicsContext")
)

const (
	smoothingAntiAlias   = 4
	textClearTypeGridFit = 5
	interpHighQualityBic = 7
	unitPixel            = 2
	fontRegular          = 0
	fontBold             = 1
	alignNear            = 0
	alignCenter          = 1
	pixelFormat32bppPARG = 0xE200B
)

// rectF is a GDI+ RectF.
type rectF struct{ X, Y, W, H float32 }

func f32(v float32) uintptr { return uintptr(math.Float32bits(v)) }

func gpOK(r uintptr, what string) error {
	if r != 0 {
		return fmt.Errorf("gdiplus %s: status %d", what, r)
	}
	return nil
}

func gdiplusStartup() error {
	input := struct {
		Version                  uint32
		DebugEventCallback       uintptr
		SuppressBackgroundThread int32
		SuppressExternalCodecs   int32
	}{Version: 1}
	var token uintptr
	r, _, _ := pGdiplusStartup.Call(uintptr(unsafe.Pointer(&token)), uintptr(unsafe.Pointer(&input)), 0)
	return gpOK(r, "startup")
}

type graphics uintptr

func graphicsFromHDC(hdc uintptr) graphics {
	var g uintptr
	_, _, _ = pGdipCreateFromHDC.Call(hdc, uintptr(unsafe.Pointer(&g)))
	gr := graphics(g)
	gr.quality()
	return gr
}

func (g graphics) quality() {
	_, _, _ = pGdipSetSmoothingMode.Call(uintptr(g), smoothingAntiAlias)
	_, _, _ = pGdipSetTextRenderingHint.Call(uintptr(g), textClearTypeGridFit)
	_, _, _ = pGdipSetInterpolationMode.Call(uintptr(g), interpHighQualityBic)
}

func (g graphics) close() { _, _, _ = pGdipDeleteGraphics.Call(uintptr(g)) }

func (g graphics) clear(argb uint32) { _, _, _ = pGdipGraphicsClear.Call(uintptr(g), uintptr(argb)) }

func solid(argb uint32) uintptr {
	var b uintptr
	_, _, _ = pGdipCreateSolidFill.Call(uintptr(argb), uintptr(unsafe.Pointer(&b)))
	return b
}

func roundPath(x, y, w, h, r float32) uintptr {
	var p uintptr
	_, _, _ = pGdipCreatePath.Call(0, uintptr(unsafe.Pointer(&p)))
	d := 2 * r
	if d > h {
		d = h
	}
	if d > w {
		d = w
	}
	arc := func(ax, ay, start float32) {
		_, _, _ = pGdipAddPathArc.Call(p, f32(ax), f32(ay), f32(d), f32(d), f32(start), f32(90))
	}
	arc(x, y, 180)
	arc(x+w-d, y, 270)
	arc(x+w-d, y+h-d, 0)
	arc(x, y+h-d, 90)
	_, _, _ = pGdipClosePathFigure.Call(p)
	return p
}

// fillRound fills a round rectangle.
func (g graphics) fillRound(x, y, w, h, r float32, argb uint32) {
	p := roundPath(x, y, w, h, r)
	b := solid(argb)
	_, _, _ = pGdipFillPath.Call(uintptr(g), b, p)
	_, _, _ = pGdipDeleteBrush.Call(b)
	_, _, _ = pGdipDeletePath.Call(p)
}

// strokeRound outlines a round rectangle (the keyboard focus ring).
func (g graphics) strokeRound(x, y, w, h, r, width float32, argb uint32) {
	p := roundPath(x, y, w, h, r)
	var pen uintptr
	_, _, _ = pGdipCreatePen1.Call(uintptr(argb), f32(width), unitPixel, uintptr(unsafe.Pointer(&pen)))
	_, _, _ = pGdipDrawPath.Call(uintptr(g), pen, p)
	_, _, _ = pGdipDeletePen.Call(pen)
	_, _, _ = pGdipDeletePath.Call(p)
}

// card draws a white round card with a soft shadow.
func (g graphics) card(x, y, w, h, r, s float32) {
	g.fillRound(x, y+4*s, w, h, r, 0x0c1e468c)
	g.fillRound(x, y+2*s, w, h, r, 0x0a1e468c)
	g.fillRound(x, y, w, h, r, 0xffffffff)
}

type font struct{ fam, f uintptr }

func newFont(family string, px float32, bold bool) font {
	name, _ := windows.UTF16PtrFromString(family)
	var fo font
	_, _, _ = pGdipCreateFontFamily.Call(uintptr(unsafe.Pointer(name)), 0, uintptr(unsafe.Pointer(&fo.fam)))
	style := uintptr(fontRegular)
	if bold {
		style = fontBold
	}
	_, _, _ = pGdipCreateFont.Call(fo.fam, f32(px), style, unitPixel, uintptr(unsafe.Pointer(&fo.f)))
	return fo
}

func (f font) close() {
	_, _, _ = pGdipDeleteFont.Call(f.f)
	_, _, _ = pGdipDeleteFontFamily.Call(f.fam)
}

func stringFormat(h, v uintptr) uintptr {
	var sf uintptr
	_, _, _ = pGdipCreateStringFormat.Call(0, 0, uintptr(unsafe.Pointer(&sf)))
	_, _, _ = pGdipSetStringFormatAlign.Call(sf, h)
	_, _, _ = pGdipSetStringFormatLine.Call(sf, v)
	return sf
}

// text draws s in the box, wrapping, aligned by h and v.
func (g graphics) text(s string, f font, box rectF, argb uint32, h, v uintptr) {
	u, _ := windows.UTF16FromString(s)
	b := solid(argb)
	sf := stringFormat(h, v)
	_, _, _ = pGdipDrawString.Call(uintptr(g), uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), f.f,
		uintptr(unsafe.Pointer(&box)), sf, b)
	_, _, _ = pGdipDeleteStringFormat.Call(sf)
	_, _, _ = pGdipDeleteBrush.Call(b)
}

// measure returns the width s needs on one line.
func (g graphics) measure(s string, f font) float32 {
	u, _ := windows.UTF16FromString(s)
	sf := stringFormat(alignNear, alignNear)
	layout := rectF{W: 10000, H: 1000}
	var out rectF
	_, _, _ = pGdipMeasureString.Call(uintptr(g), uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), f.f,
		uintptr(unsafe.Pointer(&layout)), sf, uintptr(unsafe.Pointer(&out)), 0, 0)
	_, _, _ = pGdipDeleteStringFormat.Call(sf)
	return out.W
}

// image is a decoded GDI+ image or bitmap.
type image uintptr

func loadImage(path string) (image, int, int, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, 0, err
	}
	var img uintptr
	r, _, _ := pGdipLoadImageFromFile.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&img)))
	if err := gpOK(r, "load "+path); err != nil {
		return 0, 0, 0, err
	}
	var w, h uint32
	_, _, _ = pGdipGetImageWidth.Call(img, uintptr(unsafe.Pointer(&w)))
	_, _, _ = pGdipGetImageHeight.Call(img, uintptr(unsafe.Pointer(&h)))
	return image(img), int(w), int(h), nil
}

func (i image) close() { _, _, _ = pGdipDisposeImage.Call(uintptr(i)) }

func (g graphics) draw(i image, x, y, w, h int) {
	_, _, _ = pGdipDrawImageRectI.Call(uintptr(g), uintptr(i), uintptr(x), uintptr(y), uintptr(w), uintptr(h))
}

// scaled returns a copy of src resampled once to w x h, so painting does not scale
// the 1204 x 1060 mascot every frame.
func scaled(src image, w, h int) image {
	var bmp uintptr
	_, _, _ = pGdipCreateBitmapFromScan.Call(uintptr(w), uintptr(h), 0, pixelFormat32bppPARG, 0, uintptr(unsafe.Pointer(&bmp)))
	var g uintptr
	_, _, _ = pGdipGetImageGraphics.Call(bmp, uintptr(unsafe.Pointer(&g)))
	gr := graphics(g)
	gr.quality()
	gr.draw(src, 0, 0, w, h)
	gr.close()
	return image(bmp)
}
