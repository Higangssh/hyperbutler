package main

import (
	"encoding/binary"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/netstack"
)

func le(vs ...uint32) []byte {
	var b []byte
	for _, v := range vs {
		b = binary.LittleEndian.AppendUint32(b, v)
	}
	return b
}

// nport is a dwPort field: the port in network byte order in the first two bytes.
func nport(p uint16) []byte { return []byte{byte(p >> 8), byte(p), 0, 0} }

func TestParseTCP4(t *testing.T) {
	b := le(2)
	row := func(state uint32, l [4]byte, lp uint16, r [4]byte, rp uint16, pid uint32) {
		b = append(b, le(state)...)
		b = append(b, l[:]...)
		b = append(b, nport(lp)...)
		b = append(b, r[:]...)
		b = append(b, nport(rp)...)
		b = append(b, le(pid)...)
	}
	row(5, [4]byte{192, 168, 1, 10}, 50000, [4]byte{93, 184, 215, 14}, 443, 1234)
	row(2, [4]byte{127, 0, 0, 1}, 2375, [4]byte{}, 0, 99)
	ss, err := ParseTable("tcp4", b)
	if err != nil {
		t.Fatal(err)
	}
	want := []Socket{
		{"tcp4", 1234, netip.MustParseAddrPort("192.168.1.10:50000"), netip.MustParseAddrPort("93.184.215.14:443"), "ESTABLISHED"},
		{"tcp4", 99, netip.MustParseAddrPort("127.0.0.1:2375"), netip.MustParseAddrPort("0.0.0.0:0"), "LISTEN"},
	}
	if !reflect.DeepEqual(ss, want) {
		t.Fatalf("got %+v", ss)
	}
	if !ss[0].External() || ss[1].External() {
		t.Errorf("External: %v %v", ss[0].External(), ss[1].External())
	}
	if _, err := ParseTable("tcp4", b[:len(b)-1]); err == nil {
		t.Error("short table accepted")
	}
}

func TestParseUDP6(t *testing.T) {
	b := le(1)
	b = append(b, netip.IPv6Loopback().AsSlice()...)
	b = append(b, le(0)...)
	b = append(b, nport(53)...)
	b = append(b, le(7)...)
	ss, err := ParseTable("udp6", b)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 || ss[0].Local != netip.MustParseAddrPort("[::1]:53") || ss[0].PID != 7 || ss[0].External() {
		t.Fatalf("got %+v", ss)
	}
	ss[0].Local = netip.MustParseAddrPort("[::]:53")
	if !ss[0].External() {
		t.Error("UDP socket on the wildcard address is not external")
	}
}

func TestFlowDelta(t *testing.T) {
	before := netstack.FlowLog{
		Flows: []netstack.FlowEntry{{Proto: "udp", Dst: "1.2.3.4:123", Route: "out", Count: 2}},
		DNS:   []netstack.DNSEntry{{Name: "a.example.", Type: "A", Count: 1}},
	}
	after := netstack.FlowLog{
		Flows: []netstack.FlowEntry{
			{Proto: "udp", Dst: "1.2.3.4:123", Route: "out", Count: 2},
			{Proto: "tcp", Dst: "5.6.7.8:443", Route: "out", Count: 1},
		},
		DNS: []netstack.DNSEntry{{Name: "a.example.", Type: "A", Count: 4}},
	}
	d := FlowDelta(before, after)
	if len(d.Flows) != 1 || d.Flows[0].Dst != "5.6.7.8:443" || len(d.DNS) != 1 || d.DNS[0].Count != 3 {
		t.Fatalf("delta %+v", d)
	}
}

func TestFailures(t *testing.T) {
	now := time.Now()
	r := Report{
		Phases: []Phase{
			{Name: "boot", Counted: true},
			{Name: "pull", Counted: false, Guest: netstack.FlowLog{Flows: []netstack.FlowEntry{{Proto: "tcp", Dst: "1.1.1.1:443", Route: "out", Count: 1}}}},
			{Name: "idle-1", Counted: true, Guest: netstack.FlowLog{DNS: []netstack.DNSEntry{{Name: "pool.ntp.org.", Type: "A", Count: 1}}}},
		},
		Sockets: []Seen{
			{Socket: Socket{Proto: "tcp4", Remote: netip.MustParseAddrPort("1.1.1.1:443"), State: "ESTABLISHED"}, Phase: "pull", First: now},
			{Socket: Socket{Proto: "tcp4", Remote: netip.MustParseAddrPort("127.0.0.1:80"), State: "ESTABLISHED"}, Phase: "idle-1", First: now},
			{Socket: Socket{Proto: "tcp6", Remote: netip.MustParseAddrPort("[2001:db8::1]:443"), State: "SYN_SENT"}, Phase: "boot", First: now},
		},
	}
	f := r.Failures()
	if len(f) != 2 || !strings.Contains(f[0], "[2001:db8::1]:443") || !strings.Contains(f[1], "pool.ntp.org") {
		t.Fatalf("failures %q", f)
	}
}

func TestRecorder(t *testing.T) {
	var r Recorder
	s := Socket{Proto: "tcp4", PID: 5, Local: netip.MustParseAddrPort("127.0.0.1:1"), Remote: netip.MustParseAddrPort("127.0.0.1:2"), State: "SYN_SENT"}
	other := s
	other.PID = 6
	t0 := time.Now()
	r.Add("boot", t0, []Socket{s, other}, map[uint32]bool{5: true})
	s.State = "ESTABLISHED"
	r.Add("idle-1", t0.Add(time.Second), []Socket{s}, map[uint32]bool{5: true})
	all := r.All()
	if len(all) != 1 || all[0].Phase != "boot" || all[0].Polls != 2 || all[0].State != "ESTABLISHED" || r.Polls != 2 {
		t.Fatalf("got %+v", all)
	}
}
