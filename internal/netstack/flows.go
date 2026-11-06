package netstack

import (
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxFlowEntries bounds each table of the flow log. The guest chooses the keys, so a
// guest that connects to many destinations only fills the table and then counts
// what did not fit.
const maxFlowEntries = 256

// FlowLog is what the guest asked the host network for: every TCP connection and UDP
// flow it opened (to any destination, allowed or not) and every name it resolved
// through the stack's DNS server, aggregated by destination. It is kept for
// diagnostics, so `bench/no-telemetry` can tell what the guest sends out at idle
// (REQ-UX-005); the debug endpoint serves it only when it is on.
type FlowLog struct {
	Flows []FlowEntry `json:"flows"`
	DNS   []DNSEntry  `json:"dns"`
	// FlowsLost and DNSLost count events that did not fit the tables.
	FlowsLost uint64 `json:"flows_lost"`
	DNSLost   uint64 `json:"dns_lost"`
}

// FlowEntry is one destination the guest connected to.
type FlowEntry struct {
	Proto string `json:"proto"` // "tcp" or "udp"
	Dst   string `json:"dst"`   // the address the guest asked for
	// Route is how the stack handled it: "out" (dialed on the host network),
	// "forward" (a HostForward), "host-gateway" or "denied".
	Route string    `json:"route"`
	Count uint64    `json:"count"`
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
	seq   uint64    // insertion order: First may repeat at the clock's resolution
}

// DNSEntry is one name the guest resolved.
type DNSEntry struct {
	Name  string    `json:"name"`
	Type  string    `json:"type"`
	Count uint64    `json:"count"`
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
	seq   uint64
}

type flowKey struct {
	proto string
	dst   netip.AddrPort
	route string
}

type dnsKey struct{ name, typ string }

type flowLog struct {
	mu        sync.Mutex
	flows     map[flowKey]*FlowEntry
	dns       map[dnsKey]*DNSEntry
	flowsLost uint64
	dnsLost   uint64
	seq       uint64
}

// routeDenied is the Route of a flow the stack refused.
const routeDenied = "denied"

func routeName(tgt Target, err error) string {
	if err != nil {
		return routeDenied
	}
	switch tgt.Kind {
	case TargetForward:
		return "forward"
	case TargetHostGateway:
		return "host-gateway"
	default:
		return "out"
	}
}

func (l *flowLog) flow(proto string, dst netip.AddrPort, route string) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	k := flowKey{proto, dst, route}
	if e := l.flows[k]; e != nil {
		e.Count++
		e.Last = now
		return
	}
	if len(l.flows) >= maxFlowEntries {
		l.flowsLost++
		return
	}
	if l.flows == nil {
		l.flows = map[flowKey]*FlowEntry{}
	}
	l.flows[k] = &FlowEntry{Proto: proto, Dst: dst.String(), Route: route, Count: 1, First: now, Last: now, seq: l.seq}
	l.seq++
}

func (l *flowLog) query(name, typ string) {
	now := time.Now()
	name = strings.ToLower(name)
	l.mu.Lock()
	defer l.mu.Unlock()
	k := dnsKey{name, typ}
	if e := l.dns[k]; e != nil {
		e.Count++
		e.Last = now
		return
	}
	if len(l.dns) >= maxFlowEntries {
		l.dnsLost++
		return
	}
	if l.dns == nil {
		l.dns = map[dnsKey]*DNSEntry{}
	}
	l.dns[k] = &DNSEntry{Name: name, Type: typ, Count: 1, First: now, Last: now, seq: l.seq}
	l.seq++
}

func (l *flowLog) snapshot() FlowLog {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := FlowLog{Flows: []FlowEntry{}, DNS: []DNSEntry{}, FlowsLost: l.flowsLost, DNSLost: l.dnsLost}
	for _, e := range l.flows {
		out.Flows = append(out.Flows, *e)
	}
	for _, e := range l.dns {
		out.DNS = append(out.DNS, *e)
	}
	sort.Slice(out.Flows, func(i, j int) bool { return out.Flows[i].seq < out.Flows[j].seq })
	sort.Slice(out.DNS, func(i, j int) bool { return out.DNS[i].seq < out.DNS[j].seq })
	return out
}

// Flows returns the flow log so far.
func (n *Stack) Flows() FlowLog { return n.flows.snapshot() }
