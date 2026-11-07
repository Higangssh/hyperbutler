package egress

import (
	"crypto/tls"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/Higangssh/hyperbutler/internal/netstack"
)

var (
	addrNPM   = netip.MustParseAddr("104.16.0.1")
	addrOther = netip.MustParseAddr("151.101.0.1")
)

// clock is a settable Options.Now.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func entries(t *testing.T, specs ...string) []Entry {
	t.Helper()
	out := make([]Entry, 0, len(specs))
	for _, s := range specs {
		e, err := ParseEntry(s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		out = append(out, e)
	}
	return out
}

func newPolicy(t *testing.T, mode Mode, c *clock, specs ...string) *Policy {
	t.Helper()
	p, err := New(mode, entries(t, specs...), Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func ap(a netip.Addr, port uint16) netip.AddrPort { return netip.AddrPortFrom(a, port) }

func blockedKinds(p *Policy) map[string]string {
	out := map[string]string{}
	for _, b := range p.Blocked() {
		out[b.Kind] = b.Name
	}
	return out
}

func TestParseMode(t *testing.T) {
	for _, m := range []Mode{Open, Dev, Locked} {
		if got, err := ParseMode(string(m)); err != nil || got != m {
			t.Errorf("%q: %v %v", m, got, err)
		}
	}
	for _, s := range []string{"", "Open", "dev ", "off", "\x00"} {
		if _, err := ParseMode(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestNewAndHook(t *testing.T) {
	if _, err := New(Open, nil, Options{}); err == nil || !strings.Contains(err.Error(), "nil hook") {
		t.Fatalf("New(Open): %v", err)
	}
	if _, err := New(Mode("x"), nil, Options{}); err == nil {
		t.Fatal("unknown mode accepted")
	}
	if _, err := New(Mode(""), nil, Options{}); err == nil {
		t.Fatal("empty mode accepted")
	}
	if _, err := New(Dev, nil, Options{PinTTL: -1}); err == nil {
		t.Fatal("negative option accepted")
	}
	if _, err := New(Dev, []Entry{{Name: "UPPER.example"}}, Options{}); err == nil {
		t.Fatal("non-canonical entry accepted")
	}

	h, p, err := Hook(Open, nil, Options{})
	if err != nil || p != nil {
		t.Fatalf("Hook(Open): %v %v", p, err)
	}
	if h != nil {
		t.Fatalf("Hook(Open) leaked a typed nil: %#v", h)
	}
	cfg := netstack.Config{Egress: h}
	if cfg.Egress != nil {
		t.Fatal("a nil hook is not nil in the config")
	}

	h, p, err = Hook(Dev, DevPreset(), Options{})
	if err != nil || h == nil || p == nil || h.(*Policy) != p {
		t.Fatalf("Hook(Dev): %v %v %v", h, p, err)
	}
	h, p, err = Hook(Mode("bad"), nil, Options{})
	if err == nil || h != nil || p != nil {
		t.Fatalf("Hook(bad): %v %v %v", h, p, err)
	}
}

func TestParseEntry(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Entry
	}{
		{"example.com", Entry{Name: "example.com"}},
		{"EXAMPLE.Com.", Entry{Name: "example.com"}},
		{"example.com:8443", Entry{Name: "example.com", Port: 8443}},
		{"example.com.:22", Entry{Name: "example.com", Port: 22}},
		{"*.example.com", Entry{Name: "example.com", Wildcard: true}},
		{"lan:nas.home", Entry{Name: "nas.home", LAN: true}},
		{"LAN:nas", Entry{Name: "nas", LAN: true}},
		{"lan", Entry{Name: "lan"}},
		{"lan.example.com", Entry{Name: "lan.example.com"}},
		{"xn--bcher-kva.example", Entry{Name: "xn--bcher-kva.example"}},
		{"a-b.c1.example", Entry{Name: "a-b.c1.example"}},
	} {
		got, err := ParseEntry(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("%q: %+v %v, want %+v", tc.in, got, err, tc.want)
			continue
		}
		back, err := ParseEntry(got.String())
		if err != nil || back != got {
			t.Errorf("%q: %q does not read back: %+v %v", tc.in, got, back, err)
		}
	}
	long := strings.Repeat("a", 64) + ".example"
	huge := strings.Repeat("abc.", 70) + "example"
	for _, in := range []string{
		"", ".", "..", "a..b", "a.b..", "*", "*.", "*.com", "*.*.example.com", "a*.example.com", "*a.example.com",
		"-a.example", "a-.example", "a_b.example", "a b.example", long, huge, strings.Repeat("a", 5000),
		"127.0.0.1", "1.2.3.4:80", "127.1", "0x7f.1", "[::1]", "[::1]:22", "::1", "lan:10.0.0.1",
		"example.com:", "example.com:0", "example.com:65536", "example.com:080", "example.com:-1", "example.com:http", "a:b:80",
		"*.example.com:443", "lan:nas:8080", "lan:*.example.com", "lan:", "lan:22", "lan:lan:x", "lan.:22",
		"exa\x00mple.com", "example.com\n", "http://example.com", "user@example.com", "example.com/path", "a\\.example",
	} {
		if e, err := ParseEntry(in); err == nil {
			t.Errorf("%q accepted: %+v", in, e)
		}
	}
	// Unicode: refused, pointed at punycode. The Kelvin sign is not turned into a k.
	for _, in := range []string{"bücher.example", "exaKple.com", "аpple.com", "lan:bücher.example"} {
		_, err := ParseEntry(in)
		if err == nil || !strings.Contains(err.Error(), "punycode") {
			t.Errorf("%q: %v", in, err)
		}
		if err != nil && !isPrintableASCII(err.Error()) {
			t.Errorf("%q: error has raw bytes: %q", in, err.Error())
		}
	}
}

// A wildcard on a zone where anyone can get a name would allow every tenant.
func TestWildcardOnPublicSuffixRefused(t *testing.T) {
	for _, in := range []string{"*.co.uk", "*.github.io", "*.s3.amazonaws.com", "*.pages.dev"} {
		if e, err := ParseEntry(in); err == nil {
			t.Errorf("%q accepted: %+v", in, e)
		}
	}
	for _, in := range []string{"*.example.com", "*.foo.github.io", "*.example.co.uk", "*.corp.home"} {
		if _, err := ParseEntry(in); err != nil {
			t.Errorf("%q: %v", in, err)
		}
	}
	// The exact names are not wildcards, and Allow makes the same check.
	for _, in := range []string{"co.uk", "github.io"} {
		if _, err := ParseEntry(in); err != nil {
			t.Errorf("%q: %v", in, err)
		}
	}
	p := newPolicy(t, Dev, newClock())
	if err := p.Allow(Entry{Name: "github.io", Wildcard: true}); err == nil {
		t.Error("Allow took a wildcard on a public suffix")
	}
}

func TestBlockedLogSurvivesPortScan(t *testing.T) {
	if blockedReserved() > maxBlocked {
		t.Fatalf("the shares add up to %d", blockedReserved())
	}
	c := newClock()
	p := newPolicy(t, Dev, c, "a.example")
	// One address on many ports is one line.
	target := netip.MustParseAddr("203.0.113.9")
	for port := range 300 {
		p.Conn(ap(target, uint16(port+1)))
	}
	b := p.Blocked()
	if len(b) != 1 || b[0].Name != target.String() || b[0].Kind != KindAddress || b[0].Count != 300 {
		t.Fatalf("%+v", b)
	}
	// Scans that change the key, of each kind a scan can reach: the table stays bounded.
	p.Answer("a.example", []netip.Addr{addrNPM})
	for i := range 600 {
		p.Conn(ap(netip.AddrFrom4([4]byte{203, 0, byte(i >> 8), byte(i)}), 443))
		p.Conn(ap(addrNPM, uint16(i+1000)))
		p.UDP(ap(addrNPM, uint16(i+1000)))
	}
	if n := len(p.Blocked()); n > maxBlocked {
		t.Fatalf("%d entries", n)
	}
	if p.BlockedLost() == 0 {
		t.Fatal("nothing was lost; the scan did not fill the table")
	}
	// A later name is still recorded.
	p.AllowQuery("later.evil.example", dns.TypeA)
	p.Inspect(ap(addrNPM, 443), hello(sniExt("later-sni.example")))
	found := map[string]bool{}
	for _, e := range p.Blocked() {
		found[e.Name] = true
	}
	if !found["later.evil.example"] || !found["later-sni.example"] {
		t.Fatalf("later names were lost: %+v", p.Blocked())
	}
	// The share of query names is not the whole table's: they fill what is free.
	q := newPolicy(t, Dev, c)
	for i := range maxBlocked + 50 {
		q.AllowQuery(fmt.Sprintf("n%d.example", i), dns.TypeA)
	}
	want := maxBlocked - blockedReserved() + blockedReserve(KindQuery)
	if n := len(q.Blocked()); n != want {
		t.Fatalf("%d query names, want %d", n, want)
	}
	q.Conn(ap(addrNPM, 443))
	if blockedKinds(q)[KindAddress] == "" {
		t.Fatal("a flood of query names took the share of addresses")
	}
}

func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] >= 0x7f {
			return false
		}
	}
	return true
}

func TestDevPreset(t *testing.T) {
	preset := DevPreset()
	seen := map[Entry]bool{}
	for _, e := range preset {
		if e.Wildcard || e.LAN {
			t.Errorf("%v: the preset has exact names only", e)
		}
		if err := e.validate(); err != nil {
			t.Errorf("%v: %v", e, err)
		}
		if seen[e] {
			t.Errorf("%v is listed twice", e)
		}
		seen[e] = true
	}
	for _, e := range []Entry{{Name: "registry.npmjs.org"}, {Name: "files.pythonhosted.org"}, {Name: "ghcr.io"}, {Name: "generativelanguage.googleapis.com"}, {Name: "github.com"}, {Name: "github.com", Port: 22}} {
		if !seen[e] {
			t.Errorf("%v is missing", e)
		}
	}
	if len(preset) != 26 {
		t.Errorf("%d entries", len(preset))
	}
	preset[0].Name = "changed"
	if DevPreset()[0].Name == "changed" {
		t.Error("DevPreset shares its slice")
	}
}

func TestAllowQuery(t *testing.T) {
	for _, mode := range []Mode{Dev, Locked} {
		t.Run(string(mode), func(t *testing.T) {
			p := newPolicy(t, mode, newClock(), "a.example", "b.example:22")
			for _, q := range []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeCNAME} {
				if !p.AllowQuery("a.example", q) || !p.AllowQuery("A.EXAMPLE.", q) || !p.AllowQuery("b.example", q) {
					t.Errorf("type %d of an allowed name refused", q)
				}
				for _, name := range []string{"sub.a.example", "aa.example", "example", "a.example.evil.test", "", ".", "a.example..", "a\x00.example", "127.0.0.1"} {
					if p.AllowQuery(name, q) {
						t.Errorf("%q type %d allowed", name, q)
					}
				}
			}
			for _, q := range []uint16{dns.TypeMX, dns.TypeNS, dns.TypeSRV, dns.TypeTXT, dns.TypePTR, dns.TypeSOA, dns.TypeHTTPS, dns.TypeSVCB, dns.TypeANY, dns.TypeNULL, 0} {
				if p.AllowQuery("a.example", q) {
					t.Errorf("type %d of an allowed name allowed", q)
				}
			}
			if blockedKinds(p)[KindQueryType] == "" || blockedKinds(p)[KindQuery] == "" {
				t.Errorf("not logged: %+v", p.Blocked())
			}
		})
	}
}

