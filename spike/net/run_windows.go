package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/hcn"
	"github.com/Higangssh/hyperbutler/spike/net/probe"
	"github.com/Higangssh/hyperbutler/spike/net/relay"
)

var (
	guestDir = filepath.Join(os.TempDir(), "hb-dev", "hyperbutler-spike", "guest")
	modDir   = filepath.Join(os.TempDir(), "hb-dev", "hyperbutler-spike", "linux-virt", "lib", "modules", "6.18.58-0-virt", "kernel")
)

// runBaseline runs the probe on the host and saves the roots the guest will need.
func runBaseline() (string, error) {
	var roots bytes.Buffer
	seen := map[string]bool{}
	cfg := probe.Config{
		ResolveLocally: true,
		OnTLS:          func(cs tls.ConnectionState) { rootsFromChains(cs.VerifiedChains, seen, &roots) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	t := time.Now()
	err := probe.Run(ctx, cfg)
	fmt.Printf("WBNET step=done ok=%v ms=%.1f\n", err == nil, float64(time.Since(t).Microseconds())/1000)
	if err != nil {
		return "", err
	}
	p := filepath.Join(workDir, "roots.pem")
	fmt.Printf("collected %d root(s) into %s\n", len(seen), p)
	return roots.String(), os.WriteFile(p, roots.Bytes(), 0o600)
}

func loadRoots() (string, error) {
	b, err := os.ReadFile(filepath.Join(workDir, "roots.pem"))
	if os.IsNotExist(err) {
		return runBaseline()
	}
	return string(b), err
}

func probeBinary() string { return filepath.Join(workDir, "guestprobe") }

func runHNS(args []string) error {
	fs := flag.NewFlagSet("hns", flag.ExitOnError)
	prefix := fs.String("prefix", "10.239.77.0/24", "NAT subnet (must not overlap host networks)")
	ipMode := fs.String("ip", "static", `"static" or "dhcp"`)
	dns := fs.String("dns", "", "nameserver for the guest probe (default: the first host DNS server)")
	keep := fs.Bool("keep", false, "do not delete the network and endpoint afterwards")
	netOnly := fs.Bool("net-only", false, "only create and delete the network and endpoint; no VM")
	typ := fs.String("type", "NAT", "HNS network type (NAT, ICS, Internal)")
	verbose := fs.Bool("v", false, "print the whole serial console")
	reach := fs.String("reach", "", "comma-separated host:port targets the guest dials (VPN checks)")
	bulk := fs.Uint("bulk", 0, "guest TCP port for the bulk throughput test (0 = skip)")
	dnsTry := fs.String("dnstry", "", "extra comma-separated DNS servers the guest tries with nslookup")
	_ = fs.Parse(args)

	if err := hcn.Load(); err != nil {
		return fmt.Errorf("load computenetwork.dll: %w", err)
	}
	before, err := hcn.EnumerateNetworks("")
	if err != nil {
		return err
	}
	fmt.Printf("networks before: %v\n", before)

	p, err := netip.ParsePrefix(*prefix)
	if err != nil {
		return err
	}
	p = p.Masked()
	gw := p.Addr().Next()
	guestIP := netip.PrefixFrom(gw.Next(), p.Bits())

	n, err := hcn.NATNetwork("hyperbutler-spike-nat", p, gw)
	if err != nil {
		return err
	}
	n.Type = *typ
	// Record each ID before the object exists, so a crash between create and record
	// cannot leave an object that cleanup does not know about.
	netID, err := hcn.NewID()
	if err != nil {
		return err
	}
	if err := record("network", netID); err != nil {
		return err
	}
	if !*keep {
		defer cleanupAndVerify(before)
		// Go on Windows exits on Ctrl+C without running defers.
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		defer signal.Stop(sig)
		go func() {
			if _, ok := <-sig; ok {
				fmt.Println("interrupted")
				cleanupAndVerify(before)
				os.Exit(1)
			}
		}()
	}
	t := time.Now()
	_, err = hcn.CreateNetwork(netID, n)
	fmt.Printf("HcnCreateNetwork(type=%s %v gw %v): %s, err=%v\n", n.Type, p, gw, ms(time.Since(t)), err)
	if err != nil {
		forget(netID)
		return err
	}
	doc, err := hcn.NetworkProperties(netID)
	if err != nil {
		return err
	}
	fmt.Println("network:", doc)

	t = time.Now()
	ep, err := hcn.StaticEndpoint("hyperbutler-spike-ep", netID, guestIP, gw, nil)
	if err != nil {
		return err
	}
	epID, err := hcn.NewID()
	if err != nil {
		return err
	}
	if err := record("endpoint", epID); err != nil {
		return err
	}
	_, err = hcn.CreateEndpoint(epID, netID, ep)
	fmt.Printf("HcnCreateEndpoint(%v): %s, err=%v\n", guestIP, ms(time.Since(t)), err)
	if err != nil {
		forget(epID)
		return err
	}
	edoc, err := hcn.EndpointProperties(epID)
	if err != nil {
		return err
	}
	fmt.Println("endpoint:", edoc)
	e, err := hcn.ParseEndpoint(edoc)
	if err != nil {
		return err
	}
	if *netOnly {
		return nil
	}

	roots, err := loadRoots()
	if err != nil {
		return err
	}
	initrd, err := buildInitrd(guestDir, probeBinary(), roots, nil)
	if err != nil {
		return err
	}
	if *dns == "" {
		*dns = hostDNS()
	}
	ipArg := guestIP.String()
	if *ipMode == "dhcp" {
		ipArg = "dhcp"
	}
	id := fmt.Sprintf("hyperbutler-net-spike-%d", time.Now().UnixNano())
	spec := vmSpec{
		id:     id,
		initrd: initrd,
		memMB:  512,
		cmdline: fmt.Sprintf("console=ttyS0 rdinit=/wbinit loglevel=3 WB_NET=direct WB_IP=%s WB_GW=%s WB_DNS=%s WB_DNSTRY=%s,%s",
			ipArg, gw, *dns, gw, *dns) + extraCmdline(*dnsTry, *reach),
		adapters: map[string]any{
			"eth0": map[string]any{"EndpointId": e.ID, "MacAddress": e.MacAddress},
		},
	}
	if *bulk > 0 {
		spec.cmdline += fmt.Sprintf(" WB_BULK=%d", *bulk)
	}
	fmt.Printf("endpoint MAC %s, passed to HCS as the adapter MAC\n", e.MacAddress)
	dialBulk := func(string) (net.Conn, error) {
		return net.DialTimeout("tcp", netip.AddrPortFrom(guestIP.Addr(), uint16(*bulk)).String(), 5*time.Second)
	}
	return bootAndProbe(spec, 3*time.Minute, *verbose, nil, dialBulk)
}

// bootAndProbe boots spec and drives the guest. When the guest announces step=serving,
// dialBulk (if set) is used to time three bulk reads from it.
func bootAndProbe(spec vmSpec, timeout time.Duration, verbose bool, beforeStart func(runtimeID string) (func(), error), dialBulk func(runtimeID string) (net.Conn, error)) error {
	t := time.Now()
	v, runtimeID, err := createVM(spec, filepath.Join(guestDir, "vmlinuz-virt"))
	if err != nil {
		return err
	}
	defer v.stop()
	fmt.Printf("HcsCreateComputeSystem: %s (runtime id %s)\n", ms(time.Since(t)), runtimeID)
	if beforeStart != nil {
		stop, err := beforeStart(runtimeID)
		if err != nil {
			return err
		}
		defer stop()
	}
	if err := v.boot(`\\.\pipe\` + spec.id + "-com1"); err != nil {
		return err
	}
	fmt.Printf("HcsStartComputeSystem + console: %s\n", ms(time.Since(t)))
	var onResult func(probe.Result)
	if dialBulk != nil {
		onResult = func(r probe.Result) {
			if r.Step == "serving" && r.OK {
				go timeBulk(func() (net.Conn, error) { return dialBulk(runtimeID) })
			}
		}
	}
	res := v.drive(timeout, verbose, onResult)
	for _, r := range res {
		if r.Step == "done" && r.OK {
			return nil
		}
	}
	return fmt.Errorf("probe did not complete")
}

func cleanupAndVerify(before []string) {
	fmt.Println("cleanup:")
	if err := runCleanup(); err != nil {
		fmt.Println("  cleanup:", err)
	}
	nets, err := hcn.EnumerateNetworks("")
	if err != nil {
		fmt.Println("  verify:", err)
		return
	}
	eps, _ := hcn.EnumerateEndpoints("")
	fmt.Printf("  networks after: %v (before: %v)\n  endpoints after: %v\n", nets, before, eps)
}

func hostDNS() string {
	// The first IPv4 DNS server of an up adapter, via GetAdaptersAddresses.
	var size uint32 = 15000
	for i := 0; i < 3; i++ {
		buf := make([]byte, size)
		aa := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		err := windows.GetAdaptersAddresses(windows.AF_INET, 0, 0, aa, &size)
		if errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) {
			continue
		}
		if err != nil {
			break
		}
		for a := aa; a != nil; a = a.Next {
			if a.OperStatus != windows.IfOperStatusUp {
				continue
			}
			for d := a.FirstDnsServerAddress; d != nil; d = d.Next {
				if ip := d.Address.IP(); ip != nil && ip.To4() != nil {
					return ip.String()
				}
			}
		}
		break
	}
	return "1.1.1.1"
}

// runRelay boots a VM with no network adapter. The guest reaches the internet only
// through a CONNECT relay on an hvsock listener in this process.
func runRelay(args []string) error {
	fs := flag.NewFlagSet("relay", flag.ExitOnError)
	port := fs.Uint("port", 5099, "AF_VSOCK port the guest connects to")
	verbose := fs.Bool("v", false, "print the whole serial console")
	logTunnels := fs.Bool("log", false, "log each tunnel")
	reach := fs.String("reach", "", "comma-separated host:port targets the guest dials (VPN checks)")
	bulk := fs.Uint("bulk", 0, "guest vsock port for the bulk throughput test (0 = skip)")
	_ = fs.Parse(args)

	roots, err := loadRoots()
	if err != nil {
		return err
	}
	mods := []string{
		filepath.Join(modDir, "net", "vmw_vsock", "vsock.ko.gz"),
		filepath.Join(modDir, "net", "vmw_vsock", "hv_sock.ko.gz"),
	}
	initrd, err := buildInitrd(guestDir, probeBinary(), roots, mods)
	if err != nil {
		return err
	}
	svc := winio.VsockServiceID(uint32(*port))
	sd, err := userSD()
	if err != nil {
		return err
	}
	id := fmt.Sprintf("hyperbutler-net-spike-%d", time.Now().UnixNano())
	spec := vmSpec{
		id:      id,
		initrd:  initrd,
		memMB:   512,
		cmdline: fmt.Sprintf("console=ttyS0 rdinit=/wbinit loglevel=3 WB_NET=relay WB_PORT=%d WB_MODS=vsock.ko,hv_sock.ko", *port) + extraCmdline("", *reach),
		hvsocket: map[string]any{
			"HvSocketConfig": map[string]any{
				"ServiceTable": map[string]any{
					svc.String(): map[string]any{
						"BindSecurityDescriptor":    sd,
						"ConnectSecurityDescriptor": sd,
						"AllowWildcardBinds":        false,
					},
				},
			},
		},
	}
	if *bulk > 0 {
		table := spec.hvsocket["HvSocketConfig"].(map[string]any)["ServiceTable"].(map[string]any)
		table[winio.VsockServiceID(uint32(*bulk)).String()] = map[string]any{
			"BindSecurityDescriptor":    sd,
			"ConnectSecurityDescriptor": sd,
		}
	}
	srv := &relay.Server{}
	if *logTunnels {
		srv.Logf = func(f string, a ...any) { fmt.Printf("  host: "+f+"\n", a...) }
	}
	var dialBulk func(string) (net.Conn, error)
	if *bulk > 0 {
		spec.cmdline += fmt.Sprintf(" WB_BULK=%d", *bulk)
		dialBulk = func(runtimeID string) (net.Conn, error) {
			vmID, err := guid.FromString(strings.Trim(runtimeID, "{}"))
			if err != nil {
				return nil, err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return winio.Dial(ctx, &winio.HvsockAddr{VMID: vmID, ServiceID: winio.VsockServiceID(uint32(*bulk))})
		}
	}
	return bootAndProbe(spec, 3*time.Minute, *verbose, func(runtimeID string) (func(), error) {
		vmID, err := guid.FromString(strings.Trim(runtimeID, "{}"))
		if err != nil {
			return nil, fmt.Errorf("runtime id %q: %w", runtimeID, err)
		}
		t := time.Now()
		ln, err := winio.ListenHvsock(&winio.HvsockAddr{VMID: vmID, ServiceID: svc})
		fmt.Printf("ListenHvsock(%s, %s): %s, err=%v\n", vmID, svc, ms(time.Since(t)), err)
		if err != nil {
			return nil, err
		}
		go func() { _ = srv.Serve(ln) }()
		return func() { ln.Close() }, nil
	}, dialBulk)
}

// timeBulk reads three bulk transfers from the guest and prints the throughput.
func timeBulk(dial func() (net.Conn, error)) {
	for i := 0; i < 3; i++ {
		t := time.Now()
		c, err := dial()
		if err != nil {
			fmt.Printf("  host: bulk %d: dial: %v\n", i, err)
			return
		}
		n, err := io.Copy(io.Discard, c)
		el := time.Since(t)
		c.Close()
		fmt.Printf("  host: bulk %d: %d bytes in %s = %.2f Gbit/s (err=%v)\n", i, n, ms(el), float64(n)*8/el.Seconds()/1e9, err)
	}
}

// userSD is an SDDL that grants only the current user.
func userSD() (string, error) {
	tok := windows.GetCurrentProcessToken()
	u, err := tok.GetTokenUser()
	if err != nil {
		return "", err
	}
	return "D:P(A;;FA;;;" + u.User.Sid.String() + ")", nil
}

// extraCmdline appends the optional guest variables. Values must not contain spaces.
func extraCmdline(dnsTry, reach string) string {
	s := ""
	if dnsTry != "" {
		s += " WB_DNSTRY2=" + dnsTry
	}
	if reach != "" {
		s += " WB_REACH=" + reach
		// The first name-based target is also looked up with nslookup on every server.
		for _, t := range strings.Split(reach, ",") {
			if h, _, err := net.SplitHostPort(t); err == nil {
				if _, err := netip.ParseAddr(h); err != nil {
					s += " WB_NSNAME=" + h
					break
				}
			}
		}
	}
	return s
}
