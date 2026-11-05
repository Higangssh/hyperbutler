//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Higangssh/hyperbutler/internal/share/relay"
	"github.com/Higangssh/hyperbutler/internal/vmsecret"
)

// newShareRelay is the relay to the host's share server, which connects to AF_VSOCK
// port, mounted at mount with queues connections. Every connection must come from the
// host and begin with secret, the VM's (ADR 0004 rule 6): a peer that does not is
// closed before anything is written to it.
func newShareRelay(port uint32, mount string, queues int, secret vmsecret.Secret) *shareRelay {
	a := &shareAcceptor{port: port, secret: secret, fd: -1}
	return &shareRelay{
		queues: queues, logf: log.Printf,
		dial: a.accept,
		start: func(conns []io.ReadWriteCloser) (relayHandle, error) {
			r, err := relay.Start(relay.Config{Mountpoint: mount, Conns: conns, Logf: log.Printf})
			if err != nil {
				return nil, err
			}
			return r, nil
		},
		unmount:    func() { _ = unix.Unmount(mount, unix.MNT_DETACH) },
		minBackoff: 100 * time.Millisecond, maxBackoff: 5 * time.Second,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
}

// acceptWait bounds the wait for one connection from the host. The relay loop retries
// after it, so a host that has not connected yet (still booting, or bind mounts off)
// costs a log line, not a stuck mount.
const acceptWait = 5 * time.Second

// shareRingBuffer is the socket buffer size asked for on the listener, which hv_sock
// doubles and uses for the VMBus rings of the connections it accepts: 256 KiB, its
// maximum. A connection the guest dials gets 24 KiB rings whatever is asked, which held
// 1 MiB writes to about 270 MB/s (ADR 0006, "Ring size"; ADR 0007).
const shareRingBuffer = 128 << 10

// shareAcceptor hands the relay the host's connections to port, one per accept. The
// host dials, so the connections get the listener's large VMBus rings.
type shareAcceptor struct {
	port   uint32
	secret vmsecret.Secret

	mu sync.Mutex
	fd int // the listening socket, -1 until the first accept
}

// accept waits up to acceptWait for a connection from the host (CID 2) and checks that
// it sends secret first (vmsecret.Verify, within vmsecret.DefaultTimeout), before
// anything is written; a peer that is not the host, or does not send it, is closed and
// the wait goes on. The accept and the check are non-blocking; the connection returned
// is blocking, as is /dev/fuse in the relay: every relay goroutine then waits in its own
// read(2) or write(2) and is woken by the kernel directly. Mixing poller-driven sockets
// with blocking /dev/fuse reads left replies waiting up to ~15 ms for the runtime's
// background poll (see relay.Relay).
func (a *shareAcceptor) accept() (io.ReadWriteCloser, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fd < 0 {
		fd, err := listenShare(a.port)
		if err != nil {
			return nil, err
		}
		a.fd = fd
	}
	deadline := time.Now().Add(acceptWait)
	var refused error
	for {
		left := time.Until(deadline)
		if left <= 0 {
			if refused != nil {
				return nil, refused
			}
			return nil, fmt.Errorf("the host has not connected to vsock port %d", a.port)
		}
		pfd := []unix.PollFd{{Fd: int32(a.fd), Events: unix.POLLIN}}
		if _, err := unix.Poll(pfd, int(left.Milliseconds())+1); err != nil && !errors.Is(err, unix.EINTR) {
			return nil, err
		}
		fd, sa, err := unix.Accept4(a.fd, unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK)
		switch {
		case errors.Is(err, unix.EAGAIN), errors.Is(err, unix.EINTR), errors.Is(err, unix.ECONNABORTED):
			continue
		case err != nil:
			return nil, fmt.Errorf("accept on vsock port %d: %w", a.port, err)
		}
		if vm, ok := sa.(*unix.SockaddrVM); !ok || vm.CID != unix.VMADDR_CID_HOST {
			_ = unix.Close(fd)
			continue
		}
		if err := vmsecret.Verify(&pollReader{fd: fd}, a.secret); err != nil {
			_ = unix.Close(fd)
			refused = fmt.Errorf("refused a connection on vsock port %d that did not send the VM secret first: %w", a.port, err)
			continue
		}
		if err := unix.SetNonblock(fd, false); err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
		return &blockingConn{File: os.NewFile(uintptr(fd), fmt.Sprintf("vsock:2:%d", a.port)), fd: fd}, nil
	}
}

// listenShare listens on port for the host, with socket buffers that give accepted
// connections hv_sock's largest VMBus rings. SO_*BUFFORCE passes net.core.[rw]mem_max
// (hyperbutlerd runs as root); without it the rings are the default 208 KiB.
func listenShare(port uint32) (int, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return -1, fmt.Errorf("socket(AF_VSOCK): %w", err)
	}
	for _, opt := range []int{unix.SO_SNDBUFFORCE, unix.SO_RCVBUFFORCE} {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, opt, shareRingBuffer); err != nil {
			// Still works, with the default rings (about 208 KiB).
			log.Printf("shares: socket buffers of the listener on vsock port %d: %v", port, err)
		}
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("bind vsock port %d: %w", port, err)
	}
	if err := unix.Listen(fd, 8); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("listen vsock port %d: %w", port, err)
	}
	return fd, nil
}

// blockingConn is a blocking socket whose Close wakes goroutines blocked in it: close(2)
// alone does not interrupt a blocked read, and os.File defers the close until it
// returns, so Close shuts the socket down first.
type blockingConn struct {
	*os.File
	fd   int
	once sync.Once
}

func (c *blockingConn) Close() error {
	c.once.Do(func() { _ = unix.Shutdown(c.fd, unix.SHUT_RDWR) })
	return c.File.Close()
}

// pollReader reads a non-blocking socket within a deadline (SetReadDeadline), for the
// secret check before the connection turns blocking.
type pollReader struct {
	fd       int
	deadline time.Time
}

func (r *pollReader) SetReadDeadline(t time.Time) error {
	r.deadline = t
	return nil
}

func (r *pollReader) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	for {
		n, err := unix.Read(r.fd, b)
		switch {
		case err == nil && n == 0:
			return 0, io.EOF
		case err == nil:
			return n, nil
		case errors.Is(err, unix.EINTR):
			continue
		case !errors.Is(err, unix.EAGAIN):
			return 0, err
		}
		wait := -1
		if !r.deadline.IsZero() {
			left := time.Until(r.deadline)
			if left <= 0 {
				return 0, os.ErrDeadlineExceeded
			}
			wait = int(left.Milliseconds()) + 1
		}
		pfd := []unix.PollFd{{Fd: int32(r.fd), Events: unix.POLLIN}}
		if _, err := unix.Poll(pfd, wait); err != nil && !errors.Is(err, unix.EINTR) {
			return 0, err
		}
	}
}
