//go:build windows

package vm

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/Higangssh/hyperbutler/internal/guid"
	"golang.org/x/sys/windows"
)

var (
	// NewLazySystemDLL loads from System32 only, never from the current directory.
	modComputecore = windows.NewLazySystemDLL("computecore.dll")

	procHcsCreateOperation            = modComputecore.NewProc("HcsCreateOperation")
	procHcsCloseOperation             = modComputecore.NewProc("HcsCloseOperation")
	procHcsWaitForOperationResult     = modComputecore.NewProc("HcsWaitForOperationResult")
	procHcsCreateComputeSystem        = modComputecore.NewProc("HcsCreateComputeSystem")
	procHcsStartComputeSystem         = modComputecore.NewProc("HcsStartComputeSystem")
	procHcsTerminateComputeSystem     = modComputecore.NewProc("HcsTerminateComputeSystem")
	procHcsCloseComputeSystem         = modComputecore.NewProc("HcsCloseComputeSystem")
	procHcsGetComputeSystemProperties = modComputecore.NewProc("HcsGetComputeSystemProperties")
	procHcsPauseComputeSystem         = modComputecore.NewProc("HcsPauseComputeSystem")
	procHcsResumeComputeSystem        = modComputecore.NewProc("HcsResumeComputeSystem")
	procHcsShutDownComputeSystem      = modComputecore.NewProc("HcsShutDownComputeSystem")
	procHcsOpenComputeSystem          = modComputecore.NewProc("HcsOpenComputeSystem")
	procHcsEnumerateComputeSystems    = modComputecore.NewProc("HcsEnumerateComputeSystems")
	procHcsGrantVMAccess              = modComputecore.NewProc("HcsGrantVmAccess")
	procHcsRevokeVMAccess             = modComputecore.NewProc("HcsRevokeVmAccess")
)

// opTimeout bounds every HCS operation.
const opTimeout = 60 * time.Second

// HRESULTs seen on Windows 11 Home.
const (
	hrAccessDenied    = 0x80070005 // E_ACCESSDENIED
	hrHcsAccessDenied = 0x8037011B // HCS_E_ACCESS_DENIED
)

// Error is a failed HCS call.
type Error struct {
	Op     string
	HR     uint32
	Result string // HCS result document, if any
}

// IsAccessDenied reports whether err is an HCS call that failed because the caller may
// not use HCS (not in Administrators or Hyper-V Administrators, see ADR 0004).
func IsAccessDenied(err error) bool {
	var he *Error
	return errors.As(err, &he) && (he.HR == hrAccessDenied || he.HR == hrHcsAccessDenied)
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("vm: %s: HRESULT 0x%08X", e.Op, e.HR)
	if e.HR == hrAccessDenied || e.HR == hrHcsAccessDenied {
		msg += " (access denied: the user must be in Hyper-V Administrators, see ADR 0004)"
	}
	if e.Result != "" {
		msg += ": " + e.Result
	}
	return msg
}

// System is a handle to an HCS compute system. Its methods are safe for concurrent use.
// The VM is created with ShouldTerminateOnLastHandleClosed, so it also dies when the
// process that owns the handle exits.
type System struct {
	id string

	mu     sync.Mutex
	h      uintptr // 0 after Close
	closed bool
}

// Create creates (but does not start) a compute system named id from cfg.
func Create(id string, cfg *Config) (*System, error) {
	doc, err := cfg.Document()
	if err != nil {
		return nil, err
	}
	if err := modComputecore.Load(); err != nil {
		return nil, fmt.Errorf("vm: load computecore.dll (is Virtual Machine Platform enabled?): %w", err)
	}
	idp, err := windows.UTF16PtrFromString(id)
	if err != nil {
		return nil, fmt.Errorf("vm: system id: %w", err)
	}
	docp, err := windows.UTF16PtrFromString(string(doc))
	if err != nil {
		return nil, fmt.Errorf("vm: document: %w", err)
	}
	var h uintptr
	if _, err := call("HcsCreateComputeSystem", func(op uintptr) uintptr {
		hr, _, _ := procHcsCreateComputeSystem.Call(uintptr(unsafe.Pointer(idp)), uintptr(unsafe.Pointer(docp)), op, 0, uintptr(unsafe.Pointer(&h)))
		return hr
	}); err != nil {
		if h != 0 {
			procHcsCloseComputeSystem.Call(h)
		}
		return nil, err
	}
	return &System{id: id, h: h}, nil
}

