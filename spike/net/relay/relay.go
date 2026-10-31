// Package relay is the smallest possible user-mode network path for the guest: an HTTP
// CONNECT tunnel over hvsock. The guest asks for host:port, the host resolves the name
// and dials it with ordinary Windows sockets, and then copies bytes both ways.
//
// It stands in for a full user-mode stack such as gvisor-tap-vsock in the
// guest-networking spike. It carries TCP only (no UDP, ICMP or inbound connections), so
// it is not a design for hyperbutler; it measures the hvsock + Windows-socket path that such a
// stack would also take.
package relay

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxHeaderLen bounds the CONNECT request and response headers.
const maxHeaderLen = 4096

// maxHostLen is the longest DNS name (RFC 1035).
const maxHostLen = 253

// ParseConnect reads one CONNECT request from r and returns its target as host and port.
// Only "CONNECT host:port HTTP/1.x" is accepted, followed by header lines up to an empty
// line. Header lines are read and ignored. Everything is bounded by maxHeaderLen.
func ParseConnect(r *bufio.Reader) (host string, port uint16, err error) {
	budget := maxHeaderLen
	readLine := func() (string, error) {
		var b []byte
		for {
			c, err := r.ReadByte()
			if err != nil {
				return "", err
			}
			budget--
			if budget < 0 {
				return "", errors.New("relay: request header too long")
			}
			if c == '\n' {
				return strings.TrimSuffix(string(b), "\r"), nil
			}
			b = append(b, c)
		}
	}
	line, err := readLine()
	if err != nil {
		return "", 0, err
	}
	parts := strings.Split(line, " ")
	if len(parts) != 3 || parts[0] != "CONNECT" || (parts[2] != "HTTP/1.1" && parts[2] != "HTTP/1.0") {
		return "", 0, fmt.Errorf("relay: bad request line %q", short(line))
	}
	host, port, err = SplitTarget(parts[1])
	if err != nil {
		return "", 0, err
	}
	for {
		h, err := readLine()
		if err != nil {
			return "", 0, err
		}
		if h == "" {
			return host, port, nil
		}
	}
}

// SplitTarget validates "host:port". host is a DNS name of letters, digits, '-' and '.',
// or an IPv4 address, or a bracketed IPv6 address. port is 1..65535.
func SplitTarget(target string) (string, uint16, error) {
	h, p, err := net.SplitHostPort(target)
	if err != nil {
		return "", 0, fmt.Errorf("relay: bad target %q: %w", short(target), err)
	}
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil || n == 0 || p[0] == '+' || (len(p) > 1 && p[0] == '0') {
		return "", 0, fmt.Errorf("relay: bad port %q", short(p))
	}
	if a, err := netip.ParseAddr(h); err == nil {
		if a.Zone() != "" {
			return "", 0, fmt.Errorf("relay: zoned address %q", short(h))
		}
		if a.Is6() && !strings.HasPrefix(target, "[") {
			return "", 0, fmt.Errorf("relay: unbracketed IPv6 %q", short(target))
		}
		return a.String(), uint16(n), nil
	}
	if !validHostname(h) {
		return "", 0, fmt.Errorf("relay: bad host %q", short(h))
	}
	return strings.ToLower(strings.TrimSuffix(h, ".")), uint16(n), nil
}

func validHostname(h string) bool {
	h = strings.TrimSuffix(h, ".")
	if h == "" || len(h) > maxHostLen {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	// A name of digits and dots that did not parse as an IP (for example "1.2.3" or
	// "0x7f.1") would be read as an address by some resolvers; refuse it.
	last := h[strings.LastIndexByte(h, '.')+1:]
	allDigits := true
	for i := 0; i < len(last); i++ {
		if last[i] < '0' || last[i] > '9' {
			allDigits = false
		}
	}
	return !allDigits
}

var (
	thisNetwork  = netip.MustParsePrefix("0.0.0.0/8")
	v4Compatible = netip.MustParsePrefix("::/96") // deprecated IPv4-compatible IPv6
)

// Allowed reports whether the relay may connect to a, judged by the address alone. It
// refuses loopback, "this network" (0.0.0.0/8), IPv4-compatible IPv6, link-local and
// multicast. Addresses assigned to the host's own interfaces are refused separately by
// the Server (see Server.HostAddrs), because a connection from a host process to a host
// address is delivered locally and bypasses inbound Windows Firewall rules.
func Allowed(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || thisNetwork.Contains(a) || (a.Is6() && v4Compatible.Contains(a)) {
		return false
	}
	return !a.IsLoopback() && !a.IsUnspecified() && !a.IsLinkLocalUnicast() &&
		!a.IsLinkLocalMulticast() && !a.IsInterfaceLocalMulticast() && !a.IsMulticast()
}

// interfaceAddrs returns every address assigned to a host interface.
func interfaceAddrs() ([]netip.Addr, error) {
	ifs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range ifs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				out = append(out, ip.Unmap())
			}
		}
	}
	return out, nil
}

