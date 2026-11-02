package main

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/Higangssh/hyperbutler/internal/vmsecret"
)

// hostSessions admits the host's control connections. Each must begin with the VM's
// secret (ADR 0004 rule 6); only one that does replaces the current connection (the host
// reconnected), so a peer without the secret can neither be served nor end the host's
// session. A refused connection is closed without a byte written to it.
type hostSessions struct {
	secret  vmsecret.Secret
	timeout time.Duration // vmsecret.DefaultTimeout if zero
	// start serves an admitted connection until ctx ends; it must not block.
	start func(ctx context.Context, c net.Conn)

	mu     sync.Mutex
	cancel context.CancelFunc
}

// admit checks c's secret and, if it is right, ends the previous session and starts c's.
// It blocks for at most the timeout; run it on its own goroutine.
func (h *hostSessions) admit(c net.Conn) error {
	if err := vmsecret.Admit(c, h.secret, h.timeout); err != nil {
		_ = c.Close()
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cancel != nil {
		h.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.start(ctx, c)
	return nil
}
