//go:build windows

package sandboxmsi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/imageset"
)

// lockedFile is a file open with FILE_SHARE_READ only, which keeps every writer out, and a
// delete or rename, on this handle's whole life.
type lockedFile struct {
	f    *os.File
	path string
}

func (l *lockedFile) Path() string { return l.path }

func (l *lockedFile) Close() error { return l.f.Close() }

func (l *lockedFile) SHA256() (string, error) {
	if _, err := l.f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, l.f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Lock opens path for an elevated installer: a regular file with one name, on a local fixed
// volume, readable by others and nothing more until Close. A handle open for writing or a
// writable mapping of the file makes the open fail with a sharing violation, so what the
// hash sees next is what stays there.
func Lock(path string) (Locked, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_SEQUENTIAL_SCAN, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(h), path)
	if err := checkLocked(h); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	final, err := imageset.FinalPathOf(h)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := checkInstallerPath(final, driveType); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &lockedFile{f: f, path: final}, nil
}

func checkLocked(h windows.Handle) error {
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return err
	}
	if fi.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_DEVICE) != 0 {
		return errors.New("not a regular file")
	}
	if ft, err := windows.GetFileType(h); err != nil || ft != windows.FILE_TYPE_DISK {
		return errors.New("not a regular file")
	}
	// A file this run just created has one name. Another is harmless to the lock, which
	// holds the file and not the name, but nothing legitimate makes one.
	if fi.NumberOfLinks != 1 {
		return fmt.Errorf("the file has %d names", fi.NumberOfLinks)
	}
	return nil
}

func driveType(root string) uint32 {
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return windows.DRIVE_UNKNOWN
	}
	return windows.GetDriveType(p)
}

// checkInstallerPath takes the final path of the locked file: msiexec cannot read a \\?\
// path, so it must fit MAX_PATH, and on a share or a removable disk what the installer reads
// is not up to this machine's sharing rules.
func checkInstallerPath(final string, drive func(string) uint32) error {
	if len(final) < 3 || final[1] != ':' || final[2] != '\\' || strings.HasPrefix(final, `\\`) {
		return fmt.Errorf("%s is not on a local disk; set HYPERBUTLER_HOME to a folder on one", final)
	}
	if t := drive(final[:3]); t != windows.DRIVE_FIXED {
		return fmt.Errorf("%s is not on a fixed local disk (drive type %d); set HYPERBUTLER_HOME to a folder on one", final, t)
	}
	if len(final) >= windows.MAX_PATH {
		return fmt.Errorf("%s is too long for Windows Installer; set HYPERBUTLER_HOME to a shorter folder", final)
	}
	return nil
}

// msiexecArgs is msiexec's whole command line. /norestart: a file in use must not restart
// the machine by itself, /passive would. No log file: this process is elevated, and every
// folder this user could name is one a lower-privilege program could link elsewhere.
func msiexecArgs(path string, reinstall bool) (string, error) {
	if strings.ContainsAny(path, "\"\r\n") {
		return "", fmt.Errorf("unusable path %q", path)
	}
	args := `/i "` + path + `" /passive /norestart`
	if reinstall {
		args += ` REINSTALL=ALL REINSTALLMODE=amus`
	}
	return args, nil
}

// shellExecuteInfo is SHELLEXECUTEINFOW, whose size the tests check.
type shellExecuteInfo struct {
	cbSize       uint32
	fMask        uint32
	hwnd         windows.HWND
	lpVerb       *uint16
	lpFile       *uint16
	lpParameters *uint16
	lpDirectory  *uint16
	nShow        int32
	hInstApp     uintptr
	lpIDList     uintptr
	lpClass      *uint16
	hkeyClass    uintptr
	dwHotKey     uint32
	hIconMonitor uintptr
	hProcess     windows.Handle
}

const (
	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoAsync        = 0x00000100
	seeMaskFlagNoUI       = 0x00000400
	// errPromptDeclined is what ShellExecuteEx fails with when UAC is answered no.
	errPromptDeclined = windows.Errno(1223)
)

var (
	shell32            = windows.NewLazySystemDLL("shell32.dll")
	procShellExecuteEx = shell32.NewProc("ShellExecuteExW")
	kernel32           = windows.NewLazySystemDLL("kernel32.dll")
	procConsoleWindow  = kernel32.NewProc("GetConsoleWindow")
)

// Exec runs System32's msiexec.exe on path with the verb runas, which is the one UAC prompt,
// from System32, and waits for it to end. A declined prompt is ErrDeclined. The caller holds
// the lock on path throughout. ctx does not end the install: stopping msiexec halfway is
// its own business, through its own window.
func Exec(_ context.Context, path string, reinstall bool) (uint32, error) {
	args, err := msiexecArgs(path, reinstall)
	if err != nil {
		return 0, err
	}
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return 0, err
	}
	// ShellExecuteEx wants COM on its thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	const coinitApartment, coinitNoOLE1 = 0x2, 0x4
	if err := windows.CoInitializeEx(0, coinitApartment|coinitNoOLE1); err == nil {
		defer windows.CoUninitialize()
	}
	hwnd, _, _ := procConsoleWindow.Call() // nil without a console; the prompt finds its own way
	h, err := shellExecuteRunAs(hwnd, filepath.Join(sys, "msiexec.exe"), args, sys)
	if err != nil {
		if errors.Is(err, errPromptDeclined) {
			return 0, ErrDeclined
		}
		return 0, fmt.Errorf("start msiexec: %w", err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if _, err := windows.WaitForSingleObject(h, windows.INFINITE); err != nil {
		return 0, fmt.Errorf("wait for msiexec: %w", err)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return 0, fmt.Errorf("read msiexec's exit code: %w", err)
	}
	return code, nil
}

func shellExecuteRunAs(hwnd uintptr, file, args, dir string) (windows.Handle, error) {
	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return 0, err
	}
	f, err := windows.UTF16PtrFromString(file)
	if err != nil {
		return 0, err
	}
	a, err := windows.UTF16PtrFromString(args)
	if err != nil {
		return 0, err
	}
	d, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	si := shellExecuteInfo{
		fMask: seeMaskNoCloseProcess | seeMaskNoAsync | seeMaskFlagNoUI,
		hwnd:  windows.HWND(hwnd), lpVerb: verb, lpFile: f, lpParameters: a, lpDirectory: d, nShow: windows.SW_SHOWNORMAL,
	}
	si.cbSize = uint32(unsafe.Sizeof(si))
	r, _, callErr := procShellExecuteEx.Call(uintptr(unsafe.Pointer(&si)))
	if r == 0 {
		return 0, callErr
	}
	if si.hProcess == 0 {
		return 0, errors.New("no process handle came back")
	}
	return si.hProcess, nil
}
