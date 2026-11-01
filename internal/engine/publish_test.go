package engine

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/socktable"
)

// fakeBinds is a binder that refuses the addresses in deny and tracks open listeners.
type fakeBinds struct {
	deny map[netip.AddrPort]bool
	open map[netip.AddrPort]bool
	log  []string
}

type fakeListener struct {
	f    *fakeBinds
	addr netip.AddrPort
}

func (l fakeListener) Close() error { delete(l.f.open, l.addr); return nil }

func (f *fakeBinds) bind(host netip.AddrPort, guest uint16) (io.Closer, error) {
	f.log = append(f.log, host.String())
	if host.Port() != guest {
		return nil, fmt.Errorf("guest port %d for %s", guest, host)
	}
	if f.deny[host] {
		return nil, fmt.Errorf("listen tcp %s: bind: %w", host, errAddrInUse)
	}
	if f.open == nil {
		f.open = map[netip.AddrPort]bool{}
	}
	f.open[host] = true
	return fakeListener{f, host}, nil
}

func noCheck(uint16) error { return nil }
func v6(ok bool) func() bool {
	return func() bool { return ok }
}

func TestPublishAllBindsBothLoopbacks(t *testing.T) {
	f := &fakeBinds{}
	cs, err := publishAll([]uint16{80, 443}, noCheck, f.bind, v6(true))
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 4 || len(f.open) != 4 {
		t.Fatalf("%d closers, open %v", len(cs), f.open)
	}
	for _, a := range []string{"127.0.0.1:80", "[::1]:80", "127.0.0.1:443", "[::1]:443"} {
		if !f.open[netip.MustParseAddrPort(a)] {
			t.Errorf("%s not published", a)
		}
	}
}

// REQ-NET-005 (proposed): one loopback refused means no listener at all, for this port
// and for the container's other ports.
func TestPublishAllIsAllOrNothing(t *testing.T) {
	for _, denied := range []string{"127.0.0.1:81", "[::1]:81"} {
		f := &fakeBinds{deny: map[netip.AddrPort]bool{netip.MustParseAddrPort(denied): true}}
		cs, err := publishAll([]uint16{80, 81, 82}, noCheck, f.bind, v6(true))
		if err == nil || cs != nil {
			t.Fatalf("%s refused: %v, %d closers", denied, err, len(cs))
		}
		if len(f.open) != 0 {
			t.Errorf("%s refused: still open %v", denied, f.open)
		}
		var pu *PortsUnavailable
		if !errors.As(err, &pu) || pu.Port != 81 || netip.AddrPortFrom(pu.Addr, pu.Port).String() != denied {
			t.Fatalf("%s refused: error %#v", denied, err)
		}
		want := "Ports are not available: exposing port TCP " + denied + " -> 0.0.0.0:0: listen tcp " + denied + ": bind: "
		if !strings.HasPrefix(err.Error(), want) {
			t.Errorf("message %q, want the prefix %q", err, want)
		}
		if strings.Contains(strings.Join(f.log, " "), ":82") {
			t.Errorf("%s refused: went on to bind port 82: %v", denied, f.log)
		}
	}
}

// Without IPv6 loopback on the host, 127.0.0.1 alone is a whole publish.
func TestPublishAllWithoutIPv6(t *testing.T) {
	f := &fakeBinds{deny: map[netip.AddrPort]bool{netip.MustParseAddrPort("[::1]:80"): true}}
	cs, err := publishAll([]uint16{80}, noCheck, f.bind, v6(false))
	if err != nil || len(cs) != 1 || !f.open[netip.MustParseAddrPort("127.0.0.1:80")] {
		t.Fatalf("%v, %d closers, open %v", err, len(cs), f.open)
	}
	// 127.0.0.1 is never optional.
	f = &fakeBinds{deny: map[netip.AddrPort]bool{netip.MustParseAddrPort("127.0.0.1:80"): true}}
	if _, err := publishAll([]uint16{80}, noCheck, f.bind, v6(false)); err == nil || len(f.open) != 0 {
		t.Fatalf("127.0.0.1 refused: %v, open %v", err, f.open)
	}
}

