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
