//go:build windows

package hostmon

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

	"github.com/Higangssh/hyperbutler/internal/winipc"
	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/diag"
)

// SampleCIM reads the perf counters of pids in one CIM query. A PID that has exited is
// simply missing from the result.
func SampleCIM(ctx context.Context, pids []uint32) (map[uint32]ProcSample, error) {
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
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script) //nolint:gosec // G204: the PowerShell script is built here from numeric process IDs only
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("hostmon: CIM query: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return ParseCIM(string(out))
}

// Pin opens pid for SYNCHRONIZE. While the handle is open Windows cannot reuse the PID,
// so a sample keyed by it always belongs to the original process, and Exited reports
// whether that process has ended.
func Pin(pid uint32) (windows.Handle, error) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return 0, fmt.Errorf("hostmon: open PID %d: %w", pid, err)
	}
	return h, nil
}

// Exited reports whether the process behind a Pin handle has ended.
func Exited(h windows.Handle) bool {
	ev, err := windows.WaitForSingleObject(h, 0)
	return err != nil || ev == windows.WAIT_OBJECT_0
}

// Snapshot lists the running processes. vmmem and vmwp cannot be opened by a
// non-elevated user, so only what the toolhelp snapshot gives is used.
func Snapshot() ([]Proc, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("hostmon: process snapshot: %w", err)
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	var ps []Proc
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		ps = append(ps, Proc{PID: e.ProcessID, PPID: e.ParentProcessID, Name: windows.UTF16ToString(e.ExeFile[:])})
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, fmt.Errorf("hostmon: process walk: %w", err)
	}
	return ps, nil
}

// PipeExists reports whether a local named pipe (`\\.\pipe\name`) exists, without
// connecting to it.
func PipeExists(name string) (bool, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return false, err
	}
	var fd windows.Win32finddata
	h, err := windows.FindFirstFile(p, &fd)
	if err != nil {
		// The pipe file system answers a missing name with "no more files".
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) || errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return false, nil
		}
		return false, err
	}
	_ = windows.FindClose(h)
	return true, nil
}

// DiagClient reads an internal/diag endpoint on a named pipe.
type DiagClient struct{ c *http.Client }

// NewDiagClient returns a client for the diag endpoint on pipe.
func NewDiagClient(pipe string) *DiagClient {
	return &DiagClient{c: &http.Client{
		Timeout: 90 * time.Second, // longer than a capped pprof profile
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return winipc.DialPipe(ctx, pipe)
			},
			MaxIdleConns:    1,
			IdleConnTimeout: time.Minute,
		},
	}}
}

// Stats reads /debug/stats.
func (d *DiagClient) Stats(ctx context.Context) (diag.Stats, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body, err := d.get(ctx, diag.StatsPath, maxStatsBody)
	if err != nil {
		return diag.Stats{}, err
	}
	return ParseStats(strings.NewReader(string(body)))
}

// MaxProfile bounds a profile fetched with Get.
const MaxProfile = 64 << 20

// Get fetches a debug path such as /debug/pprof/heap, up to MaxProfile bytes.
func (d *DiagClient) Get(ctx context.Context, path string) ([]byte, error) {
	return d.get(ctx, path, MaxProfile)
}

func (d *DiagClient) get(ctx context.Context, path string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://diag"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hostmon: diag: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hostmon: diag %s: HTTP %d", path, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("hostmon: diag %s: %w", path, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("hostmon: diag %s: more than %d bytes", path, limit)
	}
	return b, nil
}

// Close drops idle connections.
func (d *DiagClient) Close() { d.c.CloseIdleConnections() }