func TestWildcard(t *testing.T) {
	c := newClock()
	p, err := New(Dev, entries(t, "*.tenant.example", "named.tenant.example:22"), Options{Now: c.now, MaxWildcardNames: 2})
	if err != nil {
		t.Fatal(err)
	}
	q := func(name string) bool { return p.AllowQuery(name, dns.TypeA) }
	if q("tenant.example") {
		t.Error("a wildcard covers its zone's own name")
	}
	if q("xtenant.example") || q("tenant.example.evil.test") {
		t.Error("a wildcard covers a name outside its zone")
	}
	if !q("a.tenant.example") || !q("b.c.tenant.example") {
		t.Fatal("names under the wildcard refused")
	}
	if !p.AllowQuery("a.tenant.example", dns.TypeAAAA) || !q("A.tenant.example.") {
		t.Fatal("a name already seen is refused: it costs nothing to repeat")
	}
	if q("over.tenant.example") {
		t.Fatal("a third name went past the cap")
	}
	// The refused query types do not spend the cap, and past the cap nothing is pinned.
	if got := p.Answer("over.tenant.example", []netip.Addr{addrNPM}); len(got) != 0 {
		t.Fatalf("answer past the cap: %v", got)
	}
	if p.Conn(ap(addrNPM, 443)) != netstack.ConnDeny {
		t.Fatal("pinned past the cap")
	}
	for _, b := range p.Blocked() {
		if b.Kind == KindWildcardCap && (b.Name != "over.tenant.example" || b.Count != 2) {
			t.Fatalf("%+v", b)
		}
	}
	if blockedKinds(p)[KindWildcardCap] == "" {
		t.Fatalf("the cap was not counted: %+v", p.Blocked())
	}
	// An exact entry for a name under the full wildcard still resolves.
	if !q("named.tenant.example") {
		t.Fatal("an exact entry was refused by a full wildcard")
	}
	// The refused query type does not spend room in a new wildcard.
	p2, _ := New(Dev, entries(t, "*.tenant.example"), Options{Now: c.now, MaxWildcardNames: 1})
	p2.AllowQuery("x.tenant.example", dns.TypeTXT)
	if !p2.AllowQuery("y.tenant.example", dns.TypeA) {
		t.Fatal("a refused type spent the cap")
	}

	// The default cap.
	p3 := newPolicy(t, Dev, c, "*.tenant.example")
	allowed := 0
	for i := range 100 {
		if p3.AllowQuery(fmt.Sprintf("n%d.tenant.example", i), dns.TypeA) {
			allowed++
		}
	}
	if allowed != DefaultMaxWildcardNames {
		t.Fatalf("%d names allowed", allowed)
	}
}

