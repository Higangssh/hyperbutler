package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/vm"
)

// probeDevices creates (and immediately terminates) a VM whose Devices object has extra
// keys merged in, to see what HCS accepts. It answers "does HCS on this build take a
// virtio-fs device for a Linux guest?" without guessing.
func probeDevices(extra string) error {
	cfg := &vm.Config{
		Owner:          "hyperbutler-share-probe",
		KernelPath:     filepath.Join(*imageDir, "vmlinuz-virt"),
		InitrdPath:     filepath.Join(*imageDir, "initramfs-hyperbutler.gz"),
		KernelCmdLine:  "console=ttyS0",
		MemoryMB:       512,
		ProcessorCount: 1,
	}
	doc, err := cfg.Document()
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(doc, &m); err != nil {
		return err
	}
	var add map[string]any
	if err := json.Unmarshal([]byte(extra), &add); err != nil {
		return fmt.Errorf("-probe: %w", err)
	}
	devs := m["VirtualMachine"].(map[string]any)["Devices"].(map[string]any)
	for k, v := range add {
		devs[k] = v
	}
	doc, _ = json.Marshal(m)
	id := fmt.Sprintf("hyperbutler-share-probe-%d", time.Now().UnixNano()%1e9)
	cc := windows.NewLazySystemDLL("computecore.dll")
	create, start, term, closeSys := cc.NewProc("HcsCreateComputeSystem"), cc.NewProc("HcsStartComputeSystem"), cc.NewProc("HcsTerminateComputeSystem"), cc.NewProc("HcsCloseComputeSystem")
	newOp, closeOp, wait := cc.NewProc("HcsCreateOperation"), cc.NewProc("HcsCloseOperation"), cc.NewProc("HcsWaitForOperationResult")
	callOp := func(name string, f func(op uintptr) uintptr) {
		op, _, _ := newOp.Call(0, 0)
		defer closeOp.Call(op)
		hr := f(op)
		var res *uint16
		whr, _, _ := wait.Call(op, 30000, uintptr(unsafe.Pointer(&res)))
		s := ""
		if res != nil {
			s = windows.UTF16PtrToString(res)
			_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(res)))
		}
		fmt.Printf("%s: call 0x%08X, result 0x%08X %s\n", name, uint32(hr), uint32(whr), s)
	}
	var h uintptr
	idp, _ := syscall.UTF16PtrFromString(id)
	docp, _ := syscall.UTF16PtrFromString(string(doc))
	callOp("HcsCreateComputeSystem", func(op uintptr) uintptr {
		hr, _, _ := create.Call(uintptr(unsafe.Pointer(idp)), uintptr(unsafe.Pointer(docp)), op, 0, uintptr(unsafe.Pointer(&h)))
		return hr
	})
	if h == 0 {
		return nil
	}
	callOp("HcsStartComputeSystem", func(op uintptr) uintptr {
		hr, _, _ := start.Call(h, op, 0)
		return hr
	})
	callOp("HcsTerminateComputeSystem", func(op uintptr) uintptr {
		hr, _, _ := term.Call(h, op, 0)
		return hr
	})
	closeSys.Call(h)
	return nil
}
