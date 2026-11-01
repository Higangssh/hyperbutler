//go:build windows

package webview2

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// LoaderName is the WebView2 loader shipped next to hyperbutler.exe (Microsoft's
// WebView2 SDK, BSD-3-Clause).
const LoaderName = "WebView2Loader.dll"

// RuntimeURL is the download page of the Evergreen WebView2 Runtime, for the message
// shown when it is missing. It is part of Windows 11.
const RuntimeURL = "https://go.microsoft.com/fwlink/p/?LinkId=2124703"

// ErrNoRuntime is returned when the WebView2 Runtime is not installed.
var ErrNoRuntime = errors.New("the Microsoft Edge WebView2 Runtime is not installed")

// ErrNoLoader is returned when WebView2Loader.dll is not next to the executable.
var ErrNoLoader = errors.New(LoaderName + " is not next to hyperbutler.exe")

var (
	loaderOnce sync.Once
	loaderErr  error
	procCreate uintptr
	procGetVer uintptr
)

// loadLoader loads WebView2Loader.dll from the executable's directory by full path.
// LOAD_WITH_ALTERED_SEARCH_PATH makes the DLL's own dependencies resolve from its
// directory and the system, never from the working directory.
func loadLoader() error {
	loaderOnce.Do(func() {
		self, err := os.Executable()
		if err != nil {
			loaderErr = err
			return
		}
		p := filepath.Join(filepath.Dir(self), LoaderName)
		if _, err := os.Stat(p); err != nil {
			loaderErr = fmt.Errorf("%w (%s)", ErrNoLoader, p)
			return
		}
		h, err := windows.LoadLibraryEx(p, 0, windows.LOAD_WITH_ALTERED_SEARCH_PATH)
		if err != nil {
			loaderErr = fmt.Errorf("load %s: %w", p, err)
			return
		}
		if procCreate, err = windows.GetProcAddress(h, "CreateCoreWebView2EnvironmentWithOptions"); err != nil {
			loaderErr = fmt.Errorf("%s: %w", p, err)
			return
		}
		if procGetVer, err = windows.GetProcAddress(h, "GetAvailableCoreWebView2BrowserVersionString"); err != nil {
			loaderErr = fmt.Errorf("%s: %w", p, err)
		}
	})
	return loaderErr
}

// RuntimeVersion returns the installed WebView2 Runtime's version, ErrNoRuntime when
// there is none, or ErrNoLoader when the loader DLL is missing.
func RuntimeVersion() (string, error) {
	if err := loadLoader(); err != nil {
		return "", err
	}
	var p uintptr
	hr, _, _ := syscallN(procGetVer, 0, uintptr(unsafe.Pointer(&p)))
	if failed(hr) {
		if p != 0 {
			windows.CoTaskMemFree(ptrFrom(p))
		}
		if hr32(hr)&0xFFFF == uint32(windows.ERROR_FILE_NOT_FOUND) {
			return "", ErrNoRuntime
		}
		return "", &HRESULTError{Op: "GetAvailableCoreWebView2BrowserVersionString", HR: hr32(hr)}
	}
	v, ok := takeString(p, 256)
	if !ok || v == "" {
		return "", ErrNoRuntime
	}
	return v, nil
}
