package main

import (
	"time"

	"github.com/Higangssh/hyperbutler/internal/vmsecret"
)

// hostConn is a connection the host opened to the agent.
type hostConn interface {
	stream
	SetReadDeadline(t time.Time) error
}

// dialError is a failure to reach dockerd, which is expected while it starts.
type dialError struct{ err error }

func (e dialError) Error() string { return e.err.Error() }
func (e dialError) Unwrap() error { return e.err }

// maxPendingAdmits bounds the connections whose secret is still being read, apart from
// the relays: peers that connect and send nothing cannot take the relays' slots.
const maxPendingAdmits = 16

// admitHost checks that c begins with the VM's secret (ADR 0004 rule 6) through g, which
// checks connections concurrently and a bounded number at a time, and then calls
// serve(c) on its own goroutine. A connection without the secret is closed before
// dockerd is dialed and before a byte is written to it, so a peer learns only that it
// was refused; refused gets the reason.
func admitHost(g *vmsecret.Gate, c hostConn, serve func(hostConn), refused func(error)) {
	g.Go(c, func(err error) {
		if err != nil {
			refused(err)
			return
		}
		serve(c)
	})
}

// relayHost relays an admitted c to the stream dial opens (dockerd's socket). It always
// closes c.
func relayHost(c hostConn, dial func() (stream, error)) error {
	u, err := dial()
	if err != nil {
		_ = c.Close()
		return dialError{err}
	}
	return relay(c, u)
}
