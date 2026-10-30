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
)

// newShareRelay is the relay to the host's share server on AF_VSOCK port, mounted at
// mount with queues connections.
func newShareRelay(port uint32, mount string, queues int) *shareRelay {
	return &shareRelay{
		queues: queues, logf: log.Printf,
		dial: func() (io.ReadWriteCloser, error) { return dialHost(port) },
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

// dialTimeout bounds one connect to the host (hv_sock's own limit is about 2 s).
const dialTimeout = 3 * time.Second

// dialHost connects to the host (CID 2) on port. The connect is non-blocking (bounded
// by dialTimeout); the connection returned is blocking, as is /dev/fuse in the relay:
// every relay goroutine then waits in its own read(2) or write(2) and is woken by the
// kernel directly. Mixing poller-driven sockets with blocking /dev/fuse reads left
// replies waiting up to ~15 ms for the runtime's background poll (see relay.Relay).
func dialHost(port uint32) (io.ReadWriteCloser, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("socket(AF_VSOCK): %w", err)
	}
	if err := connectHost(fd, port); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return &blockingConn{File: os.NewFile(uintptr(fd), fmt.Sprintf("vsock:2:%d", port)), fd: fd}, nil
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

func connectHost(fd int, port uint32) error {
	err := unix.Connect(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_HOST, Port: port})
	if err == nil {
		return nil
	}
	if !errors.Is(err, unix.EINPROGRESS) && !errors.Is(err, unix.EINTR) {
		return err
	}
	deadline := time.Now().Add(dialTimeout)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return unix.ETIMEDOUT
		}
		pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
		n, err := unix.Poll(pfd, int(left.Milliseconds())+1)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			continue // the deadline check above ends it
		}
		soErr, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
		if err != nil {
			return err
		}
		if soErr != 0 {
			return unix.Errno(soErr)
		}
		return nil
	}
}
