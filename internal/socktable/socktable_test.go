package socktable

import (
	"encoding/binary"
	"net/netip"
	"reflect"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

func TestMain(m *testing.M) {
	leaktest.VerifyTestMain(m)
}

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

func TestParseTCP6(t *testing.T) {
	b := le(1)
	b = append(b, netip.MustParseAddr("fe80::1").AsSlice()...)
	b = append(b, le(3)...) // scope
	b = append(b, nport(8080)...)
	b = append(b, make([]byte, 16)...)
	b = append(b, le(0)...)
	b = append(b, nport(0)...)
	b = append(b, le(2)...) // LISTEN
	b = append(b, le(42)...)
	ss, err := ParseTable("tcp6", b)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 || ss[0].Local != netip.MustParseAddrPort("[fe80::1]:8080") || ss[0].State != "LISTEN" || ss[0].PID != 42 {
		t.Fatalf("got %+v", ss)
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

func TestParseBadTables(t *testing.T) {
	for _, tc := range []struct {
		proto string
		b     []byte
	}{
		{"tcp4", nil},
		{"tcp4", le(1)},             // a row claimed, none there
		{"tcp6", le(0xFFFFFFFF)},    // a huge count
		{"udp4", append(le(1), 1)},  // a short row
		{"sctp", le(0)},             // unknown table
		{"udp6", le(2, 0, 0, 0, 0)}, // two rows claimed in 16 bytes
	} {
		if _, err := ParseTable(tc.proto, tc.b); err == nil {
			t.Errorf("%s %x: accepted", tc.proto, tc.b)
		}
	}
	if ss, err := ParseTable("tcp6", le(0)); err != nil || len(ss) != 0 {
		t.Errorf("empty table: %v %v", ss, err)
	}
}

func TestHolder(t *testing.T) {
	ss := []Socket{
		{Proto: "tcp4", Local: netip.MustParseAddrPort("127.0.0.1:80"), State: "ESTABLISHED"},
		{Proto: "udp4", Local: netip.MustParseAddrPort("0.0.0.0:80")},
		{Proto: "tcp6", Local: netip.MustParseAddrPort("[::1]:81"), State: "LISTEN"},
		{Proto: "tcp4", PID: 9, Local: netip.MustParseAddrPort("192.168.1.5:80"), State: "LISTEN"},
	}
	if s, ok := Holder(ss, 80); !ok || s.PID != 9 {
		t.Errorf("port 80: %+v %v; want the listener on the LAN address", s, ok)
	}
	if _, ok := Holder(ss, 81); !ok {
		t.Error("port 81: the [::1] listener is not found")
	}
	if s, ok := Holder(ss, 82); ok {
		t.Errorf("port 82: %+v", s)
	}
}

func FuzzParseTable(f *testing.F) {
	f.Add("tcp4", le(1, 2, 0x0100007f, 80, 0, 0, 4))
	f.Add("tcp6", le(0))
	f.Add("udp4", le(1, 0, 53, 7))
	f.Add("udp6", le(0xFFFFFFFF))
	f.Fuzz(func(t *testing.T, proto string, b []byte) {
		ss, err := ParseTable(proto, b)
		if err != nil {
			return
		}
		if 4+len(ss)*12 > len(b) {
			t.Fatalf("%d rows from %d bytes", len(ss), len(b))
		}
	})
}
