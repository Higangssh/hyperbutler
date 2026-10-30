package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// The hcs subcommand repeats the QEMU experiment on HCS: the VHDX is attached as a Hyper-V
// SCSI disk, so guest discards go through storvsc -> vhdmp. HCS needs an elevated process
// on this machine (ADR 0002): run non-elevated, HcsCreateComputeSystem fails with
// HCS_E_ACCESS_DENIED. The elevated run was never done; see the doc for the command.

var (
	modComputecore           = syscall.NewLazyDLL("computecore.dll")
	procHcsCreateOperation   = modComputecore.NewProc("HcsCreateOperation")
	procHcsCloseOperation    = modComputecore.NewProc("HcsCloseOperation")
	procHcsWaitForOperation  = modComputecore.NewProc("HcsWaitForOperationResult")
	procHcsCreateSystem      = modComputecore.NewProc("HcsCreateComputeSystem")
	procHcsStartSystem       = modComputecore.NewProc("HcsStartComputeSystem")
	procHcsTerminateSystem   = modComputecore.NewProc("HcsTerminateComputeSystem")
	procHcsCloseSystem       = modComputecore.NewProc("HcsCloseComputeSystem")
	procHcsGrantVmAccess     = modComputecore.NewProc("HcsGrantVmAccess")
	procHcsRevokeVmAccess    = modComputecore.NewProc("HcsRevokeVmAccess")
	procHcsResumeSystem      = modComputecore.NewProc("HcsResumeComputeSystem")
	procHcsGetProperties     = modComputecore.NewProc("HcsGetComputeSystemProperties")
	procGetDiskFreeSpaceEx   = modKernel32.NewProc("GetDiskFreeSpaceExW")
	procLocalFree            = modKernel32.NewProc("LocalFree")
	errAccessDenied          = uint32(0x80070005) // E_ACCESSDENIED
	errHcsAccessDenied       = uint32(0x8037011B) // HCS_E_ACCESS_DENIED, seen on this machine
	hcsInfinite              = uint32(0xFFFFFFFF)
	defaultHCSGuestModuleSet = "scsi_transport_fc.ko,hv_storvsc.ko,sd_mod.ko"
)

func hcsCall(name string, timeout time.Duration, fn func(op uintptr) uintptr) (string, error) {
	op, _, _ := procHcsCreateOperation.Call(0, 0)
	if op == 0 {
		return "", fmt.Errorf("%s: HcsCreateOperation failed", name)
	}
	defer procHcsCloseOperation.Call(op)
	if hr := uint32(fn(op)); hr != 0 {
		if hr == errHcsAccessDenied || hr == errAccessDenied {
			return "", fmt.Errorf("%s: access denied, HRESULT 0x%08X (HCS needs an elevated process on this machine)", name, hr)
		}
		return "", fmt.Errorf("%s: HRESULT 0x%08X", name, hr)
	}
	var doc *uint16
	ms := hcsInfinite
	if timeout > 0 {
		ms = uint32(timeout.Milliseconds())
	}
	hr, _, _ := procHcsWaitForOperation.Call(op, uintptr(ms), uintptr(unsafe.Pointer(&doc)))
	result := ""
	if doc != nil {
		n := 0
		for p := unsafe.Pointer(doc); *(*uint16)(p) != 0; p = unsafe.Add(p, 2) {
			n++
		}
		result = syscall.UTF16ToString(unsafe.Slice(doc, n))
		procLocalFree.Call(uintptr(unsafe.Pointer(doc)))
	}
	if uint32(hr) != 0 {
		if uint32(hr) == errHcsAccessDenied || uint32(hr) == errAccessDenied {
			return result, fmt.Errorf("%s: access denied (HCS needs an elevated process on this machine): %s", name, result)
		}
		return result, fmt.Errorf("%s: HRESULT 0x%08X: %s", name, uint32(hr), result)
	}
	return result, nil
}

// activityConn records when the guest last printed anything, for watchStall.
type activityConn struct {
	f    *os.File
	last atomic.Int64
}

func (a *activityConn) touch()                      { a.last.Store(time.Now().UnixNano()) }
func (a *activityConn) Write(p []byte) (int, error) { return a.f.Write(p) }
func (a *activityConn) Read(p []byte) (int, error) {
	n, err := a.f.Read(p)
	if n > 0 {
		a.touch()
	}
	return n, err
}

