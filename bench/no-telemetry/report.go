package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/Higangssh/hyperbutler/internal/netstack"
)

// Socket is one row of the Windows TCP or UDP table (GetExtendedTcpTable,
// GetExtendedUdpTable with the owner PID). UDP rows have no remote end.
type Socket struct {
	Proto  string // tcp4, tcp6, udp4, udp6
	PID    uint32
	Local  netip.AddrPort
	Remote netip.AddrPort // invalid for UDP
	State  string         // TCP only
}

// tcpStates are MIB_TCP_STATE values.
var tcpStates = [...]string{1: "CLOSED", "LISTEN", "SYN_SENT", "SYN_RCVD", "ESTABLISHED", "FIN_WAIT1",
	"FIN_WAIT2", "CLOSE_WAIT", "CLOSING", "LAST_ACK", "TIME_WAIT", "DELETE_TCB"}

func tcpState(v uint32) string {
	if v > 0 && int(v) < len(tcpStates) {
		return tcpStates[v]
	}
	return fmt.Sprintf("state-%d", v)
}

// port reads a dwPort field: the port is in network byte order in its first two bytes.
func port(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }

// rows checks a table's header and returns its rows of size n each.
func rows(b []byte, n int) ([][]byte, error) {
	if len(b) < 4 {
		return nil, fmt.Errorf("table of %d bytes", len(b))
	}
	k := int(binary.LittleEndian.Uint32(b))
	// IPv6 rows are 4-byte aligned after the 4-byte count, so no padding.
	if k < 0 || k > (len(b)-4)/n {
		return nil, fmt.Errorf("table claims %d rows of %d bytes in %d bytes", k, n, len(b))
	}
	out := make([][]byte, k)
	for i := range out {
		out[i] = b[4+i*n : 4+(i+1)*n]
	}
	return out, nil
}

// ParseTable decodes a MIB_TCPTABLE_OWNER_PID, MIB_TCP6TABLE_OWNER_PID,
// MIB_UDPTABLE_OWNER_PID or MIB_UDP6TABLE_OWNER_PID, by proto.
func ParseTable(proto string, b []byte) ([]Socket, error) {
	var out []Socket
	switch proto {
	case "tcp4": // state, local addr, local port, remote addr, remote port, pid
		rs, err := rows(b, 24)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			out = append(out, Socket{Proto: proto, PID: binary.LittleEndian.Uint32(r[20:]),
				Local:  netip.AddrPortFrom(netip.AddrFrom4([4]byte(r[4:8])), port(r[8:])),
				Remote: netip.AddrPortFrom(netip.AddrFrom4([4]byte(r[12:16])), port(r[16:])),
				State:  tcpState(binary.LittleEndian.Uint32(r))})
		}
	case "tcp6": // local addr, scope, port, remote addr, scope, port, state, pid
		rs, err := rows(b, 56)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			out = append(out, Socket{Proto: proto, PID: binary.LittleEndian.Uint32(r[52:]),
				Local:  netip.AddrPortFrom(netip.AddrFrom16([16]byte(r[0:16])), port(r[20:])),
				Remote: netip.AddrPortFrom(netip.AddrFrom16([16]byte(r[24:40])), port(r[44:])),
				State:  tcpState(binary.LittleEndian.Uint32(r[48:]))})
		}
	case "udp4": // local addr, local port, pid
		rs, err := rows(b, 12)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			out = append(out, Socket{Proto: proto, PID: binary.LittleEndian.Uint32(r[8:]),
				Local: netip.AddrPortFrom(netip.AddrFrom4([4]byte(r[0:4])), port(r[4:]))})
		}
	case "udp6": // local addr, scope, port, pid
		rs, err := rows(b, 28)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			out = append(out, Socket{Proto: proto, PID: binary.LittleEndian.Uint32(r[24:]),
				Local: netip.AddrPortFrom(netip.AddrFrom16([16]byte(r[0:16])), port(r[20:]))})
		}
	default:
		return nil, fmt.Errorf("unknown table %q", proto)
	}
	return out, nil
}

