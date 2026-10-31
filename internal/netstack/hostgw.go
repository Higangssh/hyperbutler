package netstack

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The host gateway (REQ-NET-004, ADR 0006 "Host gateway"): as in Docker Desktop, the
// guest and its containers reach services on this host's loopback as
// host.docker.internal. The name resolves to one spare address of the guest subnet
// (HostGatewayAddr, 192.168.127.254 by default), and a connection to that address on
// port P is dialed to 127.0.0.1:P on this host (or [::1]:P when 127.0.0.1:P refuses a
// TCP connection). Nothing else is reachable through it: the target is always this
// host's loopback, never an address the guest chose, so the address is not a proxy to
// other hosts or to the host's LAN, VPN or link-local addresses, which the Policy keeps
// refusing for every other destination.

// HostGatewayNames resolve to the host gateway address. Docker Desktop has both;
// gateway.docker.internal is an alias here (see ADR 0006).
var HostGatewayNames = []string{"host.docker.internal", "gateway.docker.internal"}

// DefaultMaxHostGatewayConns bounds the connections relayed to this host's loopback
// at one time, within MaxTCPConns.
const DefaultMaxHostGatewayConns = 256

// maxHostGatewayUDPFlows bounds the UDP flows to this host's loopback, within
// MaxUDPFlows: a quarter of the default flow table.
const maxHostGatewayUDPFlows = 256

// Errors of the host gateway.
var (
	ErrHostGatewayOff     = errors.New("netstack: the host gateway (host.docker.internal) is turned off")
	ErrHostGatewayPort    = errors.New("netstack: the host gateway does not forward this port")
	ErrHostGatewayRefused = errors.New("netstack: nothing listens on this host's loopback on that port")
)

// HostGateway is the live setting of the host gateway.
type HostGateway struct {
	// Enabled turns it on. Off, the address stays on the link but every connection to
	// it is refused and the names do not resolve (NXDOMAIN).
	Enabled bool
	// Ports limits the forwarded ports (TCP and UDP alike); empty means every port.
	Ports PortList
}

// Allows reports whether the host gateway forwards port p.
func (h HostGateway) Allows(p uint16) bool {
	return h.Enabled && p != 0 && (len(h.Ports) == 0 || h.Ports.Contains(p))
}

func (h HostGateway) String() string {
	if !h.Enabled {
		return "off"
	}
	return "ports " + h.Ports.String()
}

// PortRange is an inclusive range of ports, 1 <= Lo <= Hi.
type PortRange struct{ Lo, Hi uint16 }

// PortList is a sorted list of disjoint, non-adjacent port ranges. Empty means all
// ports where a PortList is a filter.
type PortList []PortRange

// Contains reports whether p is in l.
func (l PortList) Contains(p uint16) bool {
	i := sort.Search(len(l), func(i int) bool { return l[i].Hi >= p })
	return i < len(l) && l[i].Lo <= p
}

// String is the canonical form ParsePortList reads back: "all" for an empty list,
// otherwise "80,443,8000-8100".
func (l PortList) String() string {
	if len(l) == 0 {
		return "all"
	}
	var b strings.Builder
	for i, r := range l {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(int(r.Lo)))
		if r.Hi != r.Lo {
			b.WriteByte('-')
			b.WriteString(strconv.Itoa(int(r.Hi)))
		}
	}
	return b.String()
}

const (
	maxPortListLen   = 512
	maxPortListItems = 64
)

// validate checks the invariant Contains relies on: sorted, disjoint, non-adjacent
// ranges of ports 1..65535, at most maxPortListItems of them.
func (l PortList) validate() error {
	if len(l) > maxPortListItems {
		return fmt.Errorf("port list has more than %d ranges", maxPortListItems)
	}
	for i, r := range l {
		if r.Lo == 0 || r.Hi < r.Lo {
			return fmt.Errorf("bad port range %d-%d", r.Lo, r.Hi)
		}
		if i > 0 && uint32(r.Lo) <= uint32(l[i-1].Hi)+1 {
			return errors.New("port ranges are not sorted and disjoint")
		}
	}
	return nil
}

