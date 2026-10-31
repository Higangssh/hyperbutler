package netstack

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// Errors returned by Policy.Check.
var (
	ErrBlockedAddress = errors.New("netstack: destination is not routable for the guest")
	ErrHostAddress    = errors.New("netstack: destination is an address of this host")
)

// Policy decides where the guest may connect. The stack dials as an ordinary process of
// the user, so without it the guest could reach services bound to this host's loopback or
// LAN addresses past the inbound firewall, or the cloud metadata address.
type Policy struct {
	subnet   netip.Prefix
	forwards map[netip.AddrPort]netip.AddrPort
	// hgAddr is the host gateway's address (invalid without one) and hg its live
	// setting (nil without one).
	hgAddr netip.Addr
	hg     atomic.Pointer[HostGateway]

	hostAddrs func() ([]netip.Addr, error)
	mu        sync.Mutex
	cached    map[netip.Addr]bool
	cachedAt  time.Time
	cacheErr  error
	now       func() time.Time
}

// hostAddrCacheTTL bounds how stale the list of host addresses may be. Interface changes
// (a VPN connecting) take effect within this time.
const hostAddrCacheTTL = time.Second

func newPolicy(c *Config) *Policy {
	p := &Policy{
		subnet:    c.Subnet,
		forwards:  make(map[netip.AddrPort]netip.AddrPort, len(c.HostForwards)),
		hostAddrs: c.HostAddrs,
		now:       time.Now,
	}
	for _, h := range c.HostForwards {
		p.forwards[h.Virtual] = h.Host
	}
	if c.HostGateway != nil {
		p.hgAddr = c.HostGatewayAddr()
		h := *c.HostGateway
		h.Ports = append(PortList(nil), h.Ports...)
		p.hg.Store(&h)
	}
	if p.hostAddrs == nil {
		p.hostAddrs = interfaceAddrs
	}
	return p
}

// TargetKind says how a guest connection is dialed.
type TargetKind int

const (
	// TargetOut is any other destination (the internet, the LAN, a VPN): dialed as
	// is, and Check runs again on the address connected to.
	TargetOut TargetKind = iota
	// TargetForward is a configured HostForward, whose target is exempt from Check.
	TargetForward
	// TargetHostGateway is the host gateway: this host's loopback on the same port
	// (see dialHostGateway), exempt from Check.
	TargetHostGateway
)

// Target is where the host dials a guest connection.
type Target struct {
	Addr netip.AddrPort
	Kind TargetKind
}

// Route returns where a guest connection to dst should be dialed on the host.
func (p *Policy) Route(dst netip.AddrPort) (Target, error) {
	dst = netip.AddrPortFrom(dst.Addr().Unmap(), dst.Port())
	if t, ok := p.forwards[dst]; ok {
		return Target{Addr: t, Kind: TargetForward}, nil
	}
	if p.hgAddr.IsValid() && dst.Addr() == p.hgAddr {
		hg := p.hg.Load()
		switch {
		case !hg.Enabled:
			return Target{}, ErrHostGatewayOff
		case !hg.Allows(dst.Port()):
			return Target{}, fmt.Errorf("%w: port %d (forwarded: %s)", ErrHostGatewayPort, dst.Port(), hg.Ports)
		}
		return Target{Addr: hostGatewayTarget(dst.Port()), Kind: TargetHostGateway}, nil
	}
	if err := p.Check(dst.Addr()); err != nil {
		return Target{}, err
	}
	if dst.Port() == 0 {
		return Target{}, fmt.Errorf("%w: port 0", ErrBlockedAddress)
	}
	return Target{Addr: dst, Kind: TargetOut}, nil
}

// Check reports whether the host may dial addr on the guest's behalf. It refuses:
//   - unspecified, loopback, link-local (including 169.254.169.254), multicast, the
//     limited broadcast address, 0.0.0.0/8, 240.0.0.0/4 and IPv4-compatible or
//     6to4/Teredo forms that embed such addresses;
//   - the guest subnet itself (only HostForwards live there);
//   - every address assigned to an interface of this host.
func (p *Policy) Check(addr netip.Addr) error {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return ErrBlockedAddress
	}
	if blockedRange(addr) {
		return fmt.Errorf("%w: %s", ErrBlockedAddress, addr)
	}
	if p.subnet.Contains(addr) {
		return fmt.Errorf("%w: %s is in the guest subnet", ErrBlockedAddress, addr)
	}
	host, err := p.hostSet()
	if err != nil {
		// Fail closed: without the host's addresses we cannot tell them apart.
		return fmt.Errorf("%w: reading host addresses: %w", ErrHostAddress, err)
	}
	if host[addr] {
		return fmt.Errorf("%w: %s", ErrHostAddress, addr)
	}
	return nil
}

var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"),        // IPv4-compatible (deprecated) and ::, ::1
	netip.MustParsePrefix("fe80::/10"),    // link-local
	netip.MustParsePrefix("fec0::/10"),    // site-local (deprecated)
	netip.MustParsePrefix("ff00::/8"),     // multicast
	netip.MustParsePrefix("2002::/16"),    // 6to4: embeds an IPv4 address
	netip.MustParsePrefix("2001::/32"),    // Teredo: embeds an IPv4 address
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64: embeds an IPv4 address
}

func blockedRange(a netip.Addr) bool {
	if a.IsUnspecified() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsLinkLocalMulticast() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func (p *Policy) hostSet() (map[netip.Addr]bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now := p.now(); p.cached == nil || now.Sub(p.cachedAt) >= hostAddrCacheTTL || now.Before(p.cachedAt) {
		addrs, err := p.hostAddrs()
		p.cachedAt = now
		p.cacheErr = err
		if err == nil {
			m := make(map[netip.Addr]bool, len(addrs))
			for _, a := range addrs {
				m[a.Unmap().WithZone("")] = true
			}
			p.cached = m
		} else {
			p.cached = nil
		}
	}
	return p.cached, p.cacheErr
}

func interfaceAddrs() ([]netip.Addr, error) {
	as, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(as))
	for _, a := range as {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if na, ok := netip.AddrFromSlice(ip); ok {
			out = append(out, na.Unmap())
		}
	}
	return out, nil
}
