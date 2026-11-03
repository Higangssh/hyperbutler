//go:build windows

package winipc

import (
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modkernel32                     = windows.NewLazySystemDLL("kernel32.dll")
	procGetQueuedCompletionStatusEx = modkernel32.NewProc("GetQueuedCompletionStatusEx")
)

// overlappedEntry is OVERLAPPED_ENTRY.
type overlappedEntry struct {
	key        uintptr
	overlapped *windows.Overlapped
	internal   uintptr
	bytes      uint32
}

var (
	portOnce sync.Once
	port     windows.Handle
	portErr  error
)

// ioPort returns the process-wide completion port, creating it and starting
// ioCompletionLoop on first use. The port and the goroutine live for the process.
func ioPort() (windows.Handle, error) {
	portOnce.Do(func() {
		h, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 0xffffffff)
		if err != nil {
			portErr = os.NewSyscallError("CreateIoCompletionPort", err)
			return
		}
		port = h
		go ioCompletionLoop(h)
	})
	return port, portErr
}

// maxCompletions is how many completions one GetQueuedCompletionStatusEx call dequeues.
const maxCompletions = 64

// ioCompletionLoop hands every completion on the port to the goroutine waiting for it.
// Each OVERLAPPED queued to the port is the first field of an ioOp, whose channel has
// room for exactly the one result.
func ioCompletionLoop(port windows.Handle) {
	var entries [maxCompletions]overlappedEntry
	// SyscallN directly: LazyProc.Call would allocate its argument slice on every wake.
	fn := procGetQueuedCompletionStatusEx.Addr()
	for {
		var n uint32
		r, _, err := syscall.SyscallN(fn, uintptr(port),
			uintptr(unsafe.Pointer(&entries[0])), maxCompletions, uintptr(unsafe.Pointer(&n)),
			windows.INFINITE, 0)
		if r == 0 {
			// An infinite, non-alertable wait fails only if the port is gone, and nothing
			// closes it: every pending operation would hang, so fail loudly.
			panic("winipc: GetQueuedCompletionStatusEx: " + err.Error())
		}
		for i := range entries[:n] {
			e := &entries[i]
			if e.overlapped != nil {
				op := (*ioOp)(unsafe.Pointer(e.overlapped))
				var ioErr error
				// Internal holds the operation's NTSTATUS; only error and warning codes
				// (the high bit set) fail it, as GetOverlappedResult decides.
				if st := windows.NTStatus(op.o.Internal & 0xffffffff); st&0x80000000 != 0 {
					ioErr = st.Errno()
				}
				op.ch <- ioResult{n: e.bytes, err: ioErr}
			}
			*e = overlappedEntry{}
		}
	}
}

type ioResult struct {
	n   uint32
	err error
}

// ioOp is one overlapped operation at a time. o must stay the first field: the
// completion loop converts the *windows.Overlapped it dequeues back to the *ioOp.
type ioOp struct {
	o  windows.Overlapped
	ch chan ioResult
	// buf holds the caller's buffer while the kernel may still write into it. Storing
	// it here also makes the buffer escape to the heap at every call site, so it can
	// never be on a goroutine stack that moves while the I/O is pending.
	buf []byte
}

// newIOOp allocates an ioOp on the heap: the kernel and the completion loop keep a
// pointer to it, so it must not live on a stack. noinline keeps the allocation from
// being folded into a caller's frame.
//
//go:noinline
func newIOOp() *ioOp { return &ioOp{ch: make(chan ioResult, 1)} }

// deadline is one direction's deadline: a channel that is closed when it passes.
type deadline struct {
	mu  sync.Mutex
	gen *deadlineGen
	t   *time.Timer
}

type deadlineGen struct {
	ch   chan struct{}
	once sync.Once
}

func newDeadlineGen() *deadlineGen { return &deadlineGen{ch: make(chan struct{})} }

func (g *deadlineGen) fire() { g.once.Do(func() { close(g.ch) }) }

func (g *deadlineGen) fired() bool {
	select {
	case <-g.ch:
		return true
	default:
		return false
	}
}