// ID returns the compute system name given to Create.
func (s *System) ID() string { return s.id }

// withHandle runs fn with the handle, or fails if s is closed. The lock is held so Close
// cannot release the handle while fn uses it.
func (s *System) withHandle(fn func(h uintptr) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("vm: system is closed")
	}
	return fn(s.h)
}

// Start boots the VM.
func (s *System) Start() error {
	return s.withHandle(func(h uintptr) error {
		_, err := call("HcsStartComputeSystem", func(op uintptr) uintptr {
			hr, _, _ := procHcsStartComputeSystem.Call(h, op, 0)
			return hr
		})
		return err
	})
}

// RuntimeID returns the VM's runtime GUID: the VM ID that AF_HYPERV (hvsock) addresses.
func (s *System) RuntimeID() (guid.GUID, error) {
	var doc string
	err := s.withHandle(func(h uintptr) error {
		q, err := windows.UTF16PtrFromString(`{"PropertyTypes":[]}`)
		if err != nil {
			return err
		}
		doc, err = call("HcsGetComputeSystemProperties", func(op uintptr) uintptr {
			hr, _, _ := procHcsGetComputeSystemProperties.Call(h, op, uintptr(unsafe.Pointer(q)))
			return hr
		})
		return err
	})
	if err != nil {
		return guid.GUID{}, err
	}
	return parseRuntimeID(doc)
}

// Pause freezes the VM's virtual processors. Pausing aborts the host's hvsock
// connections to the guest (docs/research/guest-services.md).
func (s *System) Pause() error { return s.simple("HcsPauseComputeSystem", procHcsPauseComputeSystem) }

// Resume undoes Pause.
func (s *System) Resume() error {
	return s.simple("HcsResumeComputeSystem", procHcsResumeComputeSystem)
}

// ShutDown asks the guest to shut down through the Hyper-V shutdown integration
// component (hv_utils in a Linux guest) and waits for HCS to accept the request. The VM
// stops later, when the guest powers off; State reports when.
func (s *System) ShutDown() error {
	return s.simple("HcsShutDownComputeSystem", procHcsShutDownComputeSystem)
}

func (s *System) simple(name string, proc *windows.LazyProc) error {
	return s.withHandle(func(h uintptr) error {
		_, err := call(name, func(op uintptr) uintptr {
			hr, _, _ := proc.Call(h, op, 0)
			return hr
		})
		return err
	})
}

// State returns the HCS state of the system, such as "Running", "Paused" or "Stopped".
// A system that HCS no longer knows reports "Stopped".
func (s *System) State() (string, error) {
	var doc string
	err := s.withHandle(func(h uintptr) error {
		q, err := windows.UTF16PtrFromString(`{"PropertyTypes":[]}`)
		if err != nil {
			return err
		}
		doc, err = call("HcsGetComputeSystemProperties", func(op uintptr) uintptr {
			hr, _, _ := procHcsGetComputeSystemProperties.Call(h, op, uintptr(unsafe.Pointer(q)))
			return hr
		})
		return err
	})
	var he *Error
	if errors.As(err, &he) && alreadyGone(he.HR) {
		return "Stopped", nil
	}
	if err != nil {
		return "", err
	}
	return parseState(doc)
}

// Open opens an existing compute system by name, for example one listed by List.
func Open(id string) (*System, error) {
	if err := modComputecore.Load(); err != nil {
		return nil, fmt.Errorf("vm: load computecore.dll (is Virtual Machine Platform enabled?): %w", err)
	}
	idp, err := windows.UTF16PtrFromString(id)
	if err != nil {
		return nil, fmt.Errorf("vm: system id: %w", err)
	}
	var h uintptr
	if hr, _, _ := procHcsOpenComputeSystem.Call(uintptr(unsafe.Pointer(idp)), windows.GENERIC_ALL, uintptr(unsafe.Pointer(&h))); hr != 0 {
		return nil, &Error{Op: "HcsOpenComputeSystem", HR: dword(hr)}
	}
	return &System{id: id, h: h}, nil
}

