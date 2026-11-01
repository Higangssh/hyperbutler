package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Higangssh/hyperbutler/internal/socktable"
)

// Publishing a container's ports on the host (REQ-NET-001, and REQ-NET-005 as proposed
// in docs/research/usage-scenarios.md, S3.4): each port is mirrored on 127.0.0.1 and
// [::1] together, and a container's ports are published all together or not at all.
// Half a publish is worse than none: with 127.0.0.1:P held by a host program and
// [::1]:P mirrored, `localhost` reached the container while 127.0.0.1 reached the host
// program, and nothing said so.

// loopbacks are the host addresses a published port is mirrored on.
var loopbacks = [...]netip.Addr{netip.AddrFrom4([4]byte{127, 0, 0, 1}), netip.IPv6Loopback()}

// PortsUnavailable is a container port that cannot be published on the host. Its
// message follows Docker's own ("Ports are not available: exposing port TCP ..."), so
// the user sees the same error as with Docker Desktop.
type PortsUnavailable struct {
	Port uint16
	// Addr is the host address hyperbutler would listen on.
	Addr netip.Addr
	// Cause is the bind error, or, from the socket table, why the port counts as taken.
	Cause error
}

func (e *PortsUnavailable) Error() string {
	ap := netip.AddrPortFrom(e.Addr, e.Port)
	return fmt.Sprintf("Ports are not available: exposing port TCP %s -> 0.0.0.0:0: %v", ap, e.Cause)
}

func (e *PortsUnavailable) Unwrap() error { return e.Cause }

// errAddrInUse is the text of a bind refused because the address is in use, as Windows
// words WSAEADDRINUSE (the system's own text is used where it is available).
var errAddrInUse = errors.New("Only one usage of each socket address (protocol/network address/port) is normally permitted.") //nolint:staticcheck // ST1005: Windows' own wording, quoted

// heldBy is the cause for a port the socket table shows taken: a bind of addr would
// fail (or would take clients away from the holder), followed by who holds it.
func heldBy(addr netip.AddrPort, inUse error, holder netip.AddrPort, pid uint32) error {
	return fmt.Errorf("listen tcp %s: bind: %w (another program, process %d, listens on %s)", addr, inUse, pid, holder)
}

// portFreeIn reports, as a *PortsUnavailable, a TCP listener on port p in ss on any
// local address, other than those of process self (this engine). inUse is the bind
// error a listener there means.
func portFreeIn(ss []socktable.Socket, p uint16, self uint32, inUse error) error {
	for _, s := range ss {
		if !strings.HasPrefix(s.Proto, "tcp") || s.State != "LISTEN" || s.Local.Port() != p || s.PID == self {
			continue
		}
		// The address the clients that lose out use: [::1] for an IPv6 loopback
		// listener, 127.0.0.1 for everything else (a wildcard or interface listener
		// holds the port for all of them).
		ip := loopbacks[0]
		if a := s.Local.Addr(); a.Is6() && !a.Is4In6() && a.IsLoopback() {
			ip = loopbacks[1]
		}
		return &PortsUnavailable{Port: p, Addr: ip, Cause: heldBy(netip.AddrPortFrom(ip, p), inUse, s.Local, s.PID)}
	}
	return nil
}

// listenCause returns the "listen tcp ADDR: bind: ..." part of a bind error, as
// Docker's message has it, or err itself.
func listenCause(err error) error {
	var oe *net.OpError
	if errors.As(err, &oe) {
		return oe
	}
	return err
}

// binder opens one host listener that relays to guest port guest.
type binder func(host netip.AddrPort, guest uint16) (io.Closer, error)