// watchStall handles the host-disk-full test. When the guest stays silent for `stall`, it
// prints the VM state and the host free space, deletes `reserve` to free host space, and
// tries HcsResumeComputeSystem in case HCS paused the VM. It does this once.
func watchStall(a *activityConn, system uintptr, stall time.Duration, reserve, vhdx string, start time.Time) {
	for {
		time.Sleep(2 * time.Second)
		if time.Since(time.Unix(0, a.last.Load())) < stall {
			continue
		}
		fmt.Printf("  [%6.1fs] guest silent for %v\n", time.Since(start).Seconds(), stall)
		printState(system)
		report("at stall", vhdx)
		fmt.Println("  host free bytes:", freeBytes(filepath.Dir(vhdx)))
		if err := os.Remove(reserve); err != nil {
			fmt.Println("  remove reserve:", err)
		} else {
			fmt.Println("  removed reserve file; host free bytes now:", freeBytes(filepath.Dir(vhdx)))
		}
		_, err := hcsCall("HcsResumeComputeSystem", 30*time.Second, func(op uintptr) uintptr {
			hr, _, _ := procHcsResumeSystem.Call(system, op, 0)
			return hr
		})
		fmt.Println("  HcsResumeComputeSystem:", errString(err))
		printState(system)
		a.touch()
		return
	}
}

func printState(system uintptr) {
	props, err := hcsCall("HcsGetComputeSystemProperties", 10*time.Second, func(op uintptr) uintptr {
		hr, _, _ := procHcsGetProperties.Call(system, op, uintptr(unsafe.Pointer(u16(`{"PropertyTypes":[]}`))))
		return hr
	})
	if err != nil {
		fmt.Println("  state:", err)
		return
	}
	var p struct{ State, Stopped any }
	_ = json.Unmarshal([]byte(props), &p)
	fmt.Printf("  state: %v\n", p.State)
}

func freeBytes(dir string) uint64 {
	var free uint64
	p, _ := syscall.UTF16PtrFromString(dir)
	procGetDiskFreeSpaceEx.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&free)), 0, 0)
	return free
}

func errString(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

func u16(s string) *uint16 {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		panic(err)
	}
	return p
}

func hcsConfig(kernel, initrd, cmdline, pipe, vhdx string, sparse bool, schemaMinor int, memory map[string]any) ([]byte, error) {
	attachment := map[string]any{"Type": "VirtualDisk", "Path": vhdx}
	if sparse {
		// Without this, vhdmp refuses to open a sparse VHDX (the same "must not be sparse"
		// error OpenVirtualDisk returns; measured in the vhdx subcommand).
		attachment["AlwaysAllowSparseFiles"] = true
	}
	devices := map[string]any{
		"ComPorts": comPorts(pipe),
		"Scsi": map[string]any{
			"0": map[string]any{"Attachments": map[string]any{"0": attachment}},
		},
	}
	if hvSocketSDDL != "" {
		devices["HvSocket"] = hvSocketDevice(hvSocketSDDL)
	}
	doc := map[string]any{
		"SchemaVersion":                     map[string]int{"Major": 2, "Minor": schemaMinor},
		"Owner":                             "hyperbutler-spike",
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
				"Memory":    memory,
				"Processor": map[string]any{"Count": 2},
			},
			"Devices": devices,
		},
	}
	return json.MarshalIndent(doc, "", "  ")
}