// External reports whether a socket may talk to another machine: a TCP connection
// whose remote end is not loopback, or a UDP socket not bound to loopback (UDP rows
// carry no remote end, so any such socket is suspect). Listeners are not requests.
func (s Socket) External() bool {
	if strings.HasPrefix(s.Proto, "tcp") {
		if s.State == "LISTEN" {
			return false
		}
		a := s.Remote.Addr().Unmap()
		return !a.IsLoopback() && !a.IsUnspecified()
	}
	return !s.Local.Addr().Unmap().IsLoopback()
}

// Seen is a socket of a watched process, aggregated over the polls that saw it.
type Seen struct {
	Socket
	Phase       string // phase of the first poll that saw it
	First, Last time.Time
	Polls       int
}

type seenKey struct {
	proto         string
	pid           uint32
	local, remote netip.AddrPort
}

// Recorder collects the sockets of the watched processes over many polls.
type Recorder struct {
	m     map[seenKey]*Seen
	Polls int
}

// Add records one poll's sockets that belong to pids.
func (r *Recorder) Add(phase string, t time.Time, ss []Socket, pids map[uint32]bool) {
	if r.m == nil {
		r.m = map[seenKey]*Seen{}
	}
	r.Polls++
	for _, s := range ss {
		if !pids[s.PID] {
			continue
		}
		k := seenKey{s.Proto, s.PID, s.Local, s.Remote}
		if e := r.m[k]; e != nil {
			e.Last, e.State = t, s.State
			e.Polls++
			continue
		}
		r.m[k] = &Seen{Socket: s, Phase: phase, First: t, Last: t, Polls: 1}
	}
}

// All returns every socket seen, in order of first sighting.
func (r *Recorder) All() []Seen {
	out := make([]Seen, 0, len(r.m))
	for _, e := range r.m {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].First.Equal(out[j].First) {
			return out[i].First.Before(out[j].First)
		}
		return out[i].Local.String()+out[i].Remote.String() < out[j].Local.String()+out[j].Remote.String()
	})
	return out
}

// NetVars is the engine's "net" debug document (engine.NetVars), decoded here so the
// report builds on every platform.
type NetVars struct {
	Stats netstack.Stats   `json:"stats"`
	Flows netstack.FlowLog `json:"flows"`
}

