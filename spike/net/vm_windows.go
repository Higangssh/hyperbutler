package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/Higangssh/hyperbutler/spike/net/probe"
)

var (
	computecore             = syscall.NewLazyDLL("computecore.dll")
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procHcsCreateOperation  = computecore.NewProc("HcsCreateOperation")
	procHcsCloseOperation   = computecore.NewProc("HcsCloseOperation")
	procHcsWaitForOperation = computecore.NewProc("HcsWaitForOperationResult")
	procHcsCreateSystem     = computecore.NewProc("HcsCreateComputeSystem")
	procHcsStartSystem      = computecore.NewProc("HcsStartComputeSystem")
	procHcsTerminateSystem  = computecore.NewProc("HcsTerminateComputeSystem")
	procHcsCloseSystem      = computecore.NewProc("HcsCloseComputeSystem")
	procHcsGetProperties    = computecore.NewProc("HcsGetComputeSystemProperties")
	procLocalFree           = kernel32.NewProc("LocalFree")
)

func u16(s string) *uint16 {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		panic(err)
	}
	return p
}

func hcsCall(name string, timeout time.Duration, fn func(op uintptr) uintptr) (string, error) {
	op, _, _ := procHcsCreateOperation.Call(0, 0)
	if op == 0 {
		return "", fmt.Errorf("%s: HcsCreateOperation failed", name)
	}
	defer procHcsCloseOperation.Call(op)
	if hr := uint32(fn(op)); hr != 0 {
		return "", fmt.Errorf("%s: HRESULT 0x%08X", name, hr)
	}
	var doc *uint16
	hr, _, _ := procHcsWaitForOperation.Call(op, uintptr(uint32(timeout.Milliseconds())), uintptr(unsafe.Pointer(&doc)))
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
		return result, fmt.Errorf("%s: HRESULT 0x%08X: %s", name, uint32(hr), result)
	}
	return result, nil
}

// vmSpec is what differs between the hns and relay experiments.
type vmSpec struct {
	id       string
	cmdline  string
	initrd   string
	adapters map[string]any // VirtualMachine.Devices.NetworkAdapters, or nil
	hvsocket map[string]any // VirtualMachine.Devices.HvSocket, or nil
	memMB    int
}

func vmDoc(s vmSpec, kernel, pipe string) ([]byte, error) {
	devices := map[string]any{
		"ComPorts": map[string]any{"0": map[string]any{"NamedPipe": pipe}},
	}
	if s.adapters != nil {
		devices["NetworkAdapters"] = s.adapters
	}
	if s.hvsocket != nil {
		devices["HvSocket"] = s.hvsocket
	}
	doc := map[string]any{
		"SchemaVersion":                     map[string]int{"Major": 2, "Minor": 1},
		"Owner":                             "hyperbutler-net-spike",
		"ShouldTerminateOnLastHandleClosed": true,
		"VirtualMachine": map[string]any{
			"StopOnReset": true,
			"Chipset": map[string]any{
				"LinuxKernelDirect": map[string]any{
					"KernelFilePath": kernel,
					"InitRdPath":     s.initrd,
					"KernelCmdLine":  s.cmdline,
				},
			},
			"ComputeTopology": map[string]any{
				"Memory": map[string]any{
					"SizeInMB": s.memMB, "AllowOvercommit": true,
					"EnableDeferredCommit": true, "EnableHotHint": true, "EnableColdDiscardHint": true,
				},
				"Processor": map[string]any{"Count": 2},
			},
			"Devices": devices,
		},
	}
	return json.MarshalIndent(doc, "", "  ")
}

// vm is a running compute system and its serial console.
type vm struct {
	system  uintptr
	console *os.File
	start   time.Time
}

