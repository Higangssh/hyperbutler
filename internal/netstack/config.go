package netstack

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/Higangssh/hyperbutler/internal/netstack/frame"
)

// Defaults for Config. The subnet is gvisor-tap-vsock's default. It exists only between
// the guest and this process, so it cannot collide with a host route; it only shadows the
// same prefix for the guest.
var (
	DefaultSubnet     = netip.MustParsePrefix("192.168.127.0/24")
	DefaultGatewayMAC = net.HardwareAddr{0x5a, 0x94, 0xef, 0xe4, 0x0c, 0xdd}
)

const (
	DefaultMTU            = 9000
	DefaultMaxTCPConns    = 512
	DefaultMaxTCPInFlight = 512
	DefaultMaxUDPFlows    = 1024
	DefaultMaxDNSInFlight = 64
	DefaultDialTimeout    = 10 * time.Second
	DefaultUDPIdleTimeout = 60 * time.Second
	DefaultDNSTimeout     = 5 * time.Second
	maxHostForwards       = 64
	maxSpecLen            = 128
)

// Config describes the virtual network. The zero value of every optional field selects
// its default.
type Config struct {
	// Subnet is the guest network. The gateway is its first host address and the guest
	// its second, both static: there is no DHCP server.
	Subnet netip.Prefix
	// GatewayMAC is the MAC address of the stack's NIC.
	GatewayMAC net.HardwareAddr
	// MTU of the link, frame.MinMTU..frame.MaxMTU. The guest must use the same value.
	MTU int
	// HostForwards are the only way the guest can reach this host: a connection to
	// Virtual (an address in Subnet other than the gateway and guest) is relayed to Host.
	// Everything else that belongs to the host is refused (see Policy).
	HostForwards []HostForward
	// HostGateway, if set, is the initial setting of the host gateway
	// (host.docker.internal, REQ-NET-004) at HostGatewayAddr; Stack.SetHostGateway
	// changes it later. nil means the stack has none: the address is an ordinary
	// refused subnet address and the names go to the host resolver. A HostForward on
	// the same address and port takes precedence.
	HostGateway *HostGateway

	MaxTCPConns int
	// MaxHostGatewayConns bounds host gateway TCP connections, within MaxTCPConns.
	MaxHostGatewayConns int
	MaxTCPInFlight      int
	MaxUDPFlows         int
	MaxDNSInFlight      int
	DialTimeout         time.Duration
	UDPIdleTimeout      time.Duration
	DNSTimeout          time.Duration

	// Resolver answers the guest's DNS queries. nil means the host resolver
	// (net.DefaultResolver, which on Windows uses GetAddrInfoW and DnsQuery, so NRPT,
	// VPN split DNS and the hosts file apply).
	Resolver Resolver
	// HostAddrs lists this host's own addresses. nil means the addresses of every local
	// interface, read again at most once per second. Tests replace it.
	HostAddrs func() ([]netip.Addr, error)
}

// HostForward relays guest TCP connections to Virtual to Host on this machine.
type HostForward struct {
	Virtual netip.AddrPort
	Host    netip.AddrPort
}

func (h HostForward) String() string { return h.Virtual.String() + "=" + h.Host.String() }

// Gateway returns the gateway address of c's subnet.
func (c Config) Gateway() netip.Addr { return c.Subnet.Masked().Addr().Next() }

// GuestIP returns the guest's static address.
func (c Config) GuestIP() netip.Addr { return c.Gateway().Next() }

// withDefaults returns a copy of c with defaults filled in.
func (c Config) withDefaults() Config {
	if !c.Subnet.IsValid() {
		c.Subnet = DefaultSubnet
	}
	c.Subnet = c.Subnet.Masked()
	if c.GatewayMAC == nil {
		c.GatewayMAC = DefaultGatewayMAC
	}
	if c.MTU == 0 {
		c.MTU = DefaultMTU
	}
	def := func(v *int, d int) {
		if *v == 0 {
			*v = d
		}
	}
	def(&c.MaxTCPConns, DefaultMaxTCPConns)
	def(&c.MaxTCPInFlight, DefaultMaxTCPInFlight)
	def(&c.MaxUDPFlows, DefaultMaxUDPFlows)
	def(&c.MaxDNSInFlight, DefaultMaxDNSInFlight)
	def(&c.MaxHostGatewayConns, DefaultMaxHostGatewayConns)
	defd := func(v *time.Duration, d time.Duration) {
		if *v == 0 {
			*v = d
		}
	}
	defd(&c.DialTimeout, DefaultDialTimeout)
	defd(&c.UDPIdleTimeout, DefaultUDPIdleTimeout)
	defd(&c.DNSTimeout, DefaultDNSTimeout)
	return c
}