// set moves the deadline to t (zero: none). Operations already waiting see the change:
// they wait on the same channel unless it has already fired.
func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.gen == nil {
		d.gen = newDeadlineGen()
	}
	if d.t != nil {
		if !d.t.Stop() {
			// The timer has fired and its callback closes this generation's channel,
			// if it has not already: the old deadline did pass. Start a new one.
			d.gen = newDeadlineGen()
		}
		d.t = nil
	}
	if d.gen.fired() {
		d.gen = newDeadlineGen()
	}
	if t.IsZero() {
		return
	}
	g := d.gen
	if dur := time.Until(t); dur > 0 {
		d.t = time.AfterFunc(dur, g.fire)
		return
	}
	g.fire()
}

// wait returns the channel that is closed when the current deadline passes.
func (d *deadline) wait() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.gen == nil {
		d.gen = newDeadlineGen()
	}
	return d.gen.ch
}

// file is an overlapped handle (a pipe or a socket) associated with the completion
// port. Reads and writes are each serialized (rmu, wmu), so each direction reuses one
// ioOp. Every use of h happens between begin and end, and close releases h only after
// every such use has ended, so a handle value is never used after it was closed (and
// possibly reused by the OS).
type file struct {
	h      windows.Handle
	socket bool

	mu       sync.Mutex
	cond     sync.Cond // signaled when inflight drops to zero after close, or flushing changes
	inflight int
	flushing int // synchronous FlushFileBuffers calls in progress
	closed   bool
	closing  chan struct{} // closed by close

	rmu, wmu sync.Mutex
	rop, wop *ioOp
	rd, wd   deadline
}

// newFile associates h with the completion port. On error the caller still owns h.
func newFile(h windows.Handle, socket bool) (*file, error) {
	p, err := ioPort()
	if err != nil {
		return nil, err
	}
	if _, err := windows.CreateIoCompletionPort(h, p, 0, 0); err != nil {
		return nil, os.NewSyscallError("CreateIoCompletionPort", err)
	}
	// Synchronous completions are handled inline and must not also be queued: the
	// ioOp would be reused while a stale packet for it is still on its way.
	if err := windows.SetFileCompletionNotificationModes(h,
		windows.FILE_SKIP_COMPLETION_PORT_ON_SUCCESS|windows.FILE_SKIP_SET_EVENT_ON_HANDLE); err != nil {
		return nil, os.NewSyscallError("SetFileCompletionNotificationModes", err)
	}
	f := &file{h: h, socket: socket, closing: make(chan struct{}), rop: newIOOp(), wop: newIOOp()}
	f.cond.L = &f.mu
	return f, nil
}

// begin registers a use of h; it fails with net.ErrClosed once close has started.
func (f *file) begin() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return net.ErrClosed
	}
	f.inflight++
	return nil
}

func (f *file) end() {
	f.mu.Lock()
	f.inflight--
	if f.closed && f.inflight == 0 {
		f.cond.Broadcast()
	}
	f.mu.Unlock()
}

func (f *file) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// maxFlushCancel bounds how long close keeps canceling a FlushFileBuffers that does not
// return.
const maxFlushCancel = 5 * time.Second

// close cancels every pending operation, waits until all of them have returned, and
// closes h. It is idempotent; a second call returns at once.
func (f *file) close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	close(f.closing)
	f.mu.Unlock()

	// Every waiting operation also cancels its own I/O when it sees closing; this
	// covers the ones that have not started waiting yet.
	_ = windows.CancelIoEx(f.h, nil)

	var flushSince time.Time
	f.mu.Lock()
	for f.inflight > 0 {
		if f.flushing == 0 {
			f.cond.Wait()
			continue
		}
		// FlushFileBuffers blocks its thread rather than completing on the port, and
		// may enter the kernel just after a cancel. Keep canceling until it returns.
		if flushSince.IsZero() {
			flushSince = time.Now()
		} else if time.Since(flushSince) > maxFlushCancel && f.inflight == f.flushing {
			// Closing the handle is safe while a synchronous call is in it (the call
			// holds the file object), and nothing else is pending.
			break
		}
		f.mu.Unlock()
		_ = windows.CancelIoEx(f.h, nil)
		time.Sleep(time.Millisecond)
		f.mu.Lock()
	}
	f.mu.Unlock()
	if f.socket {
		return os.NewSyscallError("closesocket", windows.Closesocket(f.h))
	}
	return os.NewSyscallError("CloseHandle", windows.CloseHandle(f.h))
}

