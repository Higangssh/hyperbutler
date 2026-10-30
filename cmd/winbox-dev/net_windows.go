//go:build windows

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"

	"github.com/Higangssh/winbox/internal/netstack"
	"github.com/Higangssh/winbox/internal/transport"
	"github.com/Higangssh/winbox/internal/vm"
)

// netFlags are the user-mode networking options of `winbox-dev run` (ADR 0006).
type netFlags struct {
	mode        string
	subnet      string
	mtu         int
	port        uint
	publish     multiFlag
	publishLAN  bool
	autoPublish bool
	hostFwd     multiFlag
	benchBytes  int64
	debug       bool
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

const (
	benchTCPPort   = 5201
	benchVsockPort = 5202
)

func (nf *netFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&nf.mode, "net", "user", `guest network: "user" (user-mode stack over hvsock, ADR 0006) or "none"`)
	fs.StringVar(&nf.subnet, "net-subnet", netstack.DefaultSubnet.String(), "guest subnet (gateway .1, guest .2)")
	fs.IntVar(&nf.mtu, "net-mtu", netstack.DefaultMTU, "guest link MTU")
	fs.UintVar(&nf.port, "net-port", 1024, "AF_VSOCK port the guest forwarder connects to")
	fs.Var(&nf.publish, "publish", "publish a guest TCP port: [HOST_IP:]HOST_PORT:GUEST_PORT (repeatable)")
	fs.BoolVar(&nf.publishLAN, "publish-lan", false, "allow -publish and auto-publish on non-loopback host addresses")
	fs.BoolVar(&nf.autoPublish, "auto-publish", true, "mirror `docker run -p` ports on the host's loopback")
	fs.Var(&nf.hostFwd, "host-forward", "let the guest reach a host service: VIRTUAL_IP:PORT=HOST_IP:PORT (repeatable)")
	fs.Int64Var(&nf.benchBytes, "bench-bytes", 0, "after boot, compare raw hvsock with the stack by reading this many bytes each way, then exit")
	fs.BoolVar(&nf.debug, "net-debug", false, "log denied guest connections")
}

// guestNet is the running user-mode network of one VM.
type guestNet struct {
	flags   *netFlags
	cfg     netstack.Config
	st      *netstack.Stack
	svc     guid.GUID
	ln      net.Listener
	publish []netstack.Publish
	log     *slog.Logger

	apCancel context.CancelFunc
	apDone   chan struct{}
}

// setupNet validates the flags and builds the stack. It returns nil when networking is
// off. The caller adds svcs to the VM document and args to the kernel command line.
func setupNet(nf *netFlags, sddl string) (g *guestNet, svcs []vm.Service, args string, err error) {
	switch nf.mode {
	case "none":
		return nil, nil, "", nil
	case "user":
	default:
		return nil, nil, "", fmt.Errorf("-net %q: want user or none", nf.mode)
	}
	if nf.port == 0 || nf.port > 0xFFFFFFFE || nf.port == benchVsockPort {
		return nil, nil, "", errors.New("-net-port is out of range")
	}
	subnet, err := netip.ParsePrefix(nf.subnet)
	if err != nil {
		return nil, nil, "", fmt.Errorf("-net-subnet: %w", err)
	}
	cfg := netstack.Config{Subnet: subnet, MTU: nf.mtu}
	for _, s := range nf.hostFwd {
		h, err := netstack.ParseHostForward(s)
		if err != nil {
			return nil, nil, "", err
		}
		cfg.HostForwards = append(cfg.HostForwards, h)
	}
	var pubs []netstack.Publish
	for _, s := range nf.publish {
		p, err := netstack.ParsePublish(s, nf.publishLAN)
		if err != nil {
			return nil, nil, "", err
		}
		pubs = append(pubs, p)
	}
	level := slog.LevelWarn
	if nf.debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	st, err := netstack.New(cfg, log)
	if err != nil {
		return nil, nil, "", err
	}
	cfg = st.Config()
	g = &guestNet{flags: nf, cfg: cfg, st: st, svc: transport.VsockServiceID(uint32(nf.port)), publish: pubs, log: log}
	// The guest connects to the host on this service; only this user may listen on it,
	// and nobody may connect to it in the guest.
	svcs = []vm.Service{{ID: g.svc, BindSDDL: sddl}}
	args = fmt.Sprintf(" winbox.net=%d winbox.mtu=%d winbox.ip=%s/%d winbox.gw=%s winbox.dns=%s",
		nf.port, cfg.MTU, cfg.GuestIP(), cfg.Subnet.Bits(), cfg.Gateway(), cfg.Gateway())
	if nf.benchBytes > 0 {
		for _, off := range []uint32{0, 2, 4} { // source, sink, echo
			svcs = append(svcs, vm.Service{ID: transport.VsockServiceID(benchVsockPort + off), ConnectSDDL: sddl})
		}
		args += " winbox.netbench=1"
	}
	return g, svcs, args, nil
}