// Validate reports the first problem with c after defaults are applied.
func (c Config) Validate() error {
	c = c.withDefaults()
	if !c.Subnet.Addr().Is4() || c.Subnet.Bits() < 8 || c.Subnet.Bits() > 29 {
		return fmt.Errorf("netstack: subnet %s must be IPv4 with a prefix length of 8..29", c.Subnet)
	}
	if !c.Subnet.Addr().IsPrivate() {
		return fmt.Errorf("netstack: subnet %s is not a private (RFC 1918) range", c.Subnet)
	}
	if len(c.GatewayMAC) != 6 || c.GatewayMAC[0]&1 != 0 {
		return fmt.Errorf("netstack: gateway MAC %s must be a 6-byte unicast address", c.GatewayMAC)
	}
	if !frame.ValidMTU(c.MTU) {
		return fmt.Errorf("netstack: MTU %d is outside %d..%d", c.MTU, frame.MinMTU, frame.MaxMTU)
	}
	for _, v := range []int{c.MaxTCPConns, c.MaxTCPInFlight, c.MaxUDPFlows, c.MaxDNSInFlight, c.MaxHostGatewayConns} {
		if v < 1 || v > 1<<20 {
			return errors.New("netstack: connection limits must be 1..1048576")
		}
	}
	for _, d := range []time.Duration{c.DialTimeout, c.UDPIdleTimeout, c.DNSTimeout} {
		if d < 0 {
			return errors.New("netstack: timeouts must not be negative")
		}
	}
	if c.HostGateway != nil {
		if err := c.HostGateway.Ports.validate(); err != nil {
			return fmt.Errorf("netstack: host gateway: %w", err)
		}
	}
	if len(c.HostForwards) > maxHostForwards {
		return fmt.Errorf("netstack: at most %d host forwards", maxHostForwards)
	}
	seen := map[netip.AddrPort]bool{}
	for _, h := range c.HostForwards {
		v := h.Virtual.Addr()
		if !c.Subnet.Contains(v) || v == c.Gateway() || v == c.GuestIP() ||
			v == c.Subnet.Addr() || v == broadcast(c.Subnet) {
			return fmt.Errorf("netstack: host forward %s: virtual address must be a spare address in %s", h, c.Subnet)
		}
		if h.Virtual.Port() == 0 || h.Host.Port() == 0 || !h.Host.Addr().IsValid() {
			return fmt.Errorf("netstack: host forward %s: ports must be non-zero", h)
		}
		if seen[h.Virtual] {
			return fmt.Errorf("netstack: host forward %s is listed twice", h.Virtual)
		}
		seen[h.Virtual] = true
	}
	return nil
}

func broadcast(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	v |= host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// ParseHostForward parses "VIRTUAL_IP:PORT=HOST_IP:PORT", for example
// "192.168.127.254:5201=127.0.0.1:5201". The input comes from the command line or a
// config file and is treated as untrusted.
func ParseHostForward(s string) (HostForward, error) {
	if len(s) > maxSpecLen {
		return HostForward{}, errors.New("netstack: host forward spec is too long")
	}
	v, h, ok := strings.Cut(s, "=")
	if !ok {
		return HostForward{}, fmt.Errorf("netstack: host forward %q: want VIRTUAL_IP:PORT=HOST_IP:PORT", s)
	}
	va, err := parseAddrPort4(v)
	if err != nil {
		return HostForward{}, fmt.Errorf("netstack: host forward %q: %w", s, err)
	}
	ha, err := parseAddrPort4(h)
	if err != nil {
		return HostForward{}, fmt.Errorf("netstack: host forward %q: %w", s, err)
	}
	return HostForward{Virtual: va, Host: ha}, nil
}

// parseAddrPort4 accepts only a literal IPv4 address with a decimal, non-zero port.
func parseAddrPort4(s string) (netip.AddrPort, error) {
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		return netip.AddrPort{}, err
	}
	if !ap.Addr().Is4() {
		return netip.AddrPort{}, fmt.Errorf("%s is not an IPv4 address", ap.Addr())
	}
	if ap.Port() == 0 {
		return netip.AddrPort{}, errors.New("port 0")
	}
	return ap, nil
}