// flush calls FlushFileBuffers on h, which waits until the peer has read everything
// written to the pipe. close interrupts it. The caller is between begin and end.
func (f *file) flush() error {
	f.mu.Lock()
	f.flushing++
	f.cond.Broadcast()
	f.mu.Unlock()
	err := windows.FlushFileBuffers(f.h)
	f.mu.Lock()
	f.flushing--
	f.mu.Unlock()
	if f.isClosed() {
		return net.ErrClosed
	}
	return err
}

// testHookPending, if set (by tests only), runs between issuing an operation that went
// pending and waiting for it.
var testHookPending atomic.Pointer[func()]

// errCanceled is do's result when its cancel channel closed first.
var errCanceled = errors.New("winipc: operation canceled")

// do runs one overlapped operation on op. start issues it with the given OVERLAPPED
// and returns the syscall's byte count and error. do returns when the operation has
// completed, or after canceling it when the deadline d passes (os.ErrDeadlineExceeded),
// f closes (net.ErrClosed), or cancel closes (errCanceled). In every case the kernel is
// done with op when do returns. An operation that completes before its cancel takes
// effect returns its result, so no data is lost to a timeout. The caller is between
// begin and end.
func (f *file) do(op *ioOp, d *deadline, cancel <-chan struct{}, start func(o *windows.Overlapped) (uint32, error)) (uint32, error) {
	var expired <-chan struct{}
	if d != nil {
		expired = d.wait()
		select {
		case <-expired:
			return 0, os.ErrDeadlineExceeded
		default:
		}
	}
	op.o = windows.Overlapped{}
	n, err := start(&op.o)
	if err != windows.ERROR_IO_PENDING { //nolint:errorlint // x/sys returns the bare Errno
		// Done synchronously: no completion is queued, on success
		// (FILE_SKIP_COMPLETION_PORT_ON_SUCCESS) or on failure.
		return n, err
	}
	if h := testHookPending.Load(); h != nil {
		(*h)()
	}
	var why error
	select {
	case r := <-op.ch:
		if !f.aborted(r.err) {
			return f.result(op, r)
		}
		// Canceled by someone else: a Close that started before this select (its
		// CancelIoEx covers every operation on the handle), or, on hvsock, an abort
		// reported for the same reason. Report why, as the cancel paths below do.
		switch {
		case isDone(f.closing):
			return r.n, net.ErrClosed
		case isDone(expired):
			return r.n, os.ErrDeadlineExceeded
		case isDone(cancel):
			return r.n, errCanceled
		}
		return f.result(op, r)
	case <-f.closing:
		why = net.ErrClosed
	case <-expired:
		why = os.ErrDeadlineExceeded
	case <-cancel:
		why = errCanceled
	}
	_ = windows.CancelIoEx(f.h, &op.o)
	r := <-op.ch
	if f.aborted(r.err) {
		return r.n, why
	}
	return f.result(op, r)
}

// aborted reports whether a completion status means the operation was canceled. The
// hvsock provider completes a canceled receive with STATUS_CONNECTION_ABORTED (Winsock:
// WSAECONNABORTED) rather than the status of a canceled I/O, although the connection stays usable;
// it must not read as a broken connection. The raw status is tested, before result maps
// it to a Winsock code.
func (f *file) aborted(err error) bool {
	return errors.Is(err, windows.ERROR_OPERATION_ABORTED) ||
		(f.socket && errors.Is(err, windows.ERROR_CONNECTION_ABORTED))
}

// isDone reports whether ch is closed; a nil channel never is.
func isDone(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// result turns a completion into do's return values. A failed socket operation's
// status is a Win32 code mapped from NTSTATUS; WSAGetOverlappedResult gives the
// Winsock code that callers (and errors.Is) expect, such as WSAECONNREFUSED.
func (f *file) result(op *ioOp, r ioResult) (uint32, error) {
	if r.err == nil || !f.socket {
		return r.n, r.err
	}
	var n, flags uint32
	if err := windows.WSAGetOverlappedResult(f.h, &op.o, &n, false, &flags); err != nil {
		return n, err
	}
	return r.n, r.err
}
