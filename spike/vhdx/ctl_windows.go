package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	wguid "github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/guestctl"
	"github.com/Higangssh/hyperbutler/internal/vm"
)

// The ctl options test the guest services daemon (guest/cmd/hyperbutlerd) over the guestctl
// hvsock control channel:
//
//	hcs -ctl -mode ctl               ext4 + hyperbutlerd; writes -mb MiB, measures the page-cache
//	                                 policy (vmmem private working set until it returns) and
//	                                 the trim policy (VHDX allocation after a delete, no
//	                                 manual fstrim)
//	hcs -ctl -guard -mode guard      the free-space guard test with the budget sent over
//	                                 guestctl instead of COM2
//
// See docs/research/guest-services.md.

type ctlOpts struct {
	on        bool
	bin       string
	dargs     string
	memPoll   time.Duration
	trimPoll  time.Duration
	trimSlack uint64
}

var (
	// hvSocketSDDL, when set, makes hcsConfig add the HvSocket device with the control
	// service open to this security descriptor.
	hvSocketSDDL string
	// ctlStatusAt is when the last guestctl status arrived (UnixNano), for the host guard's
	// dead-guard check.
	ctlStatusAt atomic.Int64
	// markerHook, if set, runs at every "@@M" marker after the image is measured and before
	// the guest is acknowledged.
	markerHook func(label string)
)

func ctlFlags(fs *flag.FlagSet) *ctlOpts {
	o := &ctlOpts{}
	fs.BoolVar(&o.on, "ctl", false, "run hyperbutlerd in the guest and talk to it over the guestctl hvsock channel")
	fs.StringVar(&o.bin, "ctl-bin", filepath.Join(os.TempDir(), "hb-dev", "guestctl-bin", "hyperbutlerd"), "linux hyperbutlerd binary to pack into the initrd")
	fs.StringVar(&o.dargs, "ctl-args", "", "extra hyperbutlerd flags, comma-separated (no spaces: they go on the kernel command line)")
	fs.DurationVar(&o.memPoll, "ctl-mem-wait", 90*time.Second, "ctl mode: how long to follow vmmem after the page cache is filled")
	fs.DurationVar(&o.trimPoll, "ctl-trim-wait", 330*time.Second, "ctl mode: how long to wait for the VHDX to shrink after the delete")
	fs.Uint64Var(&o.trimSlack, "ctl-trim-slack", 64<<20, "ctl mode: the delete counts as reclaimed once the VHDX is within this of its pre-write allocation")
	return o
}

func (o *ctlOpts) cmdline() string {
	if !o.on {
		return ""
	}
	s := " WB_CTL=1"
	if o.dargs != "" {
		s += " WB_DARGS=" + o.dargs
	}
	return s
}

func userSDDL() (string, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return vm.UserOnlySDDL(tu.User.Sid.String())
}

// hvSocketDevice is the HCS HvSocket device: deny everything except connecting to the
// control service.
func hvSocketDevice(sddl string) map[string]any {
	return map[string]any{"HvSocketConfig": map[string]any{
		"DefaultBindSecurityDescriptor":    vm.DenyAllSDDL,
		"DefaultConnectSecurityDescriptor": vm.DenyAllSDDL,
		"ServiceTable": map[string]any{
			guestctl.ServiceID.String(): map[string]any{
				"BindSecurityDescriptor":    vm.DenyAllSDDL,
				"ConnectSecurityDescriptor": sddl,
			},
		},
	}}
}

func runtimeID(system uintptr) (wguid.GUID, error) {
	props, err := hcsCall("HcsGetComputeSystemProperties", 10*time.Second, func(op uintptr) uintptr {
		hr, _, _ := procHcsGetProperties.Call(system, op, uintptr(unsafe.Pointer(u16(`{"PropertyTypes":[]}`))))
		return hr
	})
	if err != nil {
		return wguid.GUID{}, err
	}
	var p struct{ RuntimeId string }
	if err := json.Unmarshal([]byte(props), &p); err != nil {
		return wguid.GUID{}, err
	}
	return wguid.FromString(p.RuntimeId)
}

