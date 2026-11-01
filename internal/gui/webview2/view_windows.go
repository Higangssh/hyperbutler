//go:build windows

package webview2

import (
	"errors"
	"fmt"
	"net/url"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// syscallN calls a native function with args.
//
//go:uintptrescapes
func syscallN(fn uintptr, args ...uintptr) (uintptr, uintptr, error) {
	return syscall.SyscallN(fn, args...)
}

// Interface ids beyond the base interfaces (WebView2.h).
var (
	iidController2 = mustGUID("{C979903E-D4CA-4228-92EB-47EE3FA96EAB}")
	iidWebView2_4  = mustGUID("{20D02D59-6DF2-42DC-BD06-F98A694B1302}")
	iidSettings3   = mustGUID("{FDB5AB74-AF33-4854-84F0-0A631DEB5EBA}")
	iidSettings4   = mustGUID("{CB56846C-4168-4D53-B04F-03B6D6796FF2}")
	iidSettings5   = mustGUID("{183E7052-1D03-43A0-AB99-98E043B66B39}")
	iidSettings6   = mustGUID("{11CB3ACD-9BC8-43B8-83BF-F40753714F87}")
	iidSettings8   = mustGUID("{9E6B0E8F-86AD-4E81-8147-A9B5EDB68650}")
)

// Method indexes in the vtables (IUnknown is 0 to 2).
const (
	// ICoreWebView2Environment
	envCreateController     = 3
	envCreateWebResourceRsp = 4

	// ICoreWebView2Controller
	ctlPutIsVisible       = 4
	ctlPutBounds          = 6
	ctlMoveFocus          = 12
	ctlNotifyParentMoved  = 23
	ctlClose              = 24
	ctlGetCoreWebView2    = 25
	ctl2PutDefaultBgColor = 27

	// ICoreWebView2
	wvGetSettings                = 3
	wvNavigate                   = 5
	wvAddNavigationStarting      = 7
	wvRemoveNavigationStarting   = 8
	wvAddNavigationCompleted     = 15
	wvRemoveNavigationCompleted  = 16
	wvAddFrameNavStarting        = 17
	wvRemoveFrameNavStarting     = 18
	wvAddPermissionRequested     = 23
	wvRemovePermissionRequested  = 24
	wvAddProcessFailed           = 25
	wvRemoveProcessFailed        = 26
	wvPostWebMessageAsJSON       = 32
	wvAddWebMessageReceived      = 34
	wvRemoveWebMessageReceived   = 35
	wvAddNewWindowRequested      = 44
	wvRemoveNewWindowRequested   = 45
	wvAddWebResourceRequested    = 55
	wvRemoveWebResourceRequested = 56
	wvAddWebResourceFilter       = 57
	wv4AddDownloadStarting       = 75
	wv4RemoveDownloadStarting    = 76

	// ICoreWebView2Settings and its successors
	setPutIsScriptEnabled         = 4
	setPutIsWebMessageEnabled     = 6
	setPutDefaultScriptDialogs    = 8
	setPutIsStatusBarEnabled      = 10
	setPutAreDevToolsEnabled      = 12
	setPutDefaultContextMenus     = 14
	setPutAreHostObjectsAllowed   = 16
	setPutIsZoomControlEnabled    = 18
	setPutIsBuiltInErrorPage      = 20
	set3PutBrowserAcceleratorKeys = 24
	set4PutPasswordAutosave       = 26
	set4PutGeneralAutofill        = 28
	set5PutPinchZoom              = 30
	set6PutSwipeNavigation        = 32
	set8PutReputationChecking     = 36

	// Event arguments
	argsNavGetURI       = 3 // ICoreWebView2NavigationStartingEventArgs
	argsNavPutCancel    = 8
	argsNewWinPutHandle = 6 // ICoreWebView2NewWindowRequestedEventArgs
	argsPermPutState    = 7 // ICoreWebView2PermissionRequestedEventArgs
	argsDownloadCancel  = 5 // ICoreWebView2DownloadStartingEventArgs
	argsMsgGetSource    = 3 // ICoreWebView2WebMessageReceivedEventArgs
	argsMsgGetJSON      = 4
	argsResGetRequest   = 3 // ICoreWebView2WebResourceRequestedEventArgs
	argsResPutResponse  = 5
	reqGetURI           = 3 // ICoreWebView2WebResourceRequest
	reqGetMethod        = 5
	argsNavDoneSuccess  = 3 // ICoreWebView2NavigationCompletedEventArgs
	argsNavDoneStatus   = 4
	argsFailedGetKind   = 3 // ICoreWebView2ProcessFailedEventArgs

	permissionDeny        = 2 // COREWEBVIEW2_PERMISSION_STATE_DENY
	resourceContextAll    = 0 // COREWEBVIEW2_WEB_RESOURCE_CONTEXT_ALL
	moveFocusProgrammatic = 0
)

// Response is what Serve returns for one request.
type Response struct {
	Status  int
	Reason  string
	Headers map[string]string
	Body    []byte
}

// Config describes a WebView.
type Config struct {
	// UserDataFolder holds the browser profile; it is created if needed.
	UserDataFolder string
	// BrowserArgs are extra Chromium arguments, such as "--disable-gpu".
	BrowserArgs string
	// Language is the browser UI language (for the few strings Chromium draws).
	Language string
	// Origin is the only origin the page may have, such as "https://hb.localhost".
	Origin string
	// Start is the first URL, under Origin.
	Start string
	// Background is the color shown before the first paint, 0xRRGGBB.
	Background uint32
	// Serve answers every request the page makes (method and URL); a request for
	// another origin is answered 403 without calling it.
	Serve func(method string, u *url.URL) Response
	// OnMessage is called on the UI thread with each web message's JSON, sent by a
	// document of Origin; messages from anywhere else are dropped.
	OnMessage func(json string)
	// OnReady is called once on the UI thread: err is nil when the page is navigating.
	OnReady func(err error)
	// OnNavigationFailed is called when a navigation of the page fails.
	OnNavigationFailed func(status int)
	// OnProcessFailed is called when a browser process fails (its kind, as WebView2
	// numbers them; 0 is the browser process itself).
	OnProcessFailed func(kind int)
	// Debug, if set, receives a line for each request or message that is refused.
	Debug func(string)
	// MaxMessage bounds a web message in UTF-16 units; longer ones are dropped.
	MaxMessage int
}

// View is a WebView2 control in a window. Every method runs on the window's thread.
type View struct {
	cfg    Config
	hwnd   uintptr
	origin *url.URL

	pin      runtime.Pinner
	handlers []*handler
	opts     *envOptions

	env, ctl, wv, settings obj
	tokens                 []token
	closed                 bool
	visible                bool
	err                    error
}

type token struct {
	remove int
	on     obj
	value  int64
	owned  bool // on is a reference of its own (a QueryInterface'd successor)
}

// New starts creating a WebView2 control filling hwnd. It returns at once; the
// control is ready when cfg.OnReady is called. The caller's thread must have COM
// initialized as a single-threaded apartment and run a message loop.
func New(hwnd uintptr, cfg Config) (*View, error) {
	if err := loadLoader(); err != nil {
		return nil, err
	}
	o, err := url.Parse(cfg.Origin)
	if err != nil || o.Scheme != "https" || o.Host == "" || o.Path != "" {
		return nil, errors.New("webview2: Origin must be https://host")
	}
	if cfg.MaxMessage <= 0 {
		cfg.MaxMessage = 1 << 20
	}
	initVtables()
	v := &View{cfg: cfg, hwnd: hwnd, origin: o, visible: true}
	v.opts = &envOptions{vtbl: &optionsVtbl, refs: 1, args: cfg.BrowserArgs, language: cfg.Language}
	v.pin.Pin(v.opts)
	regMu.Lock()
	options[uintptr(unsafe.Pointer(v.opts))] = v.opts
	regMu.Unlock()

	envDone := v.newHandler(func(hr, env uintptr) uintptr {
		v.environmentCreated(hr, obj(env))
		return sOK
	})
	data, err := windows.UTF16PtrFromString(cfg.UserDataFolder)
	if err != nil {
		v.Close()
		return nil, err
	}
	r, _, _ := syscallN(procCreate, 0, uintptr(unsafe.Pointer(data)),
		uintptr(unsafe.Pointer(v.opts)), uintptr(unsafe.Pointer(envDone)))
	if err := check("CreateCoreWebView2EnvironmentWithOptions", r); err != nil {
		v.Close()
		return nil, err
	}
	return v, nil
}

// newHandler makes a pinned handler object that calls f.
func (v *View) newHandler(f func(a, b uintptr) uintptr) *handler {
	h := &handler{vtbl: &handlerVtbl, refs: 1, invoke: f}
	v.pin.Pin(h)
	regMu.Lock()
	handlers[uintptr(unsafe.Pointer(h))] = h
	regMu.Unlock()
	v.handlers = append(v.handlers, h)
	return h
}

func (v *View) fail(err error) {
	if v.err == nil {
		v.err = err
		if v.cfg.OnReady != nil {
			v.cfg.OnReady(err)
		}
	}
}

func (v *View) environmentCreated(hr uintptr, env obj) {
	if v.closed {
		return
	}
	if err := check("create the WebView2 environment", hr); err != nil {
		v.fail(err)
		return
	}
	if env == 0 {
		v.fail(errors.New("create the WebView2 environment: no environment"))
		return
	}
	env.call(1) // AddRef: the argument is only borrowed
	v.env = env
	done := v.newHandler(func(hr, ctl uintptr) uintptr {
		v.controllerCreated(hr, obj(ctl))
		return sOK
	})
	if err := check("CreateCoreWebView2Controller", env.call(envCreateController, v.hwnd, uintptr(unsafe.Pointer(done)))); err != nil {
		v.fail(err)
	}
}

func (v *View) controllerCreated(hr uintptr, ctl obj) {
	if err := check("create the WebView2 controller", hr); err != nil {
		v.fail(err)
		return
	}
	if ctl == 0 {
		v.fail(errors.New("create the WebView2 controller: no controller"))
		return
	}
	ctl.call(1) // AddRef
	if v.closed {
		ctl.call(ctlClose)
		ctl.release()
		return
	}
	v.ctl = ctl
	if c2 := ctl.query(iidController2); c2 != 0 {
		b := v.cfg.Background
		// COREWEBVIEW2_COLOR {A, R, G, B} by value.
		c2.call(ctl2PutDefaultBgColor, uintptr(0xFF)|uintptr(b>>16&0xFF)<<8|uintptr(b>>8&0xFF)<<16|uintptr(b&0xFF)<<24)
		c2.release()
	}
	var wv uintptr
	if err := check("get_CoreWebView2", ctl.call(ctlGetCoreWebView2, uintptr(unsafe.Pointer(&wv)))); err != nil || wv == 0 {
		v.fail(errors.Join(errors.New("get the WebView2 control"), err))
		return
	}
	v.wv = obj(wv)
	if err := v.configure(); err != nil {
		v.fail(err)
		return
	}
	v.Resize()
	start, err := windows.UTF16PtrFromString(v.cfg.Start)
	if err != nil {
		v.fail(err)
		return
	}
	if err := check("Navigate", v.wv.call(wvNavigate, uintptr(unsafe.Pointer(start)))); err != nil {
		v.fail(err)
		return
	}
	if v.cfg.OnReady != nil {
		v.cfg.OnReady(nil)
	}
}

// configure applies the settings and registers the event handlers.
func (v *View) configure() error {
	var s uintptr
	if err := check("get_Settings", v.wv.call(wvGetSettings, uintptr(unsafe.Pointer(&s)))); err != nil {
		return err
	}
	v.settings = obj(s)
	set := func(o obj, i int, on bool) error {
		b := uintptr(0)
		if on {
			b = 1
		}
		return check("settings", o.call(i, b))
	}
	for _, c := range []struct {
		i  int
		on bool
	}{
		{setPutIsScriptEnabled, true},
		{setPutIsWebMessageEnabled, true},
		{setPutDefaultScriptDialogs, false},
		{setPutIsStatusBarEnabled, false},
		{setPutAreDevToolsEnabled, false},
		{setPutDefaultContextMenus, false},
		{setPutAreHostObjectsAllowed, false},
		{setPutIsZoomControlEnabled, false},
		{setPutIsBuiltInErrorPage, false},
	} {
		if err := set(v.settings, c.i, c.on); err != nil {
			return err
		}
	}
	// Newer settings, where the runtime has them: no browser shortcuts (find, print,
	// reload, DevTools, zoom, back), no autofill or saved passwords, no pinch zoom or
	// swipe navigation, and no SmartScreen check, which would send each URL the page
	// loads to Microsoft (REQ-UX-005; the page only loads its own origin).
	for _, c := range []struct {
		iid *windows.GUID
		i   int
	}{
		{iidSettings3, set3PutBrowserAcceleratorKeys},
		{iidSettings4, set4PutPasswordAutosave},
		{iidSettings4, set4PutGeneralAutofill},
		{iidSettings5, set5PutPinchZoom},
		{iidSettings6, set6PutSwipeNavigation},
		{iidSettings8, set8PutReputationChecking},
	} {
		if o := v.settings.query(c.iid); o != 0 {
			if err := set(o, c.i, false); err != nil {
				v.debug("setting %d: %v", c.i, err)
			}
			o.release()
		} else {
			v.debug("the runtime has no settings interface %v (setting %d)", c.iid, c.i)
		}
	}

	add := func(on obj, addIdx, removeIdx int, f func(sender, args uintptr) uintptr) error {
		return v.addHandler(on, addIdx, removeIdx, false, f)
	}
	// Every request, to any URL, is answered here: nothing reaches the network.
	all, _ := windows.UTF16PtrFromString("*")
	if err := check("AddWebResourceRequestedFilter", v.wv.call(wvAddWebResourceFilter, uintptr(unsafe.Pointer(all)), resourceContextAll)); err != nil {
		return err
	}
	if err := add(v.wv, wvAddWebResourceRequested, wvRemoveWebResourceRequested, v.onResource); err != nil {
		return err
	}
	if err := add(v.wv, wvAddWebMessageReceived, wvRemoveWebMessageReceived, v.onMessage); err != nil {
		return err
	}
	if err := add(v.wv, wvAddNavigationStarting, wvRemoveNavigationStarting, v.onNavigation); err != nil {
		return err
	}
	if err := add(v.wv, wvAddFrameNavStarting, wvRemoveFrameNavStarting, v.onNavigation); err != nil {
		return err
	}
	if err := add(v.wv, wvAddNewWindowRequested, wvRemoveNewWindowRequested, func(_, args uintptr) uintptr {
		// Handled without a new window: window.open and target=_blank open nothing.
		obj(args).call(argsNewWinPutHandle, 1)
		return sOK
	}); err != nil {
		return err
	}
	if err := add(v.wv, wvAddPermissionRequested, wvRemovePermissionRequested, func(_, args uintptr) uintptr {
		obj(args).call(argsPermPutState, permissionDeny)
		return sOK
	}); err != nil {
		return err
	}
	if err := add(v.wv, wvAddNavigationCompleted, wvRemoveNavigationCompleted, func(_, args uintptr) uintptr {
		var ok int32
		obj(args).call(argsNavDoneSuccess, uintptr(unsafe.Pointer(&ok)))
		if ok == 0 && v.cfg.OnNavigationFailed != nil {
			var status int32
			obj(args).call(argsNavDoneStatus, uintptr(unsafe.Pointer(&status)))
			v.cfg.OnNavigationFailed(int(status))
		}
		return sOK
	}); err != nil {
		return err
	}
	if err := add(v.wv, wvAddProcessFailed, wvRemoveProcessFailed, func(_, args uintptr) uintptr {
		var kind int32
		obj(args).call(argsFailedGetKind, uintptr(unsafe.Pointer(&kind)))
		if v.cfg.OnProcessFailed != nil {
			v.cfg.OnProcessFailed(int(kind))
		}
		return sOK
	}); err != nil {
		return err
	}
	if wv4 := v.wv.query(iidWebView2_4); wv4 != 0 {
		// The token keeps this reference until Close removes the handler.
		if err := v.addHandler(wv4, wv4AddDownloadStarting, wv4RemoveDownloadStarting, true, func(_, args uintptr) uintptr {
			obj(args).call(argsDownloadCancel, 1)
			return sOK
		}); err != nil {
			wv4.release()
			return err
		}
	}
	return nil
}

// addHandler registers f for the event that method addIdx of on adds; Close removes it
// with removeIdx (and releases on when owned).
func (v *View) addHandler(on obj, addIdx, removeIdx int, owned bool, f func(sender, args uintptr) uintptr) error {
	h := v.newHandler(f)
	var t int64
	if err := check("add event handler", on.call(addIdx, uintptr(unsafe.Pointer(h)), uintptr(unsafe.Pointer(&t)))); err != nil {
		return err
	}
	v.tokens = append(v.tokens, token{remove: removeIdx, on: on, value: t, owned: owned})
	return nil
}

// sameOrigin reports whether raw is a URL of the page's origin.
func (v *View) sameOrigin(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, false
	}
	return u, strings.EqualFold(u.Scheme, v.origin.Scheme) && strings.EqualFold(u.Host, v.origin.Host) && u.User == nil
}

