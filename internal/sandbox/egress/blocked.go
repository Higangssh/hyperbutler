package egress

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// maxBlocked bounds the blocked log. The guest chooses the names, so a guest that asks for
// many only fills the table and then counts what did not fit.
const maxBlocked = 256

// maxShownName is how much of a name the log keeps, before escaping. A server name in a
// ClientHello can be far longer than any DNS name.
const maxShownName = 255

// What a BlockedName's Kind says was refused.
const (
	// KindQuery is a DNS question for a name not on the list.
	KindQuery = "query"
	// KindQueryType is a DNS question of another type than A, AAAA and CNAME.
	KindQueryType = "query-type"
	// KindWildcardCap is a new name of a wildcard entry past Options.MaxWildcardNames.
	KindWildcardCap = "wildcard-cap"
	// KindPrivateAnswer is an address dropped from an allowed name's answer.
	KindPrivateAnswer = "private-answer"
	// KindAddress is a connection to an address no answer gave (Name is the address and
	// port).
	KindAddress = "address"
	// KindPort is a connection to a pinned address on a port no entry names.
	KindPort = "port"
	// KindServerName is a ClientHello's name that its address is not pinned for.
	KindServerName = "server-name"
	// KindNoServerName is a ClientHello without one (Name is the address and port, as for
	// the kinds below).
	KindNoServerName = "no-server-name"
	// KindECH is a ClientHello with an encrypted server name.
	KindECH = "ech"
	// KindNotTLS is something other than TLS on 443.
	KindNotTLS = "not-tls"
	// KindBadHello is a ClientHello a strict parser refuses: bad lengths, several names.
	KindBadHello = "bad-hello"
	// KindHTTPHost is a request on 80 whose Host is not pinned for its address (Name is
	// the Host), or which is malformed or has not exactly one Host (the address and port).
	KindHTTPHost = "http-host"
	// KindUDP is a UDP flow, which includes QUIC.
	KindUDP = "udp"
)

// BlockedName is one thing that was refused, and how often. Name is safe to show: only
// printable ASCII, every other byte and the backslash as \xHH, and cut to a bounded
// length. It is what the guest asked for, so it can still say anything in those letters.
type BlockedName struct {
	Name  string
	Kind  string
	Count uint64
	First time.Time
	Last  time.Time
}

type blockedKey struct{ name, kind string }

type blockedEntry struct {
	BlockedName
	seq uint64
}

type blockedLog struct {
	entries map[blockedKey]*blockedEntry
	seq     uint64
	lost    uint64
}

func (l *blockedLog) add(name, kind string, now time.Time) {
	shown := escapeName(lowerASCII(name))
	k := blockedKey{shown, kind}
	if e := l.entries[k]; e != nil {
		e.Count++
		e.Last = now
		return
	}
	if len(l.entries) >= maxBlocked {
		l.lost++
		return
	}
	if l.entries == nil {
		l.entries = map[blockedKey]*blockedEntry{}
	}
	l.entries[k] = &blockedEntry{BlockedName: BlockedName{Name: shown, Kind: kind, Count: 1, First: now, Last: now}, seq: l.seq}
	l.seq++
}

func (l *blockedLog) snapshot() []BlockedName {
	entries := make([]*blockedEntry, 0, len(l.entries))
	for _, e := range l.entries {
		entries = append(entries, e)
	}
	// By insertion: First may repeat at the clock's resolution.
	sort.Slice(entries, func(i, j int) bool { return entries[i].seq < entries[j].seq })
	out := make([]BlockedName, len(entries))
	for i, e := range entries {
		out[i] = e.BlockedName
	}
	return out
}

// escapeName returns s as it may be logged and shown (see BlockedName).
func escapeName(s string) string {
	full := len(s)
	if full > maxShownName {
		s = s[:maxShownName]
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c > 0x20 && c < 0x7f && c != '\\' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, `\x%02x`, c)
	}
	if full > maxShownName {
		fmt.Fprintf(&b, "...(%d bytes)", full)
	}
	return b.String()
}
