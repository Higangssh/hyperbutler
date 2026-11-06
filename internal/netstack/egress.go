package netstack

import (
	"errors"
	"net/netip"
	"time"
)

// Egress narrows what the guest may do on the network (ADR 0009, "Egress modes"). It is
// asked on top of Policy, never instead of it: an address the hook allows still has to
// pass Route and Check, and is dialed by the same rules. The methods are called from many
// goroutines, with what the guest chose, before anything of it reaches the host's
// resolver or network. Everything in Config.Egress's way fails closed: a decision other
// than the allowing ones is a denial.
type Egress interface {
	// AllowQuery says whether a DNS question reaches the host resolver at all. false:
	// NXDOMAIN for A/AAAA/CNAME, REFUSED for other types, and no lookup of any kind
	// happens (REQ-SBX-005: a blocked name causes no query at the host's resolver). name
	// is lower-case without the trailing dot. Names the forwarder answers itself (the
	// guest subnet's reverse names, the host gateway's names) and names it refuses to
	// pass to a resolver at all never get here.
	AllowQuery(name string, qtype uint16) bool
	// Answer filters the addresses of an allowed name and records pins. It is called for
	// every A lookup that returned addresses, without those in the guest subnet, and for
	// the AAAA NODATA lookup's too, whose output is discarded.
	Answer(name string, addrs []netip.Addr) []netip.Addr
	// Conn decides a guest TCP connection before anything is dialed.
	Conn(dst netip.AddrPort) ConnDecision
	// Inspect looks at the first bytes the guest sent on a ConnInspect connection. first
	// is everything so far, valid only during the call, and must not be changed. It
	// returns InspectMore to wait for more bytes, InspectAllow or InspectDeny.
	Inspect(dst netip.AddrPort, first []byte) InspectResult
	// UDP says whether a guest UDP flow to dst may be forwarded (DNS to the stack's own
	// server is not a flow and never asks).
	UDP(dst netip.AddrPort) bool
}

// ConnDecision is what Egress.Conn decided. The zero value denies.
type ConnDecision int

const (
	// ConnDeny refuses the connection.
	ConnDeny ConnDecision = iota
	// ConnAllow relays it as without a hook.
	ConnAllow
	// ConnInspect completes the guest's handshake, shows Egress.Inspect what the guest
	// sends first, and only then dials.
	ConnInspect
)

// InspectResult is what Egress.Inspect decided. The zero value denies.
type InspectResult int

const (
	// InspectDeny closes the connection; nothing was dialed.
	InspectDeny InspectResult = iota
	// InspectAllow dials, sends the guest's bytes on unchanged and relays as usual.
	InspectAllow
	// InspectMore waits for more bytes, within MaxInspectBytes and inspectTimeout.
	InspectMore
)

// ErrEgressDenied is why a guest flow is refused when only Config.Egress objected.
var ErrEgressDenied = errors.New("netstack: the egress policy refused the destination")

const (
	// MaxInspectBytes bounds what a ConnInspect connection may send before it is decided.
	// A ClientHello or a request head that needs more is denied.
	MaxInspectBytes = 16 << 10
	// inspectTimeout is the overall limit for the same; a guest that trickles bytes gets
	// no more time than one that sends nothing.
	inspectTimeout = 5 * time.Second
)
