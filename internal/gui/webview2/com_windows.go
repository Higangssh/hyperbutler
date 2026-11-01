//go:build windows

package webview2

import (
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// obj is a COM interface pointer that native code owns. It is kept as a uintptr: the
// memory is not Go's, so the garbage collector must not look at it.
type obj uintptr

const ptrSize = unsafe.Sizeof(uintptr(0))

// ptrFrom turns an address native code gave (as a uintptr) into a pointer without a
// uintptr-to-Pointer conversion, which vet rightly flags for Go memory; these
// addresses are never Go memory.
func ptrFrom(u uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&u)) }

// call calls method i of the interface (0 to 2 are IUnknown's) with o as this.
//
//go:uintptrescapes
func (o obj) call(i int, args ...uintptr) uintptr {
	vt := *(*unsafe.Pointer)(ptrFrom(uintptr(o)))
	fn := *(*uintptr)(unsafe.Add(vt, uintptr(i)*ptrSize))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(o)}, args...)...)
	return r
}

// release calls IUnknown::Release on o, if it is set.
func (o obj) release() {
	if o != 0 {
		o.call(2)
	}
}

// query calls IUnknown::QueryInterface; 0 when o does not have iid.
func (o obj) query(iid *windows.GUID) obj {
	var out uintptr
	if hr := o.call(0, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out))); failed(hr) {
		return 0
	}
	return obj(out)
}

// HRESULTs.
const (
	sOK          = 0
	eNoInterface = 0x80004002
	eNotImpl     = 0x80004001
	ePointer     = 0x80004003
)

// hr32 is an HRESULT: native code returns it in a register, of which the low 32 bits count.
func hr32(hr uintptr) uint32 { return uint32(hr) } //nolint:gosec // G115: an HRESULT is the low 32 bits

func failed(hr uintptr) bool { return int32(hr32(hr)) < 0 } //nolint:gosec // G115: HRESULTs are signed

// HRESULTError is a failed COM call.
type HRESULTError struct {
	Op string
	HR uint32
}

func (e *HRESULTError) Error() string { return fmt.Sprintf("%s: HRESULT 0x%08X", e.Op, e.HR) }

func check(op string, hr uintptr) error {
	if failed(hr) {
		return &HRESULTError{Op: op, HR: hr32(hr)}
	}
	return nil
}

func mustGUID(s string) *windows.GUID {
	g, err := windows.GUIDFromString(s)
	if err != nil {
		panic("webview2: bad GUID literal " + s) // a constant in this package
	}
	return &g
}

var iidIUnknown = mustGUID("{00000000-0000-0000-C000-000000000046}")

var (
	ole32               = windows.NewLazySystemDLL("ole32.dll")
	shlwapi             = windows.NewLazySystemDLL("shlwapi.dll")
	procCoTaskMemAlloc  = ole32.NewProc("CoTaskMemAlloc")
	procSHCreateMemStrm = shlwapi.NewProc("SHCreateMemStream")
)

// coTaskString returns s as a NUL-terminated UTF-16 string in CoTaskMemAlloc memory,
// which the caller (WebView2) frees.
func coTaskString(s string) uintptr {
	u, err := windows.UTF16FromString(s)
	if err != nil {
		u = []uint16{0}
	}
	p, _, _ := procCoTaskMemAlloc.Call(uintptr(len(u)) * 2)
	if p == 0 {
		return 0
	}
	copy(unsafe.Slice((*uint16)(ptrFrom(p)), len(u)), u)
	return p
}

// maxString bounds every string read from WebView2 (URIs, messages): longer ones are
// refused rather than copied.
const maxString = 4 << 20

// takeString reads and frees a CoTaskMemAlloc'd UTF-16 string WebView2 returned. ok is
// false for a nil pointer or one longer than limit UTF-16 units (it is still freed).
func takeString(p uintptr, limit int) (s string, ok bool) {
	if p == 0 {
		return "", false
	}
	defer windows.CoTaskMemFree(ptrFrom(p))
	n := 0
	for ; n <= limit; n++ {
		if *(*uint16)(unsafe.Add(ptrFrom(p), uintptr(n)*2)) == 0 {
			break
		}
	}
	if n > limit {
		return "", false
	}
	return windows.UTF16ToString(unsafe.Slice((*uint16)(ptrFrom(p)), n)), true
}

