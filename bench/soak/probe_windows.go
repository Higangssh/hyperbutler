//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/Higangssh/winbox/internal/diag"
)

// sampleCIM reads the perf counters of pids in one CIM query.
func sampleCIM(ctx context.Context, pids []uint32) (map[uint32]ProcSample, error) {
	if len(pids) == 0 {
		return map[uint32]ProcSample{}, nil
	}
	var filter []string
	for _, p := range pids {
		filter = append(filter, "IDProcess="+strconv.FormatUint(uint64(p), 10))
	}
	script := "$ErrorActionPreference='Stop'; " +
		"Get-CimInstance -ClassName Win32_PerfFormattedData_PerfProc_Process -Filter '" + strings.Join(filter, " OR ") + "' | " +
		"ForEach-Object { '{0},{1},{2},{3},{4}' -f $_.IDProcess,$_.WorkingSetPrivate,$_.HandleCount,$_.ThreadCount,$_.PrivateBytes }"
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("soak: CIM query: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return ParseCIM(string(out))
}

// pinProcess opens pid for SYNCHRONIZE. While the handle is open Windows cannot reuse
// the PID, so a sample keyed by it always belongs to the original process, and
// exited reports whether that process has ended.
func pinProcess(pid uint32) (windows.Handle, error) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return 0, fmt.Errorf("soak: open PID %d: %w", pid, err)
	}
	return h, nil
}

func exited(h windows.Handle) bool {
	ev, err := windows.WaitForSingleObject(h, 0)
	return err != nil || ev == windows.WAIT_OBJECT_0
}

// findProcesses returns the PIDs of running processes whose image name equals name,
// case-insensitively (for example "vmmem" or "winbox-dev.exe").
func findProcesses(name string) ([]uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("soak: process snapshot: %w", err)
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	var pids []uint32
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if strings.EqualFold(windows.UTF16ToString(e.ExeFile[:]), name) {
			pids = append(pids, e.ProcessID)
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, fmt.Errorf("soak: process walk: %w", err)
	}
	return pids, nil
}

// diagClient reads /debug/stats from a diag endpoint on a named pipe.
type diagClient struct{ c *http.Client }

func newDiagClient(pipe string) *diagClient {
	return &diagClient{c: &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return winio.DialPipeContext(ctx, pipe)
			},
			MaxIdleConns:    1,
			IdleConnTimeout: time.Minute,
		},
	}}
}

func (d *diagClient) stats(ctx context.Context) (diag.Stats, error) {
	var s diag.Stats
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://diag"+diag.StatsPath, nil)
	if err != nil {
		return s, err
	}
	resp, err := d.c.Do(req)
	if err != nil {
		return s, fmt.Errorf("soak: diag: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return s, fmt.Errorf("soak: diag: HTTP %d", resp.StatusCode)
	}
	return parseStats(io.LimitReader(resp.Body, 1<<20))
}

func (d *diagClient) close() { d.c.CloseIdleConnections() }
