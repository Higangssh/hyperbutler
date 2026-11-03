//go:build windows

package winipc

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/guid"
)

const (
	afHyperV      = 34 // AF_HYPERV
	hvProtocolRaw = 1  // HV_PROTOCOL_RAW
)

// Well-known VM IDs (hvsocket.h).
var (
	// WildcardVMID (HV_GUID_WILDCARD) listens for connections from any partition.
	WildcardVMID = guid.GUID{}
	// LoopbackVMID (HV_GUID_LOOPBACK) is the local partition: a dial to it reaches a
	// listener on this host.
	LoopbackVMID = guid.GUID{Data1: 0xe0e16197, Data2: 0xdd56, Data3: 0x4a10, Data4: [8]byte{0x91, 0x95, 0x5e, 0xe7, 0xa1, 0x55, 0xa8, 0x38}}
)

// HvsockAddr is an AF_HYPERV address: a VM (partition) and a service in it.
type HvsockAddr struct {
	VMID      guid.GUID
	ServiceID guid.GUID
}

func (a HvsockAddr) Network() string { return "hvsock" }
func (a HvsockAddr) String() string  { return a.VMID.String() + ":" + a.ServiceID.String() }

// rawHvsockAddr is SOCKADDR_HV. guid.GUID has Windows' GUID layout.
type rawHvsockAddr struct {
	Family    uint16
	Reserved  uint16
	VMID      guid.GUID
	ServiceID guid.GUID
}

func (a HvsockAddr) raw() rawHvsockAddr {
	return rawHvsockAddr{Family: afHyperV, VMID: a.VMID, ServiceID: a.ServiceID}
}

func (r *rawHvsockAddr) addr() HvsockAddr {
	return HvsockAddr{VMID: r.VMID, ServiceID: r.ServiceID}
}

// rawHvsockAddrFromBytes reads a SOCKADDR_HV from b. It fails unless b holds one of the
// AF_HYPERV family.
func rawHvsockAddrFromBytes(b []byte) (HvsockAddr, error) {
	var r rawHvsockAddr
	if len(b) < int(unsafe.Sizeof(r)) {
		return HvsockAddr{}, errors.New("winipc: short hvsock address")
	}
	r = *(*rawHvsockAddr)(unsafe.Pointer(&b[0]))
	if r.Family != afHyperV {
		return HvsockAddr{}, errors.New("winipc: not an AF_HYPERV address")
	}
	return r.addr(), nil
}

// acceptAddrLen is the room AcceptEx needs per address: the address and 16 bytes.
const acceptAddrLen = uint32(unsafe.Sizeof(rawHvsockAddr{})) + 16

// sockScratch holds what the kernel reads or writes during an accept or connect.
type sockScratch struct {
	sa  rawHvsockAddr
	buf [2 * acceptAddrLen]byte
}

// newSockScratch allocates on the heap, for the same reason as newIOOp.
//
//go:noinline
func newSockScratch() *sockScratch { return new(sockScratch) }

var (
	modws2_32       = windows.NewLazySystemDLL("ws2_32.dll")
	procBind        = modws2_32.NewProc("bind")
	procGetsockname = modws2_32.NewProc("getsockname")

	wsaOnce sync.Once
	wsaErr  error

	connectExMu   sync.Mutex
	connectExAddr uintptr
)

func wsaStartup() error {
	wsaOnce.Do(func() {
		var d windows.WSAData
		if err := windows.WSAStartup(0x0202, &d); err != nil {
			wsaErr = os.NewSyscallError("WSAStartup", err)
		}
	})
	return wsaErr
}

func bind(s windows.Handle, sa *rawHvsockAddr) error {
	r, _, err := procBind.Call(uintptr(s), uintptr(unsafe.Pointer(sa)), unsafe.Sizeof(*sa))
	if r != 0 { // 0, or SOCKET_ERROR
		return os.NewSyscallError("bind", err)
	}
	return nil
}

func getsockname(s windows.Handle) (HvsockAddr, error) {
	var sa rawHvsockAddr
	n := int32(unsafe.Sizeof(sa))
	r, _, err := procGetsockname.Call(uintptr(s), uintptr(unsafe.Pointer(&sa)), uintptr(unsafe.Pointer(&n)))
	if r != 0 { // 0, or SOCKET_ERROR
		return HvsockAddr{}, os.NewSyscallError("getsockname", err)
	}
	return sa.addr(), nil
}

