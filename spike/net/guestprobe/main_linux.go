// Command guestprobe runs the network probe inside the HCS guest.
//
//	guestprobe direct          use the guest's own IP stack and /etc/resolv.conf
//	guestprobe relay <port>    tunnel every TCP connection through an HTTP CONNECT relay
//	                           on the host, reached over AF_VSOCK (CID 2, the host)
package main

import (
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Higangssh/winbox/spike/net/probe"
	"github.com/Higangssh/winbox/spike/net/relay"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("WBNET step=args ok=false")
		os.Exit(2)
	}
	cfg := probe.Config{}
	if pem, err := os.ReadFile("/wbnet/roots.pem"); err == nil {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(pem)
		cfg.Roots = pool
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
		return
	case "direct":
		cfg.ResolveLocally = true
	case "relay":
		port, err := strconv.ParseUint(os.Args[2], 10, 32)
		if err != nil {
			fmt.Println("WBNET step=args ok=false")
			os.Exit(2)
		}
		cfg.Dial = func(ctx context.Context, _, addr string) (net.Conn, error) {
			c, err := dialVsock(unix.VMADDR_CID_HOST, uint32(port))
			if err != nil {
				return nil, err
			}
			if err := relay.Handshake(c, addr); err != nil {
				c.Close()
				return nil, err
			}
			return c, nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// WB_REACH is a comma-separated list of host:port targets for the VPN checks.
	if r := os.Getenv("WB_REACH"); r != "" {
		probe.Reach(ctx, cfg, strings.Split(r, ","))
	}
	t := time.Now()
	err := probe.Run(ctx, cfg)
	fmt.Printf("WBNET step=done ok=%v ms=%.1f\n", err == nil, float64(time.Since(t).Microseconds())/1000)
}

// bulkBytes is what serve sends on each connection.
const bulkBytes = 256 << 20

// serve listens on "tcp <port>" or "vsock <port>", prints step=serving, and sends
// bulkBytes on each of <count> connections for the host to time.
func serve(args []string) {
	if len(args) != 3 {
		fmt.Println("WBNET step=serving ok=false err=args")
		return
	}
	port, err1 := strconv.ParseUint(args[1], 10, 32)
	count, err2 := strconv.Atoi(args[2])
	if err1 != nil || err2 != nil {
		fmt.Println("WBNET step=serving ok=false err=args")
		return
	}
	var accept func() (io.WriteCloser, error)
	switch args[0] {
	case "tcp":
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			fmt.Printf("WBNET step=serving ok=false err=listen\n")
			return
		}
		defer ln.Close()
		accept = func() (io.WriteCloser, error) { return ln.Accept() }
	case "vsock":
		fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err == nil {
			err = unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: uint32(port)})
		}
		if err == nil {
			err = unix.Listen(fd, 4)
		}
		if err != nil {
			fmt.Printf("WBNET step=serving ok=false err=vsock_listen\n")
			return
		}
		defer unix.Close(fd)
		accept = func() (io.WriteCloser, error) {
			nfd, _, err := unix.Accept(fd)
			if err != nil {
				return nil, err
			}
			return &vsockConn{fd: nfd}, nil
		}
	default:
		fmt.Println("WBNET step=serving ok=false err=kind")
		return
	}
	fmt.Println("WBNET step=serving ok=true")
	buf := make([]byte, 256<<10)
	for i := 0; i < count; i++ {
		c, err := accept()
		if err != nil {
			fmt.Printf("WBNET step=served ok=false n=%d\n", i)
			return
		}
		sent := 0
		for sent < bulkBytes {
			n, err := c.Write(buf)
			if err != nil {
				break
			}
			sent += n
		}
		c.Close()
		fmt.Printf("WBNET step=served ok=%v n=%d bytes=%d\n", sent == bulkBytes, i, sent)
	}
}

// vsockConn is a blocking AF_VSOCK stream socket as a net.Conn. Deadlines are not
// supported; the probe relies on its context and the HTTP client timeout.
type vsockConn struct {
	fd   int
	port uint32
}

func dialVsock(cid, port uint32) (*vsockConn, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: cid, Port: port}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock connect %d:%d: %w", cid, port, err)
	}
	return &vsockConn{fd: fd, port: port}, nil
}

func (c *vsockConn) Read(p []byte) (int, error) {
	n, err := unix.Read(c.fd, p)
	if n < 0 {
		n = 0
	}
	if err == nil && n == 0 && len(p) > 0 {
		return 0, io.EOF
	}
	return n, err
}

func (c *vsockConn) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		n, err := unix.Write(c.fd, p[written:])
		if err != nil {
			return written, err
		}
		written += n
	}
	return written, nil
}

func (c *vsockConn) Close() error                       { return unix.Close(c.fd) }
func (c *vsockConn) LocalAddr() net.Addr                { return vsockAddr{} }
func (c *vsockConn) RemoteAddr() net.Addr               { return vsockAddr{} }
func (c *vsockConn) SetDeadline(t time.Time) error      { return nil }
func (c *vsockConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *vsockConn) SetWriteDeadline(t time.Time) error { return nil }

type vsockAddr struct{}

func (vsockAddr) Network() string { return "vsock" }
func (vsockAddr) String() string  { return "vsock" }
