//go:build windows

package engine

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
)

// The host program of these tests listens on 127.0.0.1 only: a wildcard listener
// raises the Windows Firewall prompt (TestPortInUseRefusesHostListener covers those,
// where HYPERBUTLER_TEST_WILDCARD=1).

func hostProgram(t *testing.T) (net.Listener, uint16) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln, uint16(ln.Addr().(*net.TCPAddr).Port)
}

// freePort returns a loopback port nobody listens on (as far as a moment ago).
func freePort(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := uint16(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()
	return p
}

func realBind(host netip.AddrPort, _ uint16) (io.Closer, error) {
	return net.Listen("tcp", host.String())
}

// TestHostPortFreeSeesLoopbackListener: the socket table finds a host program that
// holds 127.0.0.1:P only, a port a wildcard bind probe would have reported free.
func TestHostPortFreeSeesLoopbackListener(t *testing.T) {
	_, p := hostProgram(t)
	// The table lists this test's own listener under the test's PID, which hostPortFree
	// skips as the engine's own; ask as if the engine were another process.
	err := hostPortFreeExcept(p, 0)
	var pu *PortsUnavailable
	if !errors.As(err, &pu) || pu.Port != p {
		t.Fatalf("port %d held on 127.0.0.1: %v", p, err)
	}
	want := fmt.Sprintf("Ports are not available: exposing port TCP 127.0.0.1:%d -> 0.0.0.0:0: listen tcp 127.0.0.1:%d: bind: ", p, p)
	tail := fmt.Sprintf("(another program, process %d, listens on 127.0.0.1:%d)", os.Getpid(), p)
	if s := err.Error(); !strings.HasPrefix(s, want) || !strings.HasSuffix(s, tail) {
		t.Errorf("message %q", s)
	}
	if err := hostPortFree(p); err != nil {
		t.Errorf("the engine's own listener counts as taken: %v", err)
	}
	if err := hostPortFreeExcept(freePort(t), 0); err != nil {
		t.Errorf("free port: %v", err)
	}
}

// The fallback binds loopback only and still sees a 127.0.0.1 listener.
func TestLoopbackPortFree(t *testing.T) {
	_, p := hostProgram(t)
	if err := loopbackPortFree(p); err == nil {
		t.Errorf("port %d held on 127.0.0.1 reported free", p)
	}
	q := freePort(t)
	if err := loopbackPortFree(q); err != nil {
		t.Errorf("free port %d: %v", q, err)
	}
}

// TestPublishAllRealCollision repeats the split found on a VM (S3.4) with real
// sockets: a host program on 127.0.0.1:P, a container publishing a free port and P.
// The check is left out, as when the host program wins a race after it: the 127.0.0.1
// bind of P fails, so nothing stays bound, not P's [::1] half and not the other port.
func TestPublishAllRealCollision(t *testing.T) {
	_, p := hostProgram(t)
	q := freePort(t)
	ports := []uint16{q, p} // publishAll binds in this order: q is bound when p fails
	cs, err := publishAll(ports, noCheck, realBind, ipv6Available)
	if err == nil {
		for _, c := range cs {
			_ = c.Close()
		}
		t.Fatalf("published %d listeners despite the host program on 127.0.0.1:%d", len(cs), p)
	}
	var pu *PortsUnavailable
	if !errors.As(err, &pu) || pu.Port != p || pu.Addr != loopbacks[0] {
		t.Fatalf("error %v", err)
	}
	// Every listener publishAll opened is closed again: all four addresses bind.
	for _, port := range []uint16{q, p} {
		for _, ip := range loopbacks {
			if port == p && ip == loopbacks[0] {
				continue // the host program's
			}
			if ip == loopbacks[1] && !ipv6Available() {
				continue
			}
			ln, err := net.Listen("tcp", netip.AddrPortFrom(ip, port).String())
			if err != nil {
				t.Errorf("%s:%d still bound after the failed publish: %v", ip, port, err)
				continue
			}
			_ = ln.Close()
		}
	}

	// With the check, nothing is bound at all.
	var bound []string
	_, err = publishAll(ports, func(port uint16) error { return hostPortFreeExcept(port, 0) },
		func(h netip.AddrPort, g uint16) (io.Closer, error) {
			bound = append(bound, h.String())
			return realBind(h, g)
		}, ipv6Available)
	if err == nil || len(bound) != 0 {
		t.Errorf("with the check: %v, bound %v", err, bound)
	}
}

// A normal publish holds both loopbacks until closed.
func TestPublishAllReal(t *testing.T) {
	p := freePort(t)
	cs, err := publishAll([]uint16{p}, func(port uint16) error { return hostPortFreeExcept(port, 0) }, realBind, ipv6Available)
	if err != nil {
		t.Fatal(err)
	}
	want := 2
	if !ipv6Available() {
		want = 1
	}
	if len(cs) != want {
		t.Fatalf("%d listeners, want %d", len(cs), want)
	}
	if err := hostPortFreeExcept(p, 0); err == nil {
		t.Error("the published port is not in the table")
	}
	for _, c := range cs {
		_ = c.Close()
	}
}