func TestWildcardReachesWeb(t *testing.T) {
	p := newPolicy(t, Dev, newClock(), "*.tenant.example")
	if !p.AllowQuery("a.tenant.example", dns.TypeA) {
		t.Fatal("refused")
	}
	p.Answer("a.tenant.example", []netip.Addr{addrNPM})
	if p.Inspect(ap(addrNPM, 443), hello(sniExt("a.tenant.example"))) != netstack.InspectAllowTLS {
		t.Fatal("a wildcard name was refused")
	}
	if p.Inspect(ap(addrNPM, 443), hello(sniExt("b.tenant.example"))) != netstack.InspectDeny {
		t.Fatal("a name of the same wildcard, never answered for this address, was allowed")
	}
}

func TestAnswerFilters(t *testing.T) {
	p := newPolicy(t, Dev, newClock(), "pub.example", "lan:nas.home")
	in := []string{
		"10.0.0.1", "172.16.0.1", "192.168.1.1", "127.0.0.1", "169.254.169.254", "100.64.0.1", "100.127.255.254",
		"0.0.0.1", "198.18.0.1", "240.0.0.1", "::ffff:10.0.0.1", "::ffff:127.0.0.1", "::1", "fe80::1", "fc00::1", "fd12::1",
		"64:ff9b::a00:1", "2002:a00:1::", "::a00:1", "2001::1",
	}
	var addrs []netip.Addr
	for _, s := range in {
		addrs = append(addrs, netip.MustParseAddr(s))
	}
	if got := p.Answer("pub.example", addrs); len(got) != 0 {
		t.Fatalf("private answers kept: %v", got)
	}
	for _, a := range addrs {
		if p.Conn(ap(a, 443)) != netstack.ConnDeny {
			t.Errorf("%s was pinned", a)
		}
	}
	if blockedKinds(p)[KindPrivateAnswer] != "pub.example" {
		t.Errorf("not logged: %+v", p.Blocked())
	}

	mapped := netip.MustParseAddr("::ffff:93.184.215.14")
	got := p.Answer("pub.example", []netip.Addr{addrNPM, mapped, netip.MustParseAddr("2606:2800::1"), netip.MustParseAddr("10.0.0.1")})
	want := []netip.Addr{addrNPM, netip.MustParseAddr("93.184.215.14"), netip.MustParseAddr("2606:2800::1")}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%v, want %v", got, want)
		}
	}
	// A pin made in one spelling holds in the other.
	if p.Conn(ap(netip.MustParseAddr("93.184.215.14"), 443)) != netstack.ConnInspect || p.Conn(ap(mapped, 443)) != netstack.ConnInspect {
		t.Fatal("IPv4-mapped form not unmapped")
	}

	// lan: keeps them, but nothing that is no one's address.
	got = p.Answer("nas.home", []netip.Addr{netip.MustParseAddr("192.168.1.10"), netip.MustParseAddr("0.0.0.0"), netip.MustParseAddr("224.0.0.1"), netip.MustParseAddr("255.255.255.255"), netip.MustParseAddr("ff02::1")})
	if len(got) != 1 || got[0] != netip.MustParseAddr("192.168.1.10") {
		t.Fatalf("lan: %v", got)
	}
	if p.Answer("unknown.example", []netip.Addr{addrOther}) != nil || p.Conn(ap(addrOther, 443)) != netstack.ConnDeny {
		t.Fatal("a name not on the list was pinned")
	}
}