// List returns the compute systems HCS reports for the owner (all systems if owner is
// empty) that the caller may see.
func List(owner string) ([]Summary, error) {
	if err := modComputecore.Load(); err != nil {
		return nil, fmt.Errorf("vm: load computecore.dll (is Virtual Machine Platform enabled?): %w", err)
	}
	q := "{}"
	if owner != "" {
		b, err := json.Marshal(map[string][]string{"Owners": {owner}})
		if err != nil {
			return nil, err
		}
		q = string(b)
	}
	qp, err := windows.UTF16PtrFromString(q)
	if err != nil {
		return nil, err
	}
	doc, err := call("HcsEnumerateComputeSystems", func(op uintptr) uintptr {
		hr, _, _ := procHcsEnumerateComputeSystems.Call(uintptr(unsafe.Pointer(qp)), op)
		return hr
	})
	if err != nil {
		return nil, err
	}
	return parseSummaries(doc)
}

// GrantVMAccess gives the worker process of compute system id access to path (a disk
// file). It must be called before Create attaches the file.
func GrantVMAccess(id, path string) error {
	return vmAccess("HcsGrantVmAccess", procHcsGrantVMAccess, id, path)
}

// RevokeVMAccess undoes GrantVMAccess.
func RevokeVMAccess(id, path string) error {
	return vmAccess("HcsRevokeVmAccess", procHcsRevokeVMAccess, id, path)
}

func vmAccess(name string, proc *windows.LazyProc, id, path string) error {
	if err := modComputecore.Load(); err != nil {
		return fmt.Errorf("vm: load computecore.dll (is Virtual Machine Platform enabled?): %w", err)
	}
	idp, err := windows.UTF16PtrFromString(id)
	if err != nil {
		return err
	}
	pp, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	if hr, _, _ := proc.Call(uintptr(unsafe.Pointer(idp)), uintptr(unsafe.Pointer(pp))); hr != 0 {
		return &Error{Op: name, HR: dword(hr)}
	}
	return nil
}

// Terminate stops the VM at once. Terminating a VM that has already stopped is not an
// error.
func (s *System) Terminate() error {
	return s.withHandle(func(h uintptr) error {
		_, err := call("HcsTerminateComputeSystem", func(op uintptr) uintptr {
			hr, _, _ := procHcsTerminateComputeSystem.Call(h, op, 0)
			return hr
		})
		var he *Error
		if errors.As(err, &he) && alreadyGone(he.HR) {
			return nil
		}
		return err
	})
}

// alreadyGone reports whether hr says the system no longer runs. computecore.dll uses
// the 0x8037xxxx HRESULTs; the 0xC037xxxx forms are what vmcompute.dll (and hcsshim)
// report for the same conditions, accepted too so the check does not depend on which one
// a Windows build surfaces.
func alreadyGone(hr uint32) bool {
	switch hr &^ 0x40000000 {
	case 0x8037010E, // HCS_E_SYSTEM_NOT_FOUND
		0x80370110: // HCS_E_SYSTEM_ALREADY_STOPPED
		return true
	}
	return false
}

// Close releases the handle. With ShouldTerminateOnLastHandleClosed this also stops the
// VM if no other handle is open.
func (s *System) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	procHcsCloseComputeSystem.Call(s.h)
	s.h = 0
	return nil
}

// dword is the 32-bit result (HRESULT or DWORD) a Win32 call returns in a register.
func dword(r uintptr) uint32 {
	return uint32(r) //nolint:gosec // G115: the callee returns 32 bits; the rest of the register is not part of the result
}

// call runs one HCS operation synchronously and returns its result document.
func call(name string, fn func(op uintptr) uintptr) (string, error) {
	op, _, _ := procHcsCreateOperation.Call(0, 0)
	if op == 0 {
		return "", fmt.Errorf("vm: %s: HcsCreateOperation failed", name)
	}
	defer procHcsCloseOperation.Call(op)

	if hr := dword(fn(op)); hr != 0 {
		return "", &Error{Op: name, HR: hr}
	}
	var res *uint16
	hr, _, _ := procHcsWaitForOperationResult.Call(op, uintptr(opTimeout/time.Millisecond), uintptr(unsafe.Pointer(&res)))
	doc := ""
	if res != nil {
		doc = windows.UTF16PtrToString(res)
		_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(res)))
	}
	if code := dword(hr); code != 0 {
		return doc, &Error{Op: name, HR: code, Result: doc}
	}
	return doc, nil
}
