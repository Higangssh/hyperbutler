package guestctl

import (
	"context"
	"errors"
	"testing"
	"time"
)

// shutdownHandler is a fakeHandler that can power the guest off.
type shutdownHandler struct {
	*fakeHandler
	calls int
	err   error
}

func (h *shutdownHandler) Shutdown() error { h.calls++; return h.err }

func TestShutdownRoundTrip(t *testing.T) {
	for _, m := range []Msg{ShutdownRequest{}, ShutdownAccepted{}} {
		f, err := Encode(9, m)
		if err != nil {
			t.Fatal(err)
		}
		dir := FromHost
		if m.Type() == RShutdown {
			dir = FromGuest
		}
		seq, got, err := Decode(f[4:], dir)
		if err != nil || seq != 9 || got != m {
			t.Fatalf("%v: %v %v %v", m.Type(), seq, got, err)
		}
		// Wrong direction and a stray payload byte are rejected.
		if _, _, err := Decode(f[4:], 1-dir); err == nil {
			t.Fatalf("%v accepted in the wrong direction", m.Type())
		}
		if _, _, err := Decode(append(f[4:], 0), dir); err == nil {
			t.Fatalf("%v accepted with a payload", m.Type())
		}
	}
}

func TestShutdownServed(t *testing.T) {
	h := &shutdownHandler{fakeHandler: &fakeHandler{}}
	c, _ := startPair(t, h.fakeHandler, &Server{Features: FeatureShutdown}, ClientConfig{})
	// startPair installs the plain fakeHandler; the request is refused without Shutdowner.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var re *RefusedError
	if err := c.Shutdown(ctx); !errors.As(err, &re) || re.Code != ErrUnsupported {
		t.Fatalf("handler without Shutdowner: %v", err)
	}

	srv := &Server{Features: FeatureShutdown}
	c2, _ := startPairHandler(t, h, srv)
	if err := c2.Shutdown(ctx); err != nil || h.calls != 1 {
		t.Fatalf("shutdown: %v, %d calls", err, h.calls)
	}
	// A guest without the feature refuses even with a capable handler.
	c3, _ := startPairHandler(t, h, &Server{})
	if err := c3.Shutdown(ctx); !errors.As(err, &re) || re.Code != ErrUnsupported || h.calls != 1 {
		t.Fatalf("feature off: %v, %d calls", err, h.calls)
	}
	// A handler that fails to start the shutdown answers unsupported.
	h.err = errors.New("no init")
	c4, _ := startPairHandler(t, h, &Server{Features: FeatureShutdown})
	if err := c4.Shutdown(ctx); !errors.As(err, &re) {
		t.Fatalf("failing handler: %v", err)
	}
}

func TestSupervisorShutdownWithoutConnection(t *testing.T) {
	sup := &Supervisor{}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := sup.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("no connection: %v", err)
	}
}
