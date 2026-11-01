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

	"golang.org/x/sys/windows"
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
	if err := hostPortFreeExcept(freePort(t), 0); err != nil {
		t.Errorf("free port: %v", err)
	}
}

// Review L3: a host port held by this engine is another container's publish (two
// containers may publish one host port on different guest addresses, and both map to
// the host's loopback). It is refused, and the reason names the engine, not "another
// program".
func TestHostPortFreeOwnListener(t *testing.T) {
	_, p := hostProgram(t) // under this process's PID: the engine's own
	err := hostPortFree(p)
	var pu *PortsUnavailable
	if !errors.As(err, &pu) || pu.Port != p {
		t.Fatalf("port %d held by the engine: %v", p, err)
	}
	s := err.Error()
	if want := fmt.Sprintf("(another container of this engine publishes host port %d)", p); !strings.HasSuffix(s, want) {
		t.Errorf("message %q, want the suffix %q", s, want)
	}
	if strings.Contains(s, "another program") {
		t.Errorf("message %q blames another program", s)
	}
}

// The fallback binds loopback only and still sees a 127.0.0.1 listener.
func TestLoopbackPortFree(t *testing.T) {
	_, p := hostProgram(t)
	if err := loopbackPortFree(p, net.Listen, ipv6Available); err == nil {
		t.Errorf("port %d held on 127.0.0.1 reported free", p)
	}
	q := freePort(t)
	if err := loopbackPortFree(q, net.Listen, ipv6Available); err != nil {
		t.Errorf("free port %d: %v", q, err)
	}
}

// refusingListen is a listenFunc that fails the binds of the addresses in deny with
// errno, binds the rest on port 0 of the same address, and records every address asked
// for.
type refusingListen struct {
	deny  map[string]bool
	errno error
	asked []string
}

func (r *refusingListen) listen(network, address string) (net.Listener, error) {
	r.asked = append(r.asked, address)
	ap := netip.MustParseAddrPort(address)
	if r.deny[address] {
		return nil, &net.OpError{Op: "listen", Net: network, Addr: net.TCPAddrFromAddrPort(ap), Err: os.NewSyscallError("bind", r.errno)}
	}
	return net.Listen(network, netip.AddrPortFrom(ap.Addr(), 0).String())
}

// Review M1: a port in a Windows excluded port range is in no listener table, but its
// loopback bind fails with WSAEACCES. The check binds both loopbacks (never a
// wildcard) after the table says the port is free, and fails with Docker's wording,
// the bind error and a hint about excluded ranges.
func TestHostPortFreeExcludedRange(t *testing.T) {
	p := freePort(t)
	for _, denied := range []netip.Addr{loopbacks[0], loopbacks[1]} {
		addr := netip.AddrPortFrom(denied, p).String()
		r := &refusingListen{deny: map[string]bool{addr: true}, errno: windows.WSAEACCES}
		err := portFreeWith(p, 0, nil, nil, r.listen, func() bool { return true })
		var pu *PortsUnavailable
		if !errors.As(err, &pu) || pu.Port != p || pu.Addr != denied {
			t.Fatalf("%s refused: %v", addr, err)
		}
		if !errors.Is(err, windows.WSAEACCES) {
			t.Errorf("%s refused: the bind error is not in the chain: %v", addr, err)
		}
		want := fmt.Sprintf("Ports are not available: exposing port TCP %s -> 0.0.0.0:0: listen tcp %s: bind: %s ", addr, addr, windows.WSAEACCES.Error())
		s := err.Error()
		if !strings.HasPrefix(s, want) || !strings.Contains(s, "excluded port range") ||
			!strings.Contains(s, "netsh int ipv4 show excludedportrange protocol=tcp") {
			t.Errorf("message %q", s)
		}
		for _, a := range r.asked {
			if ap := netip.MustParseAddrPort(a); !ap.Addr().IsLoopback() {
				t.Errorf("bound %s, not a loopback address", a)
			}
		}
	}

	// Another bind error says what it is, without the excluded-range hint.
	addr := netip.AddrPortFrom(loopbacks[0], p).String()
	r := &refusingListen{deny: map[string]bool{addr: true}, errno: windows.WSAEADDRINUSE}
	err := portFreeWith(p, 0, nil, nil, r.listen, func() bool { return true })
	if err == nil || strings.Contains(err.Error(), "excluded") || !strings.Contains(err.Error(), windows.WSAEADDRINUSE.Error()) {
		t.Errorf("WSAEADDRINUSE: %v", err)
	}

	// A free port: both loopbacks are tried, and nothing stays bound.
	r = &refusingListen{}
	if err := portFreeWith(p, 0, nil, nil, r.listen, func() bool { return true }); err != nil {
		t.Fatalf("free port: %v", err)
	}
	if want := []string{netip.AddrPortFrom(loopbacks[0], p).String(), netip.AddrPortFrom(loopbacks[1], p).String()}; strings.Join(r.asked, " ") != strings.Join(want, " ") {
		t.Errorf("bound %v, want %v", r.asked, want)
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
