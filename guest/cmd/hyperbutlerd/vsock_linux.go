//go:build linux

package main

import (
	"errors"
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// vsockListener is a non-blocking AF_VSOCK listening socket driven by the runtime poller,
// as in guest/cmd/agent.
type vsockListener struct {
	f *os.File
}

func listenVsock(port uint32) (*vsockListener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("socket(AF_VSOCK): %w (is hv_sock loaded?)", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("bind vsock port %d: %w", port, err)
	}
	if err := unix.Listen(fd, 4); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("listen vsock port %d: %w", port, err)
	}
	return &vsockListener{f: os.NewFile(uintptr(fd), "vsock-listener")}, nil
}

// accept returns the next connection and the peer's CID.
func (l *vsockListener) accept() (*vsockConn, uint32, error) {
	rc, err := l.f.SyscallConn()
	if err != nil {
		return nil, 0, err
	}
	var (
		nfd  int
		sa   unix.Sockaddr
		aerr error
	)
	err = rc.Read(func(fd uintptr) bool {
		nfd, sa, aerr = unix.Accept4(int(fd), unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK)
		return !errors.Is(aerr, unix.EAGAIN)
	})
	if err != nil {
		return nil, 0, err
	}
	if aerr != nil {
		return nil, 0, aerr
	}
	cid := uint32(unix.VMADDR_CID_ANY) // unknown peer: never the host
	if vm, ok := sa.(*unix.SockaddrVM); ok {
		cid = vm.CID
	}
	return &vsockConn{File: os.NewFile(uintptr(nfd), "vsock")}, cid, nil
}

// vsockConn adapts an AF_VSOCK stream to net.Conn for guestctl.Server. os.File supports
// deadlines for pollable descriptors.
type vsockConn struct {
	*os.File
}

var _ net.Conn = (*vsockConn)(nil)

type vsockAddr struct{}

func (vsockAddr) Network() string { return "vsock" }
func (vsockAddr) String() string  { return "vsock" }

func (c *vsockConn) LocalAddr() net.Addr  { return vsockAddr{} }
func (c *vsockConn) RemoteAddr() net.Addr { return vsockAddr{} }
