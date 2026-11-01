//go:build windows

// Package app is the GUI's window (ADR 0008): a Win32 window holding a WebView2 control
// (internal/gui/webview2) that shows the page of internal/gui/web, with the page's
// requests answered by internal/gui/bridge. It runs on demand and exits when its window
// closes; while it runs it polls nothing on its own, and the page refreshes only while
// the window is visible (REQ-UX-011).
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/gui/bridge"
	"github.com/Higangssh/hyperbutler/internal/gui/web"
	"github.com/Higangssh/hyperbutler/internal/gui/webview2"
)

// Origin is the page's origin. It is served from memory: there is no port, and the
// WebView2 control answers every request itself.
const Origin = "https://hb.localhost"

// Exit codes of Run.
const (
	ExitOK        = 0
	ExitFailed    = 1
	ExitNoRuntime = 3 // the WebView2 Runtime (or its loader) is missing; a message box said so
)

// BrowserArgs are the WebView2 browser arguments. Software compositing (--disable-gpu)
// halves its memory and, in the spike, was the only setting with no process left
// behind after 31 closes (ADR 0008). The others turn off the browser's own background
// requests, and the resolver rule makes every name lookup of Chromium's network stack fail,
// so even a request the page could not make would go nowhere. The page itself loads
// nothing from the network: every request is answered in process.
//
// WebRTC (docs/gui.md, measured with bench/footprint -check-webrtc): with
// --webrtc-ip-handling-policy=disable_non_proxied_udp an RTCPeerConnection gathered no
// candidate (one without it), but the runtime's network process still opened UDP
// sockets for it (mDNS on 5353 and a pair of ephemeral ports), and the CSP has no
// directive that stops it (webrtc 'block' changed nothing). What keeps the socket table
// empty is web.Guard, which removes RTCPeerConnection before the page runs; the policy
// stays as a second line. --force-webrtc-ip-handling-policy is not in the runtime and
// was dropped.
//
// Memory (REQ-MEM-001's 100 MiB, docs/gui.md): the compositor (with --disable-gpu, the
// software "GPU" process) and the network service run in the browser process
// (--in-process-gpu, NetworkServiceInProcess2), as Android's WebView runs them. That is
// two processes fewer and about 10 MiB less private working set, measured after every
// screen was shown. The page loads nothing from the network and draws in software, so
// what that isolation protects is not there; a crash of either ends the browser
// process, which closes the window (OnProcessFailed) as a crash of the browser always
// did. --renderer-process-limit=1 and --disable-extensions were measured too and
// changed nothing (there is one renderer and no extension).
const BrowserArgs = "--in-process-gpu --enable-features=NetworkServiceInProcess2 " +
	"--disable-gpu --disable-background-networking --disable-component-update " +
	"--disable-domain-reliability --no-pings --disable-sync " +
	"--webrtc-ip-handling-policy=disable_non_proxied_udp " +
	"\"--host-resolver-rules=MAP * ~NOTFOUND\""

// Options configure the window.
type Options struct {
	Bridge bridge.Options
	// Title is the window title.
	Title string
	// DataDir holds the browser profile (WebView2's user data folder).
	DataDir string
	// Language is the browser's UI language.
	Language string
	// NoRuntimeTitle and NoRuntimeText are the message shown when the WebView2 Runtime
	// is missing (translated by the caller; RuntimeURL goes in the text).
	NoRuntimeTitle, NoRuntimeText string
	// CloseNote, if set, is called when the window is asked to close and returns a
	// note to show once before it closes (S0.3: the engine keeps running), or "".
	CloseNote func() string
	// ReportPaint receives "HB-GUI painted since_create_ms=N" at the first paint, for
	// bench/footprint; nil for none.
	ReportPaint io.Writer
	// Log receives diagnostic lines; nil for none.
	Log func(string)
	// InstanceKey names the one window allowed per browser profile (the resolved data
	// directory); a second Run with the same key brings the first window to the front
	// and returns, or says so when that window cannot be found.
	InstanceKey string
	// ImportRunningText asks, when the window is closed during an import, whether to
	// stop it and close; StoppingText is shown while the import stops; NotRespondingText
	// says that another window holds the instance but cannot be found. Translated by the
	// caller.
	ImportRunningText, StoppingText, NotRespondingText string
}

// importStopGrace bounds the wait for a canceled import before the process ends: the
// import undoes its item in progress and checks Docker Desktop, within about 30 s.
const importStopGrace = 40 * time.Second

