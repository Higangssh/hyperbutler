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
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/diag"
	"github.com/Higangssh/hyperbutler/internal/netstack"
	"github.com/Higangssh/hyperbutler/internal/socktable"
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
	// CRC is the per-frame CRC-32C of the offload link (netstack.Config.LinkCRC): on by
	// default; off only to measure what it costs, strict to make a mismatch end the link
	// (benches). The guest is told with hyperbutler.netcrc.
	CRC netstack.CRCMode
	// Port is the AF_VSOCK port the guest forwarder connects to; 0 means 1024.
	Port uint32
	// Publish are static published ports.
	Publish []netstack.Publish
	// HostForwards let the guest reach host services.
	HostForwards []netstack.HostForward
	// HostGateway, if set, is the initial setting of host.docker.internal
	// (REQ-NET-004); the guest's dockerd gets its address as --host-gateway-ip. nil
	// leaves the guest without one (dockerd's own default, the docker0 address).
	HostGateway *netstack.HostGateway
	// AutoPublish mirrors `docker run -p` ports on the host's loopback.
	AutoPublish bool
	// OnUnpublished, if set, is called with every port of running containers that
	// auto-publishing could not mirror (State.UnpublishedPorts), whenever that changes.
	OnUnpublished func([]UnpublishedPort)
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

	ap       *autoPublisher // set by startAutoPublish
	apCancel context.CancelFunc
	apDone   chan struct{}

	dialCancel context.CancelFunc
	dialDone   chan struct{}

	unpublish func()
}

// NetVarName is the name under which the engine publishes NetVars on its debug
// endpoint (diag.VarsPath + NetVarName).
const NetVarName = "net"

// NetVars is the guest network's diagnostics document: the stack's counters and what
// the guest asked the network for (REQ-UX-005, bench/no-telemetry).
type NetVars struct {
	Stats netstack.Stats   `json:"stats"`
	Flows netstack.FlowLog `json:"flows"`
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
	cfg := netstack.Config{Subnet: subnet, MTU: nc.MTU, HostForwards: nc.HostForwards, HostGateway: nc.HostGateway, LinkCRC: nc.CRC}
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
	g.unpublish = diag.Publish(NetVarName, func() any { return NetVars{Stats: st.Stats(), Flows: st.Flows()} })
	// Only this user may listen on the link's service (for a guest forwarder that dials
	// the host) and, in offload mode, connect to it (netfwd -vnet listens in the guest).
	svc := vm.Service{ID: g.svc, BindSDDL: sddl}
	args = fmt.Sprintf(" hyperbutler.net=%d hyperbutler.mtu=%d hyperbutler.ip=%s/%d hyperbutler.gw=%s hyperbutler.dns=%s",
		nc.Port, cfg.MTU, cfg.GuestIP(), cfg.Subnet.Bits(), cfg.Gateway(), cfg.Gateway())
	if cfg.HostGateway != nil {
		// Always passed while the stack has a host gateway, on or off: turning it off
		// refuses connections in the stack, so dockerd need not restart for it.
		args += " hyperbutler.hostgw=" + cfg.HostGatewayAddr().String()
	}
	if !nc.NoOffload {
		svc.ConnectSDDL = sddl
		args += " hyperbutler.netvnet=1"
		args += cfg.LinkCRC.KernelArg()
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
			g.dialGuest(dctx, vmid, func() { linkUp("offload, link CRC " + g.cfg.LinkCRC.String()) })
		}()
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if offload, known := g.st.Offloading(); known && offload {
				// The link is in offload mode for good; a guest program that dials
				// the host is refused quietly, however often it tries.
				_ = c.Close()
				g.log.Debug("refused a plain guest link: the link is in offload mode")
				continue
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
	if a, hg := g.st.HostGateway(); a.IsValid() {
		step("host gateway: host.docker.internal is %s, forwarding to this host's loopback (%s)", a, hg)
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
	if g.unpublish != nil {
		g.unpublish()
	}
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

// startAutoPublish runs autoPublish until close, and from then on checkStart can be
// asked about container starts.
func (g *guestNet) startAutoPublish(ctx context.Context, target transport.HvsockTarget, step func(string, ...any)) {
	d := &transport.HvsockDialer{Target: target, Retries: 2, RetryWait: 100 * time.Millisecond}
	client := &http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return d.DialContext(ctx) },
		DisableKeepAlives: true,
	}}
	g.ap = &autoPublisher{g: g, client: client, step: step, open: map[string][]io.Closer{},
		failed: map[string][]UnpublishedPort{}, check: hostPortFree}
	ctx, g.apCancel = context.WithCancel(ctx)
	g.apDone = make(chan struct{})
	go func() {
		defer close(g.apDone)
		g.autoPublish(ctx)
	}()
}

// checkStart is the API proxy's StartChecker (REQ-NET-005, proposed): it refuses the
// start of a container whose published ports a host program already holds, before
// dockerd starts it, so `docker run` and `docker start` fail with Docker's "Ports are
// not available" error as on Docker Desktop. Anything it cannot judge is let through.
func (g *guestNet) checkStart(ctx context.Context, ref string) error {
	if g.ap == nil {
		return nil
	}
	return g.ap.checkStart(ctx, ref)
}