func TestConnRules(t *testing.T) {
	for _, tc := range []struct {
		mode Mode
		port uint16
		want netstack.ConnDecision
	}{
		{Dev, 443, netstack.ConnInspect},
		{Dev, 80, netstack.ConnInspect},
		{Dev, 22, netstack.ConnAllow},
		{Dev, 53, netstack.ConnDeny},
		{Dev, 853, netstack.ConnDeny},
		{Dev, 8443, netstack.ConnDeny},
		{Dev, 23, netstack.ConnDeny},
		{Locked, 443, netstack.ConnInspect},
		{Locked, 80, netstack.ConnDeny},
		{Locked, 22, netstack.ConnAllow},
		{Locked, 53, netstack.ConnDeny},
	} {
		p := newPolicy(t, tc.mode, newClock(), "git.example", "git.example:22")
		p.Answer("git.example", []netip.Addr{addrNPM})
		if got := p.Conn(ap(addrNPM, tc.port)); got != tc.want {
			t.Errorf("%s port %d: %d, want %d", tc.mode, tc.port, got, tc.want)
		}
		// Unpinned: denied on every port.
		if got := p.Conn(ap(addrOther, tc.port)); got != netstack.ConnDeny {
			t.Errorf("%s unpinned port %d: %d", tc.mode, tc.port, got)
		}
	}
}

// host:port is by address only: any name pinned there with that port opens it for the
// address, and a name without that port does not.
func TestConnPortByAddress(t *testing.T) {
	p := newPolicy(t, Dev, newClock(), "ssh.example:2222", "web.example")
	p.Answer("web.example", []netip.Addr{addrNPM})
	if p.Conn(ap(addrNPM, 2222)) != netstack.ConnDeny {
		t.Fatal("a name without that port opened it")
	}
	p.Answer("ssh.example", []netip.Addr{addrNPM})
	if p.Conn(ap(addrNPM, 2222)) != netstack.ConnAllow {
		t.Fatal("refused")
	}
}

