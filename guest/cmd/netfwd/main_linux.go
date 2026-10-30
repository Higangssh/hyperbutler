// Command netfwd connects the guest to hyperbutler's user-mode network (ADR 0006). It creates
// a TAP device and relays its Ethernet frames, length-prefixed (internal/netstack/frame),
// over one AF_VSOCK stream to the host, where internal/netstack terminates them.
//
// The guest init configures the device's address, route and resolver. netfwd keeps the
// TAP open across reconnects, so the configuration survives a host-side restart.
//
//	netfwd -port 1024 -dev eth0 -mtu 1500
//
// With -bench-tcp and -bench-vsock it also serves an endless stream of zeros on a TCP
// port and an AF_VSOCK port, for comparing the stack with raw hvsock (hyperbutler-dev -bench).
// Development only: init enables it only with hyperbutler.netbench=1 on the command line.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Higangssh/hyperbutler/internal/netstack/frame"
)

func main() {
	port := flag.Uint("port", 1024, "host AF_VSOCK port of the network stack")
	dev := flag.String("dev", "eth0", "TAP device name")
	mtu := flag.Int("mtu", 1500, "link MTU; must match the host")
	benchTCP := flag.Int("bench-tcp", 0, "serve zeros on this TCP port (development)")
	benchVsock := flag.Uint("bench-vsock", 0, "serve zeros on this AF_VSOCK port to the host (development)")
	flag.Parse()
	if flag.NArg() != 0 || *port == 0 || *port > 0xFFFFFFFE || !frame.ValidMTU(*mtu) ||
		len(*dev) == 0 || len(*dev) >= unix.IFNAMSIZ {
		flag.Usage()
		os.Exit(2)
	}
	log.SetPrefix("hyperbutler-netfwd: ")
	log.SetFlags(0)

	tap, err := openTAP(*dev)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("%s ready, relaying to host vsock port %d (MTU %d)", *dev, *port, *mtu)
	if *benchTCP > 0 {
		go benchServeTCP(*benchTCP)
	}
	if *benchVsock > 0 {
		go benchServeVsock(uint32(*benchVsock))
	}

	// Frames the TAP produces while the host is away are dropped by the kernel once its
	// queue fills; TCP recovers them.
	wait := 50 * time.Millisecond
	for {
		conn, err := dialHost(uint32(*port))
		if err != nil {
			time.Sleep(wait)
			if wait < 2*time.Second {
				wait *= 2
			}
			continue
		}
		wait = 50 * time.Millisecond
		err = relay(tap, conn, *mtu)
		_ = conn.Close()
		log.Printf("link to host lost: %v; reconnecting", err)
	}
}

// openTAP creates (or attaches to) TAP device name, without packet information headers.
func openTAP(name string) (*os.File, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/net/tun: %w (is the tun module loaded?)", err)
	}
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("TUNSETIFF %s: %w", name, err)
	}
	// Keep the device (and the address and routes init gave it) if netfwd exits; the
	// restarted netfwd attaches to it again.
	if err := unix.IoctlSetInt(fd, unix.TUNSETPERSIST, 1); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("TUNSETPERSIST %s: %w", name, err)
	}
	return os.NewFile(uintptr(fd), "/dev/net/tun"), nil
}

// dialHost connects to the host (CID 2) on an AF_VSOCK port.
func dialHost(port uint32) (*os.File, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("socket(AF_VSOCK): %w", err)
	}
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_HOST, Port: port}); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "vsock"), nil
}

// relay copies frames both ways until either side fails.
func relay(tap *os.File, conn *os.File, mtu int) error {
	rc, err := tap.SyscallConn()
	if err != nil {
		return err
	}
	errc := make(chan error, 2)
	go func() { errc <- tapToHost(tapBatchReader(rc, mtu), conn, mtu) }()
	go func() { errc <- hostToTap(conn, tap, mtu) }()
	err = <-errc
	// Unblock the other direction. The TAP read side is woken by its own poller; the
	// vsock is closed by the caller, but a pending TAP read needs a deadline.
	_ = conn.SetDeadline(time.Now())
	_ = tap.SetReadDeadline(time.Now())
	<-errc
	_ = tap.SetReadDeadline(time.Time{})
	return err
}

// batchBytes bounds how many framed bytes go to the host in one write.
const batchBytes = 512 << 10

// readBatchFunc fills buf (from length 0) with framed frames and returns it.
type readBatchFunc func(buf []byte) ([]byte, error)