func createVM(s vmSpec, kernel string) (*vm, string, error) {
	pipe := `\\.\pipe\` + s.id + "-com1"
	cfg, err := vmDoc(s, kernel, pipe)
	if err != nil {
		return nil, "", err
	}
	if err := computecore.Load(); err != nil {
		return nil, "", err
	}
	v := &vm{start: time.Now()}
	if _, err := hcsCall("HcsCreateComputeSystem", 30*time.Second, func(op uintptr) uintptr {
		hr, _, _ := procHcsCreateSystem.Call(uintptr(unsafe.Pointer(u16(s.id))), uintptr(unsafe.Pointer(u16(string(cfg)))), op, 0, uintptr(unsafe.Pointer(&v.system)))
		return hr
	}); err != nil {
		// HcsCreateComputeSystem can return a handle and then fail asynchronously. With
		// ShouldTerminateOnLastHandleClosed, an open handle keeps a half-created VM alive.
		if v.system != 0 {
			procHcsCloseSystem.Call(v.system)
		}
		return nil, "", fmt.Errorf("%w\nconfig: %s", err, cfg)
	}
	props, err := hcsCall("HcsGetComputeSystemProperties", 10*time.Second, func(op uintptr) uintptr {
		hr, _, _ := procHcsGetProperties.Call(v.system, op, uintptr(unsafe.Pointer(u16(`{"PropertyTypes":[]}`))))
		return hr
	})
	if err != nil {
		v.stop()
		return nil, "", err
	}
	var p struct {
		RuntimeID string `json:"RuntimeId"`
	}
	_ = json.Unmarshal([]byte(props), &p)
	return v, p.RuntimeID, nil
}

func (v *vm) boot(pipe string) error {
	if _, err := hcsCall("HcsStartComputeSystem", 30*time.Second, func(op uintptr) uintptr {
		hr, _, _ := procHcsStartSystem.Call(v.system, op, 0)
		return hr
	}); err != nil {
		return err
	}
	for deadline := time.Now().Add(10 * time.Second); ; {
		h, err := syscall.CreateFile(u16(pipe), syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_EXISTING, 0, 0)
		if err == nil {
			v.console = os.NewFile(uintptr(h), pipe)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("open %s: %w", pipe, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (v *vm) stop() {
	if v.console != nil {
		v.console.Close()
	}
	_, _ = hcsCall("HcsTerminateComputeSystem", 30*time.Second, func(op uintptr) uintptr {
		hr, _, _ := procHcsTerminateSystem.Call(v.system, op, 0)
		return hr
	})
	procHcsCloseSystem.Call(v.system)
}

// drive prints the console and collects WBNET results until step=end, the console
// fails, or timeout. On timeout it terminates the VM, which closes the console pipe and
// ends the reader (closing a synchronous pipe handle under a pending ReadFile can hang),
// then waits for the reader, so results is only read after its writer has finished.
func (v *vm) drive(timeout time.Duration, verbose bool, onResult func(probe.Result)) []probe.Result {
	var results []probe.Result
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(v.console)
		sc.Buffer(make([]byte, 4096), 64<<10)
		for sc.Scan() {
			line := sc.Text()
			el := time.Since(v.start).Seconds()
			r, isResult, err := probe.ParseResult(line)
			switch {
			case err != nil:
				fmt.Printf("  [%6.2fs] bad WBNET line: %v\n", el, err)
			case isResult:
				fmt.Printf("  [%6.2fs] %s ok=%v %v\n", el, r.Step, r.OK, r.Fields)
				results = append(results, r)
				if onResult != nil {
					onResult(r)
				}
				if r.Step == "end" {
					return
				}
			case strings.HasPrefix(line, "@@I "):
				fmt.Printf("  [%6.2fs] guest: %s\n", el, strings.TrimRight(line[4:], "\r"))
			case verbose:
				fmt.Printf("  [%6.2fs] | %s\n", el, strings.TrimRight(line, "\r"))
			}
		}
		if err := sc.Err(); err != nil {
			fmt.Printf("  console read stopped: %v\n", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		fmt.Println("  timed out waiting for the guest")
		_, _ = hcsCall("HcsTerminateComputeSystem", 30*time.Second, func(op uintptr) uintptr {
			hr, _, _ := procHcsTerminateSystem.Call(v.system, op, 0)
			return hr
		})
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			fmt.Println("  console reader did not stop; results may be incomplete")
			return nil
		}
	}
	return results
}

// buildInitrd appends a gzip cpio with /wbinit, /wbnet/* and /wbmods/* to initramfs-virt.
func buildInitrd(guestDir, probeBin, rootsPEM string, mods []string) (string, error) {
	base, err := os.ReadFile(filepath.Join(guestDir, "initramfs-virt")) //nolint:gosec // G304: the path comes from the operator's own flags or this tool's work directory
	if err != nil {
		return "", err
	}
	bin, err := os.ReadFile(probeBin) //nolint:gosec // G304: the path comes from the operator's own flags or this tool's work directory
	if err != nil {
		return "", fmt.Errorf("guest probe (build with GOOS=linux): %w", err)
	}
	var extra bytes.Buffer
	zw := gzip.NewWriter(&extra)
	cw := cpioWriter{w: zw}
	cw.entry("wbinit", 0o100755, []byte(guestScript))
	cw.entry("wbnet", 0o040755, nil)
	cw.entry("wbnet/guestprobe", 0o100755, bin)
	if rootsPEM != "" {
		cw.entry("wbnet/roots.pem", 0o100644, []byte(rootsPEM))
	}
	cw.entry("wbmods", 0o040755, nil)
	for _, m := range mods {
		data, err := readModule(m)
		if err != nil {
			return "", err
		}
		cw.entry("wbmods/"+strings.TrimSuffix(filepath.Base(m), ".gz"), 0o100644, data)
	}
	cw.entry("TRAILER!!!", 0, nil)
	if cw.err != nil {
		return "", cw.err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	out := filepath.Join(workDir, "initramfs-net")
	return out, os.WriteFile(out, append(base, extra.Bytes()...), 0o600)
}

func readModule(path string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path comes from the operator's own flags or this tool's work directory
	if err != nil || !strings.HasSuffix(path, ".gz") {
		return data, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(zr)
}

// cpioWriter writes the "newc" (070701) format the kernel's initramfs unpacker reads.
type cpioWriter struct {
	w   io.Writer
	n   int64
	ino int
	err error
}

func (c *cpioWriter) write(b []byte) {
	if c.err != nil {
		return
	}
	n, err := c.w.Write(b)
	c.n += int64(n)
	c.err = err
}

func (c *cpioWriter) pad() {
	if r := c.n % 4; r != 0 {
		c.write(make([]byte, 4-r))
	}
}

func (c *cpioWriter) entry(name string, mode uint32, data []byte) {
	c.ino++
	hdr := fmt.Sprintf("070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
		c.ino, mode, 0, 0, 1, 0, len(data), 0, 0, 0, 0, len(name)+1, 0)
	c.write([]byte(hdr))
	c.write(append([]byte(name), 0))
	c.pad()
	c.write(data)
	c.pad()
}

// rootsFromChains returns the PEM of the last certificate of every verified chain.
func rootsFromChains(chains [][]*x509.Certificate, seen map[string]bool, out *bytes.Buffer) {
	for _, ch := range chains {
		if len(ch) == 0 {
			continue
		}
		root := ch[len(ch)-1]
		k := string(root.Raw)
		if seen[k] {
			continue
		}
		seen[k] = true
		_ = pem.Encode(out, &pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}) // out is a bytes.Buffer
	}
}

const guestScript = `#!/bin/sh
export PATH=/usr/bin:/usr/sbin:/bin:/sbin
/usr/bin/busybox mkdir -p /proc /sys /dev /tmp /etc /bin /sbin
/usr/bin/busybox mount -t proc proc /proc
/usr/bin/busybox mount -t sysfs sys /sys
/usr/bin/busybox mount -t devtmpfs dev /dev
/usr/bin/busybox --install -s
up() { cut -d' ' -f1 /proc/uptime; }
info() { echo "@@I $*"; }
echo "WBNET step=boot ok=true uptime_s=$(up)"
ln -sf "$(command -v kmod)" /tmp/insmod
for k in $(echo "$WB_MODS" | tr ',' ' '); do /tmp/insmod /wbmods/$k 2>&1 | sed 's/^/@@I insmod: /'; done
ip link set lo up
if [ "$WB_NET" = direct ]; then
	i=0; while [ ! -e /sys/class/net/eth0 ] && [ $i -lt 100 ]; do sleep 0.1; i=$((i+1)); done
	if [ ! -e /sys/class/net/eth0 ]; then
		echo "WBNET step=nic ok=false uptime_s=$(up)"
		ls /sys/class/net | sed 's/^/@@I have: /'
		dmesg | grep -iE 'hv_|vmbus|netvsc' | tail -n 20 | sed 's/^/@@I dmesg: /'
		echo "WBNET step=end ok=false"; poweroff -f
	fi
	echo "WBNET step=nic ok=true uptime_s=$(up) mac=$(cat /sys/class/net/eth0/address) driver=$(basename $(readlink /sys/class/net/eth0/device/driver) 2>/dev/null)"
	ip link set eth0 up
	t0=$(up)
	if [ "$WB_IP" = dhcp ]; then
		if udhcpc -i eth0 -n -q -t 5 -T 2 -s /usr/share/udhcpc/default.script >/tmp/dhcp.log 2>&1; then ok=true; else ok=false; fi
		sed 's/^/@@I udhcpc: /' /tmp/dhcp.log
	else
		ip addr add "$WB_IP" dev eth0 && ip route add default via "$WB_GW" && ok=true || ok=false
	fi
	echo "WBNET step=ip ok=$ok mode=$WB_IP start_s=$t0 uptime_s=$(up)"
	ip -4 addr show eth0 | sed 's/^/@@I /'
	ip route | sed 's/^/@@I route: /'
	cat /etc/resolv.conf 2>/dev/null | sed 's/^/@@I resolv(dhcp): /'
	if ping -c 3 -W 2 "$WB_GW" >/tmp/ping.log 2>&1; then ok=true; else ok=false; fi
	echo "WBNET step=ping_gw ok=$ok rtt=$(tail -n 1 /tmp/ping.log | sed 's/.*= //;s/ .*//')"
	if ping -c 3 -W 2 1.1.1.1 >/tmp/ping.log 2>&1; then ok=true; else ok=false; fi
	echo "WBNET step=ping_inet ok=$ok rtt=$(tail -n 1 /tmp/ping.log | sed 's/.*= //;s/ .*//')"
	for d in $(echo "$WB_DNSTRY,$WB_DNSTRY2" | tr ',' ' '); do
		for name in registry-1.docker.io $WB_NSNAME; do
			if nslookup "$name" "$d" >/tmp/ns.log 2>&1; then ok=true; else ok=false; fi
			echo "WBNET step=dns_try ok=$ok server=$d name=$name"
			grep -i -E 'can.t|timed|refused|NXDOMAIN' /tmp/ns.log | sed 's/^/@@I nslookup: /'
		done
	done
	[ -n "$WB_DNS" ] && echo "nameserver $WB_DNS" > /etc/resolv.conf
	/wbnet/guestprobe direct
elif [ "$WB_NET" = relay ]; then
	/wbnet/guestprobe relay "$WB_PORT"
fi
if [ -n "$WB_BULK" ]; then
	if [ "$WB_NET" = direct ]; then k=tcp; else k=vsock; fi
	/wbnet/guestprobe serve $k "$WB_BULK" 3
	sleep 2 # let the host drain the last transfer before poweroff
fi
echo "WBNET step=end ok=true uptime_s=$(up)"
poweroff -f
`