// ParseNetVars decodes a /debug/vars/net document.
func ParseNetVars(r io.Reader) (NetVars, error) {
	var v NetVars
	dec := json.NewDecoder(io.LimitReader(r, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return NetVars{}, fmt.Errorf("net debug document: %w", err)
	}
	return v, nil
}

// FlowDelta returns what the guest asked for between two flow logs: entries of after
// that are new or counted more often than in before, with Count set to the
// difference. Lost counts are differenced too.
func FlowDelta(before, after netstack.FlowLog) netstack.FlowLog {
	out := netstack.FlowLog{Flows: []netstack.FlowEntry{}, DNS: []netstack.DNSEntry{},
		FlowsLost: after.FlowsLost - before.FlowsLost, DNSLost: after.DNSLost - before.DNSLost}
	fb := map[string]uint64{}
	for _, f := range before.Flows {
		fb[f.Proto+" "+f.Dst+" "+f.Route] = f.Count
	}
	for _, f := range after.Flows {
		if k := f.Proto + " " + f.Dst + " " + f.Route; f.Count > fb[k] {
			f.Count -= fb[k]
			out.Flows = append(out.Flows, f)
		}
	}
	db := map[string]uint64{}
	for _, d := range before.DNS {
		db[d.Name+" "+d.Type] = d.Count
	}
	for _, d := range after.DNS {
		if k := d.Name + " " + d.Type; d.Count > db[k] {
			d.Count -= db[k]
			out.DNS = append(out.DNS, d)
		}
	}
	return out
}

// Phase is one stretch of the run.
type Phase struct {
	Name     string
	Counted  bool // whether it decides the verdict (a pull the bench made does not)
	From, To time.Time
	Guest    netstack.FlowLog
	Frames   [2]uint64 // frames from and to the guest
}

// Report is the bench's result.
type Report struct {
	EnginePID, VMWP uint32
	Phases          []Phase
	Sockets         []Seen
	Polls           int
}

// Failures lists what violates REQ-UX-005: a socket of a watched process that may
// talk to another machine, first seen in a counted phase, and anything the guest asked
// the network for in a counted phase.
//
// An engine socket to an address the guest connected to relays that guest flow, which
// the flow log already places in its phase. Its own first sighting can land in the next
// phase, because the engine dials the host side after the guest's SYN and the socket
// table is polled, so it is judged by the guest flow, not by when it was first seen.
func (r *Report) Failures() []string {
	counted := map[string]bool{}
	guest := map[string]bool{}
	for _, p := range r.Phases {
		counted[p.Name] = p.Counted
		for _, f := range p.Guest.Flows {
			guest[f.Dst] = true
		}
	}
	var out []string
	for _, s := range r.Sockets {
		if s.External() && counted[s.Phase] && (!s.Remote.IsValid() || !guest[s.Remote.String()]) {
			out = append(out, fmt.Sprintf("%s: PID %d %s %s -> %s %s", s.Phase, s.PID, s.Proto, s.Local, s.Remote, s.State))
		}
	}
	for _, p := range r.Phases {
		if !p.Counted {
			continue
		}
		for _, f := range p.Guest.Flows {
			out = append(out, fmt.Sprintf("%s: guest %s %s (%s) x%d", p.Name, f.Proto, f.Dst, f.Route, f.Count))
		}
		for _, d := range p.Guest.DNS {
			out = append(out, fmt.Sprintf("%s: guest DNS %s %s x%d", p.Name, d.Name, d.Type, d.Count))
		}
		if p.Guest.FlowsLost+p.Guest.DNSLost > 0 {
			out = append(out, fmt.Sprintf("%s: %d guest flows and %d DNS queries did not fit the engine's log", p.Name, p.Guest.FlowsLost, p.Guest.DNSLost))
		}
	}
	return out
}

// Print writes the report.
func (r *Report) Print(w io.Writer) {
	pf := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	pf("\nengine PID %d, vmwp PID %d (0: not identified), %d socket table polls\n", r.EnginePID, r.VMWP, r.Polls)
	pf("\nphase       counted  duration  guest flows  guest DNS  frames in/out\n")
	for _, p := range r.Phases {
		pf("%-11s %-8v %8.0fs  %11d  %9d  %d/%d\n", p.Name, p.Counted, p.To.Sub(p.From).Seconds(),
			len(p.Guest.Flows), len(p.Guest.DNS), p.Frames[0], p.Frames[1])
		for _, f := range p.Guest.Flows {
			pf("    guest %s %s (%s) x%d\n", f.Proto, f.Dst, f.Route, f.Count)
		}
		for _, d := range p.Guest.DNS {
			note := ""
			if !strings.Contains(strings.TrimSuffix(d.Name, "."), ".") {
				note = " (single label: Windows may also ask LLMNR, mDNS and NetBIOS)"
			}
			pf("    guest DNS %s %s x%d%s\n", d.Name, d.Type, d.Count, note)
		}
	}
	// The socket tables cannot show these: the engine resolves with
	// net.DefaultResolver, whose packets the DNS Client service (svchost) sends.
	pf("guest DNS names are looked up by the engine through Windows' DNS Client service,\n")
	pf("whose packets are not the engine's sockets; the engine makes no other lookups.\n")
	pf("\nsockets of the engine (and its children, and this VM's vmwp): %d\n", len(r.Sockets))
	for _, s := range r.Sockets {
		ext := ""
		if s.External() {
			ext = "  EXTERNAL"
		}
		rem := ""
		if s.Remote.IsValid() {
			rem = " -> " + s.Remote.String()
		}
		pf("    %-6s PID %-6d %s%s %s, first in %s, %d polls%s\n", s.Proto, s.PID, s.Local, rem, s.State, s.Phase, s.Polls, ext)
	}
	if f := r.Failures(); len(f) > 0 {
		pf("\nFAIL: REQ-UX-005, requests of hyperbutler's own:\n")
		for _, s := range f {
			pf("    %s\n", s)
		}
		return
	}
	pf("\nPASS: REQ-UX-005, no request of hyperbutler's own was seen\n")
}

// noVMWP decides what a run does when this VM's vmwp could not be identified (err): the
// VM worker's own sockets would go unwatched, so that is a failure unless allowed
// (-allow-no-vmwp).
func noVMWP(err error, allowed bool) error {
	if allowed {
		return nil
	}
	return fmt.Errorf("FAIL: this VM's vmwp was not identified, so its sockets cannot be watched: %w (-allow-no-vmwp runs the check without them)", err)
}
