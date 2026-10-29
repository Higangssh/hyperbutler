// Command hcs-spike answers one question for ADR 0002: can a non-elevated process on
// Windows 11 Home create and boot a Linux VM through the Host Compute Service?
//
// It calls computecore.dll directly (HCS API v2) with only the Go standard library,
// boots a kernel+initrd via LinuxKernelDirect, streams the serial console from a named
// pipe, and prints timings. Throwaway code: see docs/adr/0002-primary-vm-backend.md.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	computecore = syscall.NewLazyDLL("computecore.dll")
	kernel32    = syscall.NewLazyDLL("kernel32.dll")

	procHcsCreateOperation            = computecore.NewProc("HcsCreateOperation")
	procHcsCloseOperation             = computecore.NewProc("HcsCloseOperation")
	procHcsWaitForOperationResult     = computecore.NewProc("HcsWaitForOperationResult")
	procHcsCreateComputeSystem        = computecore.NewProc("HcsCreateComputeSystem")
	procHcsStartComputeSystem         = computecore.NewProc("HcsStartComputeSystem")
	procHcsTerminateComputeSystem     = computecore.NewProc("HcsTerminateComputeSystem")
	procHcsCloseComputeSystem         = computecore.NewProc("HcsCloseComputeSystem")
	procHcsGetComputeSystemProperties = computecore.NewProc("HcsGetComputeSystemProperties")
	procLocalFree                     = kernel32.NewProc("LocalFree")
)

const infinite = 0xFFFFFFFF

type hresultError struct {
	op     string
	hr     uint32
	result string
}

func (e *hresultError) Error() string {
	msg := fmt.Sprintf("%s: HRESULT 0x%08X", e.op, e.hr)
	if e.result != "" {
		msg += ": " + e.result
	}
	return msg
}

func utf16Ptr(s string) *uint16 {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		panic(err)
	}
	return p
}

// takeString copies a LocalAlloc'd PWSTR returned by HCS and frees it.
func takeString(p *uint16) string {
	if p == nil {
		return ""
	}
	s := syscall.UTF16ToString(unsafe.Slice(p, wcslen(p)))
	procLocalFree.Call(uintptr(unsafe.Pointer(p)))
	return s
}

func wcslen(p *uint16) int {
	n := 0
	for ptr := unsafe.Pointer(p); *(*uint16)(ptr) != 0; n++ {
		ptr = unsafe.Add(ptr, 2)
	}
	return n
}

// call runs one HCS operation synchronously and returns its result document.
func call(name string, timeout time.Duration, fn func(op uintptr) uintptr) (string, error) {
	op, _, _ := procHcsCreateOperation.Call(0, 0)
	if op == 0 {
		return "", fmt.Errorf("%s: HcsCreateOperation failed", name)
	}
	defer procHcsCloseOperation.Call(op)

	if hr := uint32(fn(op)); hr != 0 {
		return "", &hresultError{op: name, hr: hr}
	}
	var doc *uint16
	ms := uint32(infinite)
	if timeout > 0 {
		ms = uint32(timeout.Milliseconds())
	}
	hr, _, _ := procHcsWaitForOperationResult.Call(op, uintptr(ms), uintptr(unsafe.Pointer(&doc)))
	result := takeString(doc)
	if uint32(hr) != 0 {
		return result, &hresultError{op: name, hr: uint32(hr), result: result}
	}
	return result, nil
}

func vmConfig(kernel, initrd, cmdline, pipe string, memMB, cpus int) ([]byte, error) {
	doc := map[string]any{
		"SchemaVersion":                     map[string]int{"Major": 2, "Minor": 1},
		"Owner":                             "winbox-spike",
		"ShouldTerminateOnLastHandleClosed": true,
		"VirtualMachine": map[string]any{
			"StopOnReset": true,
			"Chipset": map[string]any{
				"LinuxKernelDirect": map[string]any{
					"KernelFilePath": kernel,
					"InitRdPath":     initrd,
					"KernelCmdLine":  cmdline,
				},
			},
			"ComputeTopology": map[string]any{
				"Memory":    map[string]any{"SizeInMB": memMB, "AllowOvercommit": true},
				"Processor": map[string]any{"Count": cpus},
			},
			"Devices": map[string]any{
				"ComPorts": map[string]any{
					"0": map[string]any{"NamedPipe": pipe},
				},
			},
		},
	}
	return json.MarshalIndent(doc, "", "  ")
}