// connectEx returns the AF_HYPERV provider's ConnectEx, loaded on first use.
func connectEx(s windows.Handle) (uintptr, error) {
	connectExMu.Lock()
	defer connectExMu.Unlock()
	if connectExAddr != 0 {
		return connectExAddr, nil
	}
	var fn uintptr
	var n uint32
	if err := windows.WSAIoctl(s, windows.SIO_GET_EXTENSION_FUNCTION_POINTER,
		(*byte)(unsafe.Pointer(&windows.WSAID_CONNECTEX)), uint32(unsafe.Sizeof(windows.WSAID_CONNECTEX)),
		(*byte)(unsafe.Pointer(&fn)), uint32(unsafe.Sizeof(fn)), &n, nil, 0); err != nil {
		return 0, os.NewSyscallError("WSAIoctl", err)
	}
	connectExAddr = fn
	return fn, nil
}

// newHvsocket creates an overlapped AF_HYPERV stream socket on the completion port.
func newHvsocket() (*file, error) {
	if err := wsaStartup(); err != nil {
		return nil, err
	}
	h, err := windows.WSASocket(afHyperV, windows.SOCK_STREAM, hvProtocolRaw, nil, 0,
		windows.WSA_FLAG_OVERLAPPED|windows.WSA_FLAG_NO_HANDLE_INHERIT)
	if err != nil {
		return nil, os.NewSyscallError("socket", err)
	}
	f, err := newFile(h, true)
	if err != nil {
		_ = windows.Closesocket(h)
		return nil, err
	}
	return f, nil
}

// HvsockListener accepts AF_HYPERV connections.
type HvsockListener struct {
	f    *file
	addr HvsockAddr
}

// ListenHvsock listens on addr. A VMID of WildcardVMID accepts from any partition.
func ListenHvsock(addr HvsockAddr) (net.Listener, error) {
	l, err := listenHvsock(addr)
	if err != nil {
		return nil, &net.OpError{Op: "listen", Net: "hvsock", Addr: addr, Err: err}
	}
	return l, nil
}

func listenHvsock(addr HvsockAddr) (*HvsockListener, error) {
	f, err := newHvsocket()
	if err != nil {
		return nil, err
	}
	sa := addr.raw()
	if err := bind(f.h, &sa); err != nil {
		_ = f.close()
		return nil, err
	}
	if err := windows.Listen(f.h, windows.SOMAXCONN); err != nil {
		_ = f.close()
		return nil, os.NewSyscallError("listen", err)
	}
	return &HvsockListener{f: f, addr: addr}, nil
}

// Accept waits for a connection and returns it, an *HvsockConn. After Close it returns
// an error that matches net.ErrClosed.
func (l *HvsockListener) Accept() (net.Conn, error) {
	c, err := l.accept()
	if err != nil {
		return nil, &net.OpError{Op: "accept", Net: "hvsock", Addr: l.addr, Err: err}
	}
	return c, nil
}

func (l *HvsockListener) accept() (*HvsockConn, error) {
	f := l.f
	f.rmu.Lock()
	defer f.rmu.Unlock()
	if err := f.begin(); err != nil {
		return nil, err
	}
	defer f.end()
	s, err := newHvsocket()
	if err != nil {
		return nil, err
	}
	sc := newSockScratch()
	_, err = f.do(f.rop, nil, nil, func(o *windows.Overlapped) (uint32, error) {
		var n uint32
		err := windows.AcceptEx(f.h, s.h, &sc.buf[0], 0, acceptAddrLen, acceptAddrLen, &n, o)
		return n, err
	})
	if err != nil {
		_ = s.close()
		return nil, wrapSyscall("AcceptEx", err)
	}
	// Give the accepted socket the listener's properties, so shutdown and getpeername
	// work on it.
	lh := f.h
	if err := windows.Setsockopt(s.h, windows.SOL_SOCKET, windows.SO_UPDATE_ACCEPT_CONTEXT,
		(*byte)(unsafe.Pointer(&lh)), int32(unsafe.Sizeof(lh))); err != nil {
		_ = s.close()
		return nil, os.NewSyscallError("setsockopt", err)
	}
	var lsa, rsa *windows.RawSockaddrAny
	var llen, rlen int32
	windows.GetAcceptExSockaddrs(&sc.buf[0], 0, acceptAddrLen, acceptAddrLen, &lsa, &llen, &rsa, &rlen)
	c := &HvsockConn{f: s, local: l.addr}
	if lsa != nil && int(llen) >= int(unsafe.Sizeof(rawHvsockAddr{})) {
		c.local = (*rawHvsockAddr)(unsafe.Pointer(lsa)).addr()
	}
	if rsa != nil && int(rlen) >= int(unsafe.Sizeof(rawHvsockAddr{})) {
		c.remote = (*rawHvsockAddr)(unsafe.Pointer(rsa)).addr()
	}
	return c, nil
}