// startCtl connects to hyperbutlerd and keeps the connection up.
func startCtl(system uintptr, start time.Time) (*guestctl.Supervisor, wguid.GUID, func(), error) {
	id, err := runtimeID(system)
	if err != nil {
		return nil, wguid.GUID{}, nil, err
	}
	logf := func(format string, args ...any) {
		fmt.Printf("  [%6.1fs] %s\n", time.Since(start).Seconds(), fmt.Sprintf(format, args...))
	}
	var lastLog time.Time
	var lastSteps uint64
	sup := &guestctl.Supervisor{
		Dial: guestctl.HvsockDialer(id),
		Logf: logf,
		Config: guestctl.ClientConfig{OnStatus: func(s guestctl.Status) {
			// Liveness counts only a guard that is applying the budget and whose step
			// counter advanced: the status goroutine answers even when the guard loop is
			// stuck. A guest can fake this, but faking it only removes the margin pause;
			// the floor pause never depends on guest data.
			if s.Guard == guestctl.GuardOK && s.GuardSteps != lastSteps {
				ctlStatusAt.Store(time.Now().UnixNano())
			}
			lastSteps = s.GuardSteps
			if time.Since(lastLog) >= 5*time.Second {
				lastLog = time.Now()
				logf("ctl status: guard=%d errno=%d free=%dMiB reserved=%dMiB target=%dMiB short=%d trims=%d trimmed=%dMiB filecache=%dMiB memfree=%dMiB cachetrims=%d freed=%dMiB",
					s.Guard, s.GuardErrno, s.FSFree>>20, s.Reserved>>20, s.Target>>20, s.Short, s.Trims, s.Trimmed>>20, s.FileCache>>20, s.MemFree>>20, s.CacheTrims, s.CacheFreed>>20)
			}
		}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = sup.Run(ctx); close(done) }()
	return sup, id, func() { cancel(); <-done }, nil
}

// vmmemPIDs lists the vmmem processes now. The VM's own vmmem is the one that appears
// between a snapshot before HcsCreateComputeSystem and one after start: a non-elevated
// caller cannot read a vmmem process's owner (GetOwner returns access denied), and other
// VMs (other hyperbutler sessions) may run at the same time.
func vmmemPIDs() map[uint32]bool {
	m := map[uint32]bool{}
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return m
	}
	defer func() { _ = syscall.CloseHandle(snap) }()
	var pe syscall.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = syscall.Process32First(snap, &pe); err == nil; err = syscall.Process32Next(snap, &pe) {
		if strings.HasPrefix(strings.ToLower(syscall.UTF16ToString(pe.ExeFile[:])), "vmmem") {
			m[pe.ProcessID] = true
		}
	}
	return m
}

// newVmmemPID waits for a vmmem process that is not in before.
func newVmmemPID(before map[uint32]bool) (uint32, error) {
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		var found []uint32
		for pid := range vmmemPIDs() {
			if !before[pid] {
				found = append(found, pid)
			}
		}
		if len(found) == 1 {
			return found[0], nil
		}
		if len(found) > 1 {
			return 0, fmt.Errorf("%d new vmmem processes; cannot tell which is this VM", len(found))
		}
	}
	return 0, fmt.Errorf("no new vmmem process")
}

// vmmemPrivateWS returns the private working set (REQ-MEM-001's metric) of process pid,
// from the raw WMI perf class (Get-Counter's counter names are localized).
func vmmemPrivateWS(pid uint32) (uint64, error) {
	script := fmt.Sprintf(`(Get-CimInstance Win32_PerfRawData_PerfProc_Process -Filter "IDProcess = %d").WorkingSetPrivate`, pid)
	out, err := exec.Command("powershell.exe", "-NoProfile", "-Command", script).Output() //nolint:gosec // G204: the PowerShell script is built here from numeric process IDs only
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return 0, fmt.Errorf("no perf data for vmmem pid %d", pid)
	}
	return strconv.ParseUint(s, 10, 64)
}

// ctlHooks returns the marker hook for -mode ctl.
func ctlHooks(o *ctlOpts, pid uint32, vhdx string, start time.Time, sup *guestctl.Supervisor) func(string) {
	var baseMem uint64
	var baseAlloc int64
	logf := func(format string, args ...any) {
		fmt.Printf("  [%6.1fs] %s\n", time.Since(start).Seconds(), fmt.Sprintf(format, args...))
	}
	mem := func(label string) uint64 {
		v, err := vmmemPrivateWS(pid)
		if err != nil {
			logf("vmmem %s: %v", label, err)
			return 0
		}
		logf("vmmem private working set %s: %d MiB", label, v>>20)
		return v
	}
	return func(label string) {
		switch label {
		case "ready":
			baseMem = mem("at ready")
			u, _ := measure(vhdx)
			baseAlloc = u.Allocated
		case "written":
			mem("after write")
		case "cache-filled":
			// REQ-MEM-002: page cache must go back to the host within 60 s.
			t0 := time.Now()
			first := time.Duration(-1)
			for time.Since(t0) <= o.memPoll {
				v := mem(fmt.Sprintf("+%2.0fs", time.Since(t0).Seconds()))
				if first < 0 && v != 0 && v <= baseMem+100<<20 {
					first = time.Since(t0)
				}
				time.Sleep(5 * time.Second)
			}
			if first >= 0 {
				logf("RESULT page cache returned (<= ready + 100 MiB) after %v", first.Round(time.Second))
			} else {
				logf("RESULT page cache NOT returned within %v", o.memPoll)
			}
		case "deleted":
			// REQ-DISK-001: the delete must come back to the host within 5 min, with no
			// manual fstrim.
			t0 := time.Now()
			for {
				u, err := measure(vhdx)
				if err == nil {
					logf("vhdx allocated +%3.0fs: %d MiB (pre-write %d MiB)", time.Since(t0).Seconds(), u.Allocated>>20, baseAlloc>>20)
					if u.Allocated <= baseAlloc+int64(o.trimSlack) {
						logf("RESULT delete reclaimed on the host after %v", time.Since(t0).Round(time.Second))
						return
					}
				}
				if time.Since(t0) > o.trimPoll {
					logf("RESULT delete NOT reclaimed within %v", o.trimPoll)
					return
				}
				time.Sleep(5 * time.Second)
			}
		case "end":
			mem("at end")
			if c := sup.Client(); c != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				rtt, err := c.Ping(ctx)
				cancel()
				logf("ctl ping: %v %v", rtt, errString(err))
			}
		}
	}
}
