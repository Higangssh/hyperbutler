package engine

import (
	"net/netip"
	"reflect"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/socktable"
)

// TestSharedPorts covers issue #2: a program that starts listening on a port after the
// engine published it (Docker Desktop on 0.0.0.0 and [::], say) gets no error from
// Windows, and localhost keeps reaching the engine's more specific loopback listener.
// status names it.
func TestSharedPorts(t *testing.T) {
	const self = 100
	l := func(proto string, pid uint32, addr string) socktable.Socket {
		return socktable.Socket{Proto: proto, PID: pid, Local: netip.MustParseAddrPort(addr), State: "LISTEN"}
	}
	ap := func(ss ...string) []netip.AddrPort {
		var out []netip.AddrPort
		for _, s := range ss {
			out = append(out, netip.MustParseAddrPort(s))
		}
		return out
	}
	engine8080 := []socktable.Socket{l("tcp4", self, "127.0.0.1:8080"), l("tcp6", self, "[::1]:8080")}
	tests := []struct {
		name      string
		ss        []socktable.Socket
		published []uint16
		want      []SharedPort
	}{
		{"none", nil, []uint16{8080}, nil},
		{"engine alone", engine8080, []uint16{8080}, nil},
		{"Docker Desktop's two wildcards, one entry", append(engine8080, l("tcp6", 7, "[::]:8080"), l("tcp4", 7, "0.0.0.0:8080")),
			[]uint16{8080}, []SharedPort{{Port: 8080, PID: 7, Listen: ap("0.0.0.0:8080", "[::]:8080")}}},
		{"LAN address", []socktable.Socket{l("tcp4", self, "127.0.0.1:3000"), l("tcp4", 9, "192.168.1.5:3000")},
			[]uint16{3000}, []SharedPort{{Port: 3000, PID: 9, Listen: ap("192.168.1.5:3000")}}},
		{"two other processes", append(engine8080, l("tcp4", 9, "0.0.0.0:8080"), l("tcp4", 7, "192.168.1.5:8080")),
			[]uint16{8080}, []SharedPort{{Port: 8080, PID: 7, Listen: ap("192.168.1.5:8080")}, {Port: 8080, PID: 9, Listen: ap("0.0.0.0:8080")}}},
		{"other port", append(engine8080, l("tcp4", 7, "0.0.0.0:8081")), []uint16{8080}, nil},
		// A loopback listener of the engine that is not a published port (the bind
		// check's probe, a debug endpoint) is not reported: the list comes from state.
		{"engine loopback listener that is not published", []socktable.Socket{
			l("tcp4", self, "127.0.0.1:6060"), l("tcp4", 7, "0.0.0.0:6060"),
		}, []uint16{8080}, nil},
		{"nothing published", append(engine8080, l("tcp4", 7, "0.0.0.0:8080")), nil, nil},
		{"not listening", append(engine8080,
			socktable.Socket{Proto: "tcp4", PID: 7, Local: netip.MustParseAddrPort("0.0.0.0:8080"), State: "ESTABLISHED"}),
			[]uint16{8080}, nil},
		{"udp is not tcp", append(engine8080, socktable.Socket{Proto: "udp4", PID: 7, Local: netip.MustParseAddrPort("0.0.0.0:8080")}),
			[]uint16{8080}, nil},
		{"pid 0 rows", append(engine8080, l("tcp4", 0, "0.0.0.0:8080")), []uint16{8080}, nil},
		{"sorted by port", []socktable.Socket{
			l("tcp4", self, "127.0.0.1:9000"), l("tcp4", self, "127.0.0.1:80"),
			l("tcp4", 7, "0.0.0.0:9000"), l("tcp4", 8, "0.0.0.0:80"),
		}, []uint16{80, 9000}, []SharedPort{
			{Port: 80, PID: 8, Listen: ap("0.0.0.0:80")},
			{Port: 9000, PID: 7, Listen: ap("0.0.0.0:9000")},
		}},
		{"duplicate rows once", append(engine8080, l("tcp4", 7, "0.0.0.0:8080"), l("tcp4", 7, "0.0.0.0:8080")),
			[]uint16{8080}, []SharedPort{{Port: 8080, PID: 7, Listen: ap("0.0.0.0:8080")}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, more := SharedPorts(tt.ss, self, tt.published)
			if !reflect.DeepEqual(got, tt.want) || more != 0 {
				t.Errorf("SharedPorts = %+v (%d more), want %+v", got, more, tt.want)
			}
		})
	}
	if got, _ := SharedPorts(append(engine8080, l("tcp4", 7, "0.0.0.0:8080")), 0, []uint16{8080}); got != nil {
		t.Errorf("engine pid 0: %+v", got)
	}
}

func TestSharedPortsBounded(t *testing.T) {
	ss := []socktable.Socket{{Proto: "tcp4", PID: 1, Local: netip.MustParseAddrPort("127.0.0.1:8080"), State: "LISTEN"}}
	for i := range 3 * MaxSharedPorts {
		ss = append(ss, socktable.Socket{Proto: "tcp4", PID: uint32(i + 2), Local: netip.MustParseAddrPort("0.0.0.0:8080"), State: "LISTEN"})
	}
	got, more := SharedPorts(ss, 1, []uint16{8080})
	if len(got) != MaxSharedPorts || more != 2*MaxSharedPorts {
		t.Errorf("%d entries and %d more, want %d and %d", len(got), more, MaxSharedPorts, 2*MaxSharedPorts)
	}
}

func TestStatePublishedPortsValidate(t *testing.T) {
	for _, tt := range []struct {
		ports []uint16
		ok    bool
	}{
		{nil, true}, {[]uint16{80, 8080}, true},
		{[]uint16{0}, false}, {[]uint16{8080, 80}, false}, {[]uint16{80, 80}, false},
		{make([]uint16, MaxPublishedPorts+1), false},
	} {
		s := State{Version: 1, Phase: PhaseStopped, PublishedPorts: tt.ports}
		if err := s.Validate(); (err == nil) != tt.ok {
			t.Errorf("%v: Validate = %v, want ok %v", tt.ports, err, tt.ok)
		}
	}
}