// Publish exposes a guest TCP port on a host address.
type Publish struct {
	Host      netip.AddrPort
	GuestPort uint16
}

func (p Publish) String() string { return fmt.Sprintf("%s->guest:%d", p.Host, p.GuestPort) }

// ParsePublish parses "[HOST_IP:]HOST_PORT:GUEST_PORT[/tcp]", the subset of docker's -p
// syntax hyperbutler supports. HOST_IP defaults to 127.0.0.1; an IPv6 host address is
// written in brackets ("[::1]:8080:80"). Only loopback host addresses are accepted unless
// allowLAN is set, so a published port is not reachable from the network by accident.
func ParsePublish(s string, allowLAN bool) (Publish, error) {
	if len(s) > maxSpecLen {
		return Publish{}, errors.New("netstack: publish spec is too long")
	}
	spec := s
	if rest, proto, ok := strings.Cut(spec, "/"); ok {
		if proto != "tcp" {
			return Publish{}, fmt.Errorf("netstack: publish %q: only tcp is supported", s)
		}
		spec = rest
	}
	i := strings.LastIndexByte(spec, ':')
	if i < 0 {
		return Publish{}, fmt.Errorf("netstack: publish %q: want [HOST_IP:]HOST_PORT:GUEST_PORT", s)
	}
	guest, err := parsePort(spec[i+1:])
	if err != nil {
		return Publish{}, fmt.Errorf("netstack: publish %q: guest port: %w", s, err)
	}
	hostPart := spec[:i]
	ip := netip.AddrFrom4([4]byte{127, 0, 0, 1})
	portStr := hostPart
	if j := strings.LastIndexByte(hostPart, ':'); j >= 0 {
		ipStr := hostPart[:j]
		portStr = hostPart[j+1:]
		if strings.HasPrefix(ipStr, "[") && strings.HasSuffix(ipStr, "]") {
			ipStr = ipStr[1 : len(ipStr)-1]
		} else if strings.Contains(ipStr, ":") {
			return Publish{}, fmt.Errorf("netstack: publish %q: IPv6 host addresses need brackets", s)
		}
		ip, err = netip.ParseAddr(ipStr)
		if err != nil {
			return Publish{}, fmt.Errorf("netstack: publish %q: %w", s, err)
		}
		if ip.Zone() != "" {
			return Publish{}, fmt.Errorf("netstack: publish %q: zoned addresses are not supported", s)
		}
		ip = ip.Unmap()
	}
	host, err := parsePort(portStr)
	if err != nil {
		return Publish{}, fmt.Errorf("netstack: publish %q: host port: %w", s, err)
	}
	if !ip.IsLoopback() && !allowLAN {
		return Publish{}, fmt.Errorf("netstack: publish %q: host address %s is not loopback (LAN publishing is opt-in)", s, ip)
	}
	if ip.IsMulticast() || (ip.Is4() && ip == netip.AddrFrom4([4]byte{255, 255, 255, 255})) {
		return Publish{}, fmt.Errorf("netstack: publish %q: %s is not a unicast address", s, ip)
	}
	return Publish{Host: netip.AddrPortFrom(ip, host), GuestPort: guest}, nil
}

func parsePort(s string) (uint16, error) {
	if s == "" || len(s) > 5 {
		return 0, fmt.Errorf("bad port %q", s)
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("bad port %q", s)
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("port %q is outside 1..65535", s)
	}
	return uint16(n), nil
}