// autoPublish follows dockerd's container events and mirrors published ports.
//
// The port numbers come from the guest. A port that any host socket already listens on
// is refused (hostPortFree), and a container's ports are published all together or not
// at all (publishAll). A guest can still claim a free loopback port before a host
// program does; see ADR 0006, review finding 1.
func (g *guestNet) autoPublish(ctx context.Context) {
	ap := g.ap
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
	// check reports a host port that is taken, without binding it (hostPortFree).
	check func(uint16) error
	mu    sync.Mutex
	open  map[string][]io.Closer
	count int
	// failed are the ports of running containers that could not be published, by
	// container; State.UnpublishedPorts shows them.
	failed map[string][]UnpublishedPort
}

func (a *autoPublisher) follow(ctx context.Context) error {
	filters := url.QueryEscape(`{"type":["container"],"event":["start","die","destroy"]}`)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/events?filters="+filters, nil)
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", path, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxInspectBytes+1))
}

func (a *autoPublisher) checkStart(ctx context.Context, ref string) error {
	b, err := a.get(ctx, "/containers/"+url.PathEscape(ref)+"/json")
	if err != nil {
		return nil // an unknown container, or dockerd not answering: dockerd says so
	}
	id, running, ports, err := startPorts(b, a.g.cfg.GuestIP())
	if err != nil || running {
		return nil
	}
	a.mu.Lock()
	mirrored := len(a.open[id]) > 0
	a.mu.Unlock()
	if mirrored {
		// Its ports are still this engine's listeners (the stop was not synced yet), and
		// the publish after the start keeps them: the bind in check would fail on them.
		return nil
	}
	for _, p := range ports {
		if err := a.check(p); err != nil {
			a.g.log.Warn("auto-publish: refused a container start: its host port is not available",
				"container", id[:12], "port", p, "err", err)
			return err
		}
	}
	return nil
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
	a.mu.Lock()
	room := maxAutoPublished - a.count
	a.mu.Unlock()
	if 2*len(ports) > room {
		a.g.log.Warn("auto-publish: limit reached", "limit", maxAutoPublished, "container", id[:12])
		a.fail(id, ports, fmt.Errorf("hyperbutler publishes at most %d host listeners", maxAutoPublished))
		return
	}
	// REQ-NET-001: 127.0.0.1, [::1] and localhost; all of a container's ports or none.
	closers, err := publishAll(ports, a.check, func(host netip.AddrPort, guest uint16) (io.Closer, error) {
		return a.g.st.Publish(netstack.Publish{Host: host, GuestPort: guest})
	}, ipv6Available)
	if err != nil {
		a.g.log.Warn("auto-publish: none of the container's ports is published: a host program already uses one",
			"container", id[:12], "ports", fmt.Sprint(ports), "err", err)
		a.fail(id, ports, err)
		return
	}
	for _, p := range ports {
		a.step("auto-published port %d on 127.0.0.1 and [::1] for container %s", p, id[:12])
	}
	a.mu.Lock()
	a.open[id] = closers
	a.count += len(closers)
	a.mu.Unlock()
}

// fail records that none of a running container's ports is published.
func (a *autoPublisher) fail(id string, ports []uint16, err error) {
	a.mu.Lock()
	a.open[id] = nil // tried: a later sync does not try again while it runs
	if len(ports) > 0 {
		a.failed[id] = unpublished(id, ports, err, time.Now().UTC().Truncate(time.Second))
	}
	a.mu.Unlock()
	a.reportFailed()
}

// reportFailed hands the current list of unpublished ports to NetConfig.OnUnpublished.
func (a *autoPublisher) reportFailed() {
	on := a.g.nc.OnUnpublished
	if on == nil {
		return
	}
	a.mu.Lock()
	var all []UnpublishedPort
	for _, us := range a.failed {
		all = append(all, us...)
	}
	a.mu.Unlock()
	sort.Slice(all, func(i, j int) bool {
		if all[i].Port != all[j].Port {
			return all[i].Port < all[j].Port
		}
		return all[i].Container < all[j].Container
	})
	if len(all) > MaxUnpublishedPorts {
		all = all[:MaxUnpublishedPorts]
	}
	on(all)
}