// onNavigation cancels every navigation of the page or a frame away from the origin.
func (v *View) onNavigation(_, args uintptr) uintptr {
	a := obj(args)
	uri, err := a.getString(argsNavGetURI, "navigation URI", maxString)
	if _, ok := v.sameOrigin(uri); err != nil || !ok {
		v.debug("refused navigation to %.200q", uri)
		a.call(argsNavPutCancel, 1)
	}
	return sOK
}

func (v *View) onMessage(_, args uintptr) uintptr {
	a := obj(args)
	src, err := a.getString(argsMsgGetSource, "message source", maxString)
	if _, ok := v.sameOrigin(src); err != nil || !ok {
		v.debug("dropped a web message from %q (%v)", src, err)
		return sOK
	}
	msg, err := a.getString(argsMsgGetJSON, "web message", v.cfg.MaxMessage)
	if err != nil || v.cfg.OnMessage == nil {
		v.debug("dropped a web message: %v", err)
		return sOK
	}
	v.cfg.OnMessage(msg)
	return sOK
}

func (v *View) onResource(_, args uintptr) uintptr {
	a := obj(args)
	var req uintptr
	if failed(a.call(argsResGetRequest, uintptr(unsafe.Pointer(&req)))) || req == 0 {
		return sOK
	}
	r := obj(req)
	defer r.release()
	uri, err1 := r.getString(reqGetURI, "request URI", maxString)
	method, err2 := r.getString(reqGetMethod, "request method", 32)
	resp := Response{Status: 403, Reason: "Forbidden"}
	if u, ok := v.sameOrigin(uri); err1 == nil && err2 == nil && ok && v.cfg.Serve != nil {
		resp = v.cfg.Serve(method, u)
	}
	if rsp, err := v.createResponse(resp); err == nil {
		a.call(argsResPutResponse, uintptr(rsp))
		rsp.release() // put_Response took its own reference
	}
	return sOK
}

