//go:build windows

package winipc

import (
	"errors"
	"net"
	"testing"
	"time"
)

// A Close that cancels an operation before its goroutine waits for it must still be
// reported as net.ErrClosed, not as the raw abort status. The hook stalls each pending
// operation until Close has run, so the completion and the closed channel are both ready
// when the operation selects.
func TestCloseBeforeWaitIsErrClosed(t *testing.T) {
	for _, kind := range []string{"pipe", "hvsock"} {
		t.Run(kind, func(t *testing.T) {
			const n = 20
			for i := 0; i < n; i++ {
				var c net.Conn
				if kind == "pipe" {
					c, _ = pipePair(t, true)
				} else {
					c, _ = hvPair(t)
				}
				closed := make(chan struct{})
				hook := func() { <-closed; time.Sleep(10 * time.Millisecond) }
				testHookPending.Store(&hook)
				errc := make(chan error, 1)
				go func() {
					_, err := c.Read(make([]byte, 8))
					errc <- err
				}()
				time.Sleep(20 * time.Millisecond)
				go func() { c.Close() }()
				// Close waits for the Read; let the hook go once Close has canceled.
				time.Sleep(20 * time.Millisecond)
				close(closed)
				err := <-errc
				testHookPending.Store(nil)
				if !errors.Is(err, net.ErrClosed) {
					t.Fatalf("iteration %d: Read = %v, want net.ErrClosed", i, err)
				}
			}
		})
	}
}