func (a *autoPublisher) remove(id string) {
	a.mu.Lock()
	cs := a.open[id]
	delete(a.open, id)
	a.count -= len(cs)
	_, hadFailed := a.failed[id]
	delete(a.failed, id)
	a.mu.Unlock()
	for _, c := range cs {
		_ = c.Close()
	}
	if len(cs) > 0 {
		a.step("unpublished %d listener(s) of container %s", len(cs), id[:12])
	}
	if hadFailed {
		a.reportFailed()
	}
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

// hostPortFree reports an error when some host socket already listens on TCP port p,
// on any address.
//
// Why ask at all: Windows lets a listener on the wildcard address (0.0.0.0:P, [::]:P)
// and one on a specific address (127.0.0.1:P) coexist, so a successful bind of
// 127.0.0.1:P does not mean the port is free. A host server on 0.0.0.0:P would then
// lose its loopback clients to the container. The reverse split happens too: with a
// host program on 127.0.0.1:P only, our 127.0.0.1 bind fails while [::1] succeeds, and
// `localhost` reaches the container while 127.0.0.1 reaches the host program
// (publishAll closes the [::1] half then). A listener on a specific interface address
// (the LAN IP) counts as well: a client of that service on this machine may use either
// address, and Docker, which binds the wildcard, collides with it too.
//
// How: the system's TCP listener tables (GetExtendedTcpTable, IPv4 and IPv6), which
// list every listener on every address, and reading them opens no socket. The earlier
// probe bound 0.0.0.0:P and [::]:P for a moment, and a wildcard bind can raise the
// Windows Firewall "allow access" prompt for hyperbutler.exe on a user's `docker run
// -p`. Listeners of this engine (another container's port) are not counted there.
//
// Then, when the tables say the port is free (or cannot be read), it binds 127.0.0.1:P
// and [::1]:P (loopback only, never a wildcard) and closes them at once. A port no
// socket holds can still refuse a bind: Hyper-V and WinNAT reserve ranges of ports
// (`netsh int ipv4 show excludedportrange protocol=tcp`), where a bind fails with
// WSAEACCES although no listener exists, and the publish after the start would then
// fail with nothing published. The bind also catches what the tables leave out: a
// listener of this engine, which is another container publishing the same host port
// (dockerd allows two containers one host port on different guest addresses, and both
// map to the host's loopback), and, when the tables cannot be read, any loopback
// listener (though not one on the wildcard or the LAN address).
func hostPortFree(p uint16) error {
	return hostPortFreeExcept(p, uint32(os.Getpid())) //nolint:gosec // G115: Windows process IDs are DWORDs
}

// hostPortFreeExcept is hostPortFree with the listeners of process self counted as the
// engine's own.
func hostPortFreeExcept(p uint16, self uint32) error {
	var r socktable.Reader
	ss, err := r.TCPListeners()
	return portFreeWith(p, self, ss, err, net.Listen, ipv6Available)
}

// listenFunc opens a TCP listener, as net.Listen does (a parameter, so tests can
// refuse a bind).
type listenFunc func(network, address string) (net.Listener, error)

// portFreeWith is hostPortFreeExcept with the listener table ss (unreadable when
// tableErr is set), the bind and the IPv6 check given.
func portFreeWith(p uint16, self uint32, ss []socktable.Socket, tableErr error, listen listenFunc, v6 func() bool) error {
	if tableErr == nil {
		if err := portFreeIn(ss, p, self, addrInUse()); err != nil {
			return err
		}
	}
	err := loopbackPortFree(p, listen, v6)
	var pu *PortsUnavailable
	if errors.As(err, &pu) && tableErr == nil && listensOn(ss, p, self) {
		// The holder is this engine: not "another program".
		pu.Cause = fmt.Errorf("%w (another container of this engine publishes host port %d)", pu.Cause, p)
	}
	return err
}

// listensOn reports whether process pid has a TCP listener on port p in ss.
func listensOn(ss []socktable.Socket, p uint16, pid uint32) bool {
	for _, s := range ss {
		if strings.HasPrefix(s.Proto, "tcp") && s.State == "LISTEN" && s.Local.Port() == p && s.PID == pid {
			return true
		}
	}
	return false
}

// loopbackPortFree binds each loopback address on port p and closes it at once. A
// host without IPv6 loopback (v6 false) needs 127.0.0.1 only.
func loopbackPortFree(p uint16, listen listenFunc, v6 func() bool) error {
	for i, ip := range loopbacks {
		ln, err := listen("tcp", netip.AddrPortFrom(ip, p).String())
		if err != nil {
			if i == 1 && !v6() {
				continue
			}
			return &PortsUnavailable{Port: p, Addr: ip, Cause: bindCause(err)}
		}
		_ = ln.Close()
	}
	return nil
}

// excludedRangeHint follows a bind refused with WSAEACCES, which on a port nobody
// listens on means Windows reserves it.
const excludedRangeHint = "(Windows reserves this port: it is in an excluded port range, such as Hyper-V and WinNAT set; " +
	"list them with `netsh int ipv4 show excludedportrange protocol=tcp`, and publish another host port)"

// bindCause is listenCause, with excludedRangeHint added for WSAEACCES.
func bindCause(err error) error {
	c := listenCause(err)
	if errors.Is(err, windows.WSAEACCES) {
		return fmt.Errorf("%w %s", c, excludedRangeHint)
	}
	return c
}

// addrInUse is WSAEADDRINUSE in the system's words, as a failed bind would say it.
func addrInUse() error {
	if s := windows.WSAEADDRINUSE.Error(); s != "" {
		return errors.New(s)
	}
	return errAddrInUse
}

func ipv6Available() bool {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}
