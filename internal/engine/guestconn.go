package engine

import (
	"log/slog"
	"net"
	"sync/atomic"
	"time"
)

// maxAcceptBackoff caps the wait after a failed accept on a host listener the guest
// connects to (the share server, the plain network link). After it connects, the guest
// waits vmsecret.DefaultTimeout for the listener's VM secret (ADR 0004 rule 6) and
// refuses the listener without it; a connection that arrives while the accept loop
// sleeps must still get the secret in time, so the cap stays well below that timeout.
const maxAcceptBackoff = 250 * time.Millisecond

// nextAcceptBackoff is the wait after another failed accept, from the previous one.
func nextAcceptBackoff(prev time.Duration) time.Duration {
	return min(max(2*prev, 5*time.Millisecond), maxAcceptBackoff)
}

// guestConn records whether the guest sent anything on a connection a host listener
// accepted.
type guestConn struct {
	net.Conn
	spoke atomic.Bool
}

func (c *guestConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 && !c.spoke.Load() {
		c.spoke.Store(true)
	}
	return n, err
}

// silentGuestHint explains a guest connection that ended before the guest sent a byte.
const silentGuestHint = "the guest closed the connection after the VM secret without sending anything, " +
	"which is how it refuses a listener whose secret it does not accept: check that the guest image " +
	"matches this engine and that its kernel command line carries hyperbutler.secret (hyperbutler logs --console)"

// silentGuests logs the connections a guest closed without sending a byte. The first is
// a warning with silentGuestHint; the later ones (the guest redials every few seconds)
// are debug, so a mismatched image does not fill the log.
type silentGuests struct{ warned atomic.Bool }

func (w *silentGuests) log(l *slog.Logger, msg string, err error) {
	if w.warned.CompareAndSwap(false, true) {
		l.Warn(msg, "err", err, "hint", silentGuestHint)
		return
	}
	l.Debug(msg, "err", err, "hint", silentGuestHint)
}
