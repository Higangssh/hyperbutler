package guid

import (
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TestSameAsWindows: GUID converts to golang.org/x/sys/windows.GUID (Windows' GUID) with
// a plain conversion, has its memory layout, and agrees with Windows on the text form.
func TestSameAsWindows(t *testing.T) {
	if unsafe.Sizeof(GUID{}) != unsafe.Sizeof(windows.GUID{}) || unsafe.Alignof(GUID{}) != unsafe.Alignof(windows.GUID{}) {
		t.Fatal("layout differs from windows.GUID")
	}
	for i := 0; i < 1000; i++ {
		g, err := NewV4()
		if err != nil {
			t.Fatal(err)
		}
		w := windows.GUID(g)
		if got := strings.Trim(strings.ToLower(w.String()), "{}"); got != g.String() {
			t.Fatalf("%s: Windows says %s", g, w)
		}
		if *(*[16]byte)(unsafe.Pointer(&w)) != g.ToWindowsArray() {
			t.Fatalf("%s: memory bytes differ from ToWindowsArray", g)
		}
		if p, err := windows.GUIDFromString("{" + strings.ToUpper(g.String()) + "}"); err != nil || GUID(p) != g {
			t.Fatalf("Windows parsed %s as %s (%v)", g, p, err)
		}
	}
}