// state is the one window of the process; the window procedure reaches it here.
type state struct {
	o       Options
	hwnd    uintptr
	view    *webview2.View
	disp    dispatcher
	d       *bridge.Dispatcher
	ctx     context.Context
	cancel  context.CancelFunc
	visible atomic.Bool
	closing bool
	// stopping is set while the window waits for a canceled import before it closes.
	stopping bool
}

var st *state

// Run opens the window and returns when it has closed, with an exit code. It must be
// called once per process, on the goroutine that should own the window.
func Run(o Options) int {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if o.Log == nil {
		o.Log = func(string) {}
	}
	if st != nil {
		return ExitFailed
	}
	// Every failure says so in a message box: a double-click has no console to print to.
	failed := func(what string, err error) int {
		o.Log(what + ": " + err.Error())
		MessageBox(o.Title, "The window could not be shown ("+what+"): "+err.Error())
		return ExitFailed
	}
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED|windows.COINIT_DISABLE_OLE1DDE); err != nil {
		return failed("CoInitializeEx", err)
	}
	defer windows.CoUninitialize()
	setDPIAware()
	// One window per state directory: the browser profile is shared, and closing the
	// first window would end the second one's browser processes with its job.
	if o.InstanceKey != "" {
		release, first, found := singleInstance(o.InstanceKey)
		if !first {
			if !found {
				o.Log("another window holds this profile, but no window of it was found")
				MessageBox(o.Title, o.NotRespondingText)
				return ExitFailed
			}
			o.Log("another window is open; bringing it to the front")
			return ExitOK
		}
		defer release()
	}

	ver, err := webview2.RuntimeVersion()
	if err != nil {
		o.Log("WebView2: " + err.Error())
		if errors.Is(err, webview2.ErrNoRuntime) || errors.Is(err, webview2.ErrNoLoader) {
			MessageBox(o.NoRuntimeTitle, o.NoRuntimeText)
			return ExitNoRuntime
		}
		MessageBox(o.NoRuntimeTitle, err.Error())
		return ExitFailed
	}
	o.Log("WebView2 Runtime " + ver)
	enterJob(o.Log)
	if err := os.MkdirAll(o.DataDir, 0o700); err != nil {
		return failed("user data folder", err)
	}
	resetLocalState(o.DataDir, o.Log)

	s := &state{o: o}
	st = s
	s.ctx, s.cancel = context.WithCancel(context.Background())
	defer s.cancel()
	s.visible.Store(true)
	bo := o.Bridge
	bo.Visible = s.visible.Load
	bo.Log = o.Log
	painted := bo.Painted
	bo.Painted = func() {
		if o.ReportPaint != nil {
			_, _ = fmt.Fprintf(o.ReportPaint, "HB-GUI painted since_create_ms=%d\n", sinceCreate())
		}
		if painted != nil {
			painted()
		}
	}
	if bo.CheckLayout || bo.Probe {
		bo.Layout = func(s string) {
			if o.ReportPaint != nil {
				_, _ = fmt.Fprintf(o.ReportPaint, "HB-GUI layout %s\n", s)
			}
		}
	}
	bo.Context = s.ctx
	bo.Notify = func(name string, data any) {
		b, err := json.Marshal(struct {
			Event string `json:"event"`
			Data  any    `json:"data"`
		}{name, data})
		if err != nil {
			return
		}
		s.disp.post(func() {
			if s.view != nil && !s.closing {
				_ = s.view.PostJSON(string(b))
			}
		})
	}
	s.d = bridge.New(bo)

	const ground = 0xEEF4FC
	hwnd, err := newWindow(windowClass(o.InstanceKey), o.Title, 1120, 780, ground, windows.NewCallback(wndproc))
	if err != nil {
		return failed("create the window", err)
	}
	s.hwnd = hwnd
	s.disp.hwnd = hwnd
	_, _, _ = procShowWindow.Call(hwnd, swShow)
	_, _, _ = procUpdateWindow.Call(hwnd)
	_, _, _ = procSetForegroundWin.Call(hwnd)

	s.view, err = webview2.New(hwnd, webview2.Config{
		UserDataFolder: o.DataDir,
		BrowserArgs:    BrowserArgs,
		Language:       o.Language,
		Origin:         Origin,
		Start:          Origin + "/",
		Background:     ground,
		MaxMessage:     bridge.MaxMessage,
		Scripts:        scripts(bo.Probe),
		Debug:          o.Log,
		Serve: func(method string, u *url.URL) webview2.Response {
			r := serve(method, u)
			o.Log(fmt.Sprintf("%s %s: %d", method, u.Path, r.Status))
			return r
		},
		OnMessage: s.onMessage,
		OnReady: func(err error) {
			if err == nil {
				o.Log("WebView2 control ready; loading the page")
			}
			if err != nil {
				o.Log("WebView2: " + err.Error())
				MessageBox(o.Title, "The window could not be shown: "+err.Error())
				s.close()
			}
		},
		OnNavigationFailed: func(status int) { o.Log(fmt.Sprintf("navigation failed (%d)", status)) },
		OnProcessFailed: func(kind int) {
			o.Log(fmt.Sprintf("a WebView2 process failed (kind %d)", kind))
			if kind == 0 { // the browser process: the control is gone
				s.close()
			}
		},
	})
	if err != nil {
		o.Log("WebView2: " + err.Error())
		MessageBox(o.Title, "The window could not be shown: "+err.Error())
		_, _, _ = procDestroyWindow.Call(hwnd)
		loop()
		return ExitFailed
	}
	loop()
	s.drain()
	// The window is gone. Give the browser processes a moment to finish, then the job
	// (closed with this process) ends any that are left.
	waitChildren(1500 * time.Millisecond)
	return ExitOK
}

