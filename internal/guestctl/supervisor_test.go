package guestctl

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/spaceguard"
)

// A guest that completes the handshake and drops at once must not get redialed at the
// minimum backoff forever (review M2).
func TestSupervisorBacksOffFlappingGuest(t *testing.T) {
	var mu sync.Mutex
	dials := 0
	sup := &Supervisor{
		Dial: func(ctx context.Context) (net.Conn, error) {
			mu.Lock()
			dials++
			mu.Unlock()
			a, b := net.Pipe()
			go func() {
				var buf [MaxBody]byte
				if _, err := ReadFrame(b, &buf); err == nil {
					hr, _ := Encode(0, HelloReply{Version: 1})
					b.Write(hr)
				}
				b.Close()
			}()
			return a, nil
		},
		MinBackoff: 10 * time.Millisecond,
		MaxBackoff: 400 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	sup.Run(ctx)
	mu.Lock()
	defer mu.Unlock()
	// 10+20+40+...+400 ms: about 9 dials in 1.5 s. Without escalation it would be ~150.
	if dials > 12 {
		t.Fatalf("%d dials in 1.5 s", dials)
	}
}

// While the host has the VM paused, a silent guest must not lose its connection, and
// budgets are held back until resume (review H2).
func TestSupervisorPausedKeepsConnection(t *testing.T) {
	h := &fakeHandler{budgetHit: make(chan struct{}, 4)}
	// A huge StatusEvery: after the first report the guest says nothing, like a
	// paused VM.
	srv := &Server{Handler: h, StatusEvery: time.Hour}
	sup := &Supervisor{
		Dial: func(ctx context.Context) (net.Conn, error) {
			a, b := net.Pipe()
			go srv.ServeConn(context.Background(), b)
			return a, nil
		},
		Config: ClientConfig{ReadTimeout: 100 * time.Millisecond},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup.SetPaused(true)
	go sup.Run(ctx)
	for sup.Client() == nil {
		time.Sleep(5 * time.Millisecond)
	}
	sup.SetBudget(spaceguard.Budget{Bytes: 3})
	time.Sleep(400 * time.Millisecond) // 4x the read timeout
	if n := sup.Connections(); n != 1 {
		t.Fatalf("%d connections while paused", n)
	}
	h.mu.Lock()
	nb := len(h.budgets)
	h.mu.Unlock()
	if nb != 0 {
		t.Fatalf("budget sent while paused")
	}
	sup.SetPaused(false)
	select {
	case <-h.budgetHit:
	case <-time.After(2 * time.Second):
		t.Fatal("no budget after resume")
	}
}
