// Package socktable reads the Windows TCP and UDP tables (GetExtendedTcpTable and
// GetExtendedUdpTable in iphlpapi, with the owning process of every row). Reading them
// opens no socket and needs no privileges: it is how the engine tells whether a host
// port is taken without binding it (a wildcard bind can raise the Windows Firewall
// prompt), and how bench/no-telemetry watches the engine's connections.
//
// The decoding is portable so its tests run everywhere; only Reader is Windows-only.
package socktable

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"strings"
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
// MIB_UDPTABLE_OWNER_PID or MIB_UDP6TABLE_OWNER_PID, by proto. The listener-only TCP
// tables (TCP_TABLE_OWNER_PID_LISTENER) have the same layout.
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

// Holder returns the first TCP listener on port p, on any local address, and whether
// there is one. Every address counts: the wildcard (0.0.0.0, ::), loopback, and a
// specific interface address, because each of them takes the port away from some
// clients.
func Holder(ss []Socket, p uint16) (Socket, bool) {
	for _, s := range ss {
		if strings.HasPrefix(s.Proto, "tcp") && s.State == "LISTEN" && s.Local.Port() == p {
			return s, true
		}
	}
	return Socket{}, false
}
