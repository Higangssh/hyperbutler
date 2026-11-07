// Package egress enforces a sandbox session's network mode (ADR 0009, "Egress modes",
// REQ-SBX-005) through the hook of package netstack. It holds what the mode allows and
// what the guest has been told; it dials nothing, and it builds on any platform.
//
// How a connection gets out in the dev and locked modes:
//
//   - DNS: only A, AAAA and CNAME of an allowed name reach the host's resolver. A blocked
//     name causes no query there, so a query cannot carry data to an outside name server.
//   - Answers: addresses in private, loopback, link-local, CGNAT and ULA ranges are dropped
//     unless the entry says lan:. What remains is pinned: address to name, on a sliding
//     window.
//   - Connections: only to a pinned address. On 443 the ClientHello's server name, and on
//     80 (dev only) the Host header, must be a name that address is pinned for; other
//     ports need an entry that names them and are checked by address only. If the server
//     answers with a HelloRetryRequest, the second hello is checked the same way.
//   - UDP is not forwarded, so QUIC falls back to TCP.
//
// What this cannot see, and the documentation says so:
//
//   - Later requests on an allowed HTTP connection, and anything inside TLS. A client can
//     send one server name and another Host header (domain fronting) to a CDN that serves
//     both, and DNS over HTTPS to an allowed name looks like any other HTTPS.
//   - What crosses a host:port entry: it is by address only.
//   - Where an allowed name points. A name the agent can influence opens 443 and 80 to any
//     public address it answers with, and a server with a default virtual host does not
//     care for the server name.
//   - Allowed names themselves: several preset hosts accept uploads from anyone, so dev
//     narrows exfiltration and only locked defends against it.
//
// Every name, address and byte the guest sends is hostile. Nothing of it is logged raw:
// see Blocked.
package egress

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/Higangssh/hyperbutler/internal/netstack"
)

// Mode is a session's network mode.
type Mode string

const (
	// Open is everything the Docker VM may reach: no hook at all (see Hook).
	Open Mode = "open"
	// Dev is the preset plus what the user allowed.
	Dev Mode = "dev"
	// Locked is what the user allowed, normally a model provider's API host, or nothing.
	// It also blocks port 80.
	Locked Mode = "locked"
)

// ParseMode parses "open", "dev" or "locked". An empty string is not a mode: choosing the
// default is the caller's.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case Open, Dev, Locked:
		return m, nil
	}
	return "", fmt.Errorf("egress: mode %s: want open, dev or locked", quoted(s))
}

// Defaults for Options.
const (
	DefaultPinTTL           = 5 * time.Minute
	DefaultMaxPins          = 4096
	DefaultMaxWildcardNames = 64
)

// Options tune a Policy. The zero value of every field selects its default.
type Options struct {
	// PinTTL is how long a pin lives after the last answer or accepted connection that
	// used it.
	PinTTL time.Duration
	// MaxPins bounds the pins (one per address and name); the oldest goes first.
	MaxPins int
	// MaxWildcardNames bounds the distinct names one wildcard entry may resolve in a
	// session. Past it they are blocked, reach no resolver, and are counted in Blocked.
	MaxWildcardNames int
	// Now is the clock. Tests replace it.
	Now func() time.Time
}

// Policy is one session's egress state. It implements netstack.Egress and is safe for
// concurrent use.
type Policy struct {
	mode     Mode
	now      func() time.Time
	ttl      time.Duration
	maxNames int

	mu      sync.Mutex
	exact   map[string]*rule
	zones   map[string]*zone
	pins    pinTable
	blocked blockedLog
}

// rule is what the entries for one exact name allow.
type rule struct {
	lan bool
	// web: an entry without a port, which means 443, and 80 in dev.
	web   bool
	ports map[uint16]bool
}

// zone is one wildcard entry and the names it has let through.
type zone struct {
	names map[string]bool
}

var _ netstack.Egress = (*Policy)(nil)

