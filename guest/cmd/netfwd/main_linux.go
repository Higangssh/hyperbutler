// Command netfwd connects the guest to hyperbutler's user-mode network (ADR 0006). It creates
// a TAP device and relays its Ethernet frames, length-prefixed (internal/netstack/frame),
// over one AF_VSOCK stream to the host, where internal/netstack terminates them.
//
// The guest init configures the device's address, route and resolver. netfwd keeps the
// TAP open across reconnects, so the configuration survives a host-side restart.
//
//	netfwd -port 1024 -dev eth0 -mtu 1500 [-vnet]
//
// Either way the link begins with the VM secret (ADR 0004 rule 6, read from -cmdline):
// without -vnet netfwd dials the host and checks that the listener sends it first,
// before netfwd sends a frame, so a listener of another host user (on a wildcard VM id)
// never carries the guest's network; a listener that does not send it is closed and
// redialled with a backoff. With -vnet the host dials and sends it (see acceptLoop).
//
// With -vnet (the host asks for it with hyperbutler.netvnet=1) the TAP has a
// virtio_net_hdr and TCP segmentation offload: the kernel hands netfwd TCP segments of
// up to 64 KiB with a partial checksum and takes the same from the host, so a bulk
// transfer costs one frame per 64 KiB on both sides instead of one per MSS. The stream
// then starts with frame.Hello (see package frame).
//
// In offload mode every frame carries a CRC-32C both ways (link version 2): netfwd
// computes it right after the TAP read and checks it before the TAP write, drops a
// frame that fails it (TCP retransmits it), logs the first few with their size, and
// reports its count to the host, whose counters show it (netstack.Stats). -crc 0
// (hyperbutler.netcrc=0) expects version 1, without the CRC; -crc strict
// (hyperbutler.netcrc=strict) ends the link at a mismatch instead, for tests and
// benches. The host's Hello must name the version netfwd expects, or netfwd refuses
// the link: engine and guest image come together, so the two disagree only in a
// broken install, and running on without the CRC would hide that.
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
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Higangssh/hyperbutler/internal/netstack/frame"
	"github.com/Higangssh/hyperbutler/internal/vmsecret"
)

func main() {
	port := flag.Uint("port", 1024, "host AF_VSOCK port of the network stack")
	dev := flag.String("dev", "eth0", "TAP device name")
	mtu := flag.Int("mtu", 1500, "link MTU; must match the host")
	vnet := flag.Bool("vnet", false, "virtio_net_hdr framing with TCP segmentation offload (the host must support it)")
	crc := flag.String("crc", "1", `with -vnet, the per-frame CRC-32C: "1", "0" (link version 1) or "strict" (a mismatch ends the link); must match the host`)
	benchTCP := flag.Int("bench-tcp", 0, "serve zeros on this TCP port (development)")
	benchVsock := flag.Uint("bench-vsock", 0, "serve zeros on this AF_VSOCK port to the host (development)")
	cmdline := flag.String("cmdline", vmsecret.DefaultCmdlinePath, "the kernel command line holding "+vmsecret.CmdlineKey+", the VM secret the host's link must begin with")
	flag.Parse()
	if flag.NArg() != 0 || *port == 0 || *port > 0xFFFFFFFE || !frame.ValidMTU(*mtu) ||
		len(*dev) == 0 || len(*dev) >= unix.IFNAMSIZ || (*crc != "1" && *crc != "0" && *crc != "strict") {
		flag.Usage()
		os.Exit(2)
	}
	log.SetPrefix("hyperbutler-netfwd: ")
	log.SetFlags(0)

	tap, err := openTAP(*dev, *vnet)
	if err != nil {
		log.Fatal(err)
	}
	lk := linkFormat{mtu: *mtu, vnet: *vnet, crc: *vnet && *crc != "0", strict: *crc == "strict", st: &linkStats{}}
	if lk.vnet {
		log.Printf("%s ready, relaying to host vsock port %d (MTU %d, offload, link version %d)", *dev, *port, *mtu, lk.version())
	} else {
		log.Printf("%s ready, relaying to host vsock port %d (MTU %d)", *dev, *port, *mtu)
	}
	if *benchTCP > 0 {
		go benchServeTCP(*benchTCP)
	}
	if *benchVsock > 0 {
		go benchServeVsock(uint32(*benchVsock))
	}

	// The host's link begins with the VM secret (ADR 0004 rule 6); without one on the
	// command line the zero secret stays, which matches no host.
	secret, err := vmsecret.Load(*cmdline)
	if err != nil {
		log.Printf("%v: refusing every link", err)
	}
	// Frames the TAP produces while the host is away are dropped by the kernel once its
	// queue fills; TCP recovers them.
	next := dialLoop(uint32(*port), secret)
	if lk.vnet {
		// Retry rather than exit: exiting would only restart netfwd, which would
		// reopen the TAP. While another process holds the port the link is down.
		ln, err := listenHost(uint32(*port))
		for wait := 50 * time.Millisecond; err != nil; ln, err = listenHost(uint32(*port)) {
			log.Printf("%v; retrying", err)
			time.Sleep(wait)
			wait = min(2*wait, 2*time.Second)
		}
		next = acceptLoop(ln, lk.version(), secret)
	}
	for {
		conn := next()
		err := relay(tap, conn, lk)
		_ = conn.Close()
		log.Printf("link to host lost: %v; reconnecting", err)
	}
}

