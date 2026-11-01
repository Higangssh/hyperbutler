package main

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/netstack"
)

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
			// The engine side of the pull's flow, first polled after the pull ended.
			{Socket: Socket{Proto: "tcp4", Remote: netip.MustParseAddrPort("1.1.1.1:443"), State: "ESTABLISHED"}, Phase: "idle-1", First: now},
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

// A run whose vmwp was not found fails unless -allow-no-vmwp is given (L-7).
func TestNoVMWP(t *testing.T) {
	cause := errors.New("no new vmwp")
	if err := noVMWP(cause, false); err == nil || !strings.HasPrefix(err.Error(), "FAIL") || !errors.Is(err, cause) {
		t.Fatalf("without -allow-no-vmwp: %v", err)
	}
	if err := noVMWP(cause, true); err != nil {
		t.Fatalf("with -allow-no-vmwp: %v", err)
	}
}