// Handshake sends a CONNECT request for addr on c and reads the reply. It reads one byte
// at a time so that no tunneled data is consumed.
func Handshake(c io.ReadWriter, addr string) error {
	if _, _, err := SplitTarget(addr); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, addr); err != nil {
		return err
	}
	var hdr []byte
	one := make([]byte, 1)
	for !strings.HasSuffix(string(hdr), "\r\n\r\n") {
		if len(hdr) >= maxHeaderLen {
			return errors.New("relay: response header too long")
		}
		if _, err := io.ReadFull(c, one); err != nil {
			return err
		}
		hdr = append(hdr, one[0])
	}
	status, _, _ := strings.Cut(string(hdr), "\r\n")
	f := strings.SplitN(status, " ", 3)
	if len(f) < 2 || (f[0] != "HTTP/1.1" && f[0] != "HTTP/1.0") || f[1] != "200" {
		return fmt.Errorf("relay: CONNECT %s: %s", addr, short(status))
	}
	return nil
}

// Server accepts CONNECT tunnels from the guest.
type Server struct {
	// Resolver resolves names; nil uses net.DefaultResolver (the Windows resolver on the
	// host, so VPN split DNS applies).
	Resolver *net.Resolver
	// DialTimeout bounds name resolution and connect. Zero means 15 s.
	DialTimeout time.Duration
	// Logf, if set, logs each tunnel.
	Logf func(format string, args ...any)
	// HostAddrs lists the host's own addresses, which the relay refuses. nil uses the
	// addresses of every interface, read at each dial (VPN adapters come and go).
	HostAddrs func() ([]netip.Addr, error)
	// MaxTunnels caps concurrent tunnels. Zero means 256.
	MaxTunnels int
}

// errorReplyTimeout bounds writing a 400 or 502 reply to a guest that does not read.
const errorReplyTimeout = 5 * time.Second

// Serve accepts connections on ln until it fails, handling each in a goroutine.
func (s *Server) Serve(ln net.Listener) error {
	max := s.MaxTunnels
	if max <= 0 {
		max = 256
	}
	sem := make(chan struct{}, max)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		select {
		case sem <- struct{}{}:
		default:
			s.logf("relay: refusing connection: %d tunnels open", max)
			c.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s.handle(c)
		}()
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReaderSize(c, 512)
	host, port, err := ParseConnect(br)
	if err != nil {
		_ = c.SetWriteDeadline(time.Now().Add(errorReplyTimeout))
		_, _ = fmt.Fprint(c, "HTTP/1.1 400 Bad Request\r\n\r\n")
		s.logf("relay: %v", err)
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	t := time.Now()
	up, err := s.dial(host, port)
	if err != nil {
		_ = c.SetWriteDeadline(time.Now().Add(errorReplyTimeout))
		_, _ = fmt.Fprint(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		s.logf("relay: %s:%d: %v", host, port, err)
		return
	}
	defer up.Close()
	s.logf("relay: %s:%d connected in %v", host, port, time.Since(t).Round(time.Millisecond))
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	// Bytes the guest sent after the header are still in br.
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(up, br)
		if cw, ok := up.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(c, up)
		if cw, ok := c.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

func (s *Server) dial(host string, port uint16) (net.Conn, error) {
	timeout := s.DialTimeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	res := s.Resolver
	if res == nil {
		res = net.DefaultResolver
	}
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		addrs, err = res.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
	}
	hostAddrs := s.HostAddrs
	if hostAddrs == nil {
		hostAddrs = interfaceAddrs
	}
	own, err := hostAddrs()
	if err != nil {
		return nil, fmt.Errorf("relay: list host addresses: %w", err)
	}
	var d net.Dialer
	lastErr := errors.New("relay: no allowed address")
	for _, a := range addrs {
		if !Allowed(a) || slices.Contains(own, a.Unmap()) {
			lastErr = fmt.Errorf("relay: %v is not an allowed destination", a)
			continue
		}
		c, err := d.DialContext(ctx, "tcp", netip.AddrPortFrom(a.Unmap(), port).String())
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func short(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}