// An entry with a port says nothing about the web ports.
func TestPortEntryDoesNotOpenWeb(t *testing.T) {
	p := newPolicy(t, Dev, newClock(), "ssh.example:22", "tls.example:443")
	p.Answer("ssh.example", []netip.Addr{addrNPM})
	p.Answer("tls.example", []netip.Addr{addrNPM})
	if p.Inspect(ap(addrNPM, 443), hello(sniExt("ssh.example"))) != netstack.InspectDeny {
		t.Fatal("ssh.example:22 opened 443")
	}
	if p.Inspect(ap(addrNPM, 443), hello(sniExt("tls.example"))) != netstack.InspectAllowTLS {
		t.Fatal("tls.example:443 did not open 443")
	}
	if p.Inspect(ap(addrNPM, 80), []byte("GET / HTTP/1.1\r\nHost: tls.example\r\n\r\n")) != netstack.InspectDeny {
		t.Fatal("tls.example:443 opened 80")
	}
}

func TestPinExpiryAndRenewal(t *testing.T) {
	c := newClock()
	p := newPolicy(t, Dev, c, "a.example", "a.example:22")
	p.Answer("a.example", []netip.Addr{addrNPM})
	c.advance(DefaultPinTTL - time.Second)
	// An accepted connection renews.
	if p.Inspect(ap(addrNPM, 443), hello(sniExt("a.example"))) != netstack.InspectAllowTLS {
		t.Fatal("refused inside the window")
	}
	c.advance(DefaultPinTTL - time.Second)
	if p.Conn(ap(addrNPM, 22)) != netstack.ConnAllow {
		t.Fatal("refused inside the renewed window")
	}
	c.advance(DefaultPinTTL - time.Second)
	// So does an answer.
	p.Answer("a.example", []netip.Addr{addrNPM})
	c.advance(DefaultPinTTL - time.Second)
	if p.Conn(ap(addrNPM, 443)) != netstack.ConnInspect {
		t.Fatal("refused after an answer renewed it")
	}
	// The Conn just above decided nothing, a refused hello neither: neither renews.
	if p.Inspect(ap(addrNPM, 443), hello(sniExt("b.example"))) != netstack.InspectDeny {
		t.Fatal("wrong name allowed")
	}
	c.advance(time.Second)
	if p.Conn(ap(addrNPM, 443)) != netstack.ConnDeny || p.Inspect(ap(addrNPM, 443), hello(sniExt("a.example"))) != netstack.InspectDeny {
		t.Fatal("the pin outlived its window")
	}
	if p.Conn(ap(addrNPM, 22)) != netstack.ConnDeny {
		t.Fatal("the pin outlived its window on another port")
	}
}

func TestPinsPerName(t *testing.T) {
	c := newClock()
	p := newPolicy(t, Dev, c, "a.example", "b.example")
	p.Answer("a.example", []netip.Addr{addrNPM})
	c.advance(DefaultPinTTL / 2)
	p.Answer("b.example", []netip.Addr{addrNPM})
	c.advance(DefaultPinTTL / 2)
	if p.Inspect(ap(addrNPM, 443), hello(sniExt("a.example"))) != netstack.InspectDeny {
		t.Fatal("a.example's pin expired, b.example's renewal must not keep it")
	}
	if p.Inspect(ap(addrNPM, 443), hello(sniExt("b.example"))) != netstack.InspectAllowTLS {
		t.Fatal("b.example is still pinned")
	}
}

func TestPinsBounded(t *testing.T) {
	c := newClock()
	p, err := New(Dev, entries(t, "a.example"), Options{Now: c.now, MaxPins: 3})
	if err != nil {
		t.Fatal(err)
	}
	addr := func(i int) netip.Addr { return netip.AddrFrom4([4]byte{8, 0, 0, byte(i)}) }
	for i := range 3 {
		p.Answer("a.example", []netip.Addr{addr(i)})
	}
	p.Answer("a.example", []netip.Addr{addr(0)}) // renewed: 1 is now the oldest
	p.Answer("a.example", []netip.Addr{addr(3)})
	if p.Conn(ap(addr(1), 443)) != netstack.ConnDeny {
		t.Fatal("the oldest pin was kept")
	}
	for _, i := range []int{0, 2, 3} {
		if p.Conn(ap(addr(i), 443)) != netstack.ConnInspect {
			t.Fatalf("pin %d dropped", i)
		}
	}
	if n := p.pins.order.Len(); n != 3 {
		t.Fatalf("%d pins", n)
	}
	// One answer larger than the table.
	var many []netip.Addr
	for i := range 200 {
		many = append(many, addr(i+10))
	}
	p.Answer("a.example", many)
	if n := p.pins.order.Len(); n != 3 || len(p.pins.byAddr) != 3 {
		t.Fatalf("%d pins, %d addresses", n, len(p.pins.byAddr))
	}
}

func TestPinTableSweeps(t *testing.T) {
	c := newClock()
	p := newPolicy(t, Dev, c, "a.example")
	for i := range 100 {
		p.Answer("a.example", []netip.Addr{netip.AddrFrom4([4]byte{8, 0, 0, byte(i)})})
	}
	c.advance(DefaultPinTTL)
	p.Answer("a.example", []netip.Addr{addrNPM})
	if n := p.pins.order.Len(); n != 1 || len(p.pins.byAddr) != 1 {
		t.Fatalf("%d pins and %d addresses left", n, len(p.pins.byAddr))
	}
}