// New builds the policy of a dev or locked session from entries. Dev does not imply the
// preset: pass DevPreset's entries with the user's. Open has no policy, see Hook.
func New(mode Mode, entries []Entry, opts Options) (*Policy, error) {
	switch mode {
	case Dev, Locked:
	case Open:
		return nil, errors.New("egress: the open mode has no policy; pass a nil hook to the network stack (see Hook)")
	default:
		return nil, fmt.Errorf("egress: mode %s: want dev or locked", quoted(string(mode)))
	}
	if opts.PinTTL < 0 || opts.MaxPins < 0 || opts.MaxWildcardNames < 0 {
		return nil, errors.New("egress: options must not be negative")
	}
	if opts.PinTTL == 0 {
		opts.PinTTL = DefaultPinTTL
	}
	if opts.MaxPins == 0 {
		opts.MaxPins = DefaultMaxPins
	}
	if opts.MaxWildcardNames == 0 {
		opts.MaxWildcardNames = DefaultMaxWildcardNames
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	p := &Policy{
		mode:     mode,
		now:      opts.Now,
		ttl:      opts.PinTTL,
		maxNames: opts.MaxWildcardNames,
		exact:    map[string]*rule{},
		zones:    map[string]*zone{},
		pins:     newPinTable(opts.MaxPins),
	}
	for _, e := range entries {
		if err := p.Allow(e); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// Hook returns what to put in netstack.Config.Egress for a mode, and the same policy with
// its methods for the session manager. For Open, all three results are nil: a nil
// interface, never a nil *Policy in a non-nil one.
func Hook(mode Mode, entries []Entry, opts Options) (netstack.Egress, *Policy, error) {
	if mode == Open {
		return nil, nil, nil
	}
	p, err := New(mode, entries, opts)
	if err != nil {
		return nil, nil, err
	}
	return p, p, nil
}

// Allow widens the policy for the rest of the session. It refuses an Entry that ParseEntry
// could not have produced.
func (p *Policy) Allow(e Entry) error {
	if err := e.validate(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e.Wildcard {
		if p.zones[e.Name] == nil {
			p.zones[e.Name] = &zone{names: map[string]bool{}}
		}
		return nil
	}
	r := p.exact[e.Name]
	if r == nil {
		r = &rule{}
		p.exact[e.Name] = r
	}
	r.lan = r.lan || e.LAN
	if e.Port == 0 {
		r.web = true
		return nil
	}
	if r.ports == nil {
		r.ports = map[uint16]bool{}
	}
	r.ports[e.Port] = true
	return nil
}

// verdict is whether a name may be resolved.
type verdict int

const (
	nameBlocked verdict = iota
	nameAllowed
	nameOverCap // covered by a wildcard that is full
)

// admit decides a name, and spends room in a wildcard on a new one. A wildcard's new
// names are what a query can carry data in, so they are counted at the question, before
// any lookup. The caller holds mu.
func (p *Policy) admit(name string) verdict {
	r := p.exact[name]
	if r != nil && r.web {
		return nameAllowed
	}
	var first *zone
	seen := false
	eachZone(p.zones, name, func(z *zone) {
		if first == nil {
			first = z // the most specific one
		}
		seen = seen || z.names[name]
	})
	switch {
	case seen:
		return nameAllowed
	case first != nil && len(first.names) < p.maxNames:
		first.names[name] = true
		return nameAllowed
	case r != nil:
		return nameAllowed
	case first != nil:
		return nameOverCap
	}
	return nameBlocked
}

// webAllowed reports whether an admitted name may be reached on port 443 or 80: a host:22
// entry alone says nothing about them. The caller holds mu.
func (p *Policy) webAllowed(name string, port uint16) bool {
	if r := p.exact[name]; r != nil && (r.web || r.ports[port]) {
		return true
	}
	seen := false
	eachZone(p.zones, name, func(z *zone) { seen = seen || z.names[name] })
	return seen
}

// eachZone calls fn for the zones that cover name, most specific first. A wildcard covers
// names below its zone at any depth, not the zone's own name.
func eachZone(zones map[string]*zone, name string, fn func(*zone)) {
	if len(zones) == 0 {
		return
	}
	for {
		i := strings.IndexByte(name, '.')
		if i < 0 {
			return
		}
		name = name[i+1:]
		if z := zones[name]; z != nil {
			fn(z)
		}
	}
}

// AllowQuery implements netstack.Egress: only A, AAAA and CNAME, and only allowed names.
func (p *Policy) AllowQuery(name string, qtype uint16) bool {
	canon, ok := canonName(name)
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !ok {
		p.blocked.add(name, KindQuery, now)
		return false
	}
	switch qtype {
	case dns.TypeA, dns.TypeAAAA, dns.TypeCNAME:
	default:
		p.blocked.add(canon, KindQueryType, now)
		return false
	}
	switch p.admit(canon) {
	case nameAllowed:
		return true
	case nameOverCap:
		p.blocked.add(canon, KindWildcardCap, now)
	default:
		p.blocked.add(canon, KindQuery, now)
	}
	return false
}

// Answer implements netstack.Egress: it drops the addresses this name may not have and
// pins the others to it. A name that is not allowed gets no addresses and no pins.
func (p *Policy) Answer(name string, addrs []netip.Addr) []netip.Addr {
	canon, ok := canonName(name)
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !ok {
		p.blocked.add(name, KindQuery, now)
		return nil
	}
	switch p.admit(canon) {
	case nameAllowed:
	case nameOverCap:
		p.blocked.add(canon, KindWildcardCap, now)
		return nil
	default:
		p.blocked.add(canon, KindQuery, now)
		return nil
	}
	lan := false
	if r := p.exact[canon]; r != nil {
		lan = r.lan
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		a = a.Unmap().WithZone("")
		if !a.IsValid() {
			continue
		}
		if neverAnswered(a) || (!lan && localRange(a)) {
			p.blocked.add(canon, KindPrivateAnswer, now)
			continue
		}
		p.pins.put(a, canon, now.Add(p.ttl), now)
		out = append(out, a)
	}
	return out
}

// Conn implements netstack.Egress. An address no answer produced is denied whatever the
// port, which is what stops IP literals and DNS, DoT or DoH to outside servers. The
// decision on 443 and 80 waits for the first bytes, so it does not renew a pin.
func (p *Policy) Conn(dst netip.AddrPort) netstack.ConnDecision {
	addr, port := dst.Addr().Unmap().WithZone(""), dst.Port()
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	names := p.pins.live(addr, now)
	if len(names) == 0 {
		// By address only: one address on many ports is one line.
		p.blocked.add(addr.String(), KindAddress, now)
		return netstack.ConnDeny
	}
	switch {
	case port == httpsPort:
		return netstack.ConnInspect
	case port == httpPort && p.mode == Dev:
		return netstack.ConnInspect
	case port == httpPort:
		p.blocked.add(dst.String(), KindPort, now)
		return netstack.ConnDeny
	}
	allowed := false
	for _, name := range names {
		if r := p.exact[name]; r != nil && r.ports[port] {
			p.pins.put(addr, name, now.Add(p.ttl), now)
			allowed = true
		}
	}
	if allowed {
		return netstack.ConnAllow
	}
	p.blocked.add(dst.String(), KindPort, now)
	return netstack.ConnDeny
}

const (
	httpsPort = 443
	httpPort  = 80
)

// Inspect implements netstack.Egress: the name the guest's first bytes ask for must be one
// the address is pinned for. It only ever parses first, so a hello that arrives in many
// pieces costs one parse per read, bounded by the stack's buffer and deadline.
func (p *Policy) Inspect(dst netip.AddrPort, first []byte) netstack.InspectResult {
	switch {
	case dst.Port() == httpsPort:
		res := parseClientHello(first)
		if res.state == parseDone && pipelinedHello(first[res.end:]) {
			// A second hello right behind the first would reach the server before the
			// stack could see that the server asked for it.
			res = helloBad(KindBadHello)
		}
		r := p.decide(dst, res, KindServerName)
		if r == netstack.InspectAllow {
			return netstack.InspectAllowTLS
		}
		return r
	case dst.Port() == httpPort && p.mode == Dev:
		return p.decide(dst, parseHTTPHead(first), KindHTTPHost)
	}
	return p.decide(dst, parsed{state: parseBad, kind: KindPort}, "")
}

// InspectReply implements netstack.Egress: only a HelloRetryRequest asks for more
// inspection. Whatever else the server sent, the client deals with.
func (p *Policy) InspectReply(_ netip.AddrPort, reply []byte) netstack.ReplyResult {
	switch parseServerReply(reply) {
	case replyMore:
		return netstack.ReplyMore
	case replyRetry:
		return netstack.ReplyRetry
	}
	return netstack.ReplyPass
}

// InspectRetry implements netstack.Egress: the second hello passes the same checks as the
// first, for the same pins.
func (p *Policy) InspectRetry(dst netip.AddrPort, next []byte) netstack.InspectResult {
	if dst.Port() != httpsPort {
		return p.decide(dst, parsed{state: parseBad, kind: KindPort}, "")
	}
	return p.decide(dst, parseRetryHello(next), KindServerName)
}

// decide turns a parser's result into a verdict: the name must be pinned for the address,
// which an accepted connection renews.
func (p *Policy) decide(dst netip.AddrPort, res parsed, nameKind string) netstack.InspectResult {
	if res.state == parseMore {
		return netstack.InspectMore
	}
	addr := dst.Addr().Unmap().WithZone("")
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if res.state != parseDone {
		p.blocked.add(dst.String(), res.kind, now)
		return netstack.InspectDeny
	}
	canon, ok := canonName(res.name)
	if !ok || !p.pins.has(addr, canon, now) || !p.webAllowed(canon, dst.Port()) {
		p.blocked.add(res.name, nameKind, now)
		return netstack.InspectDeny
	}
	p.pins.put(addr, canon, now.Add(p.ttl), now)
	return netstack.InspectAllow
}

// UDP implements netstack.Egress: never in dev and locked. QUIC falls back to TCP.
func (p *Policy) UDP(dst netip.AddrPort) bool {
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blocked.add(dst.String(), KindUDP, now)
	return false
}

// Blocked returns what was blocked so far, oldest first, escaped (see BlockedName).
func (p *Policy) Blocked() []BlockedName {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.blocked.snapshot()
}

// BlockedLost counts the events that did not fit Blocked's table.
func (p *Policy) BlockedLost() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.blocked.lost
}

// neverAnswered: addresses that no entry may bring to a guest, lan: or not.
func neverAnswered(a netip.Addr) bool {
	return a.IsUnspecified() || a.IsMulticast() || a.IsInterfaceLocalMulticast() || a == limitedBroadcast
}

var limitedBroadcast = netip.AddrFrom4([4]byte{255, 255, 255, 255})

// localRanges are what a name outside the user's own networks has no business answering:
// private, CGNAT, ULA and the rest of what does not route on the internet, and the IPv6
// forms that embed an IPv4 address. Package netstack refuses loopback and link-local at
// the dial whatever this says.
var localRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"), // ::1 and IPv4-compatible
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("fec0::/10"),
}

func localRange(a netip.Addr) bool {
	for _, r := range localRanges {
		if r.Contains(a) {
			return true
		}
	}
	return false
}

// quoted shows a string from the outside in an error: ASCII only, and not all of it.
func quoted(s string) string {
	const maxShown = 128
	if len(s) > maxShown {
		return strconv.QuoteToASCII(s[:maxShown]) + "..."
	}
	return strconv.QuoteToASCII(s)
}
