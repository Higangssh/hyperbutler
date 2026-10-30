package relay

import (
	"bufio"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseConnect(t *testing.T) {
	for _, tc := range []struct {
		req  string
		host string
		port uint16
	}{
		{"CONNECT registry-1.docker.io:443 HTTP/1.1\r\nHost: registry-1.docker.io:443\r\n\r\n", "registry-1.docker.io", 443},
		{"CONNECT Example.COM.:80 HTTP/1.0\n\n", "example.com", 80},
		{"CONNECT 100.64.0.10:22 HTTP/1.1\r\n\r\n", "100.64.0.10", 22},
		{"CONNECT [2001:db8::1]:443 HTTP/1.1\r\n\r\n", "2001:db8::1", 443},
	} {
		h, p, err := ParseConnect(bufio.NewReader(strings.NewReader(tc.req)))
		if err != nil || h != tc.host || p != tc.port {
			t.Errorf("ParseConnect(%q) = %q %d %v", tc.req, h, p, err)
		}
	}
	for _, bad := range []string{
		"GET / HTTP/1.1\r\n\r\n",
		"CONNECT a:443 HTTP/2\r\n\r\n",
		"CONNECT a:0 HTTP/1.1\r\n\r\n",
		"CONNECT a:65536 HTTP/1.1\r\n\r\n",
		"CONNECT a:+443 HTTP/1.1\r\n\r\n",
		"CONNECT a:0443 HTTP/1.1\r\n\r\n",
		"CONNECT a HTTP/1.1\r\n\r\n",
		"CONNECT -a.b:443 HTTP/1.1\r\n\r\n",
		"CONNECT a..b:443 HTTP/1.1\r\n\r\n",
		"CONNECT a_b:443 HTTP/1.1\r\n\r\n",
		"CONNECT 1.2.3:443 HTTP/1.1\r\n\r\n",
		"CONNECT 0x7f.1:443 HTTP/1.1\r\n\r\n",
		"CONNECT [fe80::1%eth0]:443 HTTP/1.1\r\n\r\n",
		"CONNECT  a:443 HTTP/1.1\r\n\r\n",
		"CONNECT a:443 HTTP/1.1\r\nHost: a\r\n", // no blank line
		"CONNECT a:443 HTTP/1.1\r\n" + strings.Repeat("X: y\r\n", 1000) + "\r\n",
	} {
		if _, _, err := ParseConnect(bufio.NewReader(strings.NewReader(bad))); err == nil {
			t.Errorf("ParseConnect(%.50q) accepted", bad)
		}
	}
}

func TestAllowed(t *testing.T) {
	for _, tc := range []struct {
		addr string
		ok   bool
	}{
		{"104.18.1.1", true},
		{"100.64.0.10", true}, // VPN (CGNAT) range must work
		{"10.1.2.3", true},    // corporate VPN routes are often private
		{"127.0.0.1", false},
		{"::1", false},
		{"::ffff:127.0.0.1", false},
		{"0.0.0.0", false},
		{"169.254.169.254", false},
		{"fe80::1", false},
		{"224.0.0.251", false},
		{"0.0.0.1", false},
		{"::7f00:1", false},
		{"::ffff:0.0.0.1", false},
	} {
		if got := Allowed(netip.MustParseAddr(tc.addr)); got != tc.ok {
			t.Errorf("Allowed(%s) = %v", tc.addr, got)
		}
	}
}

// TestServerRefusesLoopback checks that a guest cannot reach a host-loopback service,
// whether it names the address or a name that resolves to it.
func TestServerRefusesLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	s := &Server{DialTimeout: 2 * time.Second}
	for _, target := range []string{"127.0.0.1", "localhost"} {
		guest, host := net.Pipe()
		go s.handle(host)
		guest.SetDeadline(time.Now().Add(5 * time.Second))
		err := Handshake(guest, net.JoinHostPort(target, strconv.Itoa(port)))
		guest.Close()
		if err == nil || !strings.Contains(err.Error(), "502") {
			t.Errorf("%s: Handshake = %v, want a 502", target, err)
		}
	}
}

// TestServerRefusesHostAddress: a service listening on one of the host's own
// non-loopback addresses must not be reachable through the relay, because such a
// connection is delivered locally and bypasses inbound firewall rules.
func TestServerRefusesHostAddress(t *testing.T) {
	addr := nonLoopbackAddr(t)
	ln, err := net.Listen("tcp", net.JoinHostPort(addr.String(), "0"))
	if err != nil {
		t.Skip("cannot listen on", addr, err)
	}
	defer ln.Close()
	s := &Server{DialTimeout: 2 * time.Second}
	guest, host := net.Pipe()
	defer guest.Close()
	go s.handle(host)
	guest.SetDeadline(time.Now().Add(5 * time.Second))
	if err := Handshake(guest, ln.Addr().String()); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("Handshake to host address %s = %v, want a 502", ln.Addr(), err)
	}
}

// TestServerTunnel runs a tunnel end to end. It listens on a non-loopback address of
// this machine and tells the server that the host owns no addresses, so the host-address
// guard does not apply.
func TestServerTunnel(t *testing.T) {
	addr := nonLoopbackAddr(t)
	ln, err := net.Listen("tcp", net.JoinHostPort(addr.String(), "0"))
	if err != nil {
		t.Skip("cannot listen on", addr, err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c) // echo
	}()
	s := &Server{DialTimeout: 2 * time.Second, HostAddrs: func() ([]netip.Addr, error) { return nil, nil }}
	guest, host := net.Pipe()
	defer guest.Close()
	go s.handle(host)
	guest.SetDeadline(time.Now().Add(5 * time.Second))
	if err := Handshake(guest, ln.Addr().String()); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(guest, "ping"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(guest, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo = %q %v", buf, err)
	}
}

func nonLoopbackAddr(t *testing.T) netip.Addr {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skip(err)
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap().Is4() && Allowed(ip.Unmap()) && !ip.Unmap().IsLinkLocalUnicast() {
				return ip.Unmap()
			}
		}
	}
	t.Skip("no non-loopback IPv4 address")
	return netip.Addr{}
}

// TestServeMaxTunnels: connections beyond MaxTunnels are closed at once instead of
// pinning goroutines.
func TestServeMaxTunnels(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go (&Server{MaxTunnels: 2}).Serve(ln)
	var open []net.Conn
	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		open = append(open, c)
	}
	time.Sleep(100 * time.Millisecond) // let Serve take both slots
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || strings.Contains(err.Error(), "timeout") {
		t.Fatalf("third connection: read err = %v, want it closed", err)
	}
}