// getString calls a getter (this, LPWSTR*) and returns the string.
func (o obj) getString(i int, op string, limit int) (string, error) {
	var p uintptr
	if err := check(op, o.call(i, uintptr(unsafe.Pointer(&p)))); err != nil {
		return "", err
	}
	s, ok := takeString(p, limit)
	if !ok {
		return "", fmt.Errorf("%s: missing or longer than %d characters", op, limit)
	}
	return s, nil
}

// --- COM objects implemented in Go ------------------------------------------------------

// handler is a COM object of ours: a WebView2 completion or event handler, whose
// interface is IUnknown plus Invoke(this, a, b). Native code holds a pointer to it, so
// it is pinned while registered. Only its first word (the vtable) is read natively.
type handler struct {
	vtbl   *[4]uintptr
	refs   int32
	invoke func(a, b uintptr) uintptr
}

// handlerVtbl is every handler's vtable: all the handler interfaces this package uses
// have the same shape, so one set of callbacks serves them (windows.NewCallback has a
// fixed budget of callbacks per process, so they are made once).
var (
	handlerVtbl  [4]uintptr
	optionsVtbl  [11]uintptr
	options3Vtbl [5]uintptr
	vtblOnce     sync.Once

	// registry maps the address of each live object of ours to it, so the callbacks
	// find it without turning a uintptr into a pointer (options3: by the address of its
	// Options3 interface).
	regMu    sync.Mutex
	handlers = map[uintptr]*handler{}
	options  = map[uintptr]*envOptions{}
	options3 = map[uintptr]*envOptions{}
)

func initVtables() {
	vtblOnce.Do(func() {
		handlerVtbl = [4]uintptr{
			windows.NewCallback(handlerQueryInterface),
			windows.NewCallback(handlerAddRef),
			windows.NewCallback(handlerRelease),
			windows.NewCallback(handlerInvoke),
		}
		optionsVtbl = [11]uintptr{
			windows.NewCallback(optionsQueryInterface),
			windows.NewCallback(optionsAddRef),
			windows.NewCallback(optionsRelease),
			windows.NewCallback(optionsGetArgs),
			windows.NewCallback(optionsPutString),
			windows.NewCallback(optionsGetLanguage),
			windows.NewCallback(optionsPutString),
			windows.NewCallback(optionsGetTargetVersion),
			windows.NewCallback(optionsPutString),
			windows.NewCallback(optionsGetSSO),
			windows.NewCallback(optionsPutBool),
		}
		options3Vtbl = [5]uintptr{
			windows.NewCallback(options3QueryInterface),
			windows.NewCallback(optionsAddRef),
			windows.NewCallback(optionsRelease),
			windows.NewCallback(options3GetCrashReporting),
			windows.NewCallback(optionsPutBool),
		}
	})
}

func lookupHandler(this uintptr) *handler {
	regMu.Lock()
	defer regMu.Unlock()
	return handlers[this]
}

// writeOut stores v at the native out pointer out, if it is set.
func writeOut(out, v uintptr) {
	if out != 0 {
		*(*uintptr)(ptrFrom(out)) = v
	}
}

// refCount is a reference count as IUnknown returns it.
func refCount(n int32) uintptr { return uintptr(max(n, 0)) }

func handlerQueryInterface(this, iid, out uintptr) uintptr {
	h := lookupHandler(this)
	if h == nil || out == 0 {
		return ePointer
	}
	// Only IUnknown is answered: the caller already has the typed pointer, and claiming
	// any other interface (IMarshal, IAgileObject) would be a lie it might act on.
	if iid != 0 && *(*windows.GUID)(ptrFrom(iid)) == *iidIUnknown {
		atomic.AddInt32(&h.refs, 1)
		writeOut(out, this)
		return sOK
	}
	writeOut(out, 0)
	return eNoInterface
}

func handlerAddRef(this uintptr) uintptr {
	if h := lookupHandler(this); h != nil {
		return refCount(atomic.AddInt32(&h.refs, 1))
	}
	return 1
}

// handlerRelease counts down; the memory is Go's, kept reachable after Close.
func handlerRelease(this uintptr) uintptr {
	if h := lookupHandler(this); h != nil {
		return refCount(atomic.AddInt32(&h.refs, -1))
	}
	return 0
}

func handlerInvoke(this, a, b uintptr) uintptr {
	h := lookupHandler(this)
	if h == nil || h.invoke == nil {
		return sOK
	}
	return h.invoke(a, b)
}

// envOptions is our ICoreWebView2EnvironmentOptions. It also answers for
// ICoreWebView2EnvironmentOptions3 with opt3, a second interface of the same object.
type envOptions struct {
	vtbl     *[11]uintptr
	refs     int32
	args     string
	language string
	opt3     envOptions3
	// crashAsked records that the loader read IsCustomCrashReportingEnabled.
	crashAsked atomic.Bool
}

