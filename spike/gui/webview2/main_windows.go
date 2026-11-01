//go:build windows

// Command webview2 is the WebView2 half of the GUI toolkit spike (ADR 0008 "GUI
// shape"): the home screen of visual direction "B" as HTML in a WebView2 control,
// hosted in a plain Win32 window from pure Go (no cgo). WebView2 is driven through
// github.com/jchv/go-webview2/pkg/edge, which calls the COM interfaces through syscall.
//
// The page and the mascot are served from memory through WebResourceRequested on
// https://hb.localhost/, so the GUI opens no network port. It prints
// "HB-SPIKE painted ..." on stdout once the home screen is on screen with its content,
// for spike/gui/measure.
//
//	go build -ldflags "-H windowsgui" -o webview2.exe ./spike/gui/webview2
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"github.com/jchv/go-webview2/pkg/edge"
	"github.com/jchv/go-webview2/webviewloader"

	"github.com/Higangssh/hyperbutler/spike/gui/internal/spikeui"
)

//go:embed index.html
var indexHTML []byte

const origin = "https://hb.localhost/"

var (
	chromium *edge.Chromium
	disp     spikeui.Dispatcher
	msgs     map[string]string
	view     spikeui.View
	hbExe    string
	hbErr    error
	realRun  *bool
	debugJob *bool
	mascot   []byte
	ready    bool // the page has loaded; Eval before that is lost
)

func main() {
	runtime.LockOSThread()
	hbFlag := flag.String("hb", "", "hyperbutler.exe to run for status and start (default: next to this program, then PATH)")
	data := flag.String("data", "", "WebView2 user data folder (default: %TEMP%\\hb-dev\\gui-spike\\webview2-data)")
	lang := flag.String("lang", "", "UI language, for example en or ko (default: the Windows user locale)")
	mascotPath := flag.String("mascot", "", "mascot PNG (default: assets/brand/mascot.png above the working directory)")
	debug := flag.Bool("debug", false, "enable the context menu and DevTools")
	gpu := flag.Bool("gpu", false, "let WebView2 use the GPU (by default it composites in software, which halves its memory)")
	noJob := flag.Bool("no-job", false, "do not put the WebView2 processes in a kill-on-close job")
	debugJob = flag.Bool("debug-job", false, "print whether each child process is in the job")
	realRun = flag.Bool("real-start", false, "make Start run `hyperbutler start` instead of a stub")
	flag.Parse()

	if *lang == "" {
		*lang = spikeui.UserLanguage()
	}
	msgs = spikeui.Messages(*lang)
	view = spikeui.Initial(msgs)
	hbExe, hbErr = spikeui.FindCLI(*hbFlag)

	// WebView2 needs the Evergreen runtime (part of Windows 11). Without it there is
	// nothing to fall back to: say so and stop, rather than installing anything.
	if v, err := webviewloader.GetInstalledVersion(); err != nil || v == "" {
		spikeui.MessageBox(0, msgs[spikeui.KeyTitle], fmt.Sprintf(
			"The Microsoft Edge WebView2 Runtime is not installed (%v).\n\nIt is part of Windows 11; on other systems get the Evergreen runtime from https://go.microsoft.com/fwlink/p/?LinkId=2124703.", err))
		os.Exit(3)
	}

	p := *mascotPath
	if p == "" {
		p, _ = spikeui.FindAsset("assets/brand/mascot.png")
	}
	if p != "" {
		mascot, _ = os.ReadFile(p) //nolint:gosec // G304: a path the user passed, read-only
	}
	if *data == "" {
		*data = filepath.Join(os.TempDir(), "hb-dev", "gui-spike", "webview2-data")
	}

	spikeui.SetDPIAware()
	ground := spikeui.RGB(0xe6, 0xf1, 0xfd)
	hwnd := spikeui.NewWindow("hbSpikeWebView2", msgs[spikeui.KeyTitle], 960, 620, ground, wndproc)
	disp.Hwnd = hwnd
	spikeui.Show(hwnd)

	if !*noJob {
		enterJob()
	}
	if !*gpu {
		// The loader reads this from the process environment; nothing outside this
		// process changes. go-webview2 passes no environment options of its own.
		_ = os.Setenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS", "--disable-gpu")
	}
	chromium = edge.NewChromium()
	chromium.DataPath = *data
	chromium.MessageCallback = onMessage
	chromium.WebResourceRequestedCallback = onResource
	if !chromium.Embed(hwnd) {
		spikeui.MessageBox(hwnd, msgs[spikeui.KeyTitle], "Could not create the WebView2 control.")
		os.Exit(1)
	}
	if c2 := chromium.GetController().GetICoreWebView2Controller2(); c2 != nil {
		_ = c2.PutDefaultBackgroundColor(edge.COREWEBVIEW2_COLOR{A: 255, R: 0xe6, G: 0xf1, B: 0xfd})
	}
	if s, err := chromium.GetSettings(); err == nil {
		_ = s.PutAreDefaultContextMenusEnabled(*debug)
		_ = s.PutAreDevToolsEnabled(*debug)
		_ = s.PutIsStatusBarEnabled(false)
		_ = s.PutAreHostObjectsAllowed(false)
	}
	chromium.AddWebResourceRequestedFilter(origin+"*", edge.COREWEBVIEW2_WEB_RESOURCE_CONTEXT_ALL)
	chromium.Resize()
	chromium.Init(fmt.Sprintf("window.HB_MSG=%s;window.HB_VIEW=%s;window.HB_LANG=%s;",
		js(msgs), js(view), js(*lang)))
	chromium.Navigate(origin + "index.html")

	go refresh()
	spikeui.Loop(hwnd, false)
	waitChildren(1500 * time.Millisecond)
}