func runHCS(args []string) error {
	fs := flag.NewFlagSet("hcs", flag.ExitOnError)
	guestDir := fs.String("guest", filepath.Join(os.TempDir(), "hb-dev", "hyperbutler-spike", "guest"), "dir with vmlinuz-virt and initramfs-virt")
	modDir := fs.String("modules", "", "dir with the guest's Hyper-V SCSI modules (hv_storvsc is not in initramfs-virt)")
	modList := fs.String("modlist", defaultHCSGuestModuleSet, "modules from -modules to insmod, in dependency order")
	sparse := fs.Bool("sparse", false, "make the VHDX an NTFS sparse file and set AlwaysAllowSparseFiles")
	diskSize := fs.Int64("size", 2*gib, "virtual disk size in bytes")
	writeMB := fs.Int("mb", 1024, "MiB the guest writes and deletes")
	settle := fs.Duration("settle", 5*time.Second, "measure again this long after each marker")
	printCfg := fs.Bool("print-config", false, "print the HCS document and exit")
	schemaMinor := fs.Int("schema-minor", 1, "HCS schema 2.x minor version; raise it if HCS rejects AlwaysAllowSparseFiles")
	block := fs.Uint("block", 0, "VHDX block size in bytes (0 = default 32 MiB; WSL uses 1048576)")
	group := fs.Int("group", 0, "ext4 mode: delete alternating groups of this many 256 KiB files instead of all of them (0 = all)")
	memMB := fs.Int("mem", 1024, "guest memory in MiB")
	memHints := fs.String("mem-hints", "", "comma-separated HCS VirtualMachineMemory booleans to set, e.g. EnableHotHint,EnableColdDiscardHint")
	extraDir := fs.String("extra", "", "dir whose files are packed flat under /wbextra (mke2fs and its libraries for -mode ext4)")
	fsync := fs.Bool("fsync", false, "guest writes with dd conv=fsync, so data is flushed to the device before delete and fstrim")
	mode := fs.String("mode", "", `guest test: "" (FAT32 write/delete/fstrim/blkdiscard), "trimonly" (same, no blkdiscard), "align" (raw device, unaligned vs 32 MiB-aligned discards), or "ext4" (many small files on ext4; needs -extra and the ext4 modules)`)
	keep := fs.Bool("keep", false, "reuse the existing VHDX in -dir instead of creating a fresh one (for -mode check)")
	reserve := fs.String("reserve", "", "host-full test: file to delete if the guest stalls, freeing host space")
	stall := fs.Duration("stall", 30*time.Second, "host-full test: guest silence that counts as a stall")
	gopts := guardFlags(fs)
	copts := ctlFlags(fs)
	fs.Parse(args)
	if gopts.on {
		if *extraDir == "" {
			return fmt.Errorf("-guard needs -extra (mke2fs, e2fsck)")
		}
		initrdExtraFiles = append(initrdExtraFiles, gopts.bin)
	}
	if copts.on {
		if *extraDir == "" {
			return fmt.Errorf("-ctl needs -extra (mke2fs, e2fsck)")
		}
		initrdExtraFiles = append(initrdExtraFiles, copts.bin)
		sddl, err := userSDDL()
		if err != nil {
			return err
		}
		hvSocketSDDL = sddl
	}

	var mods, names []string
	if *modDir != "" {
		for _, m := range strings.Split(*modList, ",") {
			mods = append(mods, filepath.Join(*modDir, m))
			names = append(names, m)
		}
	}
	initrd, err := buildInitrdExtra(filepath.Join(*guestDir, "initramfs-virt"), mods, *extraDir)
	if err != nil {
		return err
	}
	name := "hcs.vhdx"
	if *sparse {
		name = "hcs-sparse.vhdx"
	}
	var vhdx string
	if *keep {
		vhdx = filepath.Join(*dir, name)
	} else {
		vhdx = fresh(name)
		vhdxBlockSize = uint32(*block)
		if err := createVHDX(vhdx, uint64(*diskSize), createFlagNone); err != nil {
			return err
		}
	}
	if *sparse && !*keep {
		f, err := os.OpenFile(vhdx, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		err = setSparse(f)
		f.Close()
		if err != nil {
			return err
		}
	}

	id := fmt.Sprintf("hyperbutler-vhdx-spike-%d", time.Now().UnixNano())
	pipe := `\\.\pipe\` + id + "-com1"
	if gopts.on && !copts.on {
		guardPipe = `\\.\pipe\` + id + "-com2"
	}
	cmdline := fmt.Sprintf("console=ttyS0 rdinit=/wbinit loglevel=3 WB_DEV=sda WB_MB=%d WB_MODS=%s WB_MODE=%s", *writeMB, strings.Join(names, ","), *mode)
	if *fsync {
		cmdline += " WB_FSYNC=1"
	}
	if *group > 0 {
		cmdline += fmt.Sprintf(" WB_GROUP=%d", *group)
	}
	cmdline += gopts.cmdline()
	cmdline += copts.cmdline()
	memory := map[string]any{"SizeInMB": *memMB, "AllowOvercommit": true}
	for _, h := range strings.Split(*memHints, ",") {
		if h != "" {
			memory[h] = true // e.g. EnableHotHint, EnableColdHint, EnableColdDiscardHint, EnableDeferredCommit
		}
	}
	cfg, err := hcsConfig(filepath.Join(*guestDir, "vmlinuz-virt"), initrd, cmdline, pipe, vhdx, *sparse, *schemaMinor, memory)
	if err != nil {
		return err
	}
	if *printCfg {
		fmt.Println(string(cfg))
		return nil
	}
	if err := modComputecore.Load(); err != nil {
		return fmt.Errorf("load computecore.dll: %w", err)
	}

	// The VM worker runs as a per-VM virtual account; it needs an ACE on the VHDX. The kernel
	// and initrd are not granted: the earlier elevated spike/hcs run booted from the same
	// %TEMP% paths without a grant. If the VM fails to start, grant them too.
	if hr, _, _ := procHcsGrantVmAccess.Call(uintptr(unsafe.Pointer(u16(id))), uintptr(unsafe.Pointer(u16(vhdx)))); hr != 0 {
		return fmt.Errorf("HcsGrantVmAccess: HRESULT 0x%08X", uint32(hr))
	}
	defer procHcsRevokeVmAccess.Call(uintptr(unsafe.Pointer(u16(id))), uintptr(unsafe.Pointer(u16(vhdx))))

	fmt.Printf("hcs sparse=%v image=%s guest writes %d MiB\n", *sparse, vhdx, *writeMB)
	u0 := report("before boot", vhdx)
	start := time.Now()
	vmmemBefore := vmmemPIDs()
	var system uintptr
	if _, err := hcsCall("HcsCreateComputeSystem", 30*time.Second, func(op uintptr) uintptr {
		hr, _, _ := procHcsCreateSystem.Call(uintptr(unsafe.Pointer(u16(id))), uintptr(unsafe.Pointer(u16(string(cfg)))), op, 0, uintptr(unsafe.Pointer(&system)))
		return hr
	}); err != nil {
		return err
	}
	defer func() {
		hcsCall("HcsTerminateComputeSystem", 30*time.Second, func(op uintptr) uintptr {
			hr, _, _ := procHcsTerminateSystem.Call(system, op, 0)
			return hr
		})
		procHcsCloseSystem.Call(system)
		report("after VM exit", vhdx)
	}()
	if _, err := hcsCall("HcsStartComputeSystem", 30*time.Second, func(op uintptr) uintptr {
		hr, _, _ := procHcsStartSystem.Call(system, op, 0)
		return hr
	}); err != nil {
		return err
	}

	pipe16, err := syscall.UTF16PtrFromString(pipe)
	if err != nil {
		return err
	}
	var console *os.File
	for deadline := time.Now().Add(10 * time.Second); ; {
		h, err := syscall.CreateFile(pipe16, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_EXISTING, 0, 0)
		if err == nil {
			console = os.NewFile(uintptr(h), pipe)
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("open %s: %w", pipe, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer console.Close()

	var before func()
	if *mode == "mem" {
		before = printVmmem
	}
	var conn io.ReadWriter = console
	if *reserve != "" {
		w := &activityConn{f: console}
		w.touch()
		conn = w
		go watchStall(w, system, *stall, *reserve, vhdx, start)
	}
	var sup *guestctlSupervisor
	if copts.on {
		s, rid, stop, err := startCtl(system, start)
		if err != nil {
			return err
		}
		defer stop()
		sup = s
		fmt.Printf("  ctl: VM runtime id %s, control service %s\n", rid, guestctlServiceID())
		pid, perr := newVmmemPID(vmmemBefore)
		if perr != nil {
			fmt.Println("  ctl: vmmem:", perr)
		} else {
			fmt.Printf("  ctl: this VM's vmmem is pid %d\n", pid)
		}
		markerHook = ctlHooks(copts, pid, vhdx, start, s)
		if !gopts.on {
			// Without the guard test, still send a real budget so hyperbutlerd's guard runs as
			// it would in hyperbutler (on a roomy volume it is "unlimited").
			gopts.margin, gopts.floor = 1<<30, 256<<20
			gopts.on = true
		}
	}
	if gopts.on {
		g, err := startHostGuard(gopts, system, vhdx, uint64(*diskSize), uint64(u0.Allocated), start, sup)
		if err != nil {
			return err
		}
		defer g.close()
	}
	done, err := driveGuest(conn, vhdx, start, *settle, before)
	if err != nil {
		return err
	}
	if !done {
		return fmt.Errorf("guest did not finish")
	}
	return nil
}
