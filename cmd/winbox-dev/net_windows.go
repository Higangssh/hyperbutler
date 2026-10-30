//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"

	"github.com/Higangssh/winbox/internal/engine"
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

// config turns the flags into the engine's network configuration, plus the services
// and kernel arguments the bench adds.
func (nf *netFlags) config(sddl string) (engine.NetConfig, []vm.Service, string, error) {
	nc := engine.NetConfig{MTU: nf.mtu, AutoPublish: nf.autoPublish, Debug: nf.debug}
	switch nf.mode {
	case "none":
		nc.Off = true
		return nc, nil, "", nil
	case "user":
	default:
		return nc, nil, "", fmt.Errorf("-net %q: want user or none", nf.mode)
	}
	if nf.port == 0 || nf.port > 0xFFFFFFFE || nf.port == benchVsockPort {
		return nc, nil, "", errors.New("-net-port is out of range")
	}
	nc.Port = uint32(nf.port)
	subnet, err := netip.ParsePrefix(nf.subnet)
	if err != nil {
		return nc, nil, "", fmt.Errorf("-net-subnet: %w", err)
	}
	nc.Subnet = subnet
	for _, s := range nf.hostFwd {
		h, err := netstack.ParseHostForward(s)
		if err != nil {
			return nc, nil, "", err
		}
		nc.HostForwards = append(nc.HostForwards, h)
	}
	for _, s := range nf.publish {
		p, err := netstack.ParsePublish(s, nf.publishLAN)
		if err != nil {
			return nc, nil, "", err
		}
		nc.Publish = append(nc.Publish, p)
	}
	var svcs []vm.Service
	args := ""
	if nf.benchBytes > 0 {
		for _, off := range []uint32{0, 2, 4} { // source, sink, echo
			svcs = append(svcs, vm.Service{ID: transport.VsockServiceID(benchVsockPort + off), ConnectSDDL: sddl})
		}
		args = " winbox.netbench=1"
	}
	return nc, svcs, args, nil
}

// benchEnv is what the bench needs from the running engine.
type benchEnv struct {
	st *netstack.Stack
}

// --- bench ----------------------------------------------------------------------------

// bench moves n bytes three times over each path, alternating: raw hvsock (guest netfwd's
// vsock ports) and the user-mode stack (publishes to guest netfwd's TCP ports), first
// guest to host, then host to guest (the direction of an image pull). It reports
// throughput and this process's CPU time.
func (g *benchEnv) bench(ctx context.Context, vmid guid.GUID, n int64, step func(string, ...any)) error {
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
func (g *benchEnv) benchLatency(ctx context.Context, vmid guid.GUID, load benchPath, step func(string, ...any)) error {
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
