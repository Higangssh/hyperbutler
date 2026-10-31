//go:build windows

package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"

	"github.com/Higangssh/hyperbutler/internal/netstack"
	"github.com/Higangssh/hyperbutler/internal/transport"
	"github.com/Higangssh/hyperbutler/internal/vm"
)

// NetConfig is the guest network (ADR 0006).
type NetConfig struct {
	// Off leaves the guest without a network.
	Off bool
	// Subnet is the guest subnet (gateway .1, guest .2); zero means
	// netstack.DefaultSubnet.
	Subnet netip.Prefix
	// MTU is the guest link MTU; 0 means netstack.DefaultMTU.
	MTU int
	// NoOffload turns off checksum and TCP segmentation offload on the guest link
	// (netstack.Config.Offload), for comparison.
	NoOffload bool
	// Port is the AF_VSOCK port the guest forwarder connects to; 0 means 1024.
	Port uint32
	// Publish are static published ports.
	Publish []netstack.Publish
	// HostForwards let the guest reach host services.
	HostForwards []netstack.HostForward
	// AutoPublish mirrors `docker run -p` ports on the host's loopback.
	AutoPublish bool
	// Debug logs denied guest connections.
	Debug bool
}

// DefaultNetPort is the default AF_VSOCK port of the guest network link.
const DefaultNetPort = 1024

// guestNet is the running user-mode network of one VM.
type guestNet struct {
	nc      NetConfig
	cfg     netstack.Config
	st      *netstack.Stack
	svc     guid.GUID
	ln      net.Listener
	publish []netstack.Publish
	log     *slog.Logger

	apCancel context.CancelFunc
	apDone   chan struct{}

	dialCancel context.CancelFunc
	dialDone   chan struct{}
}

// setupNet builds the stack. It returns nil when networking is off. The caller adds
// svcs to the VM document and args to the kernel command line.
func setupNet(nc NetConfig, sddl string, log *slog.Logger) (g *guestNet, svcs []vm.Service, args string, err error) {
	if nc.Off {
		return nil, nil, "", nil
	}
	if nc.Port == 0 {
		nc.Port = DefaultNetPort
	}
	if nc.Port == 0xFFFFFFFF {
		return nil, nil, "", errors.New("engine: network vsock port is out of range")
	}
	subnet := nc.Subnet
	if !subnet.IsValid() {
		subnet = netstack.DefaultSubnet
	}
	cfg := netstack.Config{Subnet: subnet, MTU: nc.MTU, HostForwards: nc.HostForwards}
	nlog := log
	if !nc.Debug {
		nlog = slog.New(levelFilter{h: log.Handler(), min: slog.LevelWarn})
	}
	st, err := netstack.New(cfg, nlog)
	if err != nil {
		return nil, nil, "", err
	}
	cfg = st.Config()
	g = &guestNet{nc: nc, cfg: cfg, st: st, svc: transport.VsockServiceID(nc.Port), publish: nc.Publish, log: log}
	// Only this user may listen on the link's service (for a guest forwarder that dials
	// the host) and, in offload mode, connect to it (netfwd -vnet listens in the guest).
	svc := vm.Service{ID: g.svc, BindSDDL: sddl}
	args = fmt.Sprintf(" hyperbutler.net=%d hyperbutler.mtu=%d hyperbutler.ip=%s/%d hyperbutler.gw=%s hyperbutler.dns=%s",
		nc.Port, cfg.MTU, cfg.GuestIP(), cfg.Subnet.Bits(), cfg.Gateway(), cfg.Gateway())
	if !nc.NoOffload {
		svc.ConnectSDDL = sddl
		args += " hyperbutler.netvnet=1"
	}
	svcs = []vm.Service{svc}
	return g, svcs, args, nil
}

// levelFilter drops records below min.
type levelFilter struct {
	h   slog.Handler
	min slog.Level
}

func (f levelFilter) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= f.min && f.h.Enabled(ctx, l)
}
func (f levelFilter) Handle(ctx context.Context, r slog.Record) error { return f.h.Handle(ctx, r) }
func (f levelFilter) WithAttrs(a []slog.Attr) slog.Handler {
	return levelFilter{h: f.h.WithAttrs(a), min: f.min}
}
func (f levelFilter) WithGroup(n string) slog.Handler {
	return levelFilter{h: f.h.WithGroup(n), min: f.min}
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
	var once sync.Once
	linkUp := func(how string) {
		once.Do(func() {
			step("guest network link up (%s/%d, gateway %s, MTU %d, %s)", g.cfg.GuestIP(), g.cfg.Subnet.Bits(), g.cfg.Gateway(), g.cfg.MTU, how)
		})
	}
	if !g.nc.NoOffload {
		dctx, cancel := context.WithCancel(ctx)
		g.dialCancel, g.dialDone = cancel, make(chan struct{})
		go func() {
			defer close(g.dialDone)
			g.dialGuest(dctx, vmid, func() { linkUp("offload") })
		}()
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			linkUp("plain")
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

// dialGuest connects to the guest forwarder in offload mode (netfwd -vnet listens on
// the link's vsock port) and serves each connection until ctx ends, dialing again when
// a link ends (netfwd restarted). A guest image whose netfwd dials the host instead
// gets a plain link through the listener, and dialing stops once that link is up.
func (g *guestNet) dialGuest(ctx context.Context, vmid guid.GUID, linkUp func()) {
	d := &transport.HvsockDialer{Target: transport.HvsockTarget{VMID: vmid, ServiceID: g.svc}}
	wait := 50 * time.Millisecond
	for ctx.Err() == nil {
		if offload, known := g.st.Offloading(); known && !offload {
			return
		}
		// A connect to a port nobody listens on yet can stay pending for about 30 s
		// (the abandoned attempt finishes in the background); bound each attempt so the
		// guest's listener is found soon after it appears. With 1 s the link came up
		// 0.8 s later than one the guest dials.
		actx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		c, err := d.DialContext(actx)
		cancel()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			wait = min(2*wait, time.Second)
			continue
		}
		wait = 50 * time.Millisecond
		linkUp()
		if err := g.st.ServeOffload(c); err != nil && ctx.Err() == nil {
			g.log.Warn("guest network link ended", "err", err)
		}
	}
}

func (g *guestNet) close() {
	if g.apCancel != nil {
		g.apCancel()
		<-g.apDone
	}
	if g.ln != nil {
		_ = g.ln.Close()
	}
	if g.dialCancel != nil {
		g.dialCancel()
	}
	g.st.Close()
	if g.dialDone != nil {
		<-g.dialDone
	}
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

// maxAutoPublished bounds the host listeners the guest can make the engine open.
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