// A port the check reports taken is refused before anything is bound.
func TestPublishAllChecksFirst(t *testing.T) {
	f := &fakeBinds{}
	taken := &PortsUnavailable{Port: 81, Addr: loopbacks[0], Cause: errAddrInUse}
	check := func(p uint16) error {
		if p == 81 {
			return taken
		}
		return nil
	}
	if _, err := publishAll([]uint16{80, 81}, check, f.bind, v6(true)); !errors.Is(err, taken) {
		t.Fatalf("error %v, want the check's", err)
	}
	if len(f.log) != 0 {
		t.Errorf("bound %v before the check refused", f.log)
	}
}

func TestPortFreeIn(t *testing.T) {
	ss := []socktable.Socket{
		{Proto: "tcp4", PID: 5, Local: netip.MustParseAddrPort("127.0.0.1:8080"), Remote: netip.MustParseAddrPort("127.0.0.1:50000"), State: "ESTABLISHED"},
		{Proto: "udp4", PID: 5, Local: netip.MustParseAddrPort("0.0.0.0:8080")},
		{Proto: "tcp4", PID: 1, Local: netip.MustParseAddrPort("127.0.0.1:9000"), State: "LISTEN"}, // this engine
		{Proto: "tcp6", PID: 7, Local: netip.MustParseAddrPort("[::1]:8081"), State: "LISTEN"},
		{Proto: "tcp4", PID: 8, Local: netip.MustParseAddrPort("192.168.1.5:8082"), State: "LISTEN"},
		{Proto: "tcp6", PID: 9, Local: netip.MustParseAddrPort("[::]:8083"), State: "LISTEN"},
		{Proto: "tcp4", PID: 10, Local: netip.MustParseAddrPort("127.0.0.1:8084"), State: "LISTEN"},
	}
	for _, tc := range []struct {
		port uint16
		want string // "" for free
	}{
		{8080, ""}, // a connection and a UDP socket are not listeners
		{9000, ""}, // this engine's own listener
		{8081, "Ports are not available: exposing port TCP [::1]:8081 -> 0.0.0.0:0: listen tcp [::1]:8081: bind: in use (another program, process 7, listens on [::1]:8081)"},
		{8082, "Ports are not available: exposing port TCP 127.0.0.1:8082 -> 0.0.0.0:0: listen tcp 127.0.0.1:8082: bind: in use (another program, process 8, listens on 192.168.1.5:8082)"},
		{8083, "Ports are not available: exposing port TCP 127.0.0.1:8083 -> 0.0.0.0:0: listen tcp 127.0.0.1:8083: bind: in use (another program, process 9, listens on [::]:8083)"},
		{8084, "Ports are not available: exposing port TCP 127.0.0.1:8084 -> 0.0.0.0:0: listen tcp 127.0.0.1:8084: bind: in use (another program, process 10, listens on 127.0.0.1:8084)"},
	} {
		err := portFreeIn(ss, tc.port, 1, errors.New("in use"))
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("port %d: %q, want %q", tc.port, got, tc.want)
		}
	}
}

