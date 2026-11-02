package main

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/vmsecret"
)

// A control connection is served only with the VM's secret first, and only such a
// connection replaces the host's current session: a peer without it cannot end it.
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
	h := &hostSessions{secret: secret, timeout: 200 * time.Millisecond,
		start: func(ctx context.Context, c net.Conn) { ch <- started{ctx, c} }}

	connect := func(send []byte) (net.Conn, chan error) {
		host, guest := net.Pipe()
		t.Cleanup(func() { host.Close(); guest.Close() })
		errc := make(chan error, 1)
		go func() { errc <- h.admit(guest) }()
		if send != nil {
			if _, err := host.Write(send); err != nil {
				t.Fatal(err)
			}
		}
		return host, errc
	}

	// The host's first session.
	_, errc := connect(secret[:])
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	first := <-ch

	// Wrong, short and missing secrets: refused, nothing started, the session stays.
	wrong := secret
	wrong[vmsecret.Size-1] ^= 1
	for name, send := range map[string][]byte{"wrong": wrong[:], "short": secret[:8], "none": nil} {
		host, errc := connect(send)
		if err := <-errc; !errors.Is(err, vmsecret.ErrMismatch) {
			t.Fatalf("%s: %v", name, err)
		}
		_ = host.SetReadDeadline(time.Now().Add(time.Second))
		if n, err := host.Read(make([]byte, 8)); n != 0 || err == nil {
			t.Fatalf("%s: the peer read %d bytes, %v", name, n, err)
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
	_, errc = connect(secret[:])
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	second := <-ch
	if first.ctx.Err() == nil || second.ctx.Err() != nil {
		t.Fatal("the new session did not replace the old one")
	}

	// A guest without a secret admits no one.
	none := &hostSessions{start: func(context.Context, net.Conn) { t.Error("started without a secret") }}
	host, guest := net.Pipe()
	defer host.Close()
	errc = make(chan error, 1)
	go func() { errc <- none.admit(guest) }()
	if _, err := host.Write(make([]byte, vmsecret.Size)); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; !errors.Is(err, vmsecret.ErrMismatch) {
		t.Fatalf("zero secret: %v", err)
	}
}
