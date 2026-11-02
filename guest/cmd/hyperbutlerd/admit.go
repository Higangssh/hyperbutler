package main

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/Higangssh/hyperbutler/internal/vmsecret"
)

// hostSessions admits the host's control connections. Each must begin with the VM's
// secret (ADR 0004 rule 6); only one that does replaces the current connection (the host
// reconnected), so a peer without the secret can neither be served nor end the host's
// session. Connections are checked concurrently through a bounded vmsecret.Gate, so
// peers that connect and send nothing hold up neither the accept loop nor the host, and
// cannot pile up goroutines. A refused connection is closed without a byte written to it.
type hostSessions struct {
	// gate holds the secret in a field fmt would print raw; Format below keeps
	// hostSessions from ever printing it.
	gate vmsecret.Gate
	// start serves an admitted connection until ctx ends; it must not block.
	start func(ctx context.Context, c net.Conn)

	mu     sync.Mutex
	cancel context.CancelFunc
}

// Format never prints the secret.
func (h *hostSessions) Format(f fmt.State, _ rune) { _, _ = fmt.Fprint(f, "hostSessions{}") }

// admit checks c's secret in the background and, if it is right, ends the previous
// session and starts c's; refused, if not nil, gets the error of a refused c.
func (h *hostSessions) admit(c net.Conn, refused func(error)) {
	h.gate.Go(c, func(err error) {
		if err != nil {
			if refused != nil {
				refused(err)
			}
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.cancel != nil {
			h.cancel()
		}
		ctx, cancel := context.WithCancel(context.Background())
		h.cancel = cancel
		h.start(ctx, c)
	})
}
