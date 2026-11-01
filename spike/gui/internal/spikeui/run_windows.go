//go:build windows

package spikeui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// FindCLI returns the hyperbutler.exe to run: flag if set, else one next to this
// executable, else one on PATH.
func FindCLI(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if self, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(self), "hyperbutler.exe")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return exec.LookPath("hyperbutler.exe")
}

// Breakaway starts the CLI outside the GUI's job (see spike/gui/webview2), so an
// engine it starts is not killed with the GUI.
var Breakaway bool

func command(ctx context.Context, exe string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, exe, args...) //nolint:gosec // G204: the hyperbutler CLI, fixed arguments
	// A GUI process starting a console program would flash a console window.
	flags := uint32(windows.CREATE_NO_WINDOW)
	if Breakaway {
		flags |= windows.CREATE_BREAKAWAY_FROM_JOB
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
	return cmd
}

// QueryStatus runs `hyperbutler status --json`. The command exits non-zero when the
// engine is not running but still prints the document, so the exit code is ignored
// when the output parses.
func QueryStatus(ctx context.Context, exe string) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := command(ctx, exe, "status", "--json")
	cmd.Stdout = &out
	runErr := cmd.Run()
	s, err := ParseStatus(&out)
	if err != nil {
		if runErr != nil {
			return Status{}, fmt.Errorf("hyperbutler status: %w", runErr)
		}
		return Status{}, err
	}
	return s, nil
}

// StartEngine runs `hyperbutler start` when real is set. Otherwise it only waits a
// moment, so the spike can be clicked without starting a VM.
func StartEngine(ctx context.Context, exe string, real bool) error {
	if !real {
		select {
		case <-time.After(1500 * time.Millisecond):
			return errors.New("stub")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if out, err := command(ctx, exe, "start").CombinedOutput(); err != nil {
		return fmt.Errorf("hyperbutler start: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// ReportFirstPaint prints the line the measuring harness (spike/gui/measure) waits
// for, with the time since this process was created. The harness measures from its
// own clock as well; this number leaves out its CreateProcess call.
func ReportFirstPaint(what string) {
	var c, e, k, u windows.Filetime
	ms := int64(-1)
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &c, &e, &k, &u); err == nil {
		var now windows.Filetime
		getSystemTimePrecise(&now)
		ms = (now.Nanoseconds() - c.Nanoseconds()) / 1e6
	}
	_, _ = fmt.Fprintf(os.Stdout, "HB-SPIKE painted %s since_create_ms=%d\n", what, ms)
}

var (
	kernel32                    = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimePrecise    = kernel32.NewProc("GetSystemTimePreciseAsFileTime")
	procGetUserDefaultLocaleNam = kernel32.NewProc("GetUserDefaultLocaleName")
)

func getSystemTimePrecise(ft *windows.Filetime) {
	_, _, _ = procGetSystemTimePrecise.Call(uintptr(unsafe.Pointer(ft)))
}

// UserLanguage returns the Windows user locale, for example "ko-KR".
func UserLanguage() string {
	buf := make([]uint16, 85) // LOCALE_NAME_MAX_LENGTH
	n, _, _ := procGetUserDefaultLocaleNam.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return "en"
	}
	return windows.UTF16ToString(buf)
}

// FindAsset looks for rel (for example "assets/brand/mascot.png") in the working
// directory, next to the executable, and in their parents, so the spikes run from the
// repository without copying the mascot. The real GUI embeds its assets.
func FindAsset(rel string) (string, error) {
	var roots []string
	if wd, err := os.Getwd(); err == nil {
		roots = append(roots, wd)
	}
	if self, err := os.Executable(); err == nil {
		roots = append(roots, filepath.Dir(self))
	}
	for _, r := range roots {
		for d := r; ; {
			p := filepath.Join(d, filepath.FromSlash(rel))
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
			up := filepath.Dir(d)
			if up == d {
				break
			}
			d = up
		}
	}
	return "", fmt.Errorf("%s not found above the working directory or the executable", rel)
}
