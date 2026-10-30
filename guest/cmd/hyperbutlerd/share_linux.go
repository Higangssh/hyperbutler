//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
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

// dialHost connects to the host (CID 2) on port. The socket is non-blocking, so the
// returned file is driven by the runtime poller and Close interrupts its readers.
func dialHost(port uint32) (io.ReadWriteCloser, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("socket(AF_VSOCK): %w", err)
	}
	if err := connectHost(fd, port); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), fmt.Sprintf("vsock:2:%d", port)), nil
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
