//go:build windows

package engine

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func isSharingViolation(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

// ErrLocked means another process holds the lock.
var ErrLocked = errors.New("engine: lock is held by another process")

// FileLock is an exclusive LockFileEx lock on the first byte of a file. The OS releases
// it when the holding process exits, however it exits, so it never goes stale after a
// crash. The file itself is left in place; its content does not matter.
type FileLock struct {
	f *os.File
}

func openLockFile(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// FILE_FLAG_OPEN_REPARSE_POINT with a check below: a link planted in place of the
	// lock file is not followed.
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("engine: lock file %s is a link or a directory", path)
	}
	return os.NewFile(uintptr(h), path), nil
}

// TryLock takes the lock at path without waiting. It returns ErrLocked if another
// process holds it.
func TryLock(path string) (*FileLock, error) {
	f, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	ol := new(windows.Overlapped)
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if err != nil {
		_ = f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("engine: lock %s: %w", path, err)
	}
	return &FileLock{f: f}, nil
}

// Lock takes the lock at path, retrying until timeout. Other processes probe the lock
// with Held, which holds it for an instant; the retry rides over that.
func Lock(path string, timeout time.Duration) (*FileLock, error) {
	deadline := time.Now().Add(timeout)
	for {
		l, err := TryLock(path)
		if !errors.Is(err, ErrLocked) || time.Now().After(deadline) {
			return l, err
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// Unlock releases the lock. It is safe to call more than once.
func (l *FileLock) Unlock() error {
	if l == nil || l.f == nil {
		return nil
	}
	ol := new(windows.Overlapped)
	err := windows.UnlockFileEx(windows.Handle(l.f.Fd()), 0, 1, 0, ol)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}

// Held reports whether some process holds the lock at path. It takes and releases the
// lock to find out, so a concurrent Lock caller must retry (Lock does). A missing lock
// file means no holder.
func Held(path string) (bool, error) {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	for i := 0; ; i++ {
		l, err := TryLock(path)
		if err == nil {
			return false, l.Unlock()
		}
		if !errors.Is(err, ErrLocked) {
			return false, err
		}
		// Another prober may hold it for an instant; the engine holds it for good.
		if i >= 3 {
			return true, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
}
