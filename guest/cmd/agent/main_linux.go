// Command agent is the hyperbutler guest agent. For now it does one thing: it listens on an
// AF_VSOCK port and relays every connection from the host to dockerd's Unix socket, so
// the host can reach the Docker API over hvsock (HCS) without any guest network.
//
// The host side dials the hvsock service GUID that maps to the port (see
// internal/transport.VsockServiceID) and serves it on a named pipe. Connections from any
// peer other than the host (CID 2) are refused, so a vsock loopback transport cannot
// turn this into a path from containers to dockerd. A host connection must begin with
// the VM's secret (hyperbutler.secret on the kernel command line, ADR 0004 rule 6), or
// it is closed without a reply.
//
// Cross-compiled with GOOS=linux CGO_ENABLED=0 and started by the guest init:
//
//	agent -port 2375 -unix /run/docker.sock
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Higangssh/hyperbutler/internal/vmsecret"
)

const (
	defaultPort     = 2375
	defaultMaxConns = 256
	dialTimeout     = 5 * time.Second
	listenBacklog   = 128
)

func main() {
	port := flag.Uint("port", defaultPort, "AF_VSOCK port to listen on")
	sock := flag.String("unix", "/run/docker.sock", "Unix socket to relay to")
	maxConns := flag.Int("max-conns", defaultMaxConns, "maximum concurrent connections")
	cmdline := flag.String("cmdline", vmsecret.DefaultCmdlinePath, "kernel command line holding "+vmsecret.CmdlineKey)
	flag.Parse()
	if flag.NArg() != 0 || *port > 0xFFFFFFFE || *sock == "" || *maxConns < 1 {
		flag.Usage()
		os.Exit(2)
	}
	log.SetPrefix("hyperbutler-agent: ")
	log.SetFlags(0)

	// Without a usable secret the zero secret stays, which admits no one.
	secret, err := vmsecret.Load(*cmdline)
	if err != nil {
		log.Printf("%v: refusing every connection", err)
	}
	ln, err := listenVsock(uint32(*port))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("listening on vsock port %d, relaying to %s", *port, *sock)
	log.Fatal(serve(ln, *sock, *maxConns, secret))
}

// vsockListener is a non-blocking AF_VSOCK listening socket driven by the runtime poller.
type vsockListener struct {
	f *os.File
}

func listenVsock(port uint32) (*vsockListener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("socket(AF_VSOCK): %w (is hv_sock or vmw_vsock_virtio_transport loaded?)", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("bind vsock port %d: %w", port, err)
	}
	if err := unix.Listen(fd, listenBacklog); err != nil {
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
	var cid uint32
	if vm, ok := sa.(*unix.SockaddrVM); ok {
		cid = vm.CID
	} else {
		cid = unix.VMADDR_CID_ANY // unknown peer: never the host
	}
	return &vsockConn{os.NewFile(uintptr(nfd), "vsock")}, cid, nil
}

// vsockConn is one accepted AF_VSOCK stream.
type vsockConn struct {
	*os.File
}

// CloseWrite shuts down the send side; the host reads EOF.
func (c *vsockConn) CloseWrite() error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) { serr = unix.Shutdown(int(fd), unix.SHUT_WR) }); err != nil {
		return err
	}
	return serr
}

// serve accepts host connections and admits each through a bounded Gate, so a peer that
// connects and sends nothing holds up neither the accept loop nor the relays; at most
// maxConns admitted connections are relayed at once.
func serve(ln *vsockListener, sock string, maxConns int, secret vmsecret.Secret) error {
	sem := make(chan struct{}, maxConns)
	gate := &vmsecret.Gate{Secret: secret, MaxPending: maxPendingAdmits}
	var backoff time.Duration
	for {
		c, cid, err := ln.accept()
		if err != nil {
			if errors.Is(err, os.ErrClosed) {
				return err
			}
			// EMFILE, ECONNABORTED and the like: retry, with a capped backoff so a
			// persistent failure cannot spin the CPU.
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			log.Printf("accept: %v; retrying in %v", err, backoff)
			time.Sleep(backoff)
			continue
		}
		backoff = 0
		if cid != unix.VMADDR_CID_HOST {
			log.Printf("refused connection from CID %d", cid)
			_ = c.Close()
			continue
		}
		admitHost(gate, c, func(hc hostConn) {
			sem <- struct{}{}
			defer func() { <-sem }()
			handle(hc, sock)
		}, func(err error) { refusals.log("refused a connection without the VM secret", err) })
	}
}

// rateLimited logs at most once per interval.
type rateLimited struct {
	mu       sync.Mutex
	last     time.Time
	interval time.Duration
}

var (
	dialFailures = &rateLimited{interval: 5 * time.Second}
	refusals     = &rateLimited{interval: 5 * time.Second}
)

func (r *rateLimited) log(what string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now := time.Now(); now.Sub(r.last) >= r.interval {
		r.last = now
		log.Printf("%s: %v", what, err)
	}
}

// handle relays an admitted host connection to dockerd.
func handle(c hostConn, sock string) {
	err := relayHost(c, func() (stream, error) {
		u, err := net.DialTimeout("unix", sock, dialTimeout)
		if err != nil {
			return nil, err
		}
		return u.(*net.UnixConn), nil
	})
	var de dialError
	switch {
	case err == nil, errors.Is(err, os.ErrClosed), errors.Is(err, net.ErrClosed):
	case errors.As(err, &de):
		// Expected while dockerd is still starting (the host retries), so log at most
		// once every few seconds.
		dialFailures.log("dial dockerd", de.err)
	default:
		log.Printf("relay: %v", err)
	}
}