// drain runs after the window is gone, however it went (a close, a failed browser
// process): an import still running is canceled and its Run, with its cleanup, is
// waited for, as are the requests still being answered (their context is canceled), so
// the process does not end under them.
func (s *state) drain() {
	s.d.CancelImport()
	if !s.d.Wait(importStopGrace) {
		s.o.Log(fmt.Sprintf("background work did not end within %v; exiting anyway", importStopGrace))
	}
}

// askClose is WM_CLOSE: during an import it asks whether to stop it. Yes cancels the
// import and keeps the window (and the process) until its Run has returned, at most
// importStopGrace; no keeps the window open. It reports whether to close now.
func (s *state) askClose(hwnd uintptr) bool {
	if s.stopping {
		return false // already waiting for the import
	}
	if !s.d.ImportRunning() {
		return true
	}
	const mbYesNo, mbIconWarning, mbDefButton2, idYes = 0x4, 0x30, 0x100, 6
	if messageBox(hwnd, s.o.Title, s.o.ImportRunningText, mbYesNo|mbIconWarning|mbDefButton2) != idYes {
		return false
	}
	s.stopping = true
	s.o.Log("close requested during an import: stopping it first")
	s.d.CancelImport()
	setWindowText(hwnd, s.o.Title+" - "+s.o.StoppingText)
	s.event("closing")
	go func() {
		ok := s.d.WaitImport(importStopGrace)
		s.disp.post(func() {
			if !ok {
				s.o.Log(fmt.Sprintf("the import did not stop within %v; closing anyway", importStopGrace))
			}
			s.showCloseNote(s.hwnd)
			s.close()
		})
	}()
	return false
}

// showCloseNote shows S0.3's note once, before the window closes.
func (s *state) showCloseNote(hwnd uintptr) {
	if s.o.CloseNote != nil && !s.closing {
		if note := s.o.CloseNote(); note != "" {
			messageBox(hwnd, s.o.Title, note, mbIconInfo)
		}
	}
}

// scripts are what runs in every document before the page's own scripts: the guard
// (no WebRTC, no frames), and with probe the WebRTC probe of bench/footprint.
func scripts(probe bool) []string {
	s := []string{web.Guard}
	if probe {
		s = append(s, web.Probe)
	}
	return s
}

// serve answers the page's requests from memory: GET of a known file, else 404.
func serve(method string, u *url.URL) webview2.Response {
	h := map[string]string{}
	for k, v := range web.Headers {
		h[k] = v
	}
	if method != http.MethodGet || u.RawQuery != "" {
		return webview2.Response{Status: 405, Reason: "Method Not Allowed", Headers: h}
	}
	body, ct, ok := web.Lookup(u.Path)
	if !ok {
		return webview2.Response{Status: 404, Reason: "Not Found", Headers: h}
	}
	h["Content-Type"] = ct
	if c := web.CacheControl(u.Path); c != "" {
		h["Cache-Control"] = c
	}
	return webview2.Response{Status: 200, Reason: "OK", Headers: h, Body: body}
}

