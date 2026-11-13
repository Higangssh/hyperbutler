package engine

import (
	"cmp"
	"net/netip"
	"slices"
	"strings"

	"github.com/Higangssh/hyperbutler/internal/socktable"
)

// A port the engine has published can be listened on later by another program: Windows
// lets a wildcard (0.0.0.0, [::]) or LAN-address bind succeed next to the engine's
// 127.0.0.1 and [::1] listeners, even with SO_EXCLUSIVEADDRUSE on them (checked on the
// reference machine), and gives connections to a loopback address to the more specific
// listener, the engine's. That program, Docker Desktop publishing the same port for
// one, sees no error and never gets localhost traffic (issue #2). Nothing can refuse its
// bind short of holding the wildcard ourselves (a firewall prompt, and LAN exposure
// that REQ-NET-003 keeps off), so `status` names it (REQ-NET-005).

// SharedPort is another process listening on a port the engine publishes, on one or
// more local addresses.
type SharedPort struct {
	Port   uint16
	PID    uint32
	Listen []netip.AddrPort // sorted
}

// MaxSharedPorts bounds what SharedPorts returns.
const MaxSharedPorts = 64

// SharedPorts lists, by port and process, the TCP listeners in ss of processes other
// than engine on the ports in published (State.PublishedPorts), sorted by port, then
// PID. It returns at most MaxSharedPorts entries, and how many more there were.
func SharedPorts(ss []socktable.Socket, engine uint32, published []uint16) (out []SharedPort, more int) {
	if engine == 0 || len(published) == 0 {
		return nil, 0
	}
	type key struct {
		port uint16
		pid  uint32
	}
	byKey := map[key][]netip.AddrPort{}
	for _, s := range ss {
		if !strings.HasPrefix(s.Proto, "tcp") || s.State != "LISTEN" || s.PID == engine || s.PID == 0 {
			continue
		}
		if p := s.Local.Port(); slices.Contains(published, p) {
			k := key{p, s.PID}
			byKey[k] = append(byKey[k], s.Local)
		}
	}
	for k, addrs := range byKey {
		slices.SortFunc(addrs, func(a, b netip.AddrPort) int { return a.Compare(b) })
		out = append(out, SharedPort{Port: k.port, PID: k.pid, Listen: slices.Compact(addrs)})
	}
	slices.SortFunc(out, func(a, b SharedPort) int {
		return cmp.Or(cmp.Compare(a.Port, b.Port), cmp.Compare(a.PID, b.PID))
	})
	if len(out) > MaxSharedPorts {
		more = len(out) - MaxSharedPorts
		out = out[:MaxSharedPorts]
	}
	return out, more
}