// Close stops listening; a pending Accept returns net.ErrClosed. Accepted connections
// stay open.
func (l *HvsockListener) Close() error { return l.f.close() }

// Addr returns the address the listener was bound to.
func (l *HvsockListener) Addr() net.Addr { return l.addr }

// HvsockDialer dials AF_HYPERV addresses. Its zero value makes one attempt. It holds no
// state between dials, so one dialer may be shared between goroutines.
type HvsockDialer struct {
	// Retries is how many more attempts to make while the connect is refused, times
	// out or finds the VM unreachable (a VM that is still booting).
	Retries uint
	// RetryWait is the pause between attempts.
	RetryWait time.Duration
}

// DialHvsock connects to addr once.
func DialHvsock(ctx context.Context, addr HvsockAddr) (net.Conn, error) {
	return (&HvsockDialer{}).Dial(ctx, addr)
}

// Dial connects to addr. ctx bounds the whole dial, including a connect that is pending
// on a service nobody listens on yet; when it ends, Dial returns an error that matches
// ctx.Err().
func (d *HvsockDialer) Dial(ctx context.Context, addr HvsockAddr) (net.Conn, error) {
	for i := uint(0); ; i++ {
		c, err := dialHvsock(ctx, addr)
		if err == nil {
			return c, nil
		}
		if i >= d.Retries || !canRedial(err) {
			return nil, &net.OpError{Op: "dial", Net: "hvsock", Addr: addr, Err: err}
		}
		t := time.NewTimer(d.RetryWait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, &net.OpError{Op: "dial", Net: "hvsock", Addr: addr, Err: ctx.Err()}
		case <-t.C:
		}
	}
}