func TestStartPorts(t *testing.T) {
	doc := `{"Id":"` + testID + `","State":{"Running":false},"HostConfig":{"NetworkMode":"bridge","PortBindings":{
		"80/tcp":[{"HostIp":"","HostPort":"8080"},{"HostIp":"::","HostPort":"8080"}],
		"443/tcp":[{"HostIp":"0.0.0.0","HostPort":"8443"}],
		"53/udp":[{"HostIp":"","HostPort":"5353"}],
		"81/tcp":[{"HostIp":"127.0.0.1","HostPort":"8081"}],
		"82/tcp":[{"HostIp":"","HostPort":""}],
		"83/tcp":[{"HostIp":"","HostPort":"0"}],
		"84/tcp":[{"HostIp":"","HostPort":"9000-9010"}],
		"85/tcp":[{"HostIp":"192.168.127.2","HostPort":"8085"}]}}}`
	id, running, ports, err := startPorts([]byte(doc), testGuestIP)
	if err != nil {
		t.Fatal(err)
	}
	if id != testID || running || !reflect.DeepEqual(ports, []uint16{8080, 8085, 8443}) {
		t.Fatalf("%s %v %v", id, running, ports)
	}
	for _, mode := range []string{"host", "none"} {
		doc := `{"Id":"` + testID + `","HostConfig":{"NetworkMode":"` + mode + `","PortBindings":{"80/tcp":[{"HostPort":"8080"}]}}}`
		if _, _, ports, err := startPorts([]byte(doc), testGuestIP); err != nil || len(ports) != 0 {
			t.Errorf("network %s: %v %v", mode, ports, err)
		}
	}
	doc = `{"Id":"` + testID + `","State":{"Running":true},"HostConfig":{}}`
	if _, running, _, err := startPorts([]byte(doc), testGuestIP); err != nil || !running {
		t.Errorf("running container: %v %v", running, err)
	}
	for _, bad := range []string{`{"Id":"x"}`, `[`, `{"Id":"` + testID + `","HostConfig":{"PortBindings":[]}}`} {
		if _, _, _, err := startPorts([]byte(bad), testGuestIP); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

func TestUnpublished(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	long := errors.New(strings.Repeat("é", maxErrorLen))
	us := unpublished(testID, []uint16{80, 81}, long, now)
	if len(us) != 2 || us[0].Container != testID[:12] || us[1].Port != 81 || !us[0].Since.Equal(now) {
		t.Fatalf("%+v", us)
	}
	for i := range us {
		if err := us[i].Validate(); err != nil {
			t.Errorf("a truncated reason does not validate: %v", err)
		}
	}
	pu := &PortsUnavailable{Port: 81, Addr: loopbacks[0], Cause: errAddrInUse}
	us = unpublished(testID, []uint16{80, 81}, pu, now)
	if us[1].Reason != pu.Error() || us[0].Reason != "held back with port 81, as a container's ports are published together" {
		t.Errorf("reasons %q, %q", us[0].Reason, us[1].Reason)
	}
	st := State{Version: stateVersion, Phase: PhaseRunning, PID: 1, UnpublishedPorts: us}
	if err := st.Validate(); err != nil {
		t.Fatal(err)
	}
	st.UnpublishedPorts = []UnpublishedPort{{Container: "XYZ", Port: 80}}
	if err := st.Validate(); err == nil {
		t.Error("a bad container id validates")
	}
	st.UnpublishedPorts = []UnpublishedPort{{Container: testID[:12]}}
	if err := st.Validate(); err == nil {
		t.Error("port 0 validates")
	}
}

func FuzzStartPorts(f *testing.F) {
	f.Add([]byte(`{"Id":"` + testID + `","HostConfig":{"PortBindings":{"80/tcp":[{"HostIp":"","HostPort":"8080"}]}}}`))
	f.Add([]byte(`{"Id":"` + testID + `","State":{"Running":true},"HostConfig":{"NetworkMode":"host"}}`))
	f.Add([]byte(`{"Id":"` + testID + `","HostConfig":{"PortBindings":{"80/tcp":[{"HostIp":"::ffff:192.168.127.2","HostPort":"08080"}],"81/tcp":[{"HostPort":"1-2"}]}}}`))
	f.Fuzz(func(t *testing.T, doc []byte) {
		id, _, ports, err := startPorts(doc, testGuestIP)
		if err != nil {
			return
		}
		if !validContainerID(id) {
			t.Fatalf("bad id %q accepted", id)
		}
		if len(ports) > maxPortsPerContainer {
			t.Fatalf("%d ports", len(ports))
		}
		for i, p := range ports {
			if p == 0 || (i > 0 && ports[i-1] >= p) {
				t.Fatalf("ports not sorted, unique and non-zero: %v", ports)
			}
		}
	})
}
