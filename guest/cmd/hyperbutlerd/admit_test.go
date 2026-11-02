package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/vmsecret"
)

// A control connection is served only with the VM's secret first, and only such a
// connection replaces the host's current session: a peer without it cannot end it, and
// a stalled peer does not delay the host.
func TestHostSessions(t *testing.T) {
	secret, err := vmsecret.New()
	if err != nil {
		t.Fatal(err)
	}
	type started struct {
		ctx context.Context
		c   net.Conn
	}
	ch := make(chan started, 4)
	h := &hostSessions{gate: vmsecret.Gate{Secret: secret, Timeout: 300 * time.Millisecond, MaxPending: 4},
		start: func(ctx context.Context, c net.Conn) { ch <- started{ctx, c} }}
	refused := make(chan error, 8)

	connect := func(send []byte) net.Conn {
		host, guest := net.Pipe()
		t.Cleanup(func() { host.Close(); guest.Close() })
		h.admit(guest, func(err error) { refused <- err })
		if send != nil {
			go func() { _, _ = host.Write(send) }()
		}
		return host
	}

	// A stalled peer first, then the host: the host's session starts at once.
	stalled := connect(secret[:8])
	t0 := time.Now()
	connect(secret[:])
	first := <-ch
	if d := time.Since(t0); d > 200*time.Millisecond {
		t.Fatalf("the host waited %v behind a stalled peer", d)
	}
	if err := <-refused; !errors.Is(err, vmsecret.ErrMismatch) {
		t.Fatalf("stalled peer: %v", err)
	}
	_ = stalled.SetReadDeadline(time.Now().Add(time.Second))
	if n, err := stalled.Read(make([]byte, 8)); n != 0 || err == nil {
		t.Fatalf("the stalled peer read %d bytes, %v", n, err)
	}

	// Wrong and missing secrets: refused, nothing started, the session stays.
	wrong := secret
	wrong[vmsecret.Size-1] ^= 1
	for name, send := range map[string][]byte{"wrong": wrong[:], "none": nil} {
		connect(send)
		if err := <-refused; !errors.Is(err, vmsecret.ErrMismatch) {
			t.Fatalf("%s: %v", name, err)
		}
		select {
		case <-ch:
			t.Fatalf("%s: a session was started", name)
		default:
		}
		if first.ctx.Err() != nil {
			t.Fatalf("%s: a refused peer ended the host's session", name)
		}
	}

	// The host reconnecting with the secret replaces the session.
	connect(secret[:])
	second := <-ch
	if first.ctx.Err() == nil || second.ctx.Err() != nil {
		t.Fatal("the new session did not replace the old one")
	}
	if s := fmt.Sprintf("%v %+v", h, h); strings.Contains(s, secret.Hex()[:8]) {
		t.Fatalf("hostSessions printed its secret: %s", s)
	}

	// A guest without a secret admits no one.
	none := &hostSessions{start: func(context.Context, net.Conn) { t.Error("started without a secret") }}
	host, guest := net.Pipe()
	defer host.Close()
	errc := make(chan error, 1)
	none.admit(guest, func(err error) { errc <- err })
	if _, err := host.Write(make([]byte, vmsecret.Size)); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; !errors.Is(err, vmsecret.ErrMismatch) {
		t.Fatalf("zero secret: %v", err)
	}
}
