package netstack

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// Resolver is the subset of *net.Resolver the DNS forwarder uses. On Windows,
// net.DefaultResolver answers through GetAddrInfoW (addresses) and DnsQuery (other
// types), so the Name Resolution Policy Table, VPN split DNS, LLMNR and the hosts file
// apply exactly as they do for host programs.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupNS(ctx context.Context, name string) ([]*net.NS, error)
	LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupAddr(ctx context.Context, addr string) ([]string, error)
}

// answerTTL is the TTL put on every answer. The host resolver does not report TTLs, and
// a short one keeps the guest close to what the host sees when a VPN connects.
const answerTTL = 10

const (
	maxUDPAnswer = 1232 // the EDNS size recommended by DNS Flag Day 2020
	maxRecords   = 64   // per answer section; the host resolver can return many
)

// dnsHandler answers guest DNS queries from the host resolver. It is not a recursive
// resolver and forwards nothing as packets: every query becomes one typed lookup.
type dnsHandler struct {
	res     Resolver
	timeout time.Duration
	// subnet is the guest network. Addresses in it are never taken from the host
	// resolver (see resolve), and its reverse names are answered here.
	subnet netip.Prefix
	// hostGateway, if set, returns the host gateway's address (invalid without one)
	// and whether it is on; HostGatewayNames are then answered here, never by the host
	// resolver.
	hostGateway func() (netip.Addr, bool)
	// onQuery, if set, is told the name and type of every query that is looked up.
	onQuery func(name, typ string)
}

// local answers the names this forwarder owns: HostGatewayNames and the reverse names
// of the guest subnet. It reports false for every other name.
func (h *dnsHandler) local(m *dns.Msg, q dns.Question) bool {
	var addr netip.Addr
	on := false
	if h.hostGateway != nil {
		addr, on = h.hostGateway()
	}
	if isHostGatewayName(q.Name) && addr.IsValid() {
		switch {
		case !on:
			// Not forwarded either: a LAN DNS server or a hosts file entry (Docker
			// Desktop writes one with the host's LAN address) must not answer for it.
			m.Rcode = dns.RcodeNameError
		case q.Qtype == dns.TypeA:
			a := addr.As4()
			m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: answerTTL}, A: net.IP(a[:])})
		default:
			// AAAA and every other type: the name exists and has no such records.
		}
		return true
	}
	if q.Qtype == dns.TypePTR {
		ip, ok := parseReverse(q.Name)
		if !ok || !h.subnet.IsValid() || !h.subnet.Contains(ip) {
			return false
		}
		// The guest subnet exists only between the guest and this process; the host
		// resolver (or the LAN, through LLMNR) has nothing to say about it.
		if on && ip == addr {
			m.Answer = append(m.Answer, &dns.PTR{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: answerTTL}, Ptr: dns.Fqdn(HostGatewayNames[0])})
		} else {
			m.Rcode = dns.RcodeNameError
		}
		return true
	}
	return false
}

// answer builds the reply to one wire-format query, or returns nil when the query must
// be dropped (it is not even a parseable DNS header, or it is a response). maxSize is
// the transport's size limit: dns.MinMsgSize for UDP without EDNS, dns.MaxMsgSize for TCP.
func (h *dnsHandler) answer(ctx context.Context, wire []byte, udp bool) []byte {
	req := new(dns.Msg)
	if err := req.Unpack(wire); err != nil {
		// Reply FORMERR when at least the header made sense, so the client stops waiting.
		var hdr dns.Msg
		if len(wire) < 12 || hdr.Unpack(wire[:12]) != nil || hdr.Response {
			return nil
		}
		m := new(dns.Msg)
		m.Id = hdr.Id
		m.Response = true
		m.Opcode = hdr.Opcode
		m.Rcode = dns.RcodeFormatError
		return pack(m)
	}
	if req.Response {
		return nil
	}
	m := new(dns.Msg)
	m.SetReply(req)
	m.RecursionAvailable = true
	m.Authoritative = false
	size := dns.MinMsgSize
	if opt := req.IsEdns0(); opt != nil {
		if opt.Version() != 0 {
			m.Rcode = dns.RcodeBadVers
			m.SetEdns0(maxUDPAnswer, false)
			return pack(m)
		}
		size = int(opt.UDPSize())
		if size < dns.MinMsgSize {
			size = dns.MinMsgSize
		}
		if size > maxUDPAnswer {
			size = maxUDPAnswer
		}
		m.SetEdns0(maxUDPAnswer, false)
	}
	switch {
	case req.Opcode != dns.OpcodeQuery:
		m.Rcode = dns.RcodeNotImplemented
	case len(req.Question) != 1:
		m.Rcode = dns.RcodeFormatError
	case req.Question[0].Qclass != dns.ClassINET:
		m.Rcode = dns.RcodeNotImplemented
	default:
		if h.onQuery != nil {
			h.onQuery(req.Question[0].Name, dns.Type(req.Question[0].Qtype).String())
		}
		ctx, cancel := context.WithTimeout(ctx, h.timeout)
		h.resolve(ctx, m, req.Question[0])
		cancel()
	}
	if !udp {
		size = dns.MaxMsgSize
	}
	m.Truncate(size)
	return pack(m)
}

func pack(m *dns.Msg) []byte {
	b, err := m.Pack()
	if err != nil {
		return nil
	}
	return b
}

// validName reports whether a query name is something the host resolver should see. It
// keeps control characters and other bytes that miekg/dns escapes (\DDD) away from
// GetAddrInfoW.
func validName(name string) bool {
	if len(name) == 0 || len(name) > 254 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '.', c == '_':
		default:
			return false
		}
	}
	return !strings.Contains(name, "..")
}

