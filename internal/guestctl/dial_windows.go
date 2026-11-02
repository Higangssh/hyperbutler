//go:build windows

package guestctl

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio/pkg/guid"

	"github.com/Higangssh/hyperbutler/internal/transport"
	"github.com/Higangssh/hyperbutler/internal/vmsecret"
)

// ServiceID is the hvsock service GUID of the control port. The VM's HCS document must
// list it in its HvSocket service table (vm.Service) for the host to be allowed to
// connect.
var ServiceID = transport.VsockServiceID(ControlPort)

// HvsockDialer returns a dial function for the control service of the VM with the given
// runtime ID (vm.System.RuntimeID), for use as Supervisor.Dial. Every connection starts
// with the VM's secret, which hyperbutlerd requires before it serves anything (ADR 0004
// rule 6).
func HvsockDialer(vmID guid.GUID, secret vmsecret.Secret) func(ctx context.Context) (net.Conn, error) {
	d := &transport.HvsockDialer{Target: transport.HvsockTarget{VMID: vmID, ServiceID: ServiceID, Secret: secret}}
	return d.DialContext
}