func TestAllowWidensAtRuntime(t *testing.T) {
	p := newPolicy(t, Locked, newClock())
	if p.AllowQuery("a.example", dns.TypeA) {
		t.Fatal("empty locked policy allowed a name")
	}
	if err := p.Allow(Entry{Name: "a.example"}); err != nil {
		t.Fatal(err)
	}
	if !p.AllowQuery("a.example", dns.TypeA) {
		t.Fatal("not widened")
	}
	for _, bad := range []Entry{{}, {Name: "A.example"}, {Name: "a.example."}, {Name: "x\x00y"}, {Name: "com", Wildcard: true}, {Name: "a.example", Wildcard: true, Port: 1}, {Name: "a.example", LAN: true, Port: 1}} {
		if err := p.Allow(bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	// A later lan: entry of the same name keeps private answers from then on.
	if got := p.Answer("a.example", []netip.Addr{netip.MustParseAddr("10.0.0.1")}); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	if err := p.Allow(Entry{Name: "a.example", LAN: true}); err != nil {
		t.Fatal(err)
	}
	if got := p.Answer("a.example", []netip.Addr{netip.MustParseAddr("10.0.0.1")}); len(got) != 1 {
		t.Fatalf("%v", got)
	}
}

func TestConcurrentUse(t *testing.T) {
	p := newPolicy(t, Dev, newClock(), "a.example", "*.b.example")
	var wg sync.WaitGroup
	h := hello(sniExt("a.example"))
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range 200 {
				name := fmt.Sprintf("n%d.b.example", k%8)
				p.AllowQuery(name, dns.TypeA)
				p.Answer(name, []netip.Addr{addrNPM})
				p.Answer("a.example", []netip.Addr{addrNPM, addrOther})
				p.Conn(ap(addrNPM, 443))
				p.Conn(ap(addrOther, 22))
				p.Inspect(ap(addrNPM, 443), h)
				p.UDP(ap(addrNPM, 443))
				_ = p.Allow(Entry{Name: fmt.Sprintf("c%d.example", i)})
				_ = p.Blocked()
			}
		}()
	}
	wg.Wait()
}