// start listens for the guest forwarder on the VM's hvsock address and opens the static
// publishes.
func (g *guestNet) start(ctx context.Context, vmid guid.GUID, step func(string, ...any)) error {
	ln, err := winio.ListenHvsock(&winio.HvsockAddr{VMID: vmid, ServiceID: g.svc})
	if err != nil {
		return fmt.Errorf("listen hvsock %s: %w", g.svc, err)
	}
	g.ln = ln
	context.AfterFunc(ctx, func() { _ = ln.Close() })
	go func() {
		first := true
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if first {
				step("guest network link up (%s/%d, gateway %s, MTU %d)", g.cfg.GuestIP(), g.cfg.Subnet.Bits(), g.cfg.Gateway(), g.cfg.MTU)
				first = false
			}
			go func() {
				if err := g.st.ServeConn(c); err != nil && ctx.Err() == nil {
					g.log.Warn("guest network link ended", "err", err)
				}
			}()
		}
	}()
	for _, p := range g.publish {
		if _, err := g.st.Publish(p); err != nil {
			return err
		}
		step("published %s", p)
	}
	return nil
}

func (g *guestNet) close() {
	if g.apCancel != nil {
		g.apCancel()
		<-g.apDone
	}
	if g.ln != nil {
		_ = g.ln.Close()
	}
	g.st.Close()
}

// --- auto-publish -------------------------------------------------------------------

// startAutoPublish runs autoPublish until close.
func (g *guestNet) startAutoPublish(ctx context.Context, target transport.HvsockTarget, step func(string, ...any)) {
	ctx, g.apCancel = context.WithCancel(ctx)
	g.apDone = make(chan struct{})
	go func() {
		defer close(g.apDone)
		g.autoPublish(ctx, target, step)
	}()
}

