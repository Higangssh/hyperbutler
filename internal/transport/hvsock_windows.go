//go:build windows

package transport

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/Microsoft/go-winio"

	"github.com/Higangssh/hyperbutler/internal/vmsecret"
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

// secretWriteTimeout bounds the write of the VM secret on a new connection: 32 bytes
// into an empty socket buffer.
const secretWriteTimeout = 5 * time.Second

// sendSecret writes the VM secret as the first bytes of c (ADR 0004 rule 6).
func sendSecret(c net.Conn, s vmsecret.Secret) error {
	if err := c.SetWriteDeadline(time.Now().Add(secretWriteTimeout)); err != nil {
		return err
	}
	if err := vmsecret.Send(c, s); err != nil {
		return fmt.Errorf("transport: send the VM secret: %w", err)
	}
	return c.SetWriteDeadline(time.Time{})
}

// DialContext connects to d.Target, and sends the target's secret first if it has one.
// The returned connection supports CloseWrite.
func (d *HvsockDialer) DialContext(ctx context.Context) (net.Conn, error) {
	// winio.HvsockDialer keeps a retry timer in the struct, so it is not safe to share
	// between goroutines; build one per dial.
	wd := &winio.HvsockDialer{Retries: d.Retries, RetryWait: d.RetryWait}
	addr := &winio.HvsockAddr{VMID: d.Target.VMID, ServiceID: d.Target.ServiceID}

	// go-winio checks ctx only between attempts, not while ConnectEx is pending, and a
	// connect to a guest port nobody listens on yet (a booting guest) stays pending
	// until the hvsock connect timeout, about 30 s. Return as soon as ctx is done and
	// let the abandoned attempt finish in the background; a connection it still
	// produces is closed.
	type result struct {
		c   net.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := wd.Dial(ctx, addr)
		if err != nil {
			ch <- result{nil, err}
			return
		}
		if !d.Target.Secret.IsZero() {
			if err := sendSecret(c, d.Target.Secret); err != nil {
				_ = c.Close()
				ch <- result{nil, err}
				return
			}
		}
		ch <- result{c, nil}
	}()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.c != nil {
				_ = r.c.Close()
			}
		}()
		return nil, ctx.Err()
	}
}
