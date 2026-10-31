//go:build windows

package share

import (
	"encoding/binary"
	"errors"
	"strings"
	"sync"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// Watch reports changes below the share root with ReadDirectoryChangesW (recursive).
// fn gets each change; overflow is called when the kernel buffer overflowed and changes
// were lost. Close the result to stop.
func (f *NTFS) Watch(fn func(kind EventKind, path, newPath []string), overflow func()) (Closer, error) {
	p, err := windows.UTF16PtrFromString(f.rootPath)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.FILE_LIST_DIRECTORY, shareAll, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, err
	}
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	stop, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		_ = windows.CloseHandle(ev)
		_ = windows.CloseHandle(h)
		return nil, err
	}
	w := &watcher{h: h, ev: ev, stop: stop, ov: new(windows.Overlapped), buf: alignedBuf(256 << 10), done: make(chan struct{})}
	// Arm the first read before returning: the kernel only records changes from the
	// first ReadDirectoryChangesW on, and a caller may change files right after Watch.
	if err := w.arm(); err != nil {
		_ = windows.CloseHandle(stop)
		_ = windows.CloseHandle(ev)
		_ = windows.CloseHandle(h)
		return nil, err
	}
	go w.loop(fn, overflow)
	return w, nil
}

type watcher struct {
	h, ev, stop windows.Handle
	// The kernel writes ov and buf when a read completes, after the call has returned,
	// so they must not live on a goroutine stack, which the runtime may move (grow) in
	// between. Keeping them in the watcher puts them on the heap.
	ov   *windows.Overlapped
	buf  []byte
	done chan struct{}
	once sync.Once
}

const watchFilter = windows.FILE_NOTIFY_CHANGE_FILE_NAME | windows.FILE_NOTIFY_CHANGE_DIR_NAME |
	windows.FILE_NOTIFY_CHANGE_SIZE | windows.FILE_NOTIFY_CHANGE_LAST_WRITE | windows.FILE_NOTIFY_CHANGE_ATTRIBUTES |
	windows.FILE_NOTIFY_CHANGE_CREATION

// arm starts the next overlapped read of changes into w.buf.
func (w *watcher) arm() error {
	*w.ov = windows.Overlapped{HEvent: w.ev}
	_ = windows.ResetEvent(w.ev)
	var n uint32 // only written for a synchronous completion
	return windows.ReadDirectoryChanges(w.h, &w.buf[0], len32(len(w.buf)), true, watchFilter, &n, w.ov, 0)
}

// loop waits for each armed read, reports its changes and arms the next one.
func (w *watcher) loop(fn func(EventKind, []string, []string), overflow func()) {
	defer close(w.done)
	for {
		var n uint32
		r, err := windows.WaitForMultipleObjects([]windows.Handle{w.ev, w.stop}, false, windows.INFINITE)
		if err != nil || r != windows.WAIT_OBJECT_0 {
			_ = windows.CancelIoEx(w.h, w.ov)
			_ = windows.GetOverlappedResult(w.h, w.ov, &n, true)
			return
		}
		err = windows.GetOverlappedResult(w.h, w.ov, &n, false)
		switch {
		case errors.Is(err, windows.ERROR_NOTIFY_ENUM_DIR):
			overflow()
		case err != nil:
			return
		case n == 0:
			overflow()
		default:
			parseNotify(w.buf[:n], fn)
		}
		if err := w.arm(); err != nil {
			return
		}
	}
}

// parseNotify decodes FILE_NOTIFY_INFORMATION records, pairs renames, and drops
// repeated modifications of the same path within one batch.
func parseNotify(b []byte, fn func(EventKind, []string, []string)) {
	le := binary.LittleEndian
	var renameFrom []string
	var lastMod string
	for off := 0; off+12 <= len(b); {
		next := int(le.Uint32(b[off:]))
		action := le.Uint32(b[off+4:])
		nameLen := int(le.Uint32(b[off+8:]))
		if nameLen%2 != 0 || off+12+nameLen > len(b) {
			return
		}
		u := make([]uint16, nameLen/2)
		for i := range u {
			u[i] = le.Uint16(b[off+12+2*i:])
		}
		raw := string(utf16.Decode(u))
		path := splitNT(raw)
		if path != nil {
			switch action {
			case windows.FILE_ACTION_ADDED:
				fn(Created, path, nil)
				lastMod = ""
			case windows.FILE_ACTION_REMOVED:
				fn(Removed, path, nil)
				lastMod = ""
			case windows.FILE_ACTION_MODIFIED:
				if raw != lastMod {
					fn(Modified, path, nil)
				}
				lastMod = raw
			case windows.FILE_ACTION_RENAMED_OLD_NAME:
				renameFrom = path
				lastMod = ""
			case windows.FILE_ACTION_RENAMED_NEW_NAME:
				if renameFrom != nil {
					fn(Renamed, renameFrom, path)
				} else {
					fn(Created, path, nil)
				}
				renameFrom = nil
				lastMod = ""
			}
		}
		if next == 0 {
			break
		}
		off += next
	}
	if renameFrom != nil {
		fn(Removed, renameFrom, nil)
	}
}

// splitNT turns a watcher path ("a\b\c") into checked Linux components, or nil if any
// component cannot be one.
func splitNT(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, `\`)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		n := fromNT(p)
		if checkName(n) != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

func (w *watcher) Close() error {
	w.once.Do(func() {
		_ = windows.SetEvent(w.stop)
		<-w.done
		_ = windows.CloseHandle(w.h)
		_ = windows.CloseHandle(w.ev)
		_ = windows.CloseHandle(w.stop)
	})
	return nil
}