// dialLoop returns a function that connects to the host (plain mode), retrying with
// backoff until it succeeds. The listener must send secret first (vmsecret.Verify): one
// that does not is not this VM's host (another host user may listen on a wildcard VM id
// while the engine is away) and is closed before a byte is written to it; the backoff
// keeps such a listener from making netfwd spin.
func dialLoop(port uint32, secret vmsecret.Secret) func() *os.File {
	return func() *os.File {
		wait := 50 * time.Millisecond
		logged := false
		for {
			conn, err := dialHost(port)
			if err == nil {
				if err = vmsecret.Verify(conn, secret); err == nil {
					return conn
				}
				_ = conn.Close()
				if !logged {
					log.Printf("refused a host link that did not begin with the VM secret: %v", err)
					logged = true
				}
			}
			time.Sleep(wait)
			wait = min(2*wait, 2*time.Second)
		}
	}
}

// acceptLoop returns a function that waits for the host to connect (offload mode) and
// to send the VM secret and then the Hello of link version. Only the host (CID 2) is
// accepted, and only with the secret; a peer without it is closed before anything is
// written to it. Connections are checked concurrently, a bounded number at a time
// (vmsecret.Gate), so a peer that connects and stalls does not delay the host's link.
// The host dials because Windows sends slowly on hvsock connections it accepted
// (ADR 0006).
func acceptLoop(ln int, version byte, secret vmsecret.Secret) func() *os.File {
	links := make(chan *os.File)
	gate := &vmsecret.Gate{Secret: secret, MaxPending: 4}
	go func() {
		for {
			conn, err := acceptHost(ln)
			if err != nil {
				log.Printf("accept: %v", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			gate.Go(conn, func(err error) {
				if err != nil {
					log.Printf("host link: refused: %v", err)
					return
				}
				if err := readHello(conn, version); err != nil {
					log.Printf("host link: %v", err)
					_ = conn.Close()
					return
				}
				links <- conn
			})
		}
	}()
	return func() *os.File { return <-links }
}

// listenHost listens on an AF_VSOCK port for the host.
func listenHost(port uint32) (int, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("socket(AF_VSOCK): %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("bind vsock port %d: %w", port, err)
	}
	if err := unix.Listen(fd, 1); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// acceptHost accepts one connection from the host (CID 2) and refuses any other peer.
func acceptHost(ln int) (*os.File, error) {
	for {
		nfd, sa, err := unix.Accept4(ln, unix.SOCK_CLOEXEC)
		if err != nil {
			if errors.Is(err, unix.EINTR) || errors.Is(err, unix.ECONNABORTED) {
				continue
			}
			return nil, err
		}
		if vm, ok := sa.(*unix.SockaddrVM); !ok || vm.CID != unix.VMADDR_CID_HOST {
			_ = unix.Close(nfd)
			continue
		}
		if err := unix.SetNonblock(nfd, true); err != nil {
			_ = unix.Close(nfd)
			return nil, err
		}
		return os.NewFile(uintptr(nfd), "vsock"), nil
	}
}

// readHello reads the host's Hello, which opens an offload-mode link, and checks that
// it is of link version.
func readHello(conn io.Reader, version byte) error {
	var b [frame.HeaderLen]byte
	if f, ok := conn.(*os.File); ok {
		_ = f.SetReadDeadline(time.Now().Add(5 * time.Second))
		defer func() { _ = f.SetReadDeadline(time.Time{}) }()
	}
	if _, err := io.ReadFull(conn, b[:]); err != nil {
		return fmt.Errorf("reading hello: %w", err)
	}
	v, ok := frame.ParseHello(b)
	if !ok {
		return fmt.Errorf("unexpected hello %x", b)
	}
	if v != version {
		// Refused rather than followed: engine and image disagree, which a matching
		// install never does, and following a version 1 host would drop the CRC.
		return fmt.Errorf("the host speaks link version %d and this guest expects %d (%s); refusing the link: the engine and the guest image do not match, or hyperbutler.netcrc is not what the engine uses",
			v, version, versionName(version))
	}
	return nil
}

func versionName(v byte) string {
	if v == frame.VersionCRC {
		return "a CRC-32C on every frame"
	}
	return "no CRC, hyperbutler.netcrc=0"
}

// TUNSETOFFLOAD flags (linux/if_tun.h).
const (
	tunFCsum   = 0x01
	tunFTSO4   = 0x02
	tunFTSOECN = 0x08
)

// openTAP creates (or attaches to) TAP device name, without packet information headers.
// With vnet, every read and write starts with a virtio_net_hdr and the device offloads
// checksums and IPv4 TCP segmentation; without it, offloads are off, so a device left
// in offload mode by an earlier netfwd hands out only plain frames.
func openTAP(name string, vnet bool) (*os.File, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/net/tun: %w (is the tun module loaded?)", err)
	}
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	flags := uint16(unix.IFF_TAP | unix.IFF_NO_PI)
	offload := 0
	if vnet {
		flags |= unix.IFF_VNET_HDR
		offload = tunFCsum | tunFTSO4 | tunFTSOECN
	}
	ifr.SetUint16(flags)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("TUNSETIFF %s: %w", name, err)
	}
	if vnet {
		if err := unix.IoctlSetPointerInt(fd, unix.TUNSETVNETHDRSZ, frame.VnetHdrLen); err != nil {
			_ = unix.Close(fd)
			return nil, fmt.Errorf("TUNSETVNETHDRSZ %s: %w", name, err)
		}
	}
	if err := unix.IoctlSetInt(fd, unix.TUNSETOFFLOAD, offload); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("TUNSETOFFLOAD %s: %w", name, err)
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

// linkFormat is the framing on the vsock stream.
type linkFormat struct {
	mtu  int
	vnet bool
	// crc (offload mode only): every frame carries a CRC-32C, link version 2.
	crc bool
	// strict: a frame from the host that fails its CRC ends the link.
	strict bool
	// st counts what the relays drop; nil counts nothing.
	st *linkStats
}

// linkStats are netfwd's counters, for the life of the process (across links).
type linkStats struct {
	// crcDropped counts frames from the host that failed their CRC.
	crcDropped atomic.Uint64
	crcLog     frame.MismatchLog
}

// version is the offload link version lk expects.
func (lk linkFormat) version() byte {
	if lk.crc {
		return frame.VersionCRC
	}
	return frame.VersionNoCRC
}

// prefix is what comes before a frame's virtio_net_hdr or Ethernet header: the length,
// and the CRC.
func (lk linkFormat) prefix() int {
	if lk.crc {
		return frame.HeaderLen + frame.CRCLen
	}
	return frame.HeaderLen
}

// maxFrame is the largest frame (the length prefix's value, not counting a CRC) in
// either direction.
func (lk linkFormat) maxFrame() int {
	if lk.vnet {
		return frame.MaxVnetFrameLen
	}
	return frame.MaxFrameLen(lk.mtu)
}

// minFrame is the smallest frame worth sending.
func (lk linkFormat) minFrame() int {
	if lk.vnet {
		return frame.MinVnetFrameLen
	}
	return frame.EthernetHeaderLen
}

// relay copies frames both ways until either side fails.
func relay(tap *os.File, conn *os.File, lk linkFormat) error {
	rc, err := tap.SyscallConn()
	if err != nil {
		return err
	}
	errc := make(chan error, 2)
	go func() { errc <- tapToHost(tapBatchReader(rc, lk), conn, lk) }()
	go func() { errc <- hostToTap(conn, tap, lk, func() { _ = conn.SetReadDeadline(time.Now()) }) }()
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
// nothing is queued. In link version 2 each frame's CRC is computed right after the
// read, from the bytes as the kernel wrote them.
func tapBatchReader(rc syscall.RawConn, lk linkFormat) readBatchFunc {
	pre := lk.prefix()
	slot := pre + lk.maxFrame()
	minLen := lk.minFrame()
	return func(buf []byte) ([]byte, error) {
		out := buf[:0]
		var rerr error
		err := rc.Read(func(fd uintptr) bool {
			for cap(out)-len(out) >= slot {
				start := len(out)
				n, err := unix.Read(int(fd), out[start+pre:start+slot])
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
				if n < minLen {
					continue
				}
				out = out[:start+pre+n]
				frame.PutHeader(out[start:], pre-frame.HeaderLen+n)
				if lk.crc {
					frame.SealCRC(out[start+frame.HeaderLen:])
				}
			}
			return true
		})
		if err != nil {
			return out, err
		}
		return out, rerr
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
	buf := make([]byte, 256<<10) // io.Discard would read 8 KiB at a time
	for {
		if _, err := c.Read(buf); err != nil {
			break
		}
	}
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