func (h *dnsHandler) resolve(ctx context.Context, m *dns.Msg, q dns.Question) {
	name := q.Name
	if !validName(name) || name == "." {
		m.Rcode = dns.RcodeRefused
		return
	}
	if h.local(m, q) {
		return
	}
	host := strings.TrimSuffix(name, ".")
	hdr := func(t uint16) dns.RR_Header {
		return dns.RR_Header{Name: name, Rrtype: t, Class: dns.ClassINET, Ttl: answerTTL}
	}
	add := func(rr dns.RR) {
		if len(m.Answer) < maxRecords {
			m.Answer = append(m.Answer, rr)
		}
	}
	var err error
	switch q.Qtype {
	case dns.TypeA:
		var ips []netip.Addr
		ips, err = h.res.LookupNetIP(ctx, "ip4", host)
		for _, ip := range ips {
			// DNS rebinding: a name from outside that points into the guest subnet
			// would let a page or a file a container fetches steer it at the host
			// gateway (this host's loopback) or a HostForward. Nothing outside has
			// a reason to name those addresses, so they are dropped.
			if ip = ip.Unmap(); ip.Is4() && !h.subnet.Contains(ip) {
				a := ip.As4()
				add(&dns.A{Hdr: hdr(dns.TypeA), A: net.IP(a[:])})
			}
		}
	case dns.TypeAAAA:
		// The stack is IPv4 only. Answer NODATA if the name exists, so clients do not
		// wait for AAAA or try addresses they cannot reach.
		_, err = h.res.LookupNetIP(ctx, "ip4", host)
	case dns.TypeCNAME:
		var c string
		c, err = h.res.LookupCNAME(ctx, host)
		// An outside name that is an alias of host.docker.internal is rebinding too.
		if err == nil && validName(c) && !strings.EqualFold(dns.Fqdn(c), name) && !isHostGatewayName(c) {
			add(&dns.CNAME{Hdr: hdr(dns.TypeCNAME), Target: dns.Fqdn(c)})
		}
	case dns.TypeMX:
		var mx []*net.MX
		mx, err = h.res.LookupMX(ctx, host)
		for _, r := range mx {
			if validName(r.Host) {
				add(&dns.MX{Hdr: hdr(dns.TypeMX), Preference: r.Pref, Mx: dns.Fqdn(r.Host)})
			}
		}
	case dns.TypeNS:
		var ns []*net.NS
		ns, err = h.res.LookupNS(ctx, host)
		for _, r := range ns {
			if validName(r.Host) {
				add(&dns.NS{Hdr: hdr(dns.TypeNS), Ns: dns.Fqdn(r.Host)})
			}
		}
	case dns.TypeSRV:
		var srv []*net.SRV
		_, srv, err = h.res.LookupSRV(ctx, "", "", host)
		for _, r := range srv {
			if validName(r.Target) {
				add(&dns.SRV{Hdr: hdr(dns.TypeSRV), Priority: r.Priority, Weight: r.Weight, Port: r.Port, Target: dns.Fqdn(r.Target)})
			}
		}
	case dns.TypeTXT:
		var txt []string
		txt, err = h.res.LookupTXT(ctx, host)
		for _, t := range txt {
			add(&dns.TXT{Hdr: hdr(dns.TypeTXT), Txt: splitTXT(t)})
		}
	case dns.TypePTR:
		ip, ok := parseReverse(name)
		if !ok {
			m.Rcode = dns.RcodeNameError
			return
		}
		var names []string
		names, err = h.res.LookupAddr(ctx, ip.String())
		for _, n := range names {
			if validName(n) {
				add(&dns.PTR{Hdr: hdr(dns.TypePTR), Ptr: dns.Fqdn(n)})
			}
		}
	default:
		// ANY, SOA, HTTPS and the rest cannot be asked through the typed host resolver.
		// NOTIMP makes resolvers fall back to A/AAAA instead of retrying.
		m.Rcode = dns.RcodeNotImplemented
		return
	}
	if err != nil {
		m.Answer = nil
		m.Rcode = rcodeFor(err)
	}
}

// rcodeFor maps a host resolver error: a name that does not exist is NXDOMAIN, a name
// without records of the type is NODATA, and anything else (timeouts, an unreachable
// server) is SERVFAIL so clients retry instead of caching a negative answer.
func rcodeFor(err error) int {
	var de *net.DNSError
	if errors.As(err, &de) && de.IsNotFound {
		return dns.RcodeNameError
	}
	var ae *net.AddrError
	if errors.As(err, &ae) {
		return dns.RcodeNameError
	}
	return dns.RcodeServerFailure
}

func splitTXT(s string) []string {
	const k = 255
	var out []string
	for len(s) > k {
		out = append(out, s[:k])
		s = s[k:]
	}
	return append(out, s)
}

// parseReverse turns "4.3.2.1.in-addr.arpa." into 1.2.3.4. IPv6 reverse names are not
// supported (the stack is IPv4 only).
func parseReverse(name string) (netip.Addr, bool) {
	const suffix = ".in-addr.arpa."
	lower := strings.ToLower(name)
	if !strings.HasSuffix(lower, suffix) {
		return netip.Addr{}, false
	}
	parts := strings.Split(strings.TrimSuffix(lower, suffix), ".")
	if len(parts) != 4 {
		return netip.Addr{}, false
	}
	s := parts[3] + "." + parts[2] + "." + parts[1] + "." + parts[0]
	ip, err := netip.ParseAddr(s)
	if err != nil || !ip.Is4() {
		return netip.Addr{}, false
	}
	return ip, true
}