func TestBlockedLog(t *testing.T) {
	c := newClock()
	p := newPolicy(t, Dev, c)
	p.AllowQuery("Evil.Example", dns.TypeA)
	c.advance(time.Second)
	p.AllowQuery("evil.example.", dns.TypeA)
	b := p.Blocked()
	if len(b) != 1 || b[0].Name != "evil.example" || b[0].Kind != KindQuery || b[0].Count != 2 || !b[0].Last.Equal(b[0].First.Add(time.Second)) {
		t.Fatalf("%+v", b)
	}

	// Hostile names come out as printable ASCII, whatever they held.
	p = newPolicy(t, Dev, c)
	p.AllowQuery("a\x00b\nc\x1b[31m\xe2\x80\xaed.\\example", dns.TypeA)
	p.Answer("\xff\xfe", nil)
	h := hello(sniExt("x\x07y\xf0\x9f\x98\x80\r\n.example"))
	p.Answer("x.example", nil)
	p.Inspect(ap(addrNPM, 443), h)
	p.Inspect(ap(addrNPM, 443), hello(sniExt(strings.Repeat("a", 10000))))
	p.Inspect(ap(addrNPM, 80), []byte("GET / HTTP/1.1\r\nHost: \x80\xff\r\n\r\n"))
	if len(p.Blocked()) < 4 {
		t.Fatalf("%+v", p.Blocked())
	}
	for _, e := range p.Blocked() {
		if !isPrintableASCII(e.Name) {
			t.Errorf("raw bytes: %q", e.Name)
		}
		if len(e.Name) > maxShownName*4+32 {
			t.Errorf("%d bytes kept", len(e.Name))
		}
		if strings.Contains(e.Name, `\`) && !strings.Contains(e.Name, `\x`) {
			t.Errorf("a backslash that is not an escape: %q", e.Name)
		}
	}
	if got := escapeName("a\\b c\x00"); got != `a\x5cb\x20c\x00` {
		t.Errorf("escape: %q", got)
	}

	// Bounded, and the rest is counted. What is in the table keeps counting.
	p = newPolicy(t, Dev, c)
	for i := range maxBlocked + 50 {
		p.AllowQuery(fmt.Sprintf("n%d.example", i), dns.TypeA)
	}
	kept := len(p.Blocked())
	if kept > maxBlocked {
		t.Fatalf("%d entries", kept)
	}
	lost := p.BlockedLost()
	if lost != uint64(maxBlocked+50-kept) {
		t.Fatalf("lost %d of %d, kept %d", lost, maxBlocked+50, kept)
	}
	p.AllowQuery("n0.example", dns.TypeA)
	if b := p.Blocked(); b[0].Name != "n0.example" || b[0].Count != 2 || p.BlockedLost() != lost {
		t.Fatalf("%+v, lost %d", b[0], p.BlockedLost())
	}
}

// The canonical table of REQ-SBX-005: each way out, in each mode that blocks it.
func TestCanonical(t *testing.T) {
	const name = "registry.npmjs.org"
	sni := hello(sniExt(name))
	for _, mode := range []Mode{Dev, Locked} {
		setup := func(t *testing.T) *Policy {
			p := newPolicy(t, mode, newClock(), name, "other.example")
			p.Answer(name, []netip.Addr{addrNPM})
			return p
		}
		t.Run(string(mode), func(t *testing.T) {
			t.Run("allowed name and server name", func(t *testing.T) {
				p := setup(t)
				if !p.AllowQuery(name, dns.TypeA) || p.Conn(ap(addrNPM, 443)) != netstack.ConnInspect || p.Inspect(ap(addrNPM, 443), sni) != netstack.InspectAllowTLS {
					t.Fatal("the ordinary path is blocked")
				}
			})
			t.Run("real TLS client", func(t *testing.T) {
				p := setup(t)
				if p.Inspect(ap(addrNPM, 443), realHello(t, &tls.Config{ServerName: name})) != netstack.InspectAllowTLS {
					t.Fatal("refused")
				}
			})
			t.Run("IPv4 literal to an unpinned address", func(t *testing.T) {
				p := setup(t)
				for _, port := range []uint16{443, 80, 22, 8443} {
					if p.Conn(ap(addrOther, port)) != netstack.ConnDeny {
						t.Errorf("port %d", port)
					}
				}
				if blockedKinds(p)[KindAddress] == "" {
					t.Error("not logged")
				}
			})
			t.Run("IPv6 literal to an unpinned address", func(t *testing.T) {
				p := setup(t)
				for _, a := range []string{"2606:4700::1", "::ffff:151.101.0.1", "::1"} {
					if p.Conn(ap(netip.MustParseAddr(a), 443)) != netstack.ConnDeny {
						t.Errorf("%s", a)
					}
				}
			})
			t.Run("DNS over TCP to an outside server", func(t *testing.T) {
				p := setup(t)
				if p.Conn(ap(netip.MustParseAddr("8.8.8.8"), 53)) != netstack.ConnDeny {
					t.Error("outside server")
				}
				if p.Conn(ap(addrNPM, 53)) != netstack.ConnDeny {
					t.Error("53 on a pinned address")
				}
				if p.Conn(ap(addrNPM, 853)) != netstack.ConnDeny {
					t.Error("DoT on a pinned address")
				}
			})
			t.Run("different server name on a pinned address", func(t *testing.T) {
				p := setup(t)
				for _, n := range []string{"evil.example", "other.example", "sub." + name, name + ".evil.example", "registry.npmjs.or"} {
					if p.Inspect(ap(addrNPM, 443), hello(sniExt(n))) != netstack.InspectDeny {
						t.Errorf("%s", n)
					}
				}
				if blockedKinds(p)[KindServerName] == "" {
					t.Error("not logged")
				}
			})
			t.Run("allowed name pinned on another address", func(t *testing.T) {
				p := setup(t)
				p.Answer("other.example", []netip.Addr{addrOther})
				if p.Inspect(ap(addrOther, 443), sni) != netstack.InspectDeny {
					t.Error("allowed")
				}
			})
			t.Run("no server name", func(t *testing.T) {
				p := setup(t)
				if p.Inspect(ap(addrNPM, 443), hello(ext(0x0a0a, nil))) != netstack.InspectDeny {
					t.Error("allowed")
				}
				if p.Inspect(ap(addrNPM, 443), realHello(t, &tls.Config{InsecureSkipVerify: true})) != netstack.InspectDeny {
					t.Error("real client allowed")
				}
				if blockedKinds(p)[KindNoServerName] == "" {
					t.Error("not logged")
				}
			})
			t.Run("ECH", func(t *testing.T) {
				p := setup(t)
				if p.Inspect(ap(addrNPM, 443), hello(sniExt(name), ext(tlsExtECH, []byte{1, 2, 3}))) != netstack.InspectDeny {
					t.Error("allowed")
				}
				if blockedKinds(p)[KindECH] == "" {
					t.Error("not logged")
				}
			})
			t.Run("not TLS on 443", func(t *testing.T) {
				p := setup(t)
				for _, b := range []string{"GET / HTTP/1.1\r\nHost: " + name + "\r\n\r\n", "SSH-2.0-x\r\n", "\x80\x2e\x01\x00\x02"} {
					if p.Inspect(ap(addrNPM, 443), []byte(b)) != netstack.InspectDeny {
						t.Errorf("%q", b)
					}
				}
			})
			t.Run("allowed name answering a private address", func(t *testing.T) {
				p := setup(t)
				rebind := netip.MustParseAddr("192.168.1.1")
				if len(p.Answer(name, []netip.Addr{rebind})) != 0 || p.Conn(ap(rebind, 443)) != netstack.ConnDeny {
					t.Error("kept")
				}
			})
			t.Run("query type other than A, AAAA, CNAME", func(t *testing.T) {
				p := setup(t)
				for _, q := range []uint16{dns.TypeTXT, dns.TypeMX, dns.TypeHTTPS, dns.TypePTR} {
					if p.AllowQuery(name, q) {
						t.Errorf("type %d", q)
					}
				}
			})
			t.Run("name outside the list", func(t *testing.T) {
				p := setup(t)
				if p.AllowQuery("tunnel-data.evil.example", dns.TypeA) {
					t.Error("allowed")
				}
			})
			t.Run("QUIC", func(t *testing.T) {
				p := setup(t)
				if p.UDP(ap(addrNPM, 443)) {
					t.Error("UDP 443 to a pinned address")
				}
				if p.UDP(ap(addrOther, 53)) {
					t.Error("UDP 53")
				}
				if blockedKinds(p)[KindUDP] == "" {
					t.Error("not logged")
				}
			})
			t.Run("hello split byte by byte", func(t *testing.T) {
				p := setup(t)
				for _, b := range [][]byte{sni, realHello(t, &tls.Config{ServerName: name}), records(handshake(helloSpec{exts: sniExt(name)}.body()), 1)} {
					for k := 1; k < len(b); k++ {
						if got := p.Inspect(ap(addrNPM, 443), b[:k]); got != netstack.InspectMore {
							t.Fatalf("after %d of %d bytes: %d", k, len(b), got)
						}
					}
					if p.Inspect(ap(addrNPM, 443), b) != netstack.InspectAllowTLS {
						t.Fatal("refused when whole")
					}
				}
			})
			t.Run("server name spellings", func(t *testing.T) {
				p := setup(t)
				for _, n := range []string{"REGISTRY.NPMJS.ORG", name + "."} {
					if p.Inspect(ap(addrNPM, 443), hello(sniExt(n))) != netstack.InspectAllowTLS {
						t.Errorf("%s refused", n)
					}
				}
				for _, n := range []string{name + "..", "*." + name, "registry.npmjs.org\x00.evil.example", "regıstry.npmjs.org"} {
					if p.Inspect(ap(addrNPM, 443), hello(sniExt(n))) != netstack.InspectDeny {
						t.Errorf("%q allowed", n)
					}
				}
			})
		})
	}
}

func TestHTTPInDevOnly(t *testing.T) {
	const name = "deb.debian.org"
	head := func(host string) []byte { return []byte("GET / HTTP/1.1\r\nHost: " + host + "\r\n\r\n") }

	p := newPolicy(t, Dev, newClock(), name, "other.example")
	p.Answer(name, []netip.Addr{addrNPM})
	dst := ap(addrNPM, 80)
	if p.Inspect(dst, head(name)) != netstack.InspectAllow || p.Inspect(dst, head(name+":80")) != netstack.InspectAllow || p.Inspect(dst, head("DEB.debian.ORG")) != netstack.InspectAllow {
		t.Fatal("refused")
	}
	if p.Inspect(dst, head("other.example")) != netstack.InspectDeny {
		t.Fatal("a Host not pinned here allowed")
	}
	if p.Inspect(dst, head("[::1]")) != netstack.InspectDeny || p.Inspect(dst, head("104.16.0.1")) != netstack.InspectDeny {
		t.Fatal("an IP as Host allowed")
	}
	if p.Inspect(dst, []byte("GET / HTTP/1.1\r\n")) != netstack.InspectMore {
		t.Fatal("no verdict should come before the head ends")
	}
	if p.Inspect(dst, []byte("GET / HTTP/1.1\r\nHost: "+name+"\r\nHost: "+name+"\r\n\r\n")) != netstack.InspectDeny {
		t.Fatal("two Hosts allowed")
	}
	if p.Inspect(dst, []byte("GET http://evil.example/ HTTP/1.1\r\nHost: "+name+"\r\n\r\n")) != netstack.InspectDeny {
		t.Fatal("an absolute target hides the real host")
	}
	if blockedKinds(p)[KindHTTPHost] == "" {
		t.Fatal("not logged")
	}

	l := newPolicy(t, Locked, newClock(), name)
	l.Answer(name, []netip.Addr{addrNPM})
	if l.Conn(dst) != netstack.ConnDeny || l.Inspect(dst, head(name)) != netstack.InspectDeny {
		t.Fatal("locked allowed port 80")
	}
	// Inspect on a port it has no parser for fails closed.
	if p.Inspect(ap(addrNPM, 22), head(name)) != netstack.InspectDeny {
		t.Fatal("port 22 inspected into an allow")
	}
}

func TestHookReturnsCanonicalForms(t *testing.T) {
	p := newPolicy(t, Dev, newClock(), "a.example")
	got := p.Answer("A.Example.", []netip.Addr{addrNPM})
	if len(got) != 1 {
		t.Fatalf("%v", got)
	}
	if p.Inspect(ap(addrNPM, 443), hello(sniExt("a.example"))) != netstack.InspectAllowTLS {
		t.Fatal("a pin made in capitals does not match")
	}
}