// createResponse makes an ICoreWebView2WebResourceResponse; the caller releases it.
func (v *View) createResponse(r Response) (obj, error) {
	var stream uintptr
	if len(r.Body) > 0 {
		s, _, _ := procSHCreateMemStrm.Call(uintptr(unsafe.Pointer(&r.Body[0])), uintptr(len(r.Body)))
		if s == 0 {
			return 0, errors.New("SHCreateMemStream failed")
		}
		stream = s
		defer obj(stream).release() // the response holds its own reference
	}
	var hb strings.Builder
	for k, val := range r.Headers {
		hb.WriteString(k + ": " + val + "\r\n")
	}
	reason, err := windows.UTF16PtrFromString(r.Reason)
	if err != nil {
		return 0, err
	}
	headers, err := windows.UTF16PtrFromString(strings.TrimSuffix(hb.String(), "\r\n"))
	if err != nil {
		return 0, err
	}
	var out uintptr
	if err := check("CreateWebResourceResponse", v.env.call(envCreateWebResourceRsp, stream, uintptr(r.Status),
		uintptr(unsafe.Pointer(reason)), uintptr(unsafe.Pointer(headers)), uintptr(unsafe.Pointer(&out)))); err != nil {
		return 0, err
	}
	return obj(out), nil
}