// autoPublish follows dockerd's container events and mirrors published ports.
//
// The port numbers come from the guest. A port that any host socket already listens on
// is refused, because a specific 127.0.0.1 bind would otherwise take loopback clients
// away from a host service bound to the wildcard address. A guest can still claim a
// free loopback port before a host program does; see ADR 0006, review finding 1.
func (g *guestNet) autoPublish(ctx context.Context, target transport.HvsockTarget, step func(string, ...any)) {
	d := &transport.HvsockDialer{Target: target, Retries: 2, RetryWait: 100 * time.Millisecond}
	client := &http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return d.DialContext(ctx) },
		DisableKeepAlives: true,
	}}
	ap := &autoPublisher{g: g, client: client, step: step, open: map[string][]io.Closer{}}
	for ctx.Err() == nil {
		if err := ap.follow(ctx); err != nil && ctx.Err() == nil {
			g.log.Warn("auto-publish: event stream ended", "err", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	ap.closeAll()
}

// maxAutoPublished bounds the host listeners the guest can make winbox-dev open.
const maxAutoPublished = 256

type autoPublisher struct {
	g      *guestNet
	client *http.Client
	step   func(string, ...any)
	mu     sync.Mutex
	open   map[string][]io.Closer
	count  int
}

func (a *autoPublisher) follow(ctx context.Context) error {
	filters := url.QueryEscape(`{"type":["container"],"event":["start","die","destroy"]}`)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/events?filters="+filters, nil)
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("events: %s", resp.Status)
	}
	// Containers that were already running when the stream opened, and those that went
	// away while it was down.
	a.sync(ctx)
	br := bufio.NewReaderSize(resp.Body, maxEventLine)
	for {
		line, err := readEventLine(br)
		if err != nil {
			return err
		}
		var ev dockerEvent
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if ev.Type != "container" || !validContainerID(ev.Actor.ID) {
			continue
		}
		switch ev.Action {
		case "start":
			a.add(ctx, ev.Actor.ID)
		case "die", "destroy":
			a.remove(ev.Actor.ID)
		}
	}
}

func (a *autoPublisher) sync(ctx context.Context) {
	b, err := a.get(ctx, "/containers/json")
	if err != nil {
		return
	}
	var list []struct {
		ID string `json:"Id"`
	}
	if json.Unmarshal(b, &list) != nil {
		return
	}
	running := map[string]bool{}
	for i, c := range list {
		if i >= maxAutoPublished || !validContainerID(c.ID) {
			break
		}
		running[c.ID] = true
	}
	a.mu.Lock()
	var gone []string
	for id := range a.open {
		if !running[id] {
			gone = append(gone, id)
		}
	}
	a.mu.Unlock()
	for _, id := range gone {
		a.remove(id)
	}
	for id := range running {
		a.add(ctx, id)
	}
}

func (a *autoPublisher) get(ctx context.Context, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", path, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxInspectBytes+1))
}

func (a *autoPublisher) add(ctx context.Context, id string) {
	a.mu.Lock()
	_, done := a.open[id]
	a.mu.Unlock()
	if done {
		return
	}
	b, err := a.get(ctx, "/containers/"+id+"/json")
	if err != nil {
		return
	}
	_, ports, skipped, err := publishedPorts(b, a.g.cfg.GuestIP())
	if err != nil {
		a.g.log.Warn("auto-publish: ignoring container", "err", err)
		return
	}
	if skipped > 0 {
		a.g.log.Warn("auto-publish: bindings to a guest-only address (such as -p 127.0.0.1:P:C) are not reachable from the host; use -p P:C",
			"container", id[:12], "skipped", skipped)
	}
	var closers []io.Closer
	for _, p := range ports {
		a.mu.Lock()
		full := a.count >= maxAutoPublished
		a.mu.Unlock()
		if full {
			a.g.log.Warn("auto-publish: limit reached", "limit", maxAutoPublished)
			break
		}
		if err := portInUse(p); err != nil {
			a.g.log.Warn("auto-publish: refusing a port a host program already uses", "port", p, "err", err)
			continue
		}
		// REQ-NET-001: 127.0.0.1, [::1] and localhost. IPv4 must work; IPv6 loopback is
		// best effort (it can be disabled on the host).
		for _, ip := range []netip.Addr{netip.AddrFrom4([4]byte{127, 0, 0, 1}), netip.IPv6Loopback()} {
			pub := netstack.Publish{Host: netip.AddrPortFrom(ip, p), GuestPort: p}
			c, err := a.g.st.Publish(pub)
			if err != nil {
				a.g.log.Warn("auto-publish failed", "addr", pub.Host, "err", err)
				continue
			}
			a.step("auto-published %s for container %s", pub, id[:12])
			closers = append(closers, c)
			a.mu.Lock()
			a.count++
			a.mu.Unlock()
		}
	}
	a.mu.Lock()
	a.open[id] = closers
	a.mu.Unlock()
}

func (a *autoPublisher) remove(id string) {
	a.mu.Lock()
	cs := a.open[id]
	delete(a.open, id)
	a.count -= len(cs)
	a.mu.Unlock()
	for _, c := range cs {
		_ = c.Close()
	}
	if len(cs) > 0 {
		a.step("unpublished %d port(s) of container %s", len(cs), id[:12])
	}
}

// portInUse reports an error when some host socket already listens on TCP port p on any
// address. Binding the wildcard address of the same family fails then (without
// SO_REUSEADDR), so successful probe binds, closed at once, mean the port is free. The
// IPv4, IPv6-only and dual-stack sockets are probed separately: Windows lets a wildcard
// listener of one kind coexist with one of another (measured: a dual-stack [::]:P
// listener passes both single-family probes, and an IPv4 0.0.0.0:P listener passes the
// dual-stack one).
func portInUse(p uint16) error {
	for _, probe := range []struct{ network, addr string }{
		{"tcp4", fmt.Sprintf("0.0.0.0:%d", p)},
		{"tcp6", fmt.Sprintf("[::]:%d", p)},
		{"tcp", fmt.Sprintf(":%d", p)}, // dual-stack
	} {
		ln, err := net.Listen(probe.network, probe.addr)
		if err != nil {
			if probe.network != "tcp4" && !ipv6Available() {
				continue
			}
			return err
		}
		_ = ln.Close()
	}
	return nil
}

func ipv6Available() bool {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func (a *autoPublisher) closeAll() {
	a.mu.Lock()
	ids := make([]string, 0, len(a.open))
	for id := range a.open {
		ids = append(ids, id)
	}
	a.mu.Unlock()
	for _, id := range ids {
		a.remove(id)
	}
}

// --- bench ----------------------------------------------------------------------------

// bench moves n bytes three times over each path, alternating: raw hvsock (guest netfwd's
// vsock ports) and the user-mode stack (publishes to guest netfwd's TCP ports), first
// guest to host, then host to guest (the direction of an image pull). It reports
// throughput and this process's CPU time.
func (g *guestNet) bench(ctx context.Context, vmid guid.GUID, n int64, step func(string, ...any)) error {
	var paths []benchPath
	for _, up := range []bool{false, true} {
		off := uint16(0)
		dir := "guest->host"
		if up {
			off, dir = 2, "host->guest"
		}
		pub, err := g.st.Publish(netstack.Publish{Host: netip.MustParseAddrPort("127.0.0.1:0"), GuestPort: benchTCPPort + off})
		if err != nil {
			return err
		}
		defer pub.Close()
		stackAddr := pub.(net.Listener).Addr().String()
		raw := &transport.HvsockDialer{Target: transport.HvsockTarget{VMID: vmid, ServiceID: transport.VsockServiceID(benchVsockPort + uint32(off))}, Retries: 20, RetryWait: 100 * time.Millisecond}
		paths = append(paths,
			benchPath{"raw hvsock " + dir, func() (net.Conn, error) { return raw.DialContext(ctx) }, up},
			benchPath{"stack " + dir, func() (net.Conn, error) { return net.DialTimeout("tcp", stackAddr, 10*time.Second) }, up})
	}
	// Wait until the guest's bench servers answer.
	for _, d := range paths[:2] {
		deadline := time.Now().Add(15 * time.Second)
		for {
			c, err := d.dial()
			if err == nil {
				buf := make([]byte, 1)
				_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
				_, err = io.ReadFull(c, buf)
				_ = c.Close()
			}
			if err == nil {
				break
			}
			if time.Now().After(deadline) || ctx.Err() != nil {
				return fmt.Errorf("bench %s not ready: %v", d.name, err)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if err := g.benchLatency(ctx, vmid, paths[2], step); err != nil {
		return err
	}
	for _, pair := range [][]benchPath{paths[:2], paths[2:]} {
		for round := 1; round <= 3; round++ {
			for _, d := range pair {
				s0 := g.st.Stats()
				got, el, cpu, err := d.run(n)
				if err != nil {
					return fmt.Errorf("bench %s: %w after %d bytes", d.name, err, got)
				}
				s1 := g.st.Stats()
				gbit := float64(got) * 8 / el.Seconds() / 1e9
				step("bench round %d %-23s %d MiB in %v = %.2f Gbit/s, winbox-dev CPU %.0f ms (%.2f cores, %.2f cores per Gbit/s), stack retransmits %d of %d segments, RTOs %d",
					round, d.name, got>>20, el.Round(time.Millisecond), gbit, float64(cpu.Milliseconds()),
					cpu.Seconds()/el.Seconds(), cpu.Seconds()/el.Seconds()/gbit,
					s1.TCPRetransmits-s0.TCPRetransmits, s1.TCPSegmentsSent-s0.TCPSegmentsSent, s1.TCPTimeouts-s0.TCPTimeouts)
			}
		}
	}
	return nil
}

// latencyRounds is the number of 64-byte request/response round trips per measurement.
const latencyRounds = 20000

// benchLatency measures small-request round trips (64 bytes out, 64 back, one at a time)
// over raw hvsock and through the stack, idle and while a bulk host->guest transfer runs
// through the stack, so GC pauses and queueing in the Go stack show up in the tail.
func (g *guestNet) benchLatency(ctx context.Context, vmid guid.GUID, load benchPath, step func(string, ...any)) error {
	pub, err := g.st.Publish(netstack.Publish{Host: netip.MustParseAddrPort("127.0.0.1:0"), GuestPort: benchTCPPort + 4})
	if err != nil {
		return err
	}
	defer pub.Close()
	stackAddr := pub.(net.Listener).Addr().String()
	raw := &transport.HvsockDialer{Target: transport.HvsockTarget{VMID: vmid, ServiceID: transport.VsockServiceID(benchVsockPort + 4)}, Retries: 20, RetryWait: 100 * time.Millisecond}
	echoes := []struct {
		name string
		dial func() (net.Conn, error)
	}{
		{"raw hvsock", func() (net.Conn, error) { return raw.DialContext(ctx) }},
		{"stack", func() (net.Conn, error) {
			c, err := net.DialTimeout("tcp", stackAddr, 10*time.Second)
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetNoDelay(true)
			}
			return c, err
		}},
	}
	for _, loaded := range []bool{false, true} {
		for _, e := range echoes {
			var stopLoad func()
			if loaded {
				done := make(chan struct{})
				lctx, cancel := context.WithCancel(ctx)
				go func() {
					defer close(done)
					for lctx.Err() == nil {
						if _, _, _, err := load.run(256 << 20); err != nil {
							return
						}
					}
				}()
				stopLoad = func() { cancel(); <-done }
				time.Sleep(200 * time.Millisecond)
			}
			lat, err := roundTrips(e.dial, latencyRounds)
			if stopLoad != nil {
				stopLoad()
			}
			if err != nil {
				return fmt.Errorf("latency %s: %w", e.name, err)
			}
			cond := "idle"
			if loaded {
				cond = "under bulk load"
			}
			step("latency %-10s %-15s n=%d p50 %v p99 %v p99.9 %v max %v", e.name, cond, len(lat),
				pct(lat, 0.50), pct(lat, 0.99), pct(lat, 0.999), lat[len(lat)-1])
		}
	}
	return nil
}

func roundTrips(dial func() (net.Conn, error), n int) ([]time.Duration, error) {
	c, err := dial()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	req := make([]byte, 64)
	resp := make([]byte, 64)
	// Warm up the connection and both stacks.
	for i := 0; i < 200; i++ {
		if _, err := c.Write(req); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(c, resp); err != nil {
			return nil, err
		}
	}
	// Go's monotonic clock on Windows ticks too coarsely (about 0.5 ms) for this; use the
	// performance counter.
	var freq int64
	if r, _, err := procQPF.Call(uintptr(unsafe.Pointer(&freq))); r == 0 || freq <= 0 {
		return nil, fmt.Errorf("QueryPerformanceFrequency: %v", err)
	}
	qpc := func() int64 {
		var v int64
		_, _, _ = procQPC.Call(uintptr(unsafe.Pointer(&v)))
		return v
	}
	out := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		t0 := qpc()
		if _, err := c.Write(req); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(c, resp); err != nil {
			return nil, err
		}
		t1 := qpc()
		out = append(out, time.Duration(float64(t1-t0)*1e9/float64(freq)))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

var (
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	procQPC  = kernel32.NewProc("QueryPerformanceCounter")
	procQPF  = kernel32.NewProc("QueryPerformanceFrequency")
)

func pct(sorted []time.Duration, p float64) time.Duration {
	i := int(float64(len(sorted)-1) * p)
	return sorted[i].Round(time.Microsecond)
}

type benchPath struct {
	name string
	dial func() (net.Conn, error)
	up   bool // host to guest
}

type closeWriter interface{ CloseWrite() error }

var benchZeros = make([]byte, 256<<10)

// run moves n bytes once: it reads them from a guest source, or writes them to a guest
// sink and waits for the sink's one-byte acknowledgement.
func (d benchPath) run(n int64) (int64, time.Duration, time.Duration, error) {
	c, err := d.dial()
	if err != nil {
		return 0, 0, 0, err
	}
	defer c.Close()
	cpu0 := processCPU()
	t0 := time.Now()
	var got int64
	if !d.up {
		got, err = io.CopyN(io.Discard, c, n)
	} else {
		for got < n && err == nil {
			k := int64(len(benchZeros))
			if n-got < k {
				k = n - got
			}
			var w int
			w, err = c.Write(benchZeros[:k])
			got += int64(w)
		}
		if err == nil {
			if cw, ok := c.(closeWriter); ok {
				err = cw.CloseWrite()
			}
		}
		if err == nil {
			var ack [1]byte
			_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
			_, err = io.ReadFull(c, ack[:])
		}
	}
	return got, time.Since(t0), processCPU() - cpu0, err
}

// processCPU is this process's user plus kernel time.
func processCPU() time.Duration {
	var c, e, k, u windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &c, &e, &k, &u); err != nil {
		return 0
	}
	ft := func(f windows.Filetime) time.Duration {
		return time.Duration((int64(f.HighDateTime)<<32 | int64(f.LowDateTime)) * 100)
	}
	return ft(k) + ft(u)
}