// ParsePortList parses a port list: "" or "all" (every port), or comma-separated ports
// and ranges such as "5432, 8000-8100". The input is from settings.json or the command
// line and is treated as untrusted: ports are decimal, 1..65535, ranges ascending.
// Overlapping and adjacent items are merged.
func ParsePortList(s string) (PortList, error) {
	if len(s) > maxPortListLen {
		return nil, fmt.Errorf("port list is longer than %d bytes", maxPortListLen)
	}
	t := strings.TrimSpace(s)
	if t == "" || strings.EqualFold(t, "all") {
		return nil, nil
	}
	items := strings.Split(t, ",")
	if len(items) > maxPortListItems {
		return nil, fmt.Errorf("port list has more than %d items", maxPortListItems)
	}
	var l PortList
	for _, it := range items {
		it = strings.TrimSpace(it)
		lo, hi, isRange := strings.Cut(it, "-")
		a, err := parsePort(strings.TrimSpace(lo))
		if err != nil {
			return nil, fmt.Errorf("port list %q: %w", s, err)
		}
		b := a
		if isRange {
			if b, err = parsePort(strings.TrimSpace(hi)); err != nil {
				return nil, fmt.Errorf("port list %q: %w", s, err)
			}
			if b < a {
				return nil, fmt.Errorf("port list %q: range %d-%d is descending", s, a, b)
			}
		}
		l = append(l, PortRange{a, b})
	}
	sort.Slice(l, func(i, j int) bool { return l[i].Lo < l[j].Lo })
	out := l[:1]
	for _, r := range l[1:] {
		last := &out[len(out)-1]
		if uint32(r.Lo) <= uint32(last.Hi)+1 {
			last.Hi = max(last.Hi, r.Hi)
			continue
		}
		out = append(out, r)
	}
	if len(out) == 1 && out[0] == (PortRange{1, 65535}) {
		return nil, nil // every port: the same as "all"
	}
	return out, nil
}

// HostGatewayAddr returns the host gateway address of c's subnet: its last host address
// (192.168.127.254 in the default subnet).
func (c Config) HostGatewayAddr() netip.Addr {
	return broadcast(c.Subnet.Masked()).Prev()
}

// hostGatewayTarget is where a host gateway connection on port is dialed first.
func hostGatewayTarget(port uint16) netip.AddrPort {
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), port)
}

// loopbackOnly is the dialer's Control hook for host gateway connections: whatever
// the dialer was asked, the socket connects to this host's loopback (127.0.0.1 or ::1)
// and nowhere else.
func loopbackOnly(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	if a := ap.Addr().Unmap(); a != netip.AddrFrom4([4]byte{127, 0, 0, 1}) && a != netip.IPv6Loopback() {
		return fmt.Errorf("%w: host gateway dial to %s", ErrBlockedAddress, ap)
	}
	return nil
}

// dialHostGateway connects a guest connection for the host gateway's port to
// 127.0.0.1:port, or, when that refuses, to [::1]:port: development servers on Windows
// often listen on "localhost", which can resolve to ::1 only. Only a refusal falls
// back, so a service that accepts on 127.0.0.1 never gets a second connection.
func (n *Stack) dialHostGateway(ctx context.Context, network string, port uint16) (net.Conn, error) {
	d := &net.Dialer{KeepAlive: 30 * time.Second, Control: loopbackOnly}
	c, err := d.DialContext(ctx, network, hostGatewayTarget(port).String())
	if err == nil || network != "tcp" || !connRefused(err) || ctx.Err() != nil {
		return c, err
	}
	c, err6 := d.DialContext(ctx, "tcp", netip.AddrPortFrom(netip.IPv6Loopback(), port).String())
	if err6 != nil {
		return nil, fmt.Errorf("%w (127.0.0.1 and [::1] port %d): %w", ErrHostGatewayRefused, port, err)
	}
	return c, nil
}

// SetHostGateway changes the host gateway's setting while the stack runs. Connections
// already relayed are not cut; new ones and DNS answers follow h at once. It fails if
// the stack was built without a host gateway (Config.HostGateway nil).
func (n *Stack) SetHostGateway(h HostGateway) error {
	if !n.pol.hgAddr.IsValid() {
		return errors.New("netstack: this stack has no host gateway")
	}
	if err := h.Ports.validate(); err != nil {
		return fmt.Errorf("netstack: host gateway: %w", err)
	}
	h.Ports = append(PortList(nil), h.Ports...)
	n.pol.hg.Store(&h)
	return nil
}

// HostGateway returns the host gateway's address and current setting; the address is
// invalid when the stack has none.
func (n *Stack) HostGateway() (netip.Addr, HostGateway) {
	if p := n.pol.hg.Load(); p != nil {
		return n.pol.hgAddr, *p
	}
	return netip.Addr{}, HostGateway{}
}

// hostGatewayDNS is what the DNS forwarder needs: the host gateway's address (invalid
// when the stack has none) and whether it is on.
func (p *Policy) hostGatewayDNS() (netip.Addr, bool) {
	h := p.hg.Load()
	if h == nil {
		return netip.Addr{}, false
	}
	return p.hgAddr, h.Enabled
}

// isHostGatewayName reports whether name (a DNS question name, with or without the
// trailing dot) is one of HostGatewayNames.
func isHostGatewayName(name string) bool {
	name = strings.TrimSuffix(name, ".")
	for _, h := range HostGatewayNames {
		if strings.EqualFold(name, h) {
			return true
		}
	}
	return false
}