func js(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
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
	if !ready {
		return // the page renders view when it reports ready
	}
	chromium.Eval("hb.render(" + js(v) + ")")
}

func onMessage(m string) {
	switch m {
	case "painted":
		spikeui.ReportFirstPaint("webview2")
		ready = true
		if *debugJob {
			reportJob()
		}
		render(view)
	case "start":
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
}

func onResource(req *edge.ICoreWebView2WebResourceRequest, args *edge.ICoreWebView2WebResourceRequestedEventArgs) {
	uri, err := req.GetUri()
	if err != nil {
		return
	}
	u, err := url.Parse(uri)
	if err != nil {
		return
	}
	var body []byte
	ctype := ""
	switch u.Path {
	case "/index.html":
		body, ctype = indexHTML, "text/html; charset=utf-8"
	case "/mascot.png":
		body, ctype = mascot, "image/png"
	}
	env := chromium.Environment()
	status, reason, headers := 200, "OK", "Content-Type: "+ctype
	if body == nil {
		status, reason, headers = 404, "Not Found", ""
	}
	resp, err := env.CreateWebResourceResponse(body, status, reason, headers)
	if err != nil {
		return
	}
	_ = args.PutResponse(resp)
}

// closeController calls ICoreWebView2Controller::Close, which go-webview2 does not
// wrap, so the browser processes are told to go before this process exits instead of
// noticing that their host is gone.
func closeController() {
	if chromium == nil || chromium.GetController() == nil {
		return
	}
	const closeIndex = 24 // IUnknown (3) + 21 methods before Close in ICoreWebView2ControllerVtbl
	c := chromium.GetController()
	vtbl := **(**[closeIndex + 1]uintptr)(unsafe.Pointer(c))
	_, _, _ = syscall.SyscallN(vtbl[closeIndex], uintptr(unsafe.Pointer(c)))
}

func wndproc(hwnd, msg, wp, lp uintptr) uintptr {
	switch msg {
	case spikeui.WM_SIZE:
		if chromium != nil {
			chromium.Resize()
		}
		return 0
	case spikeui.WM_MOVE:
		if chromium != nil {
			_ = chromium.NotifyParentWindowPositionChanged()
		}
	case spikeui.WM_SETFOCUS:
		if chromium != nil {
			chromium.Focus()
		}
	case spikeui.WM_DPICHANGED:
		spikeui.ApplyDPIChange(hwnd, lp)
		return 0
	case spikeui.WM_APP:
		disp.Drain()
		return 0
	case spikeui.WM_CLOSE:
		closeController()
	case spikeui.WM_DESTROY:
		spikeui.Quit()
		return 0
	}
	return spikeui.DefWindowProc(hwnd, msg, wp, lp)
}
