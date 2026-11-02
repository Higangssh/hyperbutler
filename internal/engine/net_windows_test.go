//go:build windows

package engine

import (
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/netstack"
	"github.com/Higangssh/hyperbutler/internal/vm"
	"github.com/Higangssh/hyperbutler/internal/vmsecret"
	"github.com/Higangssh/hyperbutler/internal/vmtemplate"
)

// netDoc builds the VM configuration of a VM with network nc and nothing else, and
// returns the network link's service and the kernel command line.
func netDoc(t *testing.T, nc NetConfig) (*guestNet, vm.Service, string) {
	t.Helper()
	g, n, err := setupNet(nc, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	c, err := vmtemplate.Build(&vmtemplate.Spec{UserSID: testSID, Secret: testVMSecret, MemoryMB: 1024, CPUs: 1, Kernel: `C:\k`, Initrd: `C:\i`, Net: n})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Services) != 3 {
		t.Fatalf("services %+v", c.Services)
	}
	return g, c.Services[2], c.KernelCmdLine
}

const testSID = "S-1-5-21-1-2-3-1001"

// Offload mode (ADR 0006): the host dials netfwd -vnet, so the link's service must admit
// the user's connect as well as its listen, and the guest must be told to listen.
func TestSetupNetOffload(t *testing.T) {
	sddl, _ := vm.UserOnlySDDL(testSID)
	for _, off := range []bool{false, true} {
		g, svc, args := netDoc(t, NetConfig{NoOffload: !off})
		g.st.Close()
		if svc.BindSDDL != sddl {
			t.Fatalf("service %+v", svc)
		}
		if got := svc.ConnectSDDL == sddl; got != off {
			t.Errorf("offload %v: connect SDDL %q", off, svc.ConnectSDDL)
		}
		if got := strings.Contains(args, " hyperbutler.netvnet=1"); got != off {
			t.Errorf("offload %v: kernel arguments %q", off, args)
		}
	}
}

// The guest is told the link's CRC mode (ADR 0006, "Integrity"); the default says
// nothing, and a plain link has no CRC to tell.
func TestSetupNetCRC(t *testing.T) {
	for _, tc := range []struct {
		mode      netstack.CRCMode
		noOffload bool
		want      string
	}{
		{netstack.CRCOn, false, ""},
		{netstack.CRCOff, false, " hyperbutler.netcrc=0"},
		{netstack.CRCStrict, false, " hyperbutler.netcrc=strict"},
		{netstack.CRCOff, true, ""},
	} {
		g, _, args := netDoc(t, NetConfig{CRC: tc.mode, NoOffload: tc.noOffload})
		if g.st.Config().LinkCRC != tc.mode {
			t.Errorf("%v: the stack's mode is %v", tc.mode, g.st.Config().LinkCRC)
		}
		g.st.Close()
		if got := strings.Contains(args, "hyperbutler.netcrc"); got != (tc.want != "") || (tc.want != "" && !strings.Contains(args, tc.want)) {
			t.Errorf("%v (no offload %v): kernel arguments %q, want %q", tc.mode, tc.noOffload, args, tc.want)
		}
	}
}

// REQ-NET-004: with a host gateway, on or off, the guest is told its address (dockerd's
// --host-gateway-ip), so turning it off or on needs no dockerd restart.
func TestSetupNetHostGateway(t *testing.T) {
	for _, hg := range []*netstack.HostGateway{nil, {Enabled: true}, {Enabled: false}} {
		g, _, args := netDoc(t, NetConfig{HostGateway: hg})
		a, cur := g.st.HostGateway()
		g.st.Close()
		has := strings.Contains(args, " hyperbutler.hostgw=192.168.127.254")
		if has != (hg != nil) || a.IsValid() != (hg != nil) {
			t.Errorf("host gateway %+v: args %q, address %s", hg, args, a)
		}
		if hg != nil && cur.Enabled != hg.Enabled {
			t.Errorf("host gateway %+v: in effect %+v", hg, cur)
		}
	}
}

// Review finding 1: a port a host program already listens on (on the wildcard address)
// must not be auto-published, or the 127.0.0.1 bind would take its loopback clients.
func TestPortInUseRefusesHostListener(t *testing.T) {
	// A wildcard bind makes Windows Firewall prompt for every freshly built engine.test.exe,
	// so it runs only where asked for (CI sets this).
	if os.Getenv("HYPERBUTLER_TEST_WILDCARD") != "1" {
		t.Skip("binds the wildcard address; set HYPERBUTLER_TEST_WILDCARD=1 to run")
	}
	// IPv4-only, IPv6-only and dual-stack wildcard listeners, as other programs create.
	for _, network := range []string{"tcp4", "tcp6", "tcp"} {
		addr := "0.0.0.0:0"
		if network == "tcp6" {
			if !ipv6Available() {
				continue
			}
			addr = "[::]:0"
		}
		ln, err := net.Listen(network, addr)
		if err != nil {
			t.Fatal(err)
		}
		p := uint16(ln.Addr().(*net.TCPAddr).Port)
		// Self 0: the listener is this test process's own, which hostPortFree counts as
		// the engine's.
		if err := hostPortFreeExcept(p, 0); err == nil {
			t.Errorf("%s: port %d held by a host listener was reported free", network, p)
		}
		ln.Close()
		if err := hostPortFreeExcept(p, 0); err != nil {
			t.Errorf("%s: free port %d reported in use: %v", network, p, err)
		}
	}
}

// ADR 0004 (Two-account run): a plain-mode link the host accepts begins with the VM
// secret, which netfwd checks before it sends a frame, so a listener without it (another
// user's, on a wildcard VM id) never carries the guest's network. Without a secret the
// host closes the link having written nothing.
func TestServePlainSendsSecretFirst(t *testing.T) {
	for _, secret := range []vmsecret.Secret{testVMSecret, {}} {
		g, _, _ := netDoc(t, NetConfig{NoOffload: true})
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		up := make(chan struct{}, 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			g.servePlain(t.Context(), ln, secret, func() { up <- struct{}{} })
		}()
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if secret.IsZero() {
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			if b, err := io.ReadAll(c); len(b) != 0 {
				t.Fatalf("a host without a secret wrote %d byte(s) (%v)", len(b), err)
			}
			select {
			case <-up:
				t.Fatal("the link came up without a secret")
			default:
			}
		} else {
			if err := vmsecret.Verify(c, secret); err != nil {
				t.Fatalf("the host did not send the VM secret first: %v", err)
			}
			select {
			case <-up:
			case <-time.After(5 * time.Second):
				t.Fatal("the link never came up")
			}
		}
		_ = c.Close()
		_ = ln.Close()
		<-done
		g.st.Close()
	}
}