// openPipe connects to the COM port pipe that the VM worker serves.
func openPipe(name string, deadline time.Time) (*os.File, error) {
	for {
		h, err := syscall.CreateFile(utf16Ptr(name), syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_EXISTING, 0, 0)
		if err == nil {
			return os.NewFile(uintptr(h), name), nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("open %s: %w", name, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func main() {
	tmp := filepath.Join(os.TempDir(), "hb-dev", "winbox-spike", "guest")
	kernel := flag.String("kernel", filepath.Join(tmp, "vmlinuz-virt"), "kernel image")
	initrd := flag.String("initrd", filepath.Join(tmp, "initramfs-virt"), "initrd")
	cmdline := flag.String("cmdline", "console=ttyS0 earlyprintk=serial loglevel=7", "kernel command line")
	marker := flag.String("marker", "Alpine Init", "serial output that counts as 'booted'")
	memMB := flag.Int("mem", 512, "memory in MB")
	cpus := flag.Int("cpus", 2, "vCPUs")
	hold := flag.Duration("hold", 0, "keep the VM running this long after boot (for memory measurement)")
	timeout := flag.Duration("timeout", 60*time.Second, "overall boot timeout")
	printCfg := flag.Bool("print-config", false, "print the HCS document and exit")
	flag.Parse()

	id := fmt.Sprintf("winbox-spike-%d", time.Now().UnixNano())
	pipe := `\\.\pipe\` + id + "-com1"
	cfg, err := vmConfig(*kernel, *initrd, *cmdline, pipe, *memMB, *cpus)
	if err != nil {
		fatal(err)
	}
	if *printCfg {
		fmt.Println(string(cfg))
		return
	}
	if err := computecore.Load(); err != nil {
		fatal(fmt.Errorf("load computecore.dll: %w", err))
	}

	t0 := time.Now()
	step := func(name string) { fmt.Printf("[%7.0f ms] %s\n", float64(time.Since(t0).Microseconds())/1000, name) }

	var system uintptr
	if _, err := call("HcsCreateComputeSystem", 30*time.Second, func(op uintptr) uintptr {
		hr, _, _ := procHcsCreateComputeSystem.Call(
			uintptr(unsafe.Pointer(utf16Ptr(id))),
			uintptr(unsafe.Pointer(utf16Ptr(string(cfg)))),
			op, 0, uintptr(unsafe.Pointer(&system)))
		return hr
	}); err != nil {
		fatal(err)
	}
	step("created compute system " + id)
	defer func() {
		_, err := call("HcsTerminateComputeSystem", 30*time.Second, func(op uintptr) uintptr {
			hr, _, _ := procHcsTerminateComputeSystem.Call(system, op, 0)
			return hr
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "terminate:", err)
		}
		procHcsCloseComputeSystem.Call(system)
		step("terminated")
	}()

	if _, err := call("HcsStartComputeSystem", 30*time.Second, func(op uintptr) uintptr {
		hr, _, _ := procHcsStartComputeSystem.Call(system, op, 0)
		return hr
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	step("started")

	console, err := openPipe(pipe, time.Now().Add(10*time.Second))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer console.Close()
	step("console pipe connected")

	booted := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(console)
		first, seen := true, false
		for sc.Scan() {
			line := sc.Text()
			if first {
				step("first serial output")
				first = false
			}
			fmt.Println("  | " + line)
			if !seen && strings.Contains(line, *marker) {
				seen = true
				close(booted)
			}
		}
	}()

	select {
	case <-booted:
		step(fmt.Sprintf("boot marker %q seen", *marker))
	case <-time.After(*timeout):
		fmt.Fprintln(os.Stderr, "timed out waiting for boot marker")
		return
	}

	if *hold > 0 {
		fmt.Printf("holding VM for %s (pid %d)\n", *hold, os.Getpid())
		time.Sleep(*hold)
		if props, err := call("HcsGetComputeSystemProperties", 10*time.Second, func(op uintptr) uintptr {
			hr, _, _ := procHcsGetComputeSystemProperties.Call(system, op, uintptr(unsafe.Pointer(utf16Ptr(`{"PropertyTypes":["Memory","Statistics"]}`))))
			return hr
		}); err == nil {
			fmt.Println("properties:", props)
		} else {
			fmt.Fprintln(os.Stderr, "properties:", err)
		}
	}
}

func fatal(err error) {
	var he *hresultError
	if errors.As(err, &he) && he.hr == 0x80070005 {
		fmt.Fprintln(os.Stderr, "ACCESS DENIED: this process is not allowed to use HCS")
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