// publishAll binds every port on 127.0.0.1 and [::1], or nothing. check, run first for
// each port, reports a port the host already uses without binding it. When [::1] cannot
// be bound and v6 reports that the host has no IPv6 loopback, 127.0.0.1 alone is
// enough. On any failure every listener already opened is closed again and the error
// says which port and why.
func publishAll(ports []uint16, check func(uint16) error, bind binder, v6 func() bool) ([]io.Closer, error) {
	var closers []io.Closer
	fail := func(err error) ([]io.Closer, error) {
		for _, c := range closers {
			_ = c.Close()
		}
		return nil, err
	}
	for _, p := range ports {
		if err := check(p); err != nil {
			return fail(err)
		}
	}
	for _, p := range ports {
		for i, ip := range loopbacks {
			c, err := bind(netip.AddrPortFrom(ip, p), p)
			if err != nil {
				if i == 1 && !v6() {
					continue // no IPv6 loopback on this host: 127.0.0.1 is all there is
				}
				return fail(&PortsUnavailable{Port: p, Addr: ip, Cause: listenCause(err)})
			}
			closers = append(closers, c)
		}
	}
	return closers, nil
}

// unpublished lists every port of a container that publishAll refused: none of the
// container's ports is published then. The port that failed has err as its reason, the
// others say they were held back with it.
func unpublished(id string, ports []uint16, err error, now time.Time) []UnpublishedPort {
	reason := err.Error()
	if len(reason) > maxErrorLen {
		reason = strings.ToValidUTF8(reason[:maxErrorLen-3], "")
	}
	var pu *PortsUnavailable
	failing := errors.As(err, &pu)
	out := make([]UnpublishedPort, 0, len(ports))
	for _, p := range ports {
		r := reason
		if failing && p != pu.Port {
			r = fmt.Sprintf("held back with port %d, as a container's ports are published together", pu.Port)
		}
		out = append(out, UnpublishedPort{Container: id[:12], Port: p, Reason: r, Since: now})
	}
	return out
}

// startInspect is the subset of a container inspect the start check reads: before a
// start, NetworkSettings.Ports is empty and the requested bindings are in HostConfig.
type startInspect struct {
	ID    string `json:"Id"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	HostConfig struct {
		NetworkMode  string                   `json:"NetworkMode"`
		PortBindings map[string][]portBinding `json:"PortBindings"`
	} `json:"HostConfig"`
}

// startPorts returns the host TCP ports a container asks for that hyperbutler would
// mirror on the host's loopback when it starts (fixed ports bound on all guest
// addresses or on guestIP). Random ports (-P, an empty or zero host port) and ranges
// are chosen by dockerd at start and are not known yet. running says the container
// already runs (dockerd answers the start with 304, and the ports are this engine's).
func startPorts(inspect []byte, guestIP netip.Addr) (id string, running bool, ports []uint16, err error) {
	if len(inspect) > maxInspectBytes {
		return "", false, nil, errors.New("container inspect document is too large")
	}
	var c startInspect
	if err := json.Unmarshal(inspect, &c); err != nil {
		return "", false, nil, fmt.Errorf("container inspect: %w", err)
	}
	if !validContainerID(c.ID) {
		return "", false, nil, fmt.Errorf("container inspect: bad id %q", c.ID)
	}
	if c.HostConfig.NetworkMode == "host" || c.HostConfig.NetworkMode == "none" {
		return c.ID, c.State.Running, nil, nil // dockerd ignores the bindings
	}
	seen := map[uint16]bool{}
	for key, bindings := range c.HostConfig.PortBindings {
		if _, proto, _ := strings.Cut(key, "/"); proto != "tcp" {
			continue
		}
		for _, b := range bindings {
			if !reachableBinding(b.HostIP, guestIP) {
				continue
			}
			p, err := strconv.ParseUint(b.HostPort, 10, 16)
			if err != nil || p == 0 || seen[uint16(p)] {
				continue
			}
			if len(seen) >= maxPortsPerContainer {
				return "", false, nil, fmt.Errorf("container %s publishes more than %d ports", c.ID[:12], maxPortsPerContainer)
			}
			seen[uint16(p)] = true
			ports = append(ports, uint16(p))
		}
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	return c.ID, c.State.Running, ports, nil
}
