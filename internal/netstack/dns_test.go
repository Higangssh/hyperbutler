package netstack

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func newTestDNS() (*dnsHandler, *fakeResolver) {
	r := &fakeResolver{a: map[string][]netip.Addr{
		"example.com":         {netip.MustParseAddr("93.184.215.14"), netip.MustParseAddr("2606:2800::1")},
		"peer.tailnet.ts.net": {netip.MustParseAddr("100.64.0.7")},
		"many.example.com":    manyAddrs(300),
	}}
	return &dnsHandler{res: r, timeout: time.Second}, r
}

func manyAddrs(n int) []netip.Addr {
	out := make([]netip.Addr, n)
	for i := range out {
		out[i] = netip.AddrFrom4([4]byte{10, 0, byte(i >> 8), byte(i)})
	}
	return out
}

func query(t *testing.T, h *dnsHandler, name string, qtype uint16, udp bool, edns uint16) *dns.Msg {
	t.Helper()
	r, _ := queryRaw(t, h, name, qtype, udp, edns)
	return r
}

func queryRaw(t *testing.T, h *dnsHandler, name string, qtype uint16, udp bool, edns uint16) (*dns.Msg, int) {
	t.Helper()
	q := new(dns.Msg)
	q.SetQuestion(name, qtype)
	if edns != 0 {
		q.SetEdns0(edns, false)
	}
	b, _ := q.Pack()
	resp := h.answer(context.Background(), b, udp)
	if resp == nil {
		t.Fatalf("%s: dropped", name)
	}
	r := new(dns.Msg)
	if err := r.Unpack(resp); err != nil {
		t.Fatalf("%s: bad response: %v", name, err)
	}
	if r.Id != q.Id || !r.Response {
		t.Fatalf("%s: id/response mismatch", name)
	}
	return r, len(resp)
}

func TestDNSAnswers(t *testing.T) {
	h, _ := newTestDNS()
	r := query(t, h, "example.com.", dns.TypeA, true, 0)
	if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 {
		t.Fatalf("A: %v", r)
	}
	r = query(t, h, "peer.tailnet.ts.net.", dns.TypeA, true, 0)
	if len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != "100.64.0.7" {
		t.Fatalf("split DNS name: %v", r)
	}
	r = query(t, h, "example.com.", dns.TypeAAAA, true, 0)
	if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
		t.Fatalf("AAAA must be NODATA: %v", r)
	}
	r = query(t, h, "nope.example.", dns.TypeA, true, 0)
	if r.Rcode != dns.RcodeNameError {
		t.Fatalf("missing name: %v", r)
	}
	r = query(t, h, "nope.example.", dns.TypeAAAA, true, 0)
	if r.Rcode != dns.RcodeNameError {
		t.Fatalf("missing name AAAA: %v", r)
	}
	r = query(t, h, "example.com.", dns.TypeMX, true, 0)
	if len(r.Answer) != 1 {
		t.Fatalf("MX with an invalid host must be filtered: %v", r)
	}
	r = query(t, h, "example.com.", dns.TypeSOA, true, 0)
	if r.Rcode != dns.RcodeNotImplemented {
		t.Fatalf("SOA: %v", r)
	}
	r = query(t, h, "14.215.184.93.in-addr.arpa.", dns.TypePTR, true, 0)
	if len(r.Answer) != 1 {
		t.Fatalf("PTR: %v", r)
	}
	r = query(t, h, "example.com.", dns.TypeTXT, true, 4096)
	if len(r.Answer) != 1 || len(r.Answer[0].(*dns.TXT).Txt) != 3 {
		t.Fatalf("TXT: %v", r)
	}
}

func TestDNSTruncation(t *testing.T) {
	h, _ := newTestDNS()
	r, n := queryRaw(t, h, "many.example.com.", dns.TypeA, true, 0)
	if n > dns.MinMsgSize || !r.Truncated {
		t.Fatalf("UDP answer is %d bytes, truncated=%v", n, r.Truncated)
	}
	_, n = queryRaw(t, h, "many.example.com.", dns.TypeA, true, 4096)
	if n > maxUDPAnswer {
		t.Fatalf("EDNS answer is %d bytes, over %d", n, maxUDPAnswer)
	}
	r = query(t, h, "many.example.com.", dns.TypeA, false, 0)
	if r.Truncated || len(r.Answer) != maxRecords {
		t.Fatalf("TCP answer: truncated=%v answers=%d", r.Truncated, len(r.Answer))
	}
}

func TestDNSRefusesOddNames(t *testing.T) {
	h, r := newTestDNS()
	for _, name := range []string{".", "a\\000b.example.", "a\\032b.", "x..y."} {
		q := new(dns.Msg)
		q.SetQuestion(name, dns.TypeA)
		b, err := q.Pack()
		if err != nil {
			continue
		}
		resp := h.answer(context.Background(), b, true)
		m := new(dns.Msg)
		if resp == nil || m.Unpack(resp) != nil {
			t.Fatalf("%q: no usable reply", name)
		}
		if m.Rcode != dns.RcodeRefused {
			t.Errorf("%q: rcode %d, want REFUSED", name, m.Rcode)
		}
	}
	if r.calls != 0 {
		t.Fatalf("resolver called %d times for refused names", r.calls)
	}
}

func TestDNSTimeoutIsServfail(t *testing.T) {
	h, r := newTestDNS()
	r.delay = time.Hour
	h.timeout = 50 * time.Millisecond
	m := query(t, h, "example.com.", dns.TypeA, true, 0)
	if m.Rcode != dns.RcodeServerFailure {
		t.Fatalf("rcode %d, want SERVFAIL", m.Rcode)
	}
}

func TestDNSDropsGarbage(t *testing.T) {
	h, _ := newTestDNS()
	if h.answer(context.Background(), []byte{1, 2, 3}, true) != nil {
		t.Fatal("short garbage answered")
	}
	resp := new(dns.Msg)
	resp.SetQuestion("example.com.", dns.TypeA)
	resp.Response = true
	b, _ := resp.Pack()
	if h.answer(context.Background(), b, true) != nil {
		t.Fatal("a response was answered (reflection)")
	}
	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	q.Question = append(q.Question, q.Question[0])
	b, _ = q.Pack()
	m := new(dns.Msg)
	if err := m.Unpack(h.answer(context.Background(), b, true)); err != nil || m.Rcode != dns.RcodeFormatError {
		t.Fatalf("two questions: %v %v", m, err)
	}
}
