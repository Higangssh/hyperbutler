package transport

import (
	"fmt"
	"net"
	"time"

	"github.com/Higangssh/hyperbutler/internal/vmsecret"
)

// secretWriteTimeout bounds the write of the VM secret on a new connection: 32 bytes
// into an empty socket buffer.
const secretWriteTimeout = 5 * time.Second

// SendSecret writes the VM secret as the first bytes of c (ADR 0004 rule 6): on every
// connection the host dials to the guest (HvsockDialer), and on every guest connection
// a host listener accepts (the share server, the plain network link), which the guest
// reads and checks before it writes anything. A zero secret is refused.
func SendSecret(c net.Conn, s vmsecret.Secret) error {
	if err := c.SetWriteDeadline(time.Now().Add(secretWriteTimeout)); err != nil {
		return err
	}
	if err := vmsecret.Send(c, s); err != nil {
		return fmt.Errorf("transport: send the VM secret: %w", err)
	}
	return c.SetWriteDeadline(time.Time{})
}