// tapBatchReader reads every frame the TAP has queued, up to the buffer's capacity, so
// that one vsock write carries many frames. It blocks (in the runtime poller) only when
// nothing is queued.
func tapBatchReader(rc syscall.RawConn, mtu int) readBatchFunc {
	slot := frame.HeaderLen + frame.MaxFrameLen(mtu)
	return func(buf []byte) ([]byte, error) {
		out := buf[:0]
		var rerr error
		err := rc.Read(func(fd uintptr) bool {
			for cap(out)-len(out) >= slot {
				start := len(out)
				n, err := unix.Read(int(fd), out[start+frame.HeaderLen:start+slot])
				if err != nil {
					if errors.Is(err, unix.EINTR) {
						continue
					}
					if errors.Is(err, unix.EAGAIN) {
						return len(out) > 0 // false: wait until readable
					}
					rerr = err
					return true
				}
				if n < frame.EthernetHeaderLen {
					continue
				}
				out = out[:start+frame.HeaderLen+n]
				frame.PutHeader(out[start:], n)
			}
			return true
		})
		if err != nil {
			return out, err
		}
		return out, rerr
	}
}

func tapToHost(read readBatchFunc, conn io.Writer, mtu int) error {
	buf := make([]byte, 0, batchBytes+frame.HeaderLen+frame.MaxFrameLen(mtu))
	for {
		b, err := read(buf)
		if len(b) > 0 {
			if _, werr := conn.Write(b); werr != nil {
				return fmt.Errorf("vsock write: %w", werr)
			}
		}
		if err != nil {
			return fmt.Errorf("tap read: %w", err)
		}
	}
}

func hostToTap(conn io.Reader, tap io.Writer, mtu int) error {
	fr, err := frame.NewReader(newBufReader(conn), mtu)
	if err != nil {
		return err
	}
	for {
		b, err := fr.Next()
		if err != nil {
			return fmt.Errorf("vsock read: %w", err)
		}
		if _, err := tap.Write(b); err != nil {
			// A frame the kernel refuses (for example while the device is down) is dropped.
			if errors.Is(err, unix.EIO) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EAGAIN) {
				continue
			}
			return fmt.Errorf("tap write: %w", err)
		}
	}
}

// --- development benchmarks ---------------------------------------------------------

var zeros = make([]byte, 256<<10)

func serveZeros(c io.WriteCloser) {
	defer c.Close()
	for {
		if _, err := c.Write(zeros); err != nil {
			return
		}
	}
}

// sink reads and discards until EOF, then writes one byte so the host knows every byte
// arrived. Port+2 of each bench port is a sink.
func sink(c io.ReadWriteCloser) {
	defer c.Close()
	_, _ = io.Copy(io.Discard, c)
	_, _ = c.Write([]byte{1})
}

// echo returns every byte it reads, for round-trip latency (port+4 of each bench port).
func echo(c io.ReadWriteCloser) {
	defer c.Close()
	_, _ = io.Copy(c, c)
}

func benchServe(c io.ReadWriteCloser, kind int) {
	switch kind {
	case 0:
		serveZeros(c)
	case 2:
		sink(c)
	default:
		echo(c)
	}
}

func benchServeTCP(port int) {
	for _, p := range []int{port, port + 2, port + 4} {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err != nil {
			log.Printf("bench tcp: %v", err)
			return
		}
		kind := p - port
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				if tc, ok := c.(*net.TCPConn); ok {
					_ = tc.SetNoDelay(true)
				}
				go benchServe(c, kind)
			}
		}()
	}
	log.Printf("bench: tcp source %d, sink %d, echo %d", port, port+2, port+4)
}

func benchServeVsock(port uint32) {
	for _, p := range []uint32{port, port + 2, port + 4} {
		fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err == nil {
			err = unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: p})
		}
		if err == nil {
			err = unix.Listen(fd, 8)
		}
		if err != nil {
			log.Printf("bench vsock: %v", err)
			return
		}
		kind := int(p - port)
		go func() {
			for {
				nfd, sa, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
				if err != nil {
					return
				}
				if vm, ok := sa.(*unix.SockaddrVM); !ok || vm.CID != unix.VMADDR_CID_HOST {
					_ = unix.Close(nfd)
					continue
				}
				go benchServe(os.NewFile(uintptr(nfd), "vsock-bench"), kind)
			}
		}()
	}
	log.Printf("bench: vsock source %d, sink %d, echo %d", port, port+2, port+4)
}
