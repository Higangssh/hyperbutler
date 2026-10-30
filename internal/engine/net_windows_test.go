//go:build windows

package engine

import (
	"net"
	"testing"
)

// Review finding 1: a port a host program already listens on (on the wildcard address)
// must not be auto-published, or the 127.0.0.1 bind would take its loopback clients.
func TestPortInUseRefusesHostListener(t *testing.T) {
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
		if err := portInUse(p); err == nil {
			t.Errorf("%s: port %d held by a host listener was reported free", network, p)
		}
		ln.Close()
		if err := portInUse(p); err != nil {
			t.Errorf("%s: free port %d reported in use: %v", network, p, err)
		}
	}
}