// PostJSON sends a message to the page (window.chrome.webview's message event).
func (v *View) PostJSON(s string) error {
	if v.wv == 0 || v.closed {
		return errors.New("webview2: not ready")
	}
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		return err
	}
	return check("PostWebMessageAsJson", v.wv.call(wvPostWebMessageAsJSON, uintptr(unsafe.Pointer(p))))
}

// Resize makes the control fill the window's client area.
func (v *View) Resize() {
	if v.ctl == 0 {
		return
	}
	var rc windows.Rect
	_ = getClientRect(v.hwnd, &rc)
	putBounds(v.ctl, &rc)
}

// SetVisible shows or hides the control. Hidden, the page's document.hidden is true and
// its timers are throttled: the window is minimized.
func (v *View) SetVisible(on bool) {
	if v.ctl == 0 || v.visible == on {
		return
	}
	v.visible = on
	b := uintptr(0)
	if on {
		b = 1
	}
	v.ctl.call(ctlPutIsVisible, b)
}

// Focus moves the keyboard focus into the page.
func (v *View) Focus() {
	if v.ctl != 0 {
		v.ctl.call(ctlMoveFocus, moveFocusProgrammatic)
	}
}

// NotifyMoved tells the control that the window moved (for popups and IME).
func (v *View) NotifyMoved() {
	if v.ctl != 0 {
		v.ctl.call(ctlNotifyParentMoved)
	}
}