// onMessage runs on the window's thread; the request is answered on a goroutine, and
// the answer is posted back to the window's thread.
func (s *state) onMessage(msg string) {
	if s.closing {
		return
	}
	var head struct {
		ID int64  `json:"id"`
		Op string `json:"op"`
	}
	_ = json.Unmarshal([]byte(msg), &head)
	// The operation only: arguments can be what the user typed (daemon.json, a setting).
	s.o.Log(fmt.Sprintf("request %d %.40q (%d bytes)", head.ID, head.Op, len(msg)))
	go func() {
		r := s.d.Handle(s.ctx, msg)
		if r.Error != nil {
			s.o.Log("answer: " + r.Error.Code)
		}
		b, err := json.Marshal(r)
		if err != nil {
			s.o.Log("encode response: " + err.Error())
			return
		}
		s.disp.post(func() {
			if s.view != nil && !s.closing {
				_ = s.view.PostJSON(string(b))
			}
		})
	}()
}

// event sends {"event": name} to the page.
func (s *state) event(name string) {
	if s.view != nil && !s.closing {
		_ = s.view.PostJSON(`{"event":"` + name + `"}`)
	}
}

// close tears the control down and destroys the window: the message loop then ends.
func (s *state) close() {
	if s.closing {
		return
	}
	s.closing = true
	s.cancel()
	s.disp.stop()
	if s.view != nil {
		s.view.Close()
	}
	_, _, _ = procDestroyWindow.Call(s.hwnd)
}

func wndproc(hwnd, m, wp, lp uintptr) uintptr {
	s := st
	if s == nil || s.hwnd == 0 {
		return defWindowProc(hwnd, m, wp, lp)
	}
	switch m {
	case wmSize:
		if s.view != nil {
			if wp == sizeMinimized {
				s.visible.Store(false)
				s.view.SetVisible(false)
				// Minimized: as little memory as the runtime can, and the page suspended
				// unless it waits for an answer or an import's progress.
				s.view.Background(true, !s.d.Busy())
			} else {
				if !s.visible.Load() {
					s.view.Background(false, false) // restored: resume, normal memory
				}
				s.view.SetVisible(true)
				s.view.Resize()
				if !s.visible.Swap(true) {
					s.event("activated")
				}
			}
		}
		return 0
	case wmMove:
		if s.view != nil {
			s.view.NotifyMoved()
		}
	case wmActivate:
		if wp&0xFFFF != waInactive && s.visible.Load() {
			s.event("activated")
		}
	case wmSetFocus:
		if s.view != nil {
			s.view.Focus()
		}
	case wmGetMinMaxInfo:
		mm := (*minMaxInfo)(ptrFrom(lp))
		dpi := dpiOf(hwnd)
		mm.minTrackSize.x, mm.minTrackSize.y = int32(720*dpi/96), int32(520*dpi/96) //nolint:gosec // G115: a DPI is at most a few thousand
		return 0
	case wmDPIChanged:
		r := (*windows.Rect)(ptrFrom(lp))
		_, _, _ = procSetWindowPos.Call(hwnd, 0, uintptr(r.Left), uintptr(r.Top), uintptr(r.Right-r.Left), uintptr(r.Bottom-r.Top), swpNoZOrder|swpNoActivate) //nolint:gosec // G115: SetWindowPos takes ints; the rectangle Windows suggested
		return 0
	case wmApp:
		s.disp.drain()
		return 0
	case wmClose:
		s.o.Log("close requested")
		if s.closing || !s.askClose(hwnd) {
			return 0
		}
		s.showCloseNote(hwnd)
		s.close()
		return 0
	case wmDestroy:
		s.hwnd = 0
		_, _, _ = procPostQuitMessage.Call(0)
		return 0
	}
	return defWindowProc(hwnd, m, wp, lp)
}

var procGetSystemTimePrecise = kernel32.NewProc("GetSystemTimePreciseAsFileTime")

// sinceCreate is the time since this process was created, in ms.
func sinceCreate() int64 {
	var c, e, k, u windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &c, &e, &k, &u); err != nil {
		return -1
	}
	var now windows.Filetime
	_, _, _ = procGetSystemTimePrecise.Call(uintptr(unsafe.Pointer(&now)))
	return (now.Nanoseconds() - c.Nanoseconds()) / 1e6
}

// resetLocalState removes the browser's "Local State" file before each start. With it,
// every launch after the first makes the WebView2 Runtime fetch its experimentation
// configuration from Microsoft (config.edge.skype.com, seen by bench/footprint's socket
// check), which no browser argument turns off; only the
// ExperimentationAndConfigurationServiceControl policy does, and hyperbutler does not
// change policies. Without the file each start is like the first, which makes no
// request. The page keeps nothing there: no cookies, passwords or settings.
func resetLocalState(dataDir string, log func(string)) {
	p := filepath.Join(dataDir, "EBWebView", "Local State")
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		log("reset the browser's Local State: " + err.Error())
	}
}