// canRedial reports whether a failed connect may succeed if tried again.
func canRedial(err error) bool {
	for _, e := range []windows.Errno{
		windows.WSAECONNREFUSED, windows.WSAENETUNREACH, windows.WSAEHOSTUNREACH, windows.WSAETIMEDOUT,
		windows.ERROR_CONNECTION_REFUSED, windows.ERROR_CONNECTION_UNAVAIL, windows.ERROR_SEM_TIMEOUT,
	} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

func dialHvsock(ctx context.Context, addr HvsockAddr) (*HvsockConn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := newHvsocket()
	if err != nil {
		return nil, err
	}
	c, err := connectHvsock(ctx, f, addr)
	if err != nil {
		_ = f.close()
		return nil, err
	}
	return c, nil
}

func connectHvsock(ctx context.Context, f *file, addr HvsockAddr) (*HvsockConn, error) {
	sc := newSockScratch()
	sc.sa = addr.raw()
	// ConnectEx needs a bound socket. Binding to the target address is what go-winio
	// has always done, and the hvsock provider accepts it.
	if err := bind(f.h, &sc.sa); err != nil {
		return nil, err
	}
	fn, err := connectEx(f.h)
	if err != nil {
		return nil, err
	}
	if err := f.begin(); err != nil {
		return nil, err
	}
	f.wmu.Lock()
	_, err = f.do(f.wop, nil, ctx.Done(), func(o *windows.Overlapped) (uint32, error) {
		r, _, e := syscall.SyscallN(fn, uintptr(f.h), uintptr(unsafe.Pointer(&sc.sa)), unsafe.Sizeof(sc.sa),
			0, 0, 0, uintptr(unsafe.Pointer(o)))
		if r != 0 {
			return 0, nil
		}
		if e == 0 {
			return 0, windows.WSAEINVAL
		}
		return 0, e
	})
	// The kernel may read sc until ConnectEx completes; do has waited for that.
	runtime.KeepAlive(sc)
	f.wmu.Unlock()
	f.end()
	if errors.Is(err, errCanceled) {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, wrapSyscall("ConnectEx", err)
	}
	// Give the socket its connected state, so shutdown and getsockname work.
	if err := windows.Setsockopt(f.h, windows.SOL_SOCKET, windows.SO_UPDATE_CONNECT_CONTEXT, nil, 0); err != nil {
		return nil, os.NewSyscallError("setsockopt", err)
	}
	local, err := getsockname(f.h)
	if err != nil {
		return nil, err
	}
	return &HvsockConn{f: f, local: local, remote: addr}, nil
}

// HvsockConn is a connected AF_HYPERV socket. CloseWrite and CloseRead shut down one
// direction (shutdown(2)); after the peer's CloseWrite, Read returns io.EOF.
type HvsockConn struct {
	f             *file
	local, remote HvsockAddr
}

func (c *HvsockConn) opErr(op string, err error) error {
	return &net.OpError{Op: op, Net: "hvsock", Source: c.local, Addr: c.remote, Err: err}
}

// maxIO caps one WSARecv or WSASend, whose length is 32 bits.
const maxIO = 1 << 30

func (c *HvsockConn) Read(b []byte) (int, error) {
	f := c.f
	f.rmu.Lock()
	defer f.rmu.Unlock()
	if err := f.begin(); err != nil {
		return 0, c.opErr("read", err)
	}
	defer f.end()
	if len(b) == 0 {
		return 0, nil
	}
	if len(b) > maxIO {
		b = b[:maxIO]
	}
	op := f.rop
	op.buf = b
	n, err := f.do(op, &f.rd, nil, func(o *windows.Overlapped) (uint32, error) {
		var n, flags uint32
		wb := windows.WSABuf{Len: ioLen(len(b)), Buf: &b[0]}
		err := windows.WSARecv(f.h, &wb, 1, &n, &flags, o, nil)
		return n, err
	})
	op.buf = nil
	if err != nil {
		return int(n), c.opErr("read", wrapSyscall("WSARecv", err))
	}
	if n == 0 {
		return 0, io.EOF
	}
	return int(n), nil
}

func (c *HvsockConn) Write(b []byte) (int, error) {
	f := c.f
	f.wmu.Lock()
	defer f.wmu.Unlock()
	if err := f.begin(); err != nil {
		return 0, c.opErr("write", err)
	}
	defer f.end()
	total := 0
	for len(b) > 0 {
		chunk := b
		if len(chunk) > maxIO {
			chunk = chunk[:maxIO]
		}
		op := f.wop
		op.buf = chunk
		n, err := f.do(op, &f.wd, nil, func(o *windows.Overlapped) (uint32, error) {
			var n uint32
			wb := windows.WSABuf{Len: ioLen(len(chunk)), Buf: &chunk[0]}
			err := windows.WSASend(f.h, &wb, 1, &n, 0, o, nil)
			return n, err
		})
		op.buf = nil
		total += int(n)
		if err != nil {
			return total, c.opErr("write", wrapSyscall("WSASend", err))
		}
		if n == 0 {
			return total, c.opErr("write", io.ErrShortWrite)
		}
		b = b[n:]
	}
	return total, nil
}

func (c *HvsockConn) shutdown(how int) error {
	if err := c.f.begin(); err != nil {
		return c.opErr("close", err)
	}
	defer c.f.end()
	if err := windows.Shutdown(c.f.h, how); err != nil {
		return c.opErr("close", os.NewSyscallError("shutdown", err))
	}
	return nil
}

// CloseWrite shuts down the sending side; the peer reads EOF after the data already
// sent.
func (c *HvsockConn) CloseWrite() error { return c.shutdown(windows.SHUT_WR) }

// CloseRead shuts down the receiving side.
func (c *HvsockConn) CloseRead() error { return c.shutdown(windows.SHUT_RD) }

// Close cancels any Read or Write in progress (they return net.ErrClosed) and closes
// the socket.
func (c *HvsockConn) Close() error {
	if err := c.f.close(); err != nil {
		return c.opErr("close", err)
	}
	return nil
}

func (c *HvsockConn) LocalAddr() net.Addr  { return c.local }
func (c *HvsockConn) RemoteAddr() net.Addr { return c.remote }

func (c *HvsockConn) SetDeadline(t time.Time) error {
	c.f.rd.set(t)
	c.f.wd.set(t)
	return nil
}

func (c *HvsockConn) SetReadDeadline(t time.Time) error  { c.f.rd.set(t); return nil }
func (c *HvsockConn) SetWriteDeadline(t time.Time) error { c.f.wd.set(t); return nil }

var (
	_ net.Conn     = (*HvsockConn)(nil)
	_ net.Listener = (*HvsockListener)(nil)
)

// ioLen is n as a WSABuf length: callers pass at most one buffer of their own, and a
// length past what a uint32 holds is cut to the largest one (the call then reads or
// writes less, which Read and Write report).
func ioLen(n int) uint32 {
	if n > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(n) //nolint:gosec // G115: bounded above
}
