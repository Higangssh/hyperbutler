//go:build windows

package transport

import (
	"context"
	"net"
	"time"

	"github.com/Higangssh/hyperbutler/internal/winipc"
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

// DialContext connects to d.Target, and sends the target's secret first if it has one.
// The returned connection supports CloseWrite. It returns as soon as ctx ends, even
// while a connect to a guest port nobody listens on yet (a booting guest) is pending.
func (d *HvsockDialer) DialContext(ctx context.Context) (net.Conn, error) {
	wd := winipc.HvsockDialer{Retries: d.Retries, RetryWait: d.RetryWait}
	c, err := wd.Dial(ctx, winipc.HvsockAddr{VMID: d.Target.VMID, ServiceID: d.Target.ServiceID})
	if err != nil {
		return nil, err
	}
	if d.Target.Secret.IsZero() {
		return c, nil
	}
	// The secret is a few bytes on a fresh connection and goes at once; ctx still
	// bounds it, by expiring the write.
	stop := context.AfterFunc(ctx, func() { _ = c.SetWriteDeadline(time.Unix(1, 0)) })
	err = SendSecret(c, d.Target.Secret)
	if !stop() {
		_ = c.Close()
		return nil, ctx.Err()
	}
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}
