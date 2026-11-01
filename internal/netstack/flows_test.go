package netstack

import (
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

func TestFlowLogIsBounded(t *testing.T) {
	var l flowLog
	for i := range maxFlowEntries + 10 {
		dst := netip.AddrPortFrom(netip.MustParseAddr("10.0.0.1"), uint16(1000+i))
		l.flow("tcp", dst, "out")
		l.query(fmt.Sprintf("n%d.example.", i), "A")
	}
	l.flow("tcp", netip.MustParseAddrPort("10.0.0.1:1000"), "out") // known: still counted
	l.query("N0.Example.", "A")                                    // names are case-insensitive
	s := l.snapshot()
	if len(s.Flows) != maxFlowEntries || s.FlowsLost != 10 || len(s.DNS) != maxFlowEntries || s.DNSLost != 10 {
		t.Fatalf("flows %d lost %d, dns %d lost %d", len(s.Flows), s.FlowsLost, len(s.DNS), s.DNSLost)
	}
	if s.Flows[0].Dst != "10.0.0.1:1000" || s.Flows[0].Count != 2 || s.DNS[0].Name != "n0.example." || s.DNS[0].Count != 2 {
		t.Fatalf("first entries: %+v %+v", s.Flows[0], s.DNS[0])
	}
}

// TestFlowsRecordGuestRequests checks that what the guest asks for shows in Flows:
// an allowed connection, a denied one, a UDP flow and a DNS query.
func TestFlowsRecordGuestRequests(t *testing.T) {
	echo := echoServer(t)
	virt := netip.MustParseAddrPort("192.168.127.254:7")
	n := newTestStack(t, func(c *Config) { c.HostForwards = []HostForward{{Virtual: virt, Host: echo}} })
	if s := n.Flows(); len(s.Flows)+len(s.DNS) != 0 {
		t.Fatalf("new stack has flows: %+v", s)
	}
	g := newFakeGuest(t, n)
	c, err := guestDial(t, g, virt)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	denied := netip.MustParseAddrPort("169.254.169.254:80")
	if c, err := guestDial(t, g, denied); err == nil {
		c.Close()
		t.Fatal("guest reached the metadata address")
	}
	gw := n.Config().Gateway()
	uc, err := gonet.DialUDP(g.s, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpipAddr(gw), Port: 53}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	q := new(dns.Msg)
	q.SetQuestion("Example.com.", dns.TypeAAAA)
	b, _ := q.Pack()
	if _, err := uc.Write(b); err != nil {
		t.Fatal(err)
	}
	_ = uc.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := uc.Read(make([]byte, 1500)); err != nil {
		t.Fatal(err)
	}
	// UDP to the metadata address: denied, and recorded.
	ud, err := gonet.DialUDP(g.s, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpipAddr(denied.Addr()), Port: 123}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer ud.Close()
	if _, err := ud.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{
		"tcp 192.168.127.254:7 forward":  false,
		"tcp 169.254.169.254:80 denied":  false,
		"udp 169.254.169.254:123 denied": false,
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		s := n.Flows()
		for _, f := range s.Flows {
			want[f.Proto+" "+f.Dst+" "+f.Route] = true
		}
		all := true
		for _, ok := range want {
			all = all && ok
		}
		gotDNS := len(s.DNS) == 1 && s.DNS[0].Name == "example.com." && s.DNS[0].Type == "AAAA"
		if all && gotDNS {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("flows %+v (want %v), dns %+v", s.Flows, want, s.DNS)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