// envOptions3 is the object's ICoreWebView2EnvironmentOptions3: IUnknown (the object's,
// so QueryInterface and the reference count are the same) and
// IsCustomCrashReportingEnabled, which is true: crash dumps of the runtime's processes
// stay in the user data folder and are not sent to Microsoft (REQ-UX-005).
type envOptions3 struct {
	vtbl  *[5]uintptr
	outer *envOptions
}

// targetVersion is the oldest runtime whose interfaces this package uses (the
// DownloadStarting event of ICoreWebView2_4 came with 1.0.902).
const targetVersion = "92.0.902.49"

var (
	iidEnvOptions  = mustGUID("{2FDE08A8-1E9A-4766-8C05-95A9CEB9D1C5}")
	iidEnvOptions3 = mustGUID("{4A5C436E-A9E3-4A2E-89C3-910D3513F5CC}")
)

// lookupOptions finds the options object of this, which is the object's first
// interface or its Options3 interface.
func lookupOptions(this uintptr) *envOptions {
	regMu.Lock()
	defer regMu.Unlock()
	if o := options[this]; o != nil {
		return o
	}
	return options3[this]
}

// options3Addr is the address native code gets for the object's Options3 interface.
func (o *envOptions) options3Addr() uintptr { return uintptr(unsafe.Pointer(&o.opt3)) }

func options3QueryInterface(this, iid, out uintptr) uintptr {
	o := lookupOptions(this)
	if o == nil {
		return ePointer
	}
	return optionsQueryInterface(uintptr(unsafe.Pointer(o)), iid, out)
}

func options3GetCrashReporting(this, out uintptr) uintptr {
	o := lookupOptions(this)
	if o == nil || out == 0 {
		return ePointer
	}
	o.crashAsked.Store(true)
	*(*int32)(ptrFrom(out)) = 1
	return sOK
}

func optionsQueryInterface(this, iid, out uintptr) uintptr {
	o := lookupOptions(this)
	if o == nil || out == 0 {
		return ePointer
	}
	if iid != 0 {
		g := *(*windows.GUID)(ptrFrom(iid))
		// The first options interface, and Options3 through its own vtable. The loader
		// asks for every newer one (ICoreWebView2EnvironmentOptions2, 4, ...) and must
		// hear that there are none, or it would call methods no vtable here has.
		self := uintptr(unsafe.Pointer(o))
		switch g {
		case *iidIUnknown, *iidEnvOptions:
			atomic.AddInt32(&o.refs, 1)
			writeOut(out, self)
			return sOK
		case *iidEnvOptions3:
			atomic.AddInt32(&o.refs, 1)
			writeOut(out, o.options3Addr())
			return sOK
		}
	}
	writeOut(out, 0)
	return eNoInterface
}

func optionsAddRef(this uintptr) uintptr {
	if o := lookupOptions(this); o != nil {
		return refCount(atomic.AddInt32(&o.refs, 1))
	}
	return 1
}

func optionsRelease(this uintptr) uintptr {
	if o := lookupOptions(this); o != nil {
		return refCount(atomic.AddInt32(&o.refs, -1))
	}
	return 0
}

func optionsGetString(this, out uintptr, get func(*envOptions) string) uintptr {
	o := lookupOptions(this)
	if o == nil || out == 0 {
		return ePointer
	}
	p := coTaskString(get(o))
	if p == 0 {
		return 0x8007000E // E_OUTOFMEMORY
	}
	writeOut(out, p)
	return sOK
}

func optionsGetArgs(this, out uintptr) uintptr {
	return optionsGetString(this, out, func(o *envOptions) string { return o.args })
}

func optionsGetLanguage(this, out uintptr) uintptr {
	return optionsGetString(this, out, func(o *envOptions) string { return o.language })
}

func optionsGetTargetVersion(this, out uintptr) uintptr {
	return optionsGetString(this, out, func(*envOptions) string { return targetVersion })
}

// optionsGetSSO: no single sign-on with the Windows account; the page signs in nowhere.
func optionsGetSSO(_, out uintptr) uintptr {
	if out == 0 {
		return ePointer
	}
	*(*int32)(ptrFrom(out)) = 0
	return sOK
}

// The options are fixed when the object is made.
func optionsPutString(_, _ uintptr) uintptr { return eNotImpl }
func optionsPutBool(_, _ uintptr) uintptr   { return eNotImpl }