// Close removes the event handlers, closes the controller (which tells the browser
// processes to exit) and releases every interface. It is safe to call more than once,
// and before the control is ready.
func (v *View) Close() {
	if v.closed {
		return
	}
	v.closed = true
	for i := len(v.tokens) - 1; i >= 0; i-- {
		t := v.tokens[i]
		t.on.call(t.remove, uintptr(t.value)) //nolint:gosec // G115: the 64-bit EventRegistrationToken passed by value
		if t.owned {
			t.on.release() // a QueryInterface'd successor (ICoreWebView2_4)
		}
	}
	v.tokens = nil
	if v.ctl != 0 {
		v.ctl.call(ctlClose)
	}
	v.settings.release()
	v.wv.release()
	v.ctl.release()
	v.env.release()
	v.settings, v.wv, v.ctl, v.env = 0, 0, 0, 0
	regMu.Lock()
	for _, h := range v.handlers {
		delete(handlers, uintptr(unsafe.Pointer(h)))
	}
	delete(options, uintptr(unsafe.Pointer(v.opts)))
	// WebView2 may still release a handler after this; the objects stay reachable (but
	// unregistered, so their callbacks do nothing) for the life of the process, which
	// is what follows a Close, so native code never reads freed memory.
	retired = append(retired, v.handlers...)
	retiredOpts = append(retiredOpts, v.opts)
	regMu.Unlock()
	v.handlers = nil
	v.pin.Unpin()
}

var (
	retired     []*handler
	retiredOpts []*envOptions
)

func (v *View) debug(format string, a ...any) {
	if v.cfg.Debug != nil {
		v.cfg.Debug(fmt.Sprintf(format, a...))
	}
}
