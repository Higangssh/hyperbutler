//go:build windows

package transport

import (
	"context"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
)

// HvsockDialer dials an AF_HYPERV socket in a utility VM, typically the guest's dockerd
// reached through a vsock port (see VsockServiceID). It is safe for concurrent use.
type HvsockDialer struct {
	Target HvsockTarget
	// Retries is how many extra attempts to make when the connect times out, is
	// refused, or the VM is unreachable (for example while the guest is still booting).
	Retries uint
	// RetryWait is the pause between attempts.
	RetryWait time.Duration
}

// DialContext connects to d.Target. The returned connection supports CloseWrite.
func (d *HvsockDialer) DialContext(ctx context.Context) (net.Conn, error) {
	// winio.HvsockDialer keeps a retry timer in the struct, so it is not safe to share
	// between goroutines; build one per dial.
	wd := &winio.HvsockDialer{Retries: d.Retries, RetryWait: d.RetryWait}
	c, err := wd.Dial(ctx, &winio.HvsockAddr{VMID: d.Target.VMID, ServiceID: d.Target.ServiceID})
	if err != nil {
		return nil, err
	}
	return c, nil
}
