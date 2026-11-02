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

// serveHost admits c only if it begins with the VM's secret (ADR 0004 rule 6), and then
// relays it to the stream dial opens (dockerd's socket). A connection without the
// secret is closed before dockerd is dialed and before a byte is written to it, so a
// peer learns only that it was refused. serveHost always closes c.
func serveHost(c hostConn, want vmsecret.Secret, timeout time.Duration, dial func() (stream, error)) error {
	if err := vmsecret.Admit(c, want, timeout); err != nil {
		_ = c.Close()
		return err
	}
	u, err := dial()
	if err != nil {
		_ = c.Close()
		return dialError{err}
	}
	return relay(c, u)
}
